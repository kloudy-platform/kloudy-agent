// Command kloudy-agent samples a machine's resource counters and aggregates
// them into fixed windows.
//
// It currently writes those windows to stdout as JSON. Shipping them to the
// Kloudy platform is the next step; keeping the pipeline observable on the
// terminal first means the numbers can be checked against top, free and iostat
// on a real machine before any of them reach a chart.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/collect"
	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
)

// version is stamped at build time by the Makefile.
var version = "dev"

type mountList []string

func (m *mountList) String() string { return fmt.Sprint(*m) }

func (m *mountList) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("kloudy-agent: ")

	var (
		root        = flag.String("root", collect.DefaultRoot, "proc filesystem to read")
		interval    = flag.Duration("interval", time.Second, "how often to sample")
		window      = flag.Duration("window", metrics.DefaultWindow, "aggregation window width")
		once        = flag.Bool("once", false, "print a single raw sample and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		mounts      mountList
	)
	flag.Var(&mounts, "mount", "filesystem to measure, repeatable (default \"/\")")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	collector := &collect.Collector{Root: *root, Mounts: mounts}

	if *once {
		if err := printSample(collector); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := run(collector, *interval, *window); err != nil {
		log.Fatal(err)
	}
}

// printSample writes one raw reading, the view the agent has before any
// derivation. It is the fastest way to check on a real machine that the
// counters being read are the ones intended.
func printSample(c *collect.Collector) error {
	s, err := c.Collect()
	if err != nil {
		return err
	}

	return writeJSON(s)
}

func run(c *collect.Collector, interval, window time.Duration) error {
	// Fail before entering the loop rather than logging an error every tick, so
	// a misconfigured agent is obvious at startup instead of filling a log.
	if _, err := c.Collect(); err != nil {
		return fmt.Errorf("first sample: %w", err)
	}

	agg := &metrics.Aggregator{Window: window}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	log.Printf("sampling %s every %s, %s windows", c.Root, interval, window)

	for {
		select {
		case <-ticker.C:
			s, err := c.Collect()
			if err != nil {
				// One unreadable sample is not fatal: the window simply carries
				// fewer readings, and the count travels with it so the platform
				// can see the window is thin.
				log.Printf("sample failed: %v", err)
				continue
			}

			if b := agg.Add(s); b != nil {
				if err := writeJSON(b); err != nil {
					return err
				}
			}

		case <-stop:
			// Ship the window in progress rather than discarding up to a full
			// window of readings on every restart.
			if b := agg.Flush(); b != nil {
				if err := writeJSON(b); err != nil {
					return err
				}
			}
			log.Print("stopped")
			return nil
		}
	}
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
