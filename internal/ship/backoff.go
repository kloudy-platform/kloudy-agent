package ship

import (
	"math/rand/v2"
	"time"
)

// Default retry bounds.
const (
	DefaultBase = 2 * time.Second
	DefaultCap  = 15 * time.Minute
)

// Backoff computes how long to wait before retrying an upload.
//
// The delay is drawn uniformly from zero up to the exponential ceiling, rather
// than being the ceiling itself. That difference matters at fleet scale: after a
// platform outage, agents retrying on a deterministic schedule reconnect in
// lockstep and hold the platform down through its own recovery. Spreading them
// across the interval turns a spike back into a flow.
type Backoff struct {
	// Base is the first interval's ceiling. Zero means DefaultBase.
	Base time.Duration

	// Cap bounds the ceiling however many attempts have failed. Zero means
	// DefaultCap.
	Cap time.Duration

	// Rand returns a value in [0,n). Nil means the global source.
	Rand func(n int64) int64
}

func (b Backoff) base() time.Duration {
	if b.Base <= 0 {
		return DefaultBase
	}
	return b.Base
}

func (b Backoff) max() time.Duration {
	if b.Cap <= 0 {
		return DefaultCap
	}
	return b.Cap
}

func (b Backoff) random(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if b.Rand != nil {
		return b.Rand(n)
	}
	return rand.Int64N(n)
}

// Delay returns how long to wait after the given number of consecutive
// failures. Attempt is 1 for the first retry.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	ceiling := b.base()
	for range attempt - 1 {
		ceiling *= 2
		if ceiling >= b.max() {
			ceiling = b.max()
			break
		}
	}

	return time.Duration(b.random(int64(ceiling)))
}

// Spread returns a fixed random offset in [0,d).
//
// It is applied once at startup to the upload schedule, so that a fleet
// installed by the same automation does not settle into posting on the same
// second of every minute forever after.
func (b Backoff) Spread(d time.Duration) time.Duration {
	return time.Duration(b.random(int64(d)))
}
