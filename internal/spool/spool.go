// Package spool persists aggregated windows to disk until the platform has
// acknowledged them.
//
// This is what makes the agent more reliable than the link it reports over. A
// network outage, a platform deploy or an agent restart delays delivery instead
// of destroying data, which is the failure the SSH collector it replaces could
// not avoid: a sample missed there was gone permanently, and invisibly.
package spool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
)

// DefaultMaxBytes bounds the spool at roughly five days of ten-second windows,
// which encode to about 700 bytes each.
//
// The bound is not an optimisation. A monitoring agent that fills the partition
// it is monitoring has caused a worse outage than the one it was there to
// report, so the spool drops its oldest data rather than ever growing without
// limit.
const DefaultMaxBytes int64 = 32 << 20

const ext = ".json"

// Spool is a bounded, disk-backed FIFO of aggregated windows.
//
// Each window is a separate file named after the instant it covers, so a plain
// lexical sort of the directory is chronological order and no index has to be
// kept consistent with the files themselves.
type Spool struct {
	// Dir is where windows are stored. It is created on first use.
	Dir string

	// MaxBytes caps the spool. Zero means DefaultMaxBytes.
	MaxBytes int64

	// OnDrop is called with the number of windows discarded to stay within the
	// cap. Silent data loss is the thing this package exists to prevent, so the
	// one case where it drops data is reported rather than hidden.
	OnDrop func(n int)

	seq uint32
}

// Entry is one spooled window and the file backing it.
type Entry struct {
	Name   string
	Bucket *metrics.Bucket
}

func (s *Spool) maxBytes() int64 {
	if s.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return s.MaxBytes
}

// Add persists one window, then enforces the cap.
func (s *Spool) Add(b *metrics.Bucket) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("spool: create dir: %w", err)
	}

	payload, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("spool: encode: %w", err)
	}

	if err := s.write(s.name(b), payload); err != nil {
		return err
	}

	return s.enforceCap()
}

// name builds a fixed-width, chronologically sortable filename. The sequence
// suffix separates two windows that share a start instant, which happens when a
// window is closed early by a reboot and the next one opens on the same
// boundary.
func (s *Spool) name(b *metrics.Bucket) string {
	s.seq++
	return fmt.Sprintf("%020d-%06d%s", b.Start.UnixNano(), s.seq, ext)
}

// write publishes the file atomically: a crash midway leaves a temporary file
// that is ignored on read, never a half-written window that would fail to parse
// forever after.
func (s *Spool) write(name string, payload []byte) error {
	final := filepath.Join(s.Dir, name)

	tmp, err := os.CreateTemp(s.Dir, "tmp-*")
	if err != nil {
		return fmt.Errorf("spool: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return fmt.Errorf("spool: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("spool: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("spool: close: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("spool: chmod: %w", err)
	}

	return os.Rename(tmp.Name(), final)
}

// files returns the spooled filenames in chronological order.
func (s *Spool) files() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("spool: read dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue // skip leftover temporary files from an interrupted write
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	return names, nil
}

// Peek returns up to n of the oldest windows, oldest first.
//
// An entry that cannot be decoded is deleted and skipped. A single corrupt file
// must not be able to block delivery of everything queued behind it, which is
// what retrying it forever would amount to.
func (s *Spool) Peek(n int) ([]Entry, error) {
	names, err := s.files()
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, min(n, len(names)))
	for _, name := range names {
		if len(out) == n {
			break
		}

		payload, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil {
			return nil, fmt.Errorf("spool: read %s: %w", name, err)
		}

		var b metrics.Bucket
		if err := json.Unmarshal(payload, &b); err != nil {
			_ = os.Remove(filepath.Join(s.Dir, name))
			continue
		}

		out = append(out, Entry{Name: name, Bucket: &b})
	}

	return out, nil
}

// Discard removes entries the platform has acknowledged.
func (s *Spool) Discard(entries []Entry) error {
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.Dir, e.Name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("spool: remove %s: %w", e.Name, err)
		}
	}
	return nil
}

// Stats reports how many windows are queued and how much disk they occupy.
func (s *Spool) Stats() (count int, bytes int64, err error) {
	names, err := s.files()
	if err != nil {
		return 0, 0, err
	}

	for _, name := range names {
		info, err := os.Stat(filepath.Join(s.Dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, 0, fmt.Errorf("spool: stat %s: %w", name, err)
		}
		bytes += info.Size()
	}

	return len(names), bytes, nil
}

// enforceCap deletes the oldest windows until the spool fits its bound.
func (s *Spool) enforceCap() error {
	names, err := s.files()
	if err != nil {
		return err
	}

	sizes := make(map[string]int64, len(names))
	var total int64
	for _, name := range names {
		info, err := os.Stat(filepath.Join(s.Dir, name))
		if err != nil {
			continue
		}
		sizes[name] = info.Size()
		total += info.Size()
	}

	dropped := 0
	for i, name := range names {
		if total <= s.maxBytes() {
			break
		}
		// Always keep the newest window, even when it alone exceeds the cap.
		// Emptying the spool completely would leave the agent unable to hold
		// anything at all, silently discarding every window it ever collects.
		if i == len(names)-1 {
			break
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("spool: drop %s: %w", name, err)
		}
		total -= sizes[name]
		dropped++
	}

	if dropped > 0 && s.OnDrop != nil {
		s.OnDrop(dropped)
	}

	return nil
}
