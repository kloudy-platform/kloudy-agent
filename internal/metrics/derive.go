// Package metrics turns raw counter samples into the rates and gauges a person
// actually reads, then aggregates them into fixed windows.
//
// Rates are derived here, on the machine, rather than on the platform. Only the
// agent has one-second resolution: once ten seconds of counters have been folded
// into a single delta, a two-second spike to full utilisation is gone for good
// and no amount of downstream processing brings it back.
package metrics

import (
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
)

// Rates is the derived view of the interval between two consecutive samples:
// percentages for CPU, bytes per second for throughput, and instantaneous
// values for anything the kernel already reports as a gauge.
type Rates struct {
	Interval time.Duration

	// CPUBusy, CPUIOWait and CPUIdle partition the interval and sum to 100.
	//
	// Waiting on disk is reported separately rather than folded into idle,
	// because a machine thrashing on I/O is not idle in any sense its owner
	// cares about, and the previous SSH script counted it as such.
	CPUBusy   float64
	CPUIOWait float64
	CPUIdle   float64

	// CPUUser, CPUSystem and CPUSteal break the busy share down further.
	//
	// Steal is time the hypervisor gave to another tenant while this machine had
	// work ready to run. It is counted inside CPUBusy because the capacity was
	// genuinely unavailable, but reported on its own so that a noisy neighbour
	// is never mistaken for the customer's own code.
	CPUUser   float64
	CPUSystem float64
	CPUSteal  float64

	MemUsed   uint64
	MemTotal  uint64
	SwapUsed  uint64
	SwapTotal uint64

	Load1  float64
	Load5  float64
	Load15 float64

	NetRx     float64
	NetTx     float64
	DiskRead  float64
	DiskWrite float64

	FS []collect.Filesystem
}

// Derive computes the rates between two consecutive samples.
//
// It reports false when no honest derivation exists: the machine rebooted, the
// samples are out of order, or the kernel's counters moved backwards. Callers
// must drop the interval rather than substitute a zero, because a zero is
// indistinguishable from a genuinely quiet machine on a chart.
func Derive(prev, cur collect.Sample) (Rates, bool) {
	interval := cur.At.Sub(prev.At)
	if interval <= 0 {
		return Rates{}, false
	}

	// A new boot id means the counters restarted at zero. Subtracting across
	// that boundary yields a huge negative delta, which is exactly the kind of
	// value that gets clamped to zero and quietly hides an outage.
	if cur.BootID != prev.BootID {
		return Rates{}, false
	}

	total := cur.CPU.Total() - prev.CPU.Total()
	if cur.CPU.Total() < prev.CPU.Total() || total == 0 {
		return Rates{}, false
	}

	share := func(now, before uint64) float64 {
		if now < before {
			return 0
		}
		return float64(now-before) / float64(total) * 100
	}

	idle := share(cur.CPU.Idle, prev.CPU.Idle)
	iowait := share(cur.CPU.IOWait, prev.CPU.IOWait)

	seconds := interval.Seconds()
	rate := func(now, before uint64) (float64, bool) {
		if now < before {
			return 0, false
		}
		return float64(now-before) / seconds, true
	}

	netRx, ok1 := rate(cur.Net.RxBytes, prev.Net.RxBytes)
	netTx, ok2 := rate(cur.Net.TxBytes, prev.Net.TxBytes)
	diskRead, ok3 := rate(cur.Disk.ReadBytes, prev.Disk.ReadBytes)
	diskWrite, ok4 := rate(cur.Disk.WriteBytes, prev.Disk.WriteBytes)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Rates{}, false
	}

	return Rates{
		Interval:  interval,
		CPUBusy:   100 - idle - iowait,
		CPUIOWait: iowait,
		CPUIdle:   idle,
		CPUUser:   share(cur.CPU.User, prev.CPU.User) + share(cur.CPU.Nice, prev.CPU.Nice),
		CPUSystem: share(cur.CPU.System, prev.CPU.System),
		CPUSteal:  share(cur.CPU.Steal, prev.CPU.Steal),

		// Used memory is derived from MemAvailable, not MemFree: the page cache
		// is reclaimable, and counting it as used makes every healthy Linux box
		// look permanently out of memory.
		MemUsed:   saturatingSub(cur.Mem.Total, cur.Mem.Available),
		MemTotal:  cur.Mem.Total,
		SwapUsed:  saturatingSub(cur.Mem.SwapTotal, cur.Mem.SwapFree),
		SwapTotal: cur.Mem.SwapTotal,

		Load1:  cur.Load.One,
		Load5:  cur.Load.Five,
		Load15: cur.Load.Fifteen,

		NetRx:     netRx,
		NetTx:     netTx,
		DiskRead:  diskRead,
		DiskWrite: diskWrite,

		FS: cur.FS,
	}, true
}

func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
