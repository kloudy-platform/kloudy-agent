package collect

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sectorSize is the unit /proc/diskstats counts in. The kernel always reports
// 512-byte sectors here regardless of the drive's physical sector size, so this
// is a protocol constant and not a property of the hardware.
const sectorSize = 512

// wholeDisk matches physical block devices but not their partitions, so that
// I/O is not counted twice (once for sda1 and again for sda). Virtual devices
// (loop, dm, ram, zram) are excluded: they either duplicate traffic already
// counted on the backing device or represent no real hardware.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

// virtualIface reports whether an interface should be excluded from the traffic
// totals. Loopback is not real traffic, and bridges and veth pairs carry copies
// of packets that are already counted on the physical interface they front, so
// including them would inflate the totals several times over.
func virtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{
		"veth", "docker", "br-", "virbr", "tap", "tun",
		"dummy", "cni", "flannel", "cali", "kube",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (c *Collector) readFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.root(), rel))
}

// parseCPU reads the aggregate "cpu" line of /proc/stat.
func (c *Collector) parseCPU() (CPUTimes, error) {
	raw, err := c.readFile("stat")
	if err != nil {
		return CPUTimes{}, fmt.Errorf("read stat: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}

		// The aggregate line carries at least 8 counters on every kernel we
		// support. Newer kernels append guest and guest_nice, which are already
		// included in user and nice, so adding them would double-count.
		if len(fields) < 9 {
			return CPUTimes{}, fmt.Errorf("stat: cpu line has %d fields, want at least 9", len(fields))
		}

		var v [8]uint64
		for i := range v {
			n, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return CPUTimes{}, fmt.Errorf("stat: field %d: %w", i+1, err)
			}
			v[i] = n
		}

		return CPUTimes{
			User: v[0], Nice: v[1], System: v[2], Idle: v[3],
			IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7],
		}, nil
	}

	return CPUTimes{}, fmt.Errorf("stat: no cpu line")
}

// parseMemory reads /proc/meminfo, whose values are in kibibytes.
func (c *Collector) parseMemory() (Memory, error) {
	raw, err := c.readFile("meminfo")
	if err != nil {
		return Memory{}, fmt.Errorf("read meminfo: %w", err)
	}

	want := map[string]*uint64{}
	var m Memory
	want["MemTotal"] = &m.Total
	want["MemAvailable"] = &m.Available
	want["MemFree"] = &m.Free
	want["Buffers"] = &m.Buffers
	want["Cached"] = &m.Cached
	want["SwapTotal"] = &m.SwapTotal
	want["SwapFree"] = &m.SwapFree

	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		target, ok := want[key]
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return Memory{}, fmt.Errorf("meminfo: %s has no value", key)
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("meminfo: %s: %w", key, err)
		}
		*target = n * 1024
		seen[key] = true
	}

	// MemTotal and MemAvailable are the two figures every derived memory number
	// depends on. Missing either means the reading is unusable, and reporting a
	// zero would render as a healthy machine with no memory in use.
	for _, key := range []string{"MemTotal", "MemAvailable"} {
		if !seen[key] {
			return Memory{}, fmt.Errorf("meminfo: %s missing", key)
		}
	}

	return m, nil
}

// parseLoad reads the run-queue averages from /proc/loadavg.
func (c *Collector) parseLoad() (Load, error) {
	raw, err := c.readFile("loadavg")
	if err != nil {
		return Load{}, fmt.Errorf("read loadavg: %w", err)
	}

	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("loadavg: %d fields, want at least 3", len(fields))
	}

	var out [3]float64
	for i := range out {
		n, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Load{}, fmt.Errorf("loadavg: field %d: %w", i, err)
		}
		out[i] = n
	}

	return Load{One: out[0], Five: out[1], Fifteen: out[2]}, nil
}

// parseUptime reads how long the machine has been up, from /proc/uptime.
func (c *Collector) parseUptime() (time.Duration, error) {
	raw, err := c.readFile("uptime")
	if err != nil {
		return 0, fmt.Errorf("read uptime: %w", err)
	}

	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, fmt.Errorf("uptime: empty")
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}

	return time.Duration(seconds * float64(time.Second)), nil
}

// parseNet sums traffic across physical interfaces from /proc/net/dev.
func (c *Collector) parseNet() (NetCounters, error) {
	raw, err := c.readFile("net/dev")
	if err != nil {
		return NetCounters{}, fmt.Errorf("read net/dev: %w", err)
	}

	var n NetCounters
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue // the two header lines carry no colon-terminated name
		}

		name = strings.TrimSpace(name)
		if virtualIface(name) {
			continue
		}

		// 8 receive columns then 8 transmit columns, in that order.
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			return NetCounters{}, fmt.Errorf("net/dev: %s has %d columns, want 16", name, len(fields))
		}

		for _, col := range []struct {
			index  int
			target *uint64
		}{
			{0, &n.RxBytes}, {1, &n.RxPackets}, {2, &n.RxErrors},
			{8, &n.TxBytes}, {9, &n.TxPackets}, {10, &n.TxErrors},
		} {
			v, err := strconv.ParseUint(fields[col.index], 10, 64)
			if err != nil {
				return NetCounters{}, fmt.Errorf("net/dev: %s column %d: %w", name, col.index, err)
			}
			*col.target += v
		}
	}

	return n, nil
}

// parseDiskIO sums I/O across whole physical disks from /proc/diskstats.
func (c *Collector) parseDiskIO() (DiskCounters, error) {
	raw, err := c.readFile("diskstats")
	if err != nil {
		return DiskCounters{}, fmt.Errorf("read diskstats: %w", err)
	}

	var d DiskCounters
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		if !wholeDisk.MatchString(fields[2]) {
			continue
		}

		// major minor name reads merged sectors-read ms writes merged sectors-written
		//   0     1    2     3      4         5       6    7      8          9
		for _, col := range []struct {
			index  int
			scale  uint64
			target *uint64
		}{
			{3, 1, &d.ReadOps},
			{5, sectorSize, &d.ReadBytes},
			{7, 1, &d.WriteOps},
			{9, sectorSize, &d.WriteBytes},
		} {
			v, err := strconv.ParseUint(fields[col.index], 10, 64)
			if err != nil {
				return DiskCounters{}, fmt.Errorf("diskstats: %s column %d: %w", fields[2], col.index, err)
			}
			*col.target += v * col.scale
		}
	}

	return d, nil
}

// parseBootID reads the identifier the kernel regenerates on every boot.
//
// It is what lets a consumer tell a genuine counter rollover from a reboot: the
// monotonic counters restart at zero on reboot, and without this marker the
// resulting negative delta would be silently clamped or charted as a spike.
func (c *Collector) parseBootID() (string, error) {
	raw, err := c.readFile("sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot_id: %w", err)
	}

	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("boot_id: empty")
	}

	return id, nil
}
