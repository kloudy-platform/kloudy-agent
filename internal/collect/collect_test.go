package collect

import (
	"errors"
	"testing"
	"time"
)

// fixed is the sample instant used throughout, so no test depends on the clock.
var fixed = time.Date(2026, 8, 29, 9, 14, 0, 0, time.UTC)

// testCollector reads the checked-in /proc fixtures with a frozen clock and a
// stubbed statfs, so every assertion holds identically on macOS and Linux.
func testCollector(t *testing.T) *Collector {
	t.Helper()

	return &Collector{
		Root:   "testdata/proc",
		Mounts: []string{"/"},
		Now:    func() time.Time { return fixed },
		Usage: func(string) (uint64, uint64, error) {
			return 42_949_672_960, 12_884_901_888, nil
		},
	}
}

func TestCollectReadsEveryCounter(t *testing.T) {
	got, err := testCollector(t).Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if !got.At.Equal(fixed) {
		t.Errorf("At = %v, want %v", got.At, fixed)
	}
	if got.BootID != "3f2b1a9c-7d4e-4f81-9a2b-5c6d7e8f9a01" {
		t.Errorf("BootID = %q", got.BootID)
	}
	if want := 89123450 * time.Millisecond; got.Uptime != want {
		t.Errorf("Uptime = %v, want %v", got.Uptime, want)
	}
	if len(got.FS) != 1 || got.FS[0].Mount != "/" || got.FS[0].Used != 12_884_901_888 {
		t.Errorf("FS = %+v", got.FS)
	}
}

// The aggregate "cpu" line must be read as-is. Summing the per-core lines would
// double every counter on this two-core fixture.
func TestParseCPUUsesAggregateLine(t *testing.T) {
	got, err := testCollector(t).parseCPU()
	if err != nil {
		t.Fatalf("parseCPU() error = %v", err)
	}

	want := CPUTimes{
		User: 259871, Nice: 1204, System: 89234, Idle: 8912340,
		IOWait: 12045, IRQ: 0, SoftIRQ: 4521, Steal: 3102,
	}
	if got != want {
		t.Errorf("parseCPU() = %+v, want %+v", got, want)
	}
	if got, want := got.Total(), uint64(9282317); got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}
}

// iowait and steal are the two states the previous SSH script discarded, and
// they are the ones that explain a slow VPS. They must survive parsing.
func TestParseCPUKeepsIOWaitAndSteal(t *testing.T) {
	got, err := testCollector(t).parseCPU()
	if err != nil {
		t.Fatalf("parseCPU() error = %v", err)
	}

	if got.IOWait == 0 {
		t.Error("IOWait = 0, want the fixture value: disk contention would be invisible")
	}
	if got.Steal == 0 {
		t.Error("Steal = 0, want the fixture value: a noisy neighbour would be invisible")
	}
}

func TestParseMemoryConvertsKibibytesToBytes(t *testing.T) {
	got, err := testCollector(t).parseMemory()
	if err != nil {
		t.Fatalf("parseMemory() error = %v", err)
	}

	want := Memory{
		Total:     4025376 * 1024,
		Available: 2841920 * 1024,
		Free:      198432 * 1024,
		Buffers:   142208 * 1024,
		Cached:    2314560 * 1024,
		SwapTotal: 2097148 * 1024,
		SwapFree:  2091004 * 1024,
	}
	if got != want {
		t.Errorf("parseMemory() = %+v, want %+v", got, want)
	}
}

func TestParseLoad(t *testing.T) {
	got, err := testCollector(t).parseLoad()
	if err != nil {
		t.Fatalf("parseLoad() error = %v", err)
	}

	want := Load{One: 0.52, Five: 0.58, Fifteen: 0.59}
	if got != want {
		t.Errorf("parseLoad() = %+v, want %+v", got, want)
	}
}

// Loopback, bridges and veth pairs carry copies of traffic already counted on
// the physical interfaces, so including them would inflate the totals.
func TestParseNetSkipsVirtualInterfaces(t *testing.T) {
	got, err := testCollector(t).parseNet()
	if err != nil {
		t.Fatalf("parseNet() error = %v", err)
	}

	want := NetCounters{
		RxBytes: 987654321 + 1000, RxPackets: 1234567 + 10, RxErrors: 1,
		TxBytes: 87654321 + 2000, TxPackets: 987654 + 20, TxErrors: 2,
	}
	if got != want {
		t.Errorf("parseNet() = %+v, want %+v", got, want)
	}
}

func TestVirtualIface(t *testing.T) {
	tests := map[string]bool{
		"lo": true, "docker0": true, "veth9a1b": true, "br-1a2b": true,
		"virbr0": true, "tun0": true, "cali123": true,
		"eth0": false, "eth1": false, "ens3": false, "enp0s3": false, "wg0": false,
	}

	for name, want := range tests {
		if got := virtualIface(name); got != want {
			t.Errorf("virtualIface(%q) = %v, want %v", name, got, want)
		}
	}
}

// Counting a partition alongside its parent disk would double the I/O, and loop
// and dm devices duplicate traffic already counted on the backing device.
func TestParseDiskIOCountsWholeDisksOnly(t *testing.T) {
	got, err := testCollector(t).parseDiskIO()
	if err != nil {
		t.Fatalf("parseDiskIO() error = %v", err)
	}

	want := DiskCounters{
		ReadOps: 45678, ReadBytes: 3456789 * sectorSize,
		WriteOps: 98765, WriteBytes: 8901234 * sectorSize,
	}
	if got != want {
		t.Errorf("parseDiskIO() = %+v, want %+v", got, want)
	}
}

func TestWholeDisk(t *testing.T) {
	tests := map[string]bool{
		"sda": true, "vda": true, "xvda": true, "nvme0n1": true, "mmcblk0": true,
		"sda1": false, "vda2": false, "nvme0n1p1": false, "mmcblk0p1": false,
		"loop0": false, "dm-0": false, "ram0": false, "zram0": false,
	}

	for name, want := range tests {
		if got := wholeDisk.MatchString(name); got != want {
			t.Errorf("wholeDisk(%q) = %v, want %v", name, got, want)
		}
	}
}

// A partial sample is worse than no sample: a zeroed field is indistinguishable
// from a genuine idle reading once charted, so a broken collector would draw a
// calm line instead of an obvious gap.
func TestCollectFailsRatherThanReturningPartialSample(t *testing.T) {
	c := testCollector(t)
	c.Root = "testdata/does-not-exist"

	if _, err := c.Collect(); err == nil {
		t.Fatal("Collect() error = nil, want a failure on an unreadable /proc")
	}
}

func TestCollectPropagatesUsageFailure(t *testing.T) {
	sentinel := errors.New("statfs boom")
	c := testCollector(t)
	c.Usage = func(string) (uint64, uint64, error) { return 0, 0, sentinel }

	_, err := c.Collect()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Collect() error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestParseMemoryRejectsMissingRequiredField(t *testing.T) {
	c := testCollector(t)
	c.Root = "testdata/broken"

	if _, err := c.parseMemory(); err == nil {
		t.Fatal("parseMemory() error = nil, want a failure when MemAvailable is absent")
	}
}
