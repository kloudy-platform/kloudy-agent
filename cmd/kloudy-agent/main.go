// Command kloudy-agent samples a machine's resource counters, aggregates them
// into fixed windows, and uploads them to the Kloudy platform.
//
// It makes outbound requests only and listens on no port.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kloudy-platform/kloudy-agent/internal/agent"
	"github.com/kloudy-platform/kloudy-agent/internal/collect"
	"github.com/kloudy-platform/kloudy-agent/internal/metrics"
	"github.com/kloudy-platform/kloudy-agent/internal/ship"
	"github.com/kloudy-platform/kloudy-agent/internal/spool"
	"github.com/kloudy-platform/kloudy-agent/internal/wire"
)

// version is stamped at build time by the Makefile.
var version = "dev"

// DefaultSpoolDir is where undelivered windows wait.
const DefaultSpoolDir = "/var/lib/kloudy-agent/spool"

type mountList []string

func (m *mountList) String() string { return fmt.Sprint(*m) }

func (m *mountList) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kloudy-agent: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		root     = flag.String("root", collect.DefaultRoot, "proc filesystem to read")
		interval = flag.Duration("interval", time.Second, "how often to sample")
		window   = flag.Duration("window", metrics.DefaultWindow, "aggregation window width")
		flush    = flag.Duration("flush", time.Minute, "how often to upload")

		// The endpoint and token default from the environment so systemd can
		// supply them through an EnvironmentFile, and the token never has to
		// appear in a command line where any local user could read it in ps.
		endpoint = flag.String("endpoint", os.Getenv("KLOUDY_ENDPOINT"), "platform ingest URL (env KLOUDY_ENDPOINT)")
		token    = flag.String("token", os.Getenv("KLOUDY_TOKEN"), "server token (env KLOUDY_TOKEN)")
		spoolDir = flag.String("spool", envOr("KLOUDY_SPOOL_DIR", DefaultSpoolDir), "where undelivered windows wait (env KLOUDY_SPOOL_DIR)")

		once        = flag.Bool("once", false, "print a single raw sample and exit")
		printOnly   = flag.Bool("print", false, "write windows to stdout instead of uploading")
		showVersion = flag.Bool("version", false, "print the version and exit")

		mounts mountList
	)
	flag.Var(&mounts, "mount", "filesystem to measure, repeatable (default \"/\")")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	collector := &collect.Collector{Root: *root, Mounts: mounts}

	if *once {
		s, err := collector.Collect()
		if err != nil {
			return err
		}
		return writeJSON(s)
	}

	settings := wire.Settings{Interval: *interval, Window: *window, Flush: *flush}

	if *printOnly {
		return printWindows(collector, settings, log)
	}

	// Falling back to stdout when the credentials are missing would let a
	// misconfigured unit look healthy while sending nothing anywhere, so the
	// stdout mode is something you ask for rather than something you land in.
	if *endpoint == "" || *token == "" {
		return errors.New("--endpoint and --token are required (or use --print)")
	}

	client, err := ship.New(*endpoint, *token, version)
	if err != nil {
		return err
	}

	a := &agent.Agent{
		Collector: collector,
		Spool: &spool.Spool{
			Dir: *spoolDir,
			OnDrop: func(n int) {
				log.Warn("spool full, dropped oldest windows", slog.Int("windows", n))
			},
		},
		Uploader: client,
		Settings: settings,
		Log:      log,
	}

	return a.Run(withSignals(context.Background()))
}

// printWindows runs the sample and aggregate loop without uploading, so the
// numbers can be checked against top, free and iostat on a real machine before
// any of them reach a chart.
func printWindows(c *collect.Collector, s wire.Settings, log *slog.Logger) error {
	if _, err := c.Collect(); err != nil {
		return fmt.Errorf("first sample: %w", err)
	}

	agg := &metrics.Aggregator{Window: s.Window}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	ctx := withSignals(context.Background())
	log.Info("printing windows to stdout", slog.Duration("interval", s.Interval), slog.Duration("window", s.Window))

	for {
		select {
		case <-ticker.C:
			sample, err := c.Collect()
			if err != nil {
				log.Warn("sample failed", slog.String("error", err.Error()))
				continue
			}
			if b := agg.Add(sample); b != nil {
				if err := writeJSON(b); err != nil {
					return err
				}
			}

		case <-ctx.Done():
			if b := agg.Flush(); b != nil {
				if err := writeJSON(b); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// withSignals cancels the returned context on SIGINT or SIGTERM, which is how
// systemd asks the agent to stop and what triggers the final flush.
func withSignals(parent context.Context) context.Context {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	_ = stop // released when the process exits
	return ctx
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
