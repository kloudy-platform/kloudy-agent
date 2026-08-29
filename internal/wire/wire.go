// Package wire defines the contract between the agent and the Kloudy platform.
//
// It is deliberately small. Everything the platform can say back to an agent is
// declared here, and it is all integers and booleans, which is what keeps a
// compromised or spoofed response from being able to make an agent do anything.
package wire

import (
	"fmt"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
)

// Batch is one upload: the windows accumulated since the last successful send.
type Batch struct {
	// Agent is the agent's version, so the platform can recognise an old fleet
	// member and reason about fields it may not send.
	Agent string `json:"agent"`

	// SentAt is the agent's clock at upload time. The platform records its own
	// receive time alongside it; the two together make clock drift diagnosable
	// instead of silently shifting a customer's charts.
	SentAt time.Time `json:"sent_at"`

	Buckets []*metrics.Bucket `json:"buckets"`
}

// Config is everything the platform is allowed to tell an agent.
//
// This type is the security boundary. It holds only durations expressed as
// integers, so there is no representable response that carries a URL, a path or
// a command. Unknown JSON fields are discarded by the decoder rather than
// rejected, so the platform can add fields without breaking older agents.
//
// Nothing here may ever become a string that the agent resolves, fetches or
// executes. An agent that can be told what to run is no longer a monitoring
// agent, and the only thing preventing that is the shape of this struct.
type Config struct {
	// IntervalSeconds is how often to sample.
	IntervalSeconds int `json:"interval"`

	// FlushSeconds is how often to upload.
	FlushSeconds int `json:"flush"`

	// WindowSeconds is the aggregation window width.
	WindowSeconds int `json:"window"`
}

// Bounds on what the platform may ask for. A response outside these is treated
// as a bug or an attack and refused: sampling every 10ms would be a denial of
// service against the customer's own machine, and an hour-long window would
// quietly destroy the resolution they are paying for.
const (
	MinInterval = time.Second
	MaxInterval = 5 * time.Minute
	MinFlush    = 10 * time.Second
	MaxFlush    = time.Hour
	MinWindow   = time.Second
	MaxWindow   = 5 * time.Minute
)

// Settings is a validated Config, expressed in the units the agent runs on.
type Settings struct {
	Interval time.Duration
	Flush    time.Duration
	Window   time.Duration
}

// Apply returns current with any field the platform set, provided every value it
// sent is within bounds. A single out-of-range value rejects the whole response:
// applying the acceptable half of an implausible instruction is how a partial
// misconfiguration becomes permanent.
func (c Config) Apply(current Settings) (Settings, error) {
	next := current

	for _, field := range []struct {
		name     string
		seconds  int
		min, max time.Duration
		target   *time.Duration
	}{
		{"interval", c.IntervalSeconds, MinInterval, MaxInterval, &next.Interval},
		{"flush", c.FlushSeconds, MinFlush, MaxFlush, &next.Flush},
		{"window", c.WindowSeconds, MinWindow, MaxWindow, &next.Window},
	} {
		if field.seconds == 0 {
			continue // unset: keep what the agent is already running
		}

		d := time.Duration(field.seconds) * time.Second
		if d < field.min || d > field.max {
			return current, fmt.Errorf("wire: %s %s out of range [%s, %s]", field.name, d, field.min, field.max)
		}

		*field.target = d
	}

	return next, nil
}

// Response is what the platform returns on a successful upload.
type Response struct {
	Accepted int    `json:"accepted"`
	Config   Config `json:"config"`
}
