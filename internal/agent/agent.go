// Package agent runs the sample, aggregate, spool and upload loop.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
	"github.com/kloudy-platform/kloudy-agent/internal/ship"
	"github.com/kloudy-platform/kloudy-agent/internal/spool"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

// DefaultMaxBatch caps how many windows go up in one request.
//
// At the default window width this is an hour of history, so a backlog drains
// over several uploads rather than arriving as one enormous request. An agent
// catching up after a long outage should not be indistinguishable from an attack
// on the ingest endpoint.
const DefaultMaxBatch = 360

// Uploader is the part of ship.Client the loop depends on, so the loop can be
// tested without a network.
type Uploader interface {
	Send(ctx context.Context, buckets []*metrics.Bucket) (wire.Response, error)
}

// Agent owns the loop. Everything it talks to is injected so the whole cycle can
// be exercised in tests with a fake clock and no sockets.
type Agent struct {
	Collector *collect.Collector
	Spool     *spool.Spool
	Uploader  Uploader
	Settings  wire.Settings
	Backoff   ship.Backoff
	Log       *slog.Logger

	// MaxBatch caps windows per upload. Zero means DefaultMaxBatch.
	MaxBatch int

	agg      *metrics.Aggregator
	failures int
	retryAt  time.Time
}

func (a *Agent) maxBatch() int {
	if a.MaxBatch <= 0 {
		return DefaultMaxBatch
	}
	return a.MaxBatch
}

// Run samples and uploads until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// Fail before entering the loop rather than logging the same error every
	// tick, so a misconfigured agent is obvious at startup instead of buried.
	if _, err := a.Collector.Collect(); err != nil {
		return err
	}

	a.agg = &metrics.Aggregator{Window: a.Settings.Window}

	sampler := time.NewTicker(a.Settings.Interval)
	defer sampler.Stop()

	// The first upload is offset by a random fraction of the period so that a
	// fleet installed by the same automation does not post in unison.
	uploader := time.NewTicker(a.Settings.Flush)
	defer uploader.Stop()

	if spread := a.Backoff.Spread(a.Settings.Flush); spread > 0 {
		timer := time.NewTimer(spread)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return a.shutdown(context.WithoutCancel(ctx))
		}
	}

	a.Log.Info("started",
		slog.Duration("interval", a.Settings.Interval),
		slog.Duration("window", a.Settings.Window),
		slog.Duration("flush", a.Settings.Flush),
	)

	for {
		select {
		case <-sampler.C:
			a.sample()

		case <-uploader.C:
			if changed := a.upload(ctx); changed {
				sampler.Reset(a.Settings.Interval)
				uploader.Reset(a.Settings.Flush)
			}

		case <-ctx.Done():
			// The parent context is already cancelled, so shutdown gets a
			// detached one: the point of flushing is to persist what would
			// otherwise be lost, and it must not be cancelled on arrival.
			return a.shutdown(context.WithoutCancel(ctx))
		}
	}
}

// sample takes one reading and spools the window it completes, if any.
func (a *Agent) sample() {
	s, err := a.Collector.Collect()
	if err != nil {
		// One unreadable sample is not fatal. The window carries fewer readings
		// and its sample count travels with it, so the platform can see that the
		// window is thin instead of trusting it as fully observed.
		a.Log.Warn("sample failed", slog.String("error", err.Error()))
		return
	}

	if b := a.agg.Add(s); b != nil {
		a.persist(b)
	}
}

func (a *Agent) persist(b *metrics.Bucket) {
	if err := a.Spool.Add(b); err != nil {
		a.Log.Error("spool write failed", slog.String("error", err.Error()))
	}
}

// upload sends the oldest spooled windows. It reports whether the platform
// changed the schedule, so the caller can reset its tickers.
func (a *Agent) upload(ctx context.Context) bool {
	if time.Now().Before(a.retryAt) {
		return false // still inside the backoff interval from an earlier failure
	}

	entries, err := a.Spool.Peek(a.maxBatch())
	if err != nil {
		a.Log.Error("spool read failed", slog.String("error", err.Error()))
		return false
	}
	if len(entries) == 0 {
		return false
	}

	buckets := make([]*metrics.Bucket, len(entries))
	for i, e := range entries {
		buckets[i] = e.Bucket
	}

	resp, err := a.Uploader.Send(ctx, buckets)
	if err != nil {
		a.handleFailure(err, entries)
		return false
	}

	if err := a.Spool.Discard(entries); err != nil {
		a.Log.Error("spool discard failed", slog.String("error", err.Error()))
	}

	a.failures = 0
	a.retryAt = time.Time{}
	a.Log.Info("uploaded", slog.Int("windows", len(entries)), slog.Int("accepted", resp.Accepted))

	return a.applyConfig(resp.Config)
}

func (a *Agent) handleFailure(err error, entries []spool.Entry) {
	a.failures++
	delay := a.Backoff.Delay(a.failures)
	a.retryAt = time.Now().Add(delay)

	kind := ship.Retryable
	if e, ok := err.(*ship.Error); ok {
		kind = e.Kind
	}

	switch kind {
	case ship.Rejected:
		// The platform will never accept this payload. Keeping it would block
		// every window queued behind it, indefinitely, so it is dropped and
		// said out loud.
		a.Log.Error("windows rejected and dropped",
			slog.Int("windows", len(entries)),
			slog.String("error", err.Error()),
		)
		if err := a.Spool.Discard(entries); err != nil {
			a.Log.Error("spool discard failed", slog.String("error", err.Error()))
		}

	case ship.Unauthorized:
		// A human can repair this. Discarding history in the meantime would lose
		// exactly the period the operator will want to look at afterwards.
		a.Log.Error("upload not authorized, keeping windows spooled",
			slog.Duration("retry_in", delay),
			slog.String("error", err.Error()),
		)

	default:
		a.Log.Warn("upload failed, will retry",
			slog.Duration("retry_in", delay),
			slog.Int("consecutive_failures", a.failures),
			slog.String("error", err.Error()),
		)
	}
}

// applyConfig adopts the platform's schedule if every value it sent is valid.
func (a *Agent) applyConfig(c wire.Config) bool {
	next, err := c.Apply(a.Settings)
	if err != nil {
		a.Log.Warn("ignoring platform config", slog.String("error", err.Error()))
		return false
	}
	if next == a.Settings {
		return false
	}

	// A different window width means the aggregator's current window no longer
	// matches what is being asked for. It is closed and kept rather than
	// silently rebucketed under the new width.
	if next.Window != a.Settings.Window {
		if b := a.agg.Flush(); b != nil {
			a.persist(b)
		}
		a.agg = &metrics.Aggregator{Window: next.Window}
	}

	a.Log.Info("config updated",
		slog.Duration("interval", next.Interval),
		slog.Duration("window", next.Window),
		slog.Duration("flush", next.Flush),
	)
	a.Settings = next

	return true
}

// shutdown persists the window in progress and makes one last attempt to send
// what is queued, so a restart costs a delay rather than a hole.
func (a *Agent) shutdown(ctx context.Context) error {
	if b := a.agg.Flush(); b != nil {
		a.persist(b)
	}

	ctx, cancel := context.WithTimeout(ctx, ship.DefaultTimeout)
	defer cancel()

	a.retryAt = time.Time{} // a pending backoff must not skip the final attempt
	a.upload(ctx)

	count, bytes, err := a.Spool.Stats()
	if err == nil && count > 0 {
		a.Log.Info("stopped with windows still spooled",
			slog.Int("windows", count),
			slog.Int64("bytes", bytes),
		)
	} else {
		a.Log.Info("stopped")
	}

	return nil
}
