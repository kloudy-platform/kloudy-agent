# Security

This agent runs on other people's production servers. That shapes what it is
allowed to do, and it is worth being explicit about.

## Reporting a vulnerability

Report privately, not as a public issue: **security@kloudy.dev**

Include what you found, how to reproduce it, and what an attacker gains. We will
acknowledge within 72 hours and keep you updated until it is resolved. If you
would like credit in the release notes, say so.

Please do not test against servers you do not own.

## What the agent can do

Deliberately very little, and the limits are structural rather than conventions
anyone has to remember:

- **It listens on no port.** All communication is outbound. Nothing on the
  network can reach it.
- **It reads. It does not execute.** It reads `/proc`, calls `statfs`, and posts
  the result. It spawns no processes and runs no shell.
- **A reply cannot make it do anything.** Everything the platform is allowed to
  send back is declared in `internal/wire`, and it is all integers. There is no
  representable response that carries a URL, a path or a command. An agent that
  can be told what to fetch and run is a botnet, and the distance between the two
  is exactly the shape of that struct.
- **It runs unprivileged.** A dedicated account with no shell and no
  capabilities. Reading these counters needs neither.
- **It is sandboxed.** The systemd unit in `deploy/` grants a read-only
  filesystem, no capabilities, no new privileges, and a single writable
  directory for its spool.

## Trust boundaries

**The token is scoped to one server and to writing metrics.** It cannot read
anything, cannot enumerate anything, and gives no access to the customer's
account. A stolen token lets an attacker falsify the charts of a machine they
have already compromised, which is not a meaningful escalation. It is stored on
the platform as a hash, so a database disclosure yields no usable credential.

**The download is the real target.** This binary is installed on every server
running Kloudy, which makes the release path the most valuable thing in this
repository to compromise. Accordingly:

- Releases are built in CI from a tagged commit, and publish `SHA256SUMS`.
- `deploy/install.sh` **requires** an expected checksum and verifies it before
  the download is made executable or moved anywhere it could be run from. There
  is no fallback and no override flag.
- Kloudy passes that checksum over the SSH channel it has already authenticated,
  so believing a malicious binary would require compromising both the download
  and that channel.

**The agent does not update itself.** Upgrades are driven by the platform over
SSH, in waves. A self-updating agent turns one bad release into a fleet-wide
outage on machines you can no longer reach.

## Out of scope

- An attacker who is already root on the monitored machine. They can read the
  token from disk, and they could equally read anything else. Nothing here is
  intended to defend a machine against its own administrator.
- Denial of service against the customer's own machine by way of an implausible
  configuration. The bounds in `internal/wire` reject those, but a hostile
  platform is not part of this threat model.
