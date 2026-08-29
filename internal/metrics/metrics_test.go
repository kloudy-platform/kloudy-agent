package metrics

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
)

const usrHz = 100 // jiffies per second per core, on a one-core fixture machine

// walker produces a stream of consecutive samples from a synthetic machine,
// advancing the kernel's cumulative counters the way a real one would.
type walker struct {
	at     time.Time
	cpu    collect.CPUTimes
	net    collect.NetCounters
	disk   collect.DiskCounters
	bootID string
}

func newWalker() *walker {
	return &walker{
		at:     time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC),
		bootID: "boot-a",
	}
}

// step advances one second, charging busy percent of it to user time and the
// remainder to idle, and moving the byte counters by the given deltas.
func (w *walker) step(busy int, netRx, diskRead uint64) collect.Sample {
	w.at = w.at.Add(time.Second)
	w.cpu.User += uint64(busy)
	w.cpu.Idle += uint64(usrHz - busy)
	w.net.RxBytes += netRx
	w.disk.ReadBytes += diskRead

	return collect.Sample{
		At:     w.at,
		BootID: w.bootID,
		Uptime: time.Hour,
		CPU:    w.cpu,
		Mem: collect.Memory{
			Total: 4 << 30, Available: 3 << 30, Free: 1 << 30,
			SwapTotal: 2 << 30, SwapFree: 2 << 30,
		},
		Load: collect.Load{One: 0.5, Five: 0.4, Fifteen: 0.3},
		Net:  w.net,
		Disk: w.disk,
		FS:   []collect.Filesystem{{Mount: "/", Total: 40 << 30, Used: 12 << 30}},
	}
}

func TestDeriveComputesCPUShares(t *testing.T) {
	w := newWalker()
	prev := w.step(0, 0, 0)
	cur := w.step(25, 0, 0)

	got, ok := Derive(prev, cur)
	if !ok {
		t.Fatal("Derive() ok = false, want true")
	}

	if got.CPUBusy != 25 {
		t.Errorf("CPUBusy = %v, want 25", got.CPUBusy)
	}
	if got.CPUIdle != 75 {
		t.Errorf("CPUIdle = %v, want 75", got.CPUIdle)
	}
	if sum := got.CPUBusy + got.CPUIOWait + got.CPUIdle; sum != 100 {
		t.Errorf("busy+iowait+idle = %v, want 100", sum)
	}
}

// Time waiting on disk must not be charged to idle. A machine stuck on I/O is
// not idle to the person paying for it, and the SSH script it replaces reported
// exactly that.
func TestDeriveReportsIOWaitOutsideIdle(t *testing.T) {
	w := newWalker()
	prev := w.step(0, 0, 0)

	cur := prev
	cur.At = prev.At.Add(time.Second)
	cur.CPU.IOWait += 40
	cur.CPU.Idle += 60

	got, ok := Derive(prev, cur)
	if !ok {
		t.Fatal("Derive() ok = false, want true")
	}

	if got.CPUIOWait != 40 {
		t.Errorf("CPUIOWait = %v, want 40", got.CPUIOWait)
	}
	if got.CPUIdle != 60 {
		t.Errorf("CPUIdle = %v, want 60", got.CPUIdle)
	}
	if got.CPUBusy != 0 {
		t.Errorf("CPUBusy = %v, want 0", got.CPUBusy)
	}
}

// Stolen time is capacity the customer paid for and did not get. It counts as
// unavailable, but stays separately reportable so it is never confused with
// their own workload.
func TestDeriveReportsStealSeparatelyInsideBusy(t *testing.T) {
	w := newWalker()
	prev := w.step(0, 0, 0)

	cur := prev
	cur.At = prev.At.Add(time.Second)
	cur.CPU.Steal += 30
	cur.CPU.Idle += 70

	got, ok := Derive(prev, cur)
	if !ok {
		t.Fatal("Derive() ok = false, want true")
	}

	if got.CPUSteal != 30 {
		t.Errorf("CPUSteal = %v, want 30", got.CPUSteal)
	}
	if got.CPUBusy != 30 {
		t.Errorf("CPUBusy = %v, want 30 (steal is unavailable capacity)", got.CPUBusy)
	}
	if got.CPUUser != 0 {
		t.Errorf("CPUUser = %v, want 0 (steal is not the customer's own load)", got.CPUUser)
	}
}

// Rates use the real elapsed time, not an assumed one second, so a late or
// delayed sample does not inflate throughput.
func TestDeriveUsesActualInterval(t *testing.T) {
	w := newWalker()
	prev := w.step(0, 0, 0)

	cur := prev
	cur.At = prev.At.Add(4 * time.Second)
	cur.CPU.Idle += 400
	cur.Net.RxBytes += 8000

	got, ok := Derive(prev, cur)
	if !ok {
		t.Fatal("Derive() ok = false, want true")
	}

	if got.NetRx != 2000 {
		t.Errorf("NetRx = %v, want 2000 (8000 bytes over 4s)", got.NetRx)
	}
}

// Used memory comes from MemAvailable. Deriving it from MemFree would count the
// reclaimable page cache as used and show every healthy machine as full.
func TestDeriveUsesAvailableNotFreeForMemory(t *testing.T) {
	w := newWalker()
	prev := w.step(0, 0, 0)
	cur := w.step(0, 0, 0)

	got, ok := Derive(prev, cur)
	if !ok {
		t.Fatal("Derive() ok = false, want true")
	}

	if want := uint64(1 << 30); got.MemUsed != want {
		t.Errorf("MemUsed = %d, want %d (total minus available)", got.MemUsed, want)
	}
}

func TestDeriveRejectsUnusableIntervals(t *testing.T) {
	w := newWalker()
	prev := w.step(10, 100, 100)

	reboot := w.step(10, 100, 100)
	reboot.BootID = "boot-b"

	backwards := w.step(10, 100, 100)
	backwards.Net.RxBytes = prev.Net.RxBytes - 1

	sameInstant := w.step(10, 100, 100)
	sameInstant.At = prev.At

	tests := map[string]collect.Sample{
		"reboot resets the counters":    reboot,
		"a counter moved backwards":     backwards,
		"no time elapsed between reads": sameInstant,
	}

	for name, cur := range tests {
		t.Run(name, func(t *testing.T) {
			if _, ok := Derive(prev, cur); ok {
				t.Error("Derive() ok = true, want false: the interval has no honest derivation")
			}
		})
	}
}

func TestStat(t *testing.T) {
	var s Stat
	for _, v := range []float64{4, 1, 10, 5} {
		s.Add(v)
	}

	if s.Min != 1 || s.Max != 10 || s.N != 4 {
		t.Errorf("Stat = %+v, want min 1 max 10 n 4", s)
	}
	if s.Avg != 5 {
		t.Errorf("Avg() = %v, want 5", s.Avg)
	}

	var empty Stat
	if empty.Avg != 0 {
		t.Errorf("Avg() on empty = %v, want 0", empty.Avg)
	}
}

// This is the reason the agent aggregates locally instead of shipping one
// reading every thirty seconds. A short burst is invisible in the mean and
// obvious in the max, and only the max survives later downsampling.
func TestBucketPreservesSpikeThatAverageHides(t *testing.T) {
	// One window wide enough to hold the whole burst, so the comparison is
	// between the mean and the max of the same ten readings.
	a := Aggregator{Window: time.Minute}
	w := newWalker()

	a.Add(w.step(0, 0, 0)) // priming read: a rate needs two

	// Eight quiet seconds and two saturated ones, inside one window.
	for range 8 {
		a.Add(w.step(5, 0, 0))
	}
	for range 2 {
		a.Add(w.step(100, 0, 0))
	}

	b := a.Flush()
	if b == nil {
		t.Fatal("Flush() = nil, want a bucket")
	}

	if b.Samples != 10 {
		t.Fatalf("Samples = %d, want 10", b.Samples)
	}
	if b.CPUBusy.Max != 100 {
		t.Errorf("CPUBusy.Max = %v, want 100: the burst must survive", b.CPUBusy.Max)
	}
	if b.CPUBusy.Min != 5 {
		t.Errorf("CPUBusy.Min = %v, want 5", b.CPUBusy.Min)
	}
	if avg := b.CPUBusy.Avg; avg > 30 {
		t.Errorf("CPUBusy.Avg = %v, want a mean that understates the burst", avg)
	}
}

// Windows align to the wall clock so that buckets from different servers share
// boundaries and can be compared and rolled up without interpolation.
func TestAggregatorAlignsWindowsToWallClock(t *testing.T) {
	a := Aggregator{Window: 10 * time.Second}
	w := newWalker()
	w.at = time.Date(2026, 8, 29, 9, 14, 3, 0, time.UTC) // mid-window start

	a.Add(w.step(10, 0, 0))
	a.Add(w.step(10, 0, 0))

	b := a.Flush()
	if b == nil {
		t.Fatal("Flush() = nil, want a bucket")
	}

	wantStart := time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC)
	if !b.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", b.Start, wantStart)
	}
	if !b.End.Equal(wantStart.Add(10 * time.Second)) {
		t.Errorf("End = %v, want %v", b.End, wantStart.Add(10*time.Second))
	}
}

func TestAggregatorEmitsOnWindowRollover(t *testing.T) {
	a := Aggregator{Window: 5 * time.Second}
	w := newWalker()

	var emitted []*Bucket
	for range 12 {
		if b := a.Add(w.step(10, 1000, 2000)); b != nil {
			emitted = append(emitted, b)
		}
	}

	if len(emitted) < 2 {
		t.Fatalf("emitted %d buckets, want at least 2 over 12 seconds of 5s windows", len(emitted))
	}
	for _, b := range emitted {
		if b.Samples == 0 {
			t.Error("emitted an empty bucket")
		}
		if b.NetRx.Avg != 1000 {
			t.Errorf("NetRx.Avg = %v, want 1000 bytes/s", b.NetRx.Avg)
		}
	}
}

// The first reading cannot produce a rate, and must not be reported as a bucket
// full of zeroes.
func TestAggregatorIgnoresFirstSample(t *testing.T) {
	var a Aggregator
	w := newWalker()

	if b := a.Add(w.step(10, 0, 0)); b != nil {
		t.Error("Add() returned a bucket for the very first sample")
	}
	if b := a.Flush(); b != nil {
		t.Error("Flush() returned a bucket built from a single sample")
	}
}

// A reboot ends the window: readings from before and after describe different
// counter epochs and must not be mixed into one statistic.
func TestAggregatorClosesWindowOnReboot(t *testing.T) {
	var a Aggregator
	w := newWalker()

	a.Add(w.step(10, 0, 0))
	a.Add(w.step(10, 0, 0))

	w.bootID = "boot-b"
	w.cpu = collect.CPUTimes{}
	w.net = collect.NetCounters{}
	w.disk = collect.DiskCounters{}

	b := a.Add(w.step(10, 0, 0))
	if b == nil {
		t.Fatal("Add() = nil across a reboot, want the pre-reboot window closed")
	}
	if b.BootID != "boot-a" {
		t.Errorf("BootID = %q, want the pre-reboot epoch", b.BootID)
	}
}

func TestBucketCarriesClosingCounters(t *testing.T) {
	var a Aggregator
	w := newWalker()

	a.Add(w.step(10, 500, 700))
	a.Add(w.step(10, 500, 700))
	a.Add(w.step(10, 500, 700))

	b := a.Flush()
	if b == nil {
		t.Fatal("Flush() = nil, want a bucket")
	}

	if b.Counters.NetRxBytes != w.net.RxBytes {
		t.Errorf("Counters.NetRxBytes = %d, want the closing value %d", b.Counters.NetRxBytes, w.net.RxBytes)
	}
	if b.Counters.DiskReadBytes != w.disk.ReadBytes {
		t.Errorf("Counters.DiskReadBytes = %d, want the closing value %d", b.Counters.DiskReadBytes, w.disk.ReadBytes)
	}
}

// Every window is written to the spool as JSON and read back before it is
// uploaded, so a Stat that cannot survive that round trip loses its mean between
// being measured and being reported. Holding a running sum instead of the mean
// itself did exactly that, and no test that stayed in memory could see it.
func TestBucketSurvivesTheRoundTripThroughTheSpool(t *testing.T) {
	a := Aggregator{Window: time.Minute}
	w := newWalker()

	a.Add(w.step(0, 0, 0))
	for _, busy := range []int{10, 20, 90, 30} {
		a.Add(w.step(busy, 1000, 2000))
	}

	before := a.Flush()
	if before == nil {
		t.Fatal("Flush() = nil, want a bucket")
	}

	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var after Bucket
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if after.Samples != before.Samples {
		t.Errorf("Samples = %d, want %d", after.Samples, before.Samples)
	}
	if after.BootID != before.BootID {
		t.Errorf("BootID = %q, want %q", after.BootID, before.BootID)
	}

	for name, pair := range map[string][2]Stat{
		"cpu_busy":  {before.CPUBusy, after.CPUBusy},
		"mem_used":  {before.MemUsed, after.MemUsed},
		"net_rx":    {before.NetRx, after.NetRx},
		"load1":     {before.Load1, after.Load1},
		"cpu_steal": {before.CPUSteal, after.CPUSteal},
	} {
		got, want := pair[1], pair[0]
		if got.Avg != round2(want.Avg) {
			t.Errorf("%s avg = %v, want %v", name, got.Avg, round2(want.Avg))
		}
		if got.Min != round2(want.Min) {
			t.Errorf("%s min = %v, want %v", name, got.Min, round2(want.Min))
		}
		if got.Max != round2(want.Max) {
			t.Errorf("%s max = %v, want %v", name, got.Max, round2(want.Max))
		}
	}

	// The burst must still be visible on the far side of the round trip.
	if after.CPUBusy.Max != 90 {
		t.Errorf("CPUBusy.Max = %v after decoding, want 90", after.CPUBusy.Max)
	}
	if after.CPUBusy.Avg == 0 {
		t.Error("CPUBusy.Avg = 0 after decoding: the mean did not survive the spool")
	}
}

// Re-encoding a decoded window must produce the same payload, because that is
// exactly what the agent does when it ships what it spooled.
func TestDecodedBucketReEncodesIdentically(t *testing.T) {
	a := Aggregator{Window: time.Minute}
	w := newWalker()
	a.Add(w.step(0, 0, 0))
	a.Add(w.step(42, 1234, 5678))
	a.Add(w.step(7, 4321, 8765))

	first, err := json.Marshal(a.Flush())
	if err != nil {
		t.Fatal(err)
	}

	var decoded Bucket
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}

	second, err := json.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("re-encoded payload differs from the original\nfirst:  %s\nsecond: %s", first, second)
	}
}
