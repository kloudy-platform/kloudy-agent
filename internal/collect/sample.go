// Package collect reads raw resource counters from a Linux /proc tree.
//
// The collector never computes rates. It reports cumulative counters exactly as
// the kernel exposes them, plus the instant they were read. Rates are derived
// downstream, over the real interval between two samples, which stays correct
// even when a batch is replayed late after a network outage.
package collect

import "time"

// Sample is one point-in-time reading of a machine's resource counters.
//
// CPU, Net and Disk hold monotonic counters that only ever grow, until the
// machine reboots and the kernel resets them to zero. BootID exists to make
// that reset detectable: a consumer seeing a new BootID must discard the
// previous counters rather than compute a nonsensical negative delta.
type Sample struct {
	At     time.Time     `json:"at"`
	BootID string        `json:"boot_id"`
	Uptime time.Duration `json:"uptime"`

	CPU  CPUTimes     `json:"cpu"`
	Mem  Memory       `json:"mem"`
	Load Load         `json:"load"`
	Net  NetCounters  `json:"net"`
	Disk DiskCounters `json:"disk"`
	FS   []Filesystem `json:"fs"`
}

// CPUTimes holds cumulative time spent in each CPU state, in USER_HZ jiffies,
// aggregated across all cores (the "cpu" line of /proc/stat).
//
// IOWait and Steal are kept as first-class fields on purpose. Folding IOWait
// into idle hides disk contention, and folding Steal into busy time makes a
// noisy neighbour on a shared hypervisor indistinguishable from the customer's
// own load. Both are among the most common causes of a slow VPS.
type CPUTimes struct {
	User    uint64 `json:"user"`
	Nice    uint64 `json:"nice"`
	System  uint64 `json:"system"`
	Idle    uint64 `json:"idle"`
	IOWait  uint64 `json:"iowait"`
	IRQ     uint64 `json:"irq"`
	SoftIRQ uint64 `json:"softirq"`
	Steal   uint64 `json:"steal"`
}

// Total returns the sum of every CPU state, the denominator for any percentage
// derived from a delta between two samples.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// Memory holds instantaneous memory figures, in bytes.
//
// Available is what the kernel reports as actually obtainable without swapping
// (MemAvailable), which accounts for reclaimable page cache. Used memory should
// be derived as Total-Available, never as Total-Free: the latter counts the page
// cache as used and makes every healthy Linux box look like it is out of memory.
type Memory struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	Free      uint64 `json:"free"`
	Buffers   uint64 `json:"buffers"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swap_total"`
	SwapFree  uint64 `json:"swap_free"`
}

// Load holds the kernel's 1, 5 and 15 minute run-queue averages.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// NetCounters holds cumulative traffic summed over physical interfaces only.
type NetCounters struct {
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
}

// DiskCounters holds cumulative I/O summed over whole physical disks only.
type DiskCounters struct {
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	ReadOps    uint64 `json:"read_ops"`
	WriteOps   uint64 `json:"write_ops"`
}

// Filesystem holds usage for one mounted filesystem, in bytes.
//
// Total and Used follow df's definitions, so the numbers shown to a customer
// match what they see over SSH. Used therefore excludes the blocks reserved for
// root, which belong to neither figure.
type Filesystem struct {
	Mount string `json:"mount"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}
