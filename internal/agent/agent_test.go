package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
	"github.com/kloudy-platform/kloudy-agent/internal/ship"
	"github.com/kloudy-platform/kloudy-agent/internal/spool"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

type fakeUploader struct {
	resp  wire.Response
	err   error
	calls int
	sent  []*metrics.Bucket
}

func (f *fakeUploader) Send(_ context.Context, buckets []*metrics.Bucket) (wire.Response, error) {
	f.calls++
	f.sent = append(f.sent, buckets...)
	return f.resp, f.err
}

func bucket(n int) *metrics.Bucket {
	start := time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC).Add(time.Duration(n) * time.Second)
	return &metrics.Bucket{Start: start, End: start.Add(10 * time.Second), Samples: 10, BootID: "boot-a"}
}

func testAgent(t *testing.T, up Uploader) *Agent {
	t.Helper()

	return &Agent{
		Spool:    &spool.Spool{Dir: t.TempDir()},
		Uploader: up,
		Settings: wire.Settings{Interval: time.Second, Flush: time.Minute, Window: 10 * time.Second},
		Log:      slog.New(slog.DiscardHandler),
		agg:      &metrics.Aggregator{Window: 10 * time.Second},
	}
}

func spoolCount(t *testing.T, a *Agent) int {
	t.Helper()

	count, _, err := a.Spool.Stats()
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	return count
}

func TestUploadSendsAndDiscardsOnSuccess(t *testing.T) {
	up := &fakeUploader{resp: wire.Response{Accepted: 2}}
	a := testAgent(t, up)

	for n := range 2 {
		a.persist(bucket(n * 10))
	}

	a.upload(context.Background())

	if up.calls != 1 {
		t.Errorf("Send called %d times, want 1", up.calls)
	}
	if len(up.sent) != 2 {
		t.Errorf("sent %d windows, want 2", len(up.sent))
	}
	if got := spoolCount(t, a); got != 0 {
		t.Errorf("spool holds %d windows after a successful upload, want 0", got)
	}
}

func TestUploadDoesNothingWhenSpoolIsEmpty(t *testing.T) {
	up := &fakeUploader{}
	a := testAgent(t, up)

	a.upload(context.Background())

	if up.calls != 0 {
		t.Errorf("Send called %d times on an empty spool, want 0", up.calls)
	}
}

// A payload the platform will never accept must not be retried forever: it would
// block every window queued behind it.
func TestRejectedBatchIsDroppedSoTheQueueKeepsMoving(t *testing.T) {
	up := &fakeUploader{err: &ship.Error{Kind: ship.Rejected, Status: 400, Err: errors.New("malformed")}}
	a := testAgent(t, up)
	a.persist(bucket(10))

	a.upload(context.Background())

	if got := spoolCount(t, a); got != 0 {
		t.Errorf("spool holds %d windows after a rejection, want the poison batch dropped", got)
	}
}

// A revoked or mistyped token is repairable by a human. Discarding history in
// the meantime would lose exactly the period the operator will want to inspect.
func TestUnauthorizedBatchIsKept(t *testing.T) {
	up := &fakeUploader{err: &ship.Error{Kind: ship.Unauthorized, Status: 401, Err: errors.New("refused")}}
	a := testAgent(t, up)
	a.persist(bucket(10))

	a.upload(context.Background())

	if got := spoolCount(t, a); got != 1 {
		t.Errorf("spool holds %d windows, want the data kept for when the token is fixed", got)
	}
}

func TestRetryableFailureKeepsDataAndBacksOff(t *testing.T) {
	up := &fakeUploader{err: &ship.Error{Kind: ship.Retryable, Status: 503, Err: errors.New("unavailable")}}
	a := testAgent(t, up)
	a.Backoff = ship.Backoff{Base: time.Minute, Cap: time.Hour, Rand: func(n int64) int64 { return n - 1 }}
	a.persist(bucket(10))

	a.upload(context.Background())

	if got := spoolCount(t, a); got != 1 {
		t.Errorf("spool holds %d windows, want the data kept", got)
	}
	if a.failures != 1 {
		t.Errorf("failures = %d, want 1", a.failures)
	}
	if a.retryAt.IsZero() {
		t.Error("retryAt is zero, want an upload deferred after a failure")
	}

	// The next tick inside the backoff interval must not send.
	a.upload(context.Background())
	if up.calls != 1 {
		t.Errorf("Send called %d times, want the retry deferred until the backoff elapses", up.calls)
	}
}

func TestSuccessClearsTheBackoff(t *testing.T) {
	up := &fakeUploader{err: &ship.Error{Kind: ship.Retryable, Err: errors.New("boom")}}
	a := testAgent(t, up)
	a.persist(bucket(10))
	a.upload(context.Background())

	up.err = nil
	a.retryAt = time.Time{} // as if the backoff had elapsed
	a.upload(context.Background())

	if a.failures != 0 {
		t.Errorf("failures = %d after a success, want 0", a.failures)
	}
	if !a.retryAt.IsZero() {
		t.Error("retryAt is still set after a success")
	}
}

func TestUploadIsCappedAtMaxBatch(t *testing.T) {
	up := &fakeUploader{}
	a := testAgent(t, up)
	a.MaxBatch = 3

	for n := range 10 {
		a.persist(bucket(n * 10))
	}

	a.upload(context.Background())

	if len(up.sent) != 3 {
		t.Errorf("sent %d windows, want the batch capped at 3", len(up.sent))
	}
	if got := spoolCount(t, a); got != 7 {
		t.Errorf("spool holds %d windows, want the remaining 7 kept for the next upload", got)
	}
}

func TestApplyConfigAdoptsValidSchedule(t *testing.T) {
	a := testAgent(t, &fakeUploader{})

	if changed := a.applyConfig(wire.Config{IntervalSeconds: 5, FlushSeconds: 120}); !changed {
		t.Fatal("applyConfig() = false, want the change reported so the tickers reset")
	}
	if a.Settings.Interval != 5*time.Second || a.Settings.Flush != 2*time.Minute {
		t.Errorf("Settings = %+v", a.Settings)
	}
}

func TestApplyConfigIgnoresOutOfRangeValues(t *testing.T) {
	a := testAgent(t, &fakeUploader{})
	before := a.Settings

	if changed := a.applyConfig(wire.Config{IntervalSeconds: 99999}); changed {
		t.Error("applyConfig() = true, want an implausible schedule refused")
	}
	if a.Settings != before {
		t.Errorf("Settings = %+v, want %+v", a.Settings, before)
	}
}

// Changing the window width must not silently rebucket readings already taken
// under the previous one.
func TestApplyConfigClosesTheWindowBeforeChangingItsWidth(t *testing.T) {
	a := testAgent(t, &fakeUploader{})
	a.agg = &metrics.Aggregator{Window: 10 * time.Second}

	// Put a window in progress by hand, since Add needs two derivable samples.
	a.persist(bucket(10))
	before := spoolCount(t, a)

	a.applyConfig(wire.Config{WindowSeconds: 30})

	if a.Settings.Window != 30*time.Second {
		t.Errorf("Window = %v, want 30s", a.Settings.Window)
	}
	if got := spoolCount(t, a); got < before {
		t.Errorf("spool holds %d windows, want nothing discarded by the width change", got)
	}
}

func TestApplyConfigReportsNoChangeForAnEmptyReply(t *testing.T) {
	a := testAgent(t, &fakeUploader{})

	if changed := a.applyConfig(wire.Config{}); changed {
		t.Error("applyConfig() = true for an empty config, want no ticker churn")
	}
}
