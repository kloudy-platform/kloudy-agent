package ship

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

const testToken = "kla_secret_do_not_leak"

func testBuckets() []*metrics.Bucket {
	b := &metrics.Bucket{
		Start:   time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 29, 9, 14, 10, 0, time.UTC),
		Samples: 10,
		BootID:  "boot-a",
	}
	b.CPUBusy.Add(42)
	return []*metrics.Bucket{b}
}

// newTestClient points a client at a test server, bypassing New's https check
// since httptest serves plain http.
func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		Endpoint: srv.URL,
		Token:    testToken,
		Version:  "1.0.0-test",
		HTTP:     srv.Client(),
		Now:      func() time.Time { return time.Date(2026, 8, 29, 9, 14, 30, 0, time.UTC) },
	}
}

// Plain http is refused with no override. An option to send a bearer token in
// clear text is one that gets switched on "just for testing" and left on.
func TestNewRefusesInsecureAndMalformedEndpoints(t *testing.T) {
	tests := map[string]struct{ endpoint, token string }{
		"plain http":  {"http://ingest.kloudy.test/v1", testToken},
		"no scheme":   {"ingest.kloudy.test/v1", testToken},
		"no host":     {"https://", testToken},
		"unparseable": {"https://%zz", testToken},
		"empty token": {"https://ingest.kloudy.test/v1", ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(tc.endpoint, tc.token, "1.0.0"); err == nil {
				t.Error("New() error = nil, want a refusal")
			}
		})
	}
}

func TestNewAcceptsHTTPS(t *testing.T) {
	if _, err := New("https://ingest.kloudy.test/v1", testToken, "1.0.0"); err != nil {
		t.Errorf("New() error = %v", err)
	}
}

func TestSendPostsGzippedBatchWithBearerToken(t *testing.T) {
	var (
		gotAuth     string
		gotEncoding string
		gotAgent    string
		gotBatch    wire.Batch
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotAgent = r.Header.Get("User-Agent")

		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("body is not gzipped: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer zr.Close()

		if err := json.NewDecoder(zr).Decode(&gotBatch); err != nil {
			t.Errorf("decode batch: %v", err)
		}

		json.NewEncoder(w).Encode(wire.Response{Accepted: 1})
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).Send(context.Background(), testBuckets()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotEncoding != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", gotEncoding)
	}
	if !strings.HasPrefix(gotAgent, "kloudy-agent/") {
		t.Errorf("User-Agent = %q", gotAgent)
	}
	if len(gotBatch.Buckets) != 1 || gotBatch.Buckets[0].Samples != 10 {
		t.Errorf("Buckets = %+v", gotBatch.Buckets)
	}
	if gotBatch.Agent != "1.0.0-test" {
		t.Errorf("Agent = %q", gotBatch.Agent)
	}
	// The agent's own clock travels with the batch so the platform can compare
	// it against its receive time and make drift diagnosable.
	if gotBatch.SentAt.IsZero() {
		t.Error("SentAt is zero, want the agent's clock")
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	tests := map[int]Kind{
		http.StatusUnauthorized:          Unauthorized,
		http.StatusForbidden:             Unauthorized,
		http.StatusBadRequest:            Rejected,
		http.StatusRequestEntityTooLarge: Rejected,
		http.StatusTooManyRequests:       Retryable,
		http.StatusInternalServerError:   Retryable,
		http.StatusBadGateway:            Retryable,
	}

	for status, want := range tests {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := newTestClient(srv).Send(context.Background(), testBuckets())
			if err == nil {
				t.Fatal("Send() error = nil, want a failure")
			}

			var shipErr *Error
			if !asShipError(err, &shipErr) {
				t.Fatalf("Send() error = %T, want *ship.Error", err)
			}
			if shipErr.Kind != want {
				t.Errorf("Kind = %v, want %v", shipErr.Kind, want)
			}
		})
	}
}

// The token is a credential that would otherwise end up in the customer's
// journal on every failed upload.
func TestErrorsNeverContainTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).Send(context.Background(), testBuckets())
	if err == nil {
		t.Fatal("Send() error = nil, want a failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error message leaks the token: %q", err.Error())
	}
}

func TestSendReturnsPlatformConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"accepted":1,"config":{"interval":5,"flush":120}}`)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Send(context.Background(), testBuckets())
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if got.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1", got.Accepted)
	}
	if got.Config.IntervalSeconds != 5 || got.Config.FlushSeconds != 120 {
		t.Errorf("Config = %+v", got.Config)
	}
}

// A trusted peer having a bad day should not be able to exhaust memory on every
// server in the fleet at once.
func TestSendBoundsTheResponseItReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"accepted":1,"padding":"`)
		chunk := strings.Repeat("A", 8<<10)
		for range 64 { // 512 KiB, far past the 64 KiB ceiling
			io.WriteString(w, chunk)
		}
		io.WriteString(w, `"}`)
	}))
	defer srv.Close()

	// Truncated at the limit, the body is no longer valid JSON, so the upload is
	// reported as failed rather than the agent buffering half a megabyte.
	if _, err := newTestClient(srv).Send(context.Background(), testBuckets()); err == nil {
		t.Error("Send() error = nil, want the oversized reply refused")
	}
}

func TestSendPropagatesContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestClient(srv).Send(ctx, testBuckets())
	if err == nil {
		t.Fatal("Send() error = nil, want a failure")
	}

	var shipErr *Error
	if !asShipError(err, &shipErr) || shipErr.Kind != Retryable {
		t.Errorf("Kind = %v, want Retryable", err)
	}
}

func asShipError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
