package metrics

import (
	"encoding/json"
	"math"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
)

// Stat is the min, mean and max of one metric over a window.
//
// Keeping the extremes is the whole point of aggregating on the agent. An
// average alone erases exactly the events worth looking at: a two-second burst
// to full CPU inside a ten-second window moves the mean by a few points and
// disappears entirely once that window is later rolled up into an hour.
type Stat struct {
	Min float64
	Max float64
	Sum float64
	N   int
}

// Add folds one reading into the statistic.
func (s *Stat) Add(v float64) {
	if s.N == 0 || v < s.Min {
		s.Min = v
	}
	if s.N == 0 || v > s.Max {
		s.Max = v
	}
	s.Sum += v
	s.N++
}

// Avg returns the mean, or zero if nothing was recorded.
func (s Stat) Avg() float64 {
	if s.N == 0 {
		return 0
	}
	return s.Sum / float64(s.N)
}

// MarshalJSON emits the compact triple the wire format carries, rounded to two
// decimals. At this resolution the extra digits describe sampling noise rather
// than the machine, and they cost real bandwidth across a fleet.
func (s Stat) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Min float64 `json:"min"`
		Avg float64 `json:"avg"`
		Max float64 `json:"max"`
	}{round2(s.Min), round2(s.Avg()), round2(s.Max)})
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// Bucket is one aggregation window, the unit the agent ships.
type Bucket struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`

	// Samples is how many readings actually landed in this window.
	//
	// It travels with the data so the platform can tell a fully observed window
	// from a thin one. A bucket built from two readings instead of ten is still
	// usable, but it is not the same evidence, and silently presenting it as
	// such is how a monitoring system starts lying.
	Samples int `json:"samples"`

	BootID string `json:"boot_id"`

	CPUBusy   Stat `json:"cpu_busy"`
	CPUIOWait Stat `json:"cpu_iowait"`
	CPUSteal  Stat `json:"cpu_steal"`
	CPUUser   Stat `json:"cpu_user"`
	CPUSystem Stat `json:"cpu_system"`

	MemUsed  Stat `json:"mem_used"`
	SwapUsed Stat `json:"swap_used"`

	Load1 Stat `json:"load1"`

	NetRx     Stat `json:"net_rx"`
	NetTx     Stat `json:"net_tx"`
	DiskRead  Stat `json:"disk_read"`
	DiskWrite Stat `json:"disk_write"`

	// Totals change so rarely that a min/max/avg would be noise. They are
	// carried as the last observed value instead.
	MemTotal  uint64               `json:"mem_total"`
	SwapTotal uint64               `json:"swap_total"`
	FS        []collect.Filesystem `json:"fs"`

	// Counters is the closing raw reading of the window. It lets the platform
	// reconcile long-run totals independently of the derived rates, and detect a
	// reboot between two buckets that no single interval spans.
	Counters Counters `json:"counters"`
}

// Counters carries the cumulative kernel values as of the end of a bucket.
type Counters struct {
	NetRxBytes     uint64 `json:"net_rx_bytes"`
	NetTxBytes     uint64 `json:"net_tx_bytes"`
	DiskReadBytes  uint64 `json:"disk_read_bytes"`
	DiskWriteBytes uint64 `json:"disk_write_bytes"`
	UptimeSeconds  uint64 `json:"uptime_seconds"`
}

func (b *Bucket) add(r Rates, s collect.Sample) {
	b.Samples++
	b.BootID = s.BootID

	b.CPUBusy.Add(r.CPUBusy)
	b.CPUIOWait.Add(r.CPUIOWait)
	b.CPUSteal.Add(r.CPUSteal)
	b.CPUUser.Add(r.CPUUser)
	b.CPUSystem.Add(r.CPUSystem)

	b.MemUsed.Add(float64(r.MemUsed))
	b.SwapUsed.Add(float64(r.SwapUsed))
	b.Load1.Add(r.Load1)

	b.NetRx.Add(r.NetRx)
	b.NetTx.Add(r.NetTx)
	b.DiskRead.Add(r.DiskRead)
	b.DiskWrite.Add(r.DiskWrite)

	b.MemTotal = r.MemTotal
	b.SwapTotal = r.SwapTotal
	b.FS = r.FS

	b.Counters = Counters{
		NetRxBytes:     s.Net.RxBytes,
		NetTxBytes:     s.Net.TxBytes,
		DiskReadBytes:  s.Disk.ReadBytes,
		DiskWriteBytes: s.Disk.WriteBytes,
		UptimeSeconds:  uint64(s.Uptime.Seconds()),
	}
}

// Aggregator folds a stream of samples into fixed, wall-clock-aligned windows.
//
// Windows are aligned to the clock rather than to the agent's start time so that
// every machine in a fleet reports the same boundaries. Buckets from different
// servers then line up without interpolation, which is what makes them
// comparable and cheap to roll up.
type Aggregator struct {
	// Window is the bucket width. Zero means DefaultWindow.
	Window time.Duration

	prev    collect.Sample
	hasPrev bool
	current *Bucket
}

// DefaultWindow is the bucket width the agent ships by default.
const DefaultWindow = 10 * time.Second

func (a *Aggregator) window() time.Duration {
	if a.Window <= 0 {
		return DefaultWindow
	}
	return a.Window
}

// Add folds one sample into the current window.
//
// It returns a bucket only when the sample belongs to a later window, meaning
// the previous one is complete and ready to ship. The first sample after start,
// and any sample that cannot be derived from its predecessor, produces nothing:
// a rate needs two readings and an unbroken counter between them.
func (a *Aggregator) Add(s collect.Sample) *Bucket {
	prev, hasPrev := a.prev, a.hasPrev
	a.prev, a.hasPrev = s, true

	if !hasPrev {
		return nil
	}

	rates, ok := Derive(prev, s)
	if !ok {
		// A reboot or a counter reset. The window in progress describes a
		// machine that no longer exists, so it is closed and shipped as-is
		// rather than mixed with readings from the new boot.
		return a.Flush()
	}

	start := s.At.Truncate(a.window())

	var completed *Bucket
	if a.current != nil && !a.current.Start.Equal(start) {
		completed = a.Flush()
	}

	if a.current == nil {
		a.current = &Bucket{Start: start, End: start.Add(a.window())}
	}

	a.current.add(rates, s)

	return completed
}

// Flush closes the window in progress and returns it, or nil if it is empty.
// Call it before shutting down so the last partial window is not lost.
func (a *Aggregator) Flush() *Bucket {
	b := a.current
	a.current = nil

	if b == nil || b.Samples == 0 {
		return nil
	}

	return b
}
