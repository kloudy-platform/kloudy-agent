package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
	"github.com/kloudy-platform/kloudy-agent/internal/ship"
	"github.com/kloudy-platform/kloudy-agent/internal/spool"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

// writeProc lays down a /proc tree whose counters have advanced by tick
// seconds' worth of activity. Static fixtures cannot exercise the pipeline: with
// counters that never move there is no interval to derive, so nothing is ever
// aggregated and nothing is ever shipped.
func writeProc(dir string, tick uint64) error {
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "sys/kernel/random"), 0o755); err != nil {
		return err
	}

	// A quarter of every second is charged to user time, the rest to idle.
	busy := 25 * tick
	idle := 75 * tick

	files := map[string]string{
		"stat": fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\ncpu0 %d 0 0 %d 0 0 0 0 0 0\n", busy, idle, busy, idle),
		"meminfo": "MemTotal:        4025376 kB\nMemFree:          198432 kB\nMemAvailable:    2841920 kB\n" +
			"Buffers:          142208 kB\nCached:          2314560 kB\nSwapTotal:       2097148 kB\nSwapFree:        2091004 kB\n",
		"loadavg": "0.52 0.58 0.59 2/1234 56789\n",
		"uptime":  fmt.Sprintf("%d.00 %d.00\n", 1000+tick, 2000+tick),
		"net/dev": "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			fmt.Sprintf("  eth0: %d %d 0 0 0 0 0 0 %d %d 0 0 0 0 0 0\n", 1000*tick, 10*tick, 500*tick, 5*tick),
		"diskstats":                 fmt.Sprintf(" 254 0 vda %d 0 %d 0 %d 0 %d 0 0 0 0\n", tick, 2*tick, tick, 4*tick),
		"sys/kernel/random/boot_id": "3f2b1a9c-7d4e-4f81-9a2b-5c6d7e8f9a01\n",
	}

	// Published by rename so a sample never observes a half-rewritten file. A
	// real /proc read is atomic; a test that was not would fail for a reason
	// the agent will never encounter in production.
	for name, content := range files {
		final := filepath.Join(dir, name)
		tmp := final + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, final); err != nil {
			return err
		}
	}

	return nil
}

// TestEndToEnd exercises the whole pipeline: reading counters, deriving rates,
// aggregating into windows, spooling them and uploading them over TLS.
func TestEndToEnd(t *testing.T) {
	proc := filepath.Join(t.TempDir(), "proc")
	if err := writeProc(proc, 1); err != nil {
		t.Fatal(err)
	}

	received := make(chan wire.Batch, 16)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("body is not gzipped: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer zr.Close()

		var batch wire.Batch
		if err := json.NewDecoder(zr).Decode(&batch); err != nil {
			t.Errorf("decode batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		select {
		case received <- batch:
		default:
		}

		json.NewEncoder(w).Encode(wire.Response{Accepted: len(batch.Buckets)})
	}))
	defer srv.Close()

	// Advance the kernel's counters in the background, as a running machine
	// would. Without this the agent would correctly ship nothing at all.
	var stopped atomic.Bool
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for tick := uint64(2); !stopped.Load(); tick++ {
			if err := writeProc(proc, tick); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	// Waited on, not just signalled: a writer still running while the temporary
	// directory is being removed makes the cleanup fail intermittently.
	defer func() {
		stopped.Store(true)
		<-writerDone
	}()

	client := &ship.Client{
		Endpoint: srv.URL,
		Token:    "test-token",
		Version:  "test",
		HTTP:     srv.Client(),
	}

	a := &Agent{
		Collector: &collect.Collector{
			Root:   proc,
			Mounts: []string{"/"},
			Usage:  func(string) (uint64, uint64, error) { return 1 << 30, 1 << 29, nil },
		},
		Spool:    &spool.Spool{Dir: t.TempDir()},
		Uploader: client,
		Settings: wire.Settings{
			Interval: 20 * time.Millisecond,
			Window:   50 * time.Millisecond,
			Flush:    100 * time.Millisecond,
		},
		// No startup spread, so the test does not wait out a random offset.
		Backoff: ship.Backoff{Rand: func(int64) int64 { return 0 }},
		Log:     slog.New(slog.DiscardHandler),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case batch := <-received:
		if len(batch.Buckets) == 0 {
			t.Fatal("received a batch with no windows")
		}
		if batch.Agent != "test" {
			t.Errorf("Agent = %q, want the version", batch.Agent)
		}
		if batch.SentAt.IsZero() {
			t.Error("SentAt is zero")
		}

		b := batch.Buckets[0]
		if b.Samples == 0 {
			t.Error("window reports zero samples")
		}
		if b.BootID == "" {
			t.Error("window carries no boot id")
		}
		// A quarter of each second is charged to user time in the fixture.
		if avg := b.CPUBusy.Avg; avg < 20 || avg > 30 {
			t.Errorf("CPUBusy.Avg = %v, want roughly 25 (min %v max %v over %d samples)", avg, b.CPUBusy.Min, b.CPUBusy.Max, b.Samples)
		}
		if b.MemTotal != 4025376*1024 {
			t.Errorf("MemTotal = %d", b.MemTotal)
		}

	case err := <-done:
		t.Fatalf("Run() returned before any batch arrived: %v", err)

	case <-ctx.Done():
		t.Fatal("no batch arrived within the timeout")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Run() did not return after cancellation")
	}
}
