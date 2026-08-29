# kloudy-agent

Resource metrics agent for [Kloudy](https://github.com/kloudy-platform).

It reads a Linux machine's counters from `/proc`, derives per-second rates, and
aggregates them into fixed windows carrying **min, mean and max** for every
metric.

This repository is a component of Kloudy, not a general-purpose monitoring tool.
It is open source so that anyone asked to run it on their server can read what it
does before doing so.

## What it does, and what it refuses to do

The agent is deliberately incapable of being useful to an attacker:

- **Outbound only.** It listens on no port. Nothing can connect to it.
- **Read-only.** It reads `/proc` and calls `statfs`. It executes nothing.
- **No remote code path.** The platform's replies may only carry integers,
  booleans and enumerations. Never a URL, a path, or a command. An agent that
  can be told to fetch and run something is a botnet, and the distance between
  the two is exactly this rule.
- **Unprivileged.** Reading these counters needs no root and no capabilities.

## Design notes

**Rates are derived on the machine, not on the platform.** Only the agent has
one-second resolution. Once ten seconds of counters have been folded into a
single delta, a two-second spike to full utilisation is gone for good.

**Windows keep the extremes, not just the mean.** An average erases exactly the
events worth looking at, and it erases them permanently: the burst is already
gone before the first roll-up happens.

**`iowait` and `steal` are first-class.** Folding wait-on-disk into idle hides
disk contention, and folding stolen time into busy makes a noisy neighbour on a
shared hypervisor indistinguishable from your own load. On a VPS these are two of
the most common causes of a machine feeling slow.

**Used memory is derived from `MemAvailable`, never `MemFree`.** The page cache
is reclaimable; counting it as used shows every healthy Linux box as full.

**A broken read fails loudly.** A partial sample with zeroed fields is worse than
no sample: once charted, a zero is indistinguishable from a genuinely quiet
machine, so a broken collector would draw a calm line over an outage.

**Reboots are detected, not smoothed.** Every sample carries `boot_id`. When it
changes, the kernel's counters have restarted at zero, and the interval spanning
the reboot is dropped rather than clamped.

## How it delivers

Windows are written to a bounded on-disk spool before any upload is attempted,
and removed only once the platform has acknowledged them. A network outage, a
platform deploy or an agent restart delays delivery instead of destroying data.

Failures are handled by what they mean, not by a single retry rule:

| Outcome | What happens |
| --- | --- |
| Accepted | Windows removed from the spool |
| `429`, `5xx`, network error | Kept, retried after an exponential backoff |
| `401`, `403` | **Kept.** A person can fix a token; the history they will want to look at afterwards must still be there |
| Other `4xx` | **Dropped.** The platform will never accept this payload, and keeping it would block every window queued behind it |

Retry delays are drawn uniformly from below the exponential ceiling rather than
set to it. Agents retrying on a deterministic schedule reconnect in lockstep
after an outage and hold the platform down through its own recovery.

The spool is bounded and drops its oldest windows when full, loudly. An agent
that fills the partition it is monitoring has caused a worse outage than the one
it existed to report.

## Status

Working: collection, rate derivation, windowed aggregation, disk spool with
replay, gzipped HTTPS upload, backoff with jitter, remote schedule configuration.

Not built yet: systemd unit, install script, release automation.

## Usage

```
kloudy-agent --once                 # print one raw sample and exit
kloudy-agent --print                # run the loop, write windows to stdout
kloudy-agent                        # run the loop and upload
kloudy-agent --interval 5s --window 30s --flush 2m
kloudy-agent --mount / --mount /var
```

The endpoint and token come from the environment so systemd can supply them
through an `EnvironmentFile` and the token never appears in a command line where
any local user could read it out of `ps`:

| Variable | Meaning |
| --- | --- |
| `KLOUDY_ENDPOINT` | Platform ingest URL. Must be https |
| `KLOUDY_TOKEN` | This server's token |
| `KLOUDY_SPOOL_DIR` | Where undelivered windows wait (default `/var/lib/kloudy-agent/spool`) |

Without credentials the agent refuses to start rather than falling back to
stdout, so a misconfigured unit cannot look healthy while sending nothing
anywhere. Use `--print` when that is what you actually want.

`--root` points the collector at a different `/proc` tree. It exists so the
parsers can be exercised against the fixtures in
`internal/collect/testdata/proc`, on a machine that has no `/proc` of its own:

```
kloudy-agent --once --root internal/collect/testdata/proc
```

## Installation

`deploy/install.sh` installs or upgrades the agent. Running it again with the
same version is a no-op; running it with a new one is an upgrade.

```
KLOUDY_AGENT_VERSION=1.4.0 \
KLOUDY_AGENT_SHA256=<checksum for this architecture> \
KLOUDY_ENDPOINT=https://ingest.kloudy.dev/v1/metrics \
KLOUDY_TOKEN=<this server's token> \
sudo -E bash install.sh
```

Every value comes from the environment, so no caller-supplied string is ever
interpolated into the script's source.

What it does: verifies the checksum before the download is made executable,
creates a `kloudy` account with no shell, installs the binary, writes
`/etc/kloudy/agent.env` at mode `0600`, installs the systemd unit, starts the
service, and confirms it is actually running before reporting success.

**The checksum is required and has no fallback.** This binary runs on every
server, which makes the download the most attractive supply-chain target in the
product, and one nobody verified is an unauthenticated code path onto all of
them. Never install this with `curl | bash`.

Detected automatically: `amd64` and `arm64`. Required on the machine: systemd,
`sha256sum`, and either `curl` or `wget`.

### How Kloudy installs it

Kloudy already holds an authenticated SSH session with the server, so it uploads
and runs this script over that channel rather than having the machine fetch it.
The script itself therefore never travels over an unauthenticated path, and the
expected checksum arrives the same way. Only the binary is downloaded, and it is
verified against a checksum the machine did not choose.

### Upgrades

The agent never updates itself. The platform's reply can tell an agent its
version is old, and the platform then schedules the upgrade over SSH, in waves.
A self-updating agent turns one bad release into a fleet-wide outage on machines
you can no longer reach.

### Uninstalling

```
sudo systemctl disable --now kloudy-agent
sudo rm -f /etc/systemd/system/kloudy-agent.service /usr/local/bin/kloudy-agent
sudo rm -rf /etc/kloudy /var/lib/kloudy-agent
sudo userdel kloudy
```

Removing the agent degrades monitoring to the platform's SSH collector rather
than breaking the server. Nothing else on the machine depends on it.

## Development

```
make check      # gofmt check, go vet, tests
make build      # local binary in dist/
make dist       # static linux/amd64 and linux/arm64 binaries plus SHA256SUMS
```

The test suite runs on any operating system. It reads checked-in `/proc`
fixtures and stubs the `statfs` syscall, so the parsing is verified without
needing a Linux machine.

## Releases

Release binaries are static and CGO-free, and every release publishes
`SHA256SUMS`. The install script must verify the checksum before executing what
it downloaded: this binary runs on every customer server, which makes it the most
attractive supply-chain target in the product.

## License

Apache 2.0. See [LICENSE](LICENSE).
