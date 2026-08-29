package spool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
)

var epoch = time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC)

// bucket builds a window starting n seconds after the epoch, padded so the
// on-disk size is predictable enough to test the cap.
func bucket(n int) *metrics.Bucket {
	b := &metrics.Bucket{
		Start:   epoch.Add(time.Duration(n) * time.Second),
		End:     epoch.Add(time.Duration(n+10) * time.Second),
		Samples: 10,
		BootID:  "boot-a",
	}
	b.CPUBusy.Add(float64(n))
	return b
}

func TestAddThenPeekReturnsChronologicalOrder(t *testing.T) {
	s := &Spool{Dir: t.TempDir()}

	// Added out of order: the spool must sort by the window's own instant, not
	// by the order it happened to receive them.
	for _, n := range []int{30, 10, 20} {
		if err := s.Add(bucket(n)); err != nil {
			t.Fatalf("Add(%d) error = %v", n, err)
		}
	}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Peek() returned %d entries, want 3", len(got))
	}

	for i, want := range []int{10, 20, 30} {
		if !got[i].Bucket.Start.Equal(bucket(want).Start) {
			t.Errorf("entry %d starts at %v, want %v", i, got[i].Bucket.Start, bucket(want).Start)
		}
	}
}

func TestPeekHonoursLimit(t *testing.T) {
	s := &Spool{Dir: t.TempDir()}
	for n := range 5 {
		if err := s.Add(bucket(n * 10)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Peek(2)
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Peek(2) returned %d entries, want 2", len(got))
	}
}

func TestDiscardRemovesOnlyAcknowledgedEntries(t *testing.T) {
	s := &Spool{Dir: t.TempDir()}
	for n := range 3 {
		if err := s.Add(bucket(n * 10)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Peek(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(got); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}

	count, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("Stats() count = %d, want 1", count)
	}
}

// The window survives the process. This is the property that makes an agent
// restart or a platform deploy a delay rather than a hole in the history.
func TestSpoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := &Spool{Dir: dir}
	if err := first.Add(bucket(10)); err != nil {
		t.Fatal(err)
	}

	// A fresh Spool over the same directory, as after a restart.
	second := &Spool{Dir: dir}
	got, err := second.Peek(10)
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Peek() returned %d entries after restart, want 1", len(got))
	}
	if got[0].Bucket.Samples != 10 {
		t.Errorf("Samples = %d, want the persisted value 10", got[0].Bucket.Samples)
	}
}

// An agent that fills the partition it is monitoring has caused a worse outage
// than the one it existed to report.
func TestCapDropsOldestAndReportsIt(t *testing.T) {
	var dropped int
	s := &Spool{
		Dir:      t.TempDir(),
		MaxBytes: 2000, // about three windows
		OnDrop:   func(n int) { dropped += n },
	}

	for n := range 20 {
		if err := s.Add(bucket(n * 10)); err != nil {
			t.Fatalf("Add(%d) error = %v", n, err)
		}
	}

	count, bytes, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if bytes > 2000 {
		t.Errorf("Stats() bytes = %d, want the spool held within its 2000 byte cap", bytes)
	}
	if count == 0 {
		t.Error("Stats() count = 0, want the newest windows retained")
	}
	if dropped == 0 {
		t.Error("OnDrop was never called: dropping data must never be silent")
	}

	// What survives must be the newest, never the oldest.
	got, err := s.Peek(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 0 && got[0].Bucket.Start.Before(epoch.Add(time.Minute)) {
		t.Errorf("oldest retained window starts at %v, want the early ones dropped first", got[0].Bucket.Start)
	}
}

// One unreadable file must not be able to block delivery of everything queued
// behind it, which is what retrying it forever would amount to.
func TestPeekDropsCorruptEntryAndContinues(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}

	if err := s.Add(bucket(10)); err != nil {
		t.Fatal(err)
	}
	// Written directly so it sorts ahead of the valid window.
	corrupt := filepath.Join(dir, "00000000000000000001-000001.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Peek() returned %d entries, want the one valid window", len(got))
	}
	if _, err := os.Stat(corrupt); !os.IsNotExist(err) {
		t.Error("the corrupt entry is still on disk, want it removed so it cannot block the queue")
	}
}

// A crash midway through a write leaves a temporary file. It must be ignored,
// not parsed as a half-written window.
func TestPeekIgnoresInterruptedWrites(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}

	if err := s.Add(bucket(10)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp-halfwritten"), []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Peek() returned %d entries, want 1", len(got))
	}
}

// The spool sits on the customer's server and holds their operational history.
// It is not world-readable.
func TestSpooledFilesAreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}
	if err := s.Add(bucket(10)); err != nil {
		t.Fatal(err)
	}

	names, err := s.files()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("got %d files, want 1", len(names))
	}

	info, err := os.Stat(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestPeekOnMissingDirIsEmpty(t *testing.T) {
	s := &Spool{Dir: filepath.Join(t.TempDir(), "not-created-yet")}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("Peek() error = %v, want an empty result on a fresh install", err)
	}
	if len(got) != 0 {
		t.Errorf("Peek() returned %d entries, want 0", len(got))
	}
}

// A cap smaller than a single window must still leave the newest one on disk.
// Emptying the spool entirely would leave the agent unable to hold anything at
// all, discarding every window it collects for as long as the setting stands.
func TestCapNeverEmptiesTheSpoolCompletely(t *testing.T) {
	s := &Spool{Dir: t.TempDir(), MaxBytes: 1} // far below one encoded window

	for n := range 3 {
		if err := s.Add(bucket(n * 10)); err != nil {
			t.Fatalf("Add(%d) error = %v", n, err)
		}
	}

	count, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Stats() count = %d, want exactly the newest window retained", count)
	}

	got, err := s.Peek(1)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Bucket.Start.Equal(bucket(20).Start) {
		t.Errorf("retained window starts at %v, want the newest", got[0].Bucket.Start)
	}
}
