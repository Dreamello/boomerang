# Boomerang

Measure end-to-end and per-leg round-trip latency through a chain of UDP agents,
from a single probe. Each node uses its own monotonic clock; no clock synchronization
is required.

```text
source → relay → destination
       ←       ←
```

## Install

Go 1.25+, Linux or macOS.

```bash
git clone https://github.com/Dreamello/boomerang.git
cd boomerang
go install .
```

Add your Go binary directory to `PATH` (`~/go/bin` by default).

## Setup

Generate a shared key:

```bash
mkdir -p ~/.config/boomerang
(umask 077; set -C; openssl rand -hex 32 > ~/.config/boomerang/key)
```

Use the same key on the source and every agent. Start an agent on each relay
and destination, with UDP 8888 allowed through its firewall:

```bash
boomerang --agent
```

The default key path is `~/.config/boomerang/key`, falling back to
`/etc/boomerang.key` on Linux. Override it with `--key-file PATH`.

## Usage

```bash
boomerang 10.0.0.20                          # direct UDP probe
boomerang 10.0.0.10 10.0.0.20 -c 20          # via a relay
boomerang 10.0.0.10 10.0.0.20 10.0.0.30      # via two relays
boomerang relay.example target.example       # hostnames also work
boomerang 10.0.0.10:9000 10.0.0.20           # per-hop port
boomerang --sport 30000 10.0.0.10 10.0.0.20  # pin source port
boomerang --json -c 20 10.0.0.10 10.0.0.20 > run.json
```

Replace the example IPs and hostnames with your hosts. List hops in traversal
order; flags can appear before, between, or after them. Run `boomerang --help`
for all options.

| Option | Meaning |
|---|---|
| `-c N` | Stop after N probes; default runs until interrupted |
| `-i 1s` | Probe interval |
| `-W 2s` | Reply timeout |
| `--holds` | Show hold time per agent |
| `--port N` | Default hop port, or agent listening port; default 8888 |
| `--sport N` | Source UDP port; default is OS-assigned |
| `--json` | JSON summary on stdout; probe lines on stderr |

`--sport` controls only the first leg. Each agent forwards and replies from its
own listening port. A `host:port` argument requires an agent listening there.

### ICMP destination

The last agent can ping a target that does not run Boomerang:

```bash
boomerang --icmp-last 10.0.0.10 10.0.0.20 -c 10
```

Use an IPv4 target. On Linux, the last agent's group must be allowed by
`net.ipv4.ping_group_range`. Restart the agent after changing that setting.
`--icmp-wait` sets the agent's ICMP reply timeout (default 1s).

## Output

```text
$ boomerang -c 3 -i 0.1s 10.0.0.10 10.0.0.20
BOOMERANG 10.0.0.20 via 10.0.0.10 sport=56087
seq=0 leg0=0.27ms leg1=0.23ms hold=0.26ms total=0.76ms
seq=1 leg0=0.38ms leg1=0.40ms hold=0.09ms total=0.87ms
seq=2 leg0=0.19ms leg1=0.15ms hold=0.07ms total=0.40ms

--- boomerang statistics (via 1 relay) ---
3 probes sent, 3 returned, 0.0% loss, time 301ms
                               min   avg   max  mdev  n
leg0  10.0.0.1  -> 10.0.0.10  0.19  0.28  0.38  0.07  3
leg1  10.0.0.10 -> 10.0.0.20  0.15  0.26  0.40  0.09  3
hold                          0.07  0.14  0.26  0.08  3
total                         0.40  0.68  0.87  0.18  3
```

- **legN** — round-trip interval for a hop pair; `leg0` starts at the source.
- **hold** — agent processing time outside its downstream wait.
- **total** — end-to-end RTT; legs plus holds add up to it.
- **mdev** — mean absolute deviation, not iputils ping's standard deviation.
- **n** — samples in the summary series.

Times are in milliseconds. Scheduling and socket overhead affect measurements;
concurrent ICMP probes can also include local queueing. Lost probes cannot
identify which leg dropped them.

## Linux service

Requires systemd 247+. Build for the target architecture (`arm64` for ARM hosts):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o boomerang-linux .
bash deploy/deploy-host.sh operator@relay.example ./boomerang-linux ~/.config/boomerang/key
```

The SSH user needs root or passwordless sudo. The script installs and enables
`boomerang-agent.service`. It also accepts `--jump`, `--identity`, and `--proxy`;
see [the script](deploy/deploy-host.sh) for details.

## Development

```bash
go test ./... -count=1 -race
go vet ./...
```

Boomerang is intended for trusted nodes. Packets are HMAC-authenticated, not
encrypted or replay-protected.

## License

Licensed under the [MIT License](LICENSE).
