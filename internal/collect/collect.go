package collect

import (
	"errors"
	"fmt"
	"time"
)

// DefaultRoot is where the kernel exposes its counters on a real machine.
const DefaultRoot = "/proc"

// Collector reads one Sample from a Linux /proc tree.
//
// Root and Usage are injectable so the parsers can be exercised against
// checked-in fixtures on any operating system. That matters more than it looks:
// the agent is developed on macOS, which has no /proc at all, so without this
// seam none of the parsing could be tested until the code was already running
// on a customer's server.
type Collector struct {
	// Root is the /proc tree to read. Empty means DefaultRoot.
	Root string

	// Mounts lists the filesystems to measure. Empty means the root filesystem.
	Mounts []string

	// Now returns the sample instant. Empty means time.Now.
	Now func() time.Time

	// Usage returns the total and used bytes of the filesystem holding path.
	// Empty means the real statfs syscall.
	Usage func(path string) (total, used uint64, err error)
}

func (c *Collector) root() string {
	if c.Root == "" {
		return DefaultRoot
	}
	return c.Root
}

func (c *Collector) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Collector) mounts() []string {
	if len(c.Mounts) == 0 {
		return []string{"/"}
	}
	return c.Mounts
}

func (c *Collector) usage(path string) (uint64, uint64, error) {
	if c.Usage == nil {
		return diskUsage(path)
	}
	return c.Usage(path)
}

// Collect reads every counter and returns them as one Sample.
//
// A failure in any reader fails the whole sample rather than yielding a partial
// one with zeroed fields. A zero is indistinguishable from a genuine idle
// reading once it is charted, so a silent partial sample would draw a calm,
// confident line over what is actually a broken collector.
func (c *Collector) Collect() (Sample, error) {
	at := c.now()

	cpu, err := c.parseCPU()
	if err != nil {
		return Sample{}, err
	}

	mem, err := c.parseMemory()
	if err != nil {
		return Sample{}, err
	}

	load, err := c.parseLoad()
	if err != nil {
		return Sample{}, err
	}

	uptime, err := c.parseUptime()
	if err != nil {
		return Sample{}, err
	}

	net, err := c.parseNet()
	if err != nil {
		return Sample{}, err
	}

	disk, err := c.parseDiskIO()
	if err != nil {
		return Sample{}, err
	}

	bootID, err := c.parseBootID()
	if err != nil {
		return Sample{}, err
	}

	filesystems, err := c.collectFilesystems()
	if err != nil {
		return Sample{}, err
	}

	return Sample{
		At:     at.UTC(),
		BootID: bootID,
		Uptime: uptime,
		CPU:    cpu,
		Mem:    mem,
		Load:   load,
		Net:    net,
		Disk:   disk,
		FS:     filesystems,
	}, nil
}

func (c *Collector) collectFilesystems() ([]Filesystem, error) {
	mounts := c.mounts()
	out := make([]Filesystem, 0, len(mounts))

	for _, mount := range mounts {
		total, used, err := c.usage(mount)
		if err != nil {
			return nil, fmt.Errorf("usage %s: %w", mount, err)
		}
		out = append(out, Filesystem{Mount: mount, Total: total, Used: used})
	}

	return out, nil
}

// ErrUnsupported is returned by the real disk usage reader on platforms where
// it is not implemented.
var ErrUnsupported = errors.New("collect: unsupported platform")
