// Package ship uploads aggregated windows to the Kloudy platform.
//
// The agent only ever makes outbound requests. It listens on no port, so
// nothing on the network can reach it, and the customer's firewall stays shut.
package ship

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

// maxResponseBytes bounds what the agent will read back. The platform is
// trusted, but a trusted peer having a bad day should not be able to exhaust
// memory on every server in the fleet at once.
const maxResponseBytes = 64 << 10

// DefaultTimeout bounds a single upload attempt.
const DefaultTimeout = 30 * time.Second

// Kind classifies a failed upload by what the caller should do about it.
type Kind int

const (
	// Retryable covers network errors, rate limiting and platform faults. The
	// data is still good; try again later.
	Retryable Kind = iota

	// Rejected means the platform will never accept this payload. The batch is
	// dropped, because retrying a poison payload forever would block every
	// window queued behind it.
	Rejected

	// Unauthorized means the credentials are not accepted. The data is kept and
	// the agent backs off hard: unlike a bad payload, this can be repaired by a
	// human, and discarding history in the meantime would lose exactly the
	// period the operator will want to look at.
	Unauthorized
)

// Error is a failed upload, classified.
type Error struct {
	Kind   Kind
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("ship: status %d: %v", e.Status, e.Err)
	}
	return fmt.Sprintf("ship: %v", e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Client uploads batches to one endpoint with one server's token.
type Client struct {
	// Endpoint is the platform's ingest URL. It must be https.
	Endpoint string

	// Token authenticates one server. It is never logged and never appears in
	// an error message.
	Token string

	// Version identifies the agent build to the platform.
	Version string

	// HTTP is the transport. Nil means a client with DefaultTimeout.
	HTTP *http.Client

	// Now returns the upload instant. Nil means time.Now.
	Now func() time.Time
}

// New validates the endpoint and returns a ready client.
//
// Plain http is refused outright, with no override flag. The token is a bearer
// credential, and an option to send it in clear text is one that eventually gets
// switched on "just for testing" and left on.
func New(endpoint, token, version string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("ship: endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("ship: endpoint must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("ship: endpoint has no host")
	}
	if token == "" {
		return nil, fmt.Errorf("ship: token is empty")
	}

	return &Client{Endpoint: endpoint, Token: token, Version: version}, nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Send uploads one batch and returns the platform's reply.
func (c *Client) Send(ctx context.Context, buckets []*metrics.Bucket) (wire.Response, error) {
	body, err := encode(wire.Batch{
		Agent:   c.Version,
		SentAt:  c.now().UTC(),
		Buckets: buckets,
	})
	if err != nil {
		return wire.Response{}, &Error{Kind: Rejected, Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return wire.Response{}, &Error{Kind: Retryable, Err: err}
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "kloudy-agent/"+c.Version)

	resp, err := c.http().Do(req)
	if err != nil {
		return wire.Response{}, &Error{Kind: Retryable, Err: err}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return wire.Response{}, &Error{Kind: Retryable, Status: resp.StatusCode, Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return wire.Response{}, &Error{
			Kind:   classify(resp.StatusCode),
			Status: resp.StatusCode,
			Err:    fmt.Errorf("upload refused"),
		}
	}

	// Unknown fields are ignored rather than rejected, so the platform can add
	// them without breaking older agents. Config holds only integers, so there
	// is no response shape that can carry something executable into the agent.
	var out wire.Response
	if err := json.Unmarshal(payload, &out); err != nil {
		return wire.Response{}, &Error{Kind: Retryable, Status: resp.StatusCode, Err: fmt.Errorf("decode reply: %w", err)}
	}

	return out, nil
}

// classify maps a status code to what the caller should do.
func classify(status int) Kind {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return Unauthorized
	case status == http.StatusTooManyRequests:
		return Retryable
	case status >= 400 && status < 500:
		// A payload the platform will not accept in its current form. Keeping it
		// would block every window queued behind it, indefinitely.
		return Rejected
	default:
		return Retryable
	}
}

func encode(b wire.Batch) ([]byte, error) {
	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return nil, fmt.Errorf("encode batch: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compress batch: %w", err)
	}

	return buf.Bytes(), nil
}
