package ship

import (
	"testing"
	"time"
)

// ceilingOnly draws the top of the interval, exposing the exponential schedule
// underneath the jitter.
func ceilingOnly() func(int64) int64 {
	return func(n int64) int64 { return n - 1 }
}

func TestBackoffGrowsExponentiallyThenStops(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: 8 * time.Second, Rand: ceilingOnly()}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		got := b.Delay(i + 1)
		if got.Round(time.Millisecond) != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// Full jitter: the delay is drawn from the whole interval below the ceiling, not
// fixed at it. After a platform outage, agents retrying on a deterministic
// schedule reconnect in lockstep and hold the platform down through its own
// recovery.
func TestBackoffSpreadsAcrossTheInterval(t *testing.T) {
	seen := map[time.Duration]bool{}
	var calls int

	b := Backoff{
		Base: time.Second,
		Cap:  time.Minute,
		Rand: func(n int64) int64 {
			calls++
			return int64(calls) % n // a different draw each call
		},
	}

	for range 20 {
		seen[b.Delay(5)] = true
	}

	if len(seen) < 10 {
		t.Errorf("got %d distinct delays over 20 draws, want the interval sampled rather than a fixed value", len(seen))
	}
}

func TestBackoffStaysWithinItsCeiling(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: 10 * time.Second}

	for attempt := 1; attempt <= 12; attempt++ {
		got := b.Delay(attempt)
		if got < 0 {
			t.Fatalf("Delay(%d) = %v, want a non-negative delay", attempt, got)
		}
		if got > 10*time.Second {
			t.Errorf("Delay(%d) = %v, want at most the 10s cap", attempt, got)
		}
	}
}

func TestBackoffTreatsZeroAndNegativeAttemptsAsTheFirst(t *testing.T) {
	b := Backoff{Base: time.Second, Cap: time.Minute, Rand: ceilingOnly()}

	for _, attempt := range []int{0, -1, -100} {
		if got := b.Delay(attempt); got.Round(time.Millisecond) != time.Second {
			t.Errorf("Delay(%d) = %v, want the first interval", attempt, got)
		}
	}
}

func TestBackoffUsesDefaultsWhenUnset(t *testing.T) {
	var b Backoff

	got := b.Delay(1)
	if got < 0 || got > DefaultBase {
		t.Errorf("Delay(1) = %v, want a value within the default base %v", got, DefaultBase)
	}
}

// Applied once at startup so a fleet installed by the same automation does not
// settle into posting on the same second of every minute forever after.
func TestSpreadStaysWithinTheRequestedRange(t *testing.T) {
	b := Backoff{}

	for range 50 {
		got := b.Spread(time.Minute)
		if got < 0 || got >= time.Minute {
			t.Fatalf("Spread(1m) = %v, want a value in [0, 1m)", got)
		}
	}
}

func TestSpreadOfZeroIsZero(t *testing.T) {
	var b Backoff
	if got := b.Spread(0); got != 0 {
		t.Errorf("Spread(0) = %v, want 0", got)
	}
}
