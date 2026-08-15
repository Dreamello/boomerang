# boomerang

Measures end-to-end **and per-leg** round-trip time through a chain of relays,
from a single probe.

`ping` tells you a path is slow. Running it on each hop separately measures
different paths at different moments. boomerang sends one probe that collects
timestamps out and back, so every leg comes from the same round trip.

```
$ boomerang relay.example target.example
BOOMERANG target.example (198.51.100.20) via relay.example (192.0.2.101)
seq=0 leg0=87.08ms leg1=31.99ms hold=0.33ms total=119.40ms
seq=1 leg0=87.21ms leg1=31.95ms hold=0.11ms total=119.27ms
seq=2 leg0=87.22ms leg1=31.93ms hold=0.12ms total=119.27ms
^C
--- boomerang statistics (via 1 relay) ---
6 probes sent, 6 returned, 0.0% loss, time 1.703s
                                                min     avg     max  mdev  n
leg0 192.0.2.5 -> 192.0.2.101:8888             86.98   87.19   87.46  0.11  6
leg1 192.0.2.101:8888 -> 198.51.100.20:8888   31.91   31.95   31.99  0.02  6
hold                                           0.11    0.16    0.33  0.06  6
total                                        119.01  119.30  119.55  0.12  6
```

## Usage

```
boomerang [options] HOP [HOP ...]   measure; the last HOP is the destination
boomerang --agent [options]         run the relay/destination daemon
```

Any number of relays works. With none, boomerang is a plain UDP ping.

```bash
boomerang 192.0.2.101 198.51.100.20           # via one relay, 1/sec until Ctrl-C
boomerang -c 20 192.0.2.101 198.51.100.20     # exactly 20 probes
boomerang 192.0.2.101                         # direct
boomerang a.example b.example c.example       # two relays
boomerang --holds 192.0.2.101 198.51.100.20   # hold time per node
```

| flag | meaning |
|---|---|
| `-c N` | stop after N probes (default: until interrupted) |
| `-i 1s` | interval between probes |
| `-W 2s` | per-probe reply timeout |
| `--holds` | show hold time per node instead of one total |
| `--port 8888` | UDP port |
| `--key-file PATH` | pre-shared key (default `/etc/boomerang.key` on Linux, `~/.config/boomerang/key` elsewhere) |
| `-v` | log dropped/unauthenticated packets |

Exit status is 0 when at least one probe returned, 1 when none did, so it works
in a shell conditional like `ping`.

### Reading the output

- **legN** — wire time for one hop pair. `leg0` starts at the local address the
  kernel selected for the route.
- **hold** — time inside the boxes: a relay's inbound plus outbound processing,
  and the destination's turnaround. `--holds` splits it per node as
  `hold0`, `hold1`, …, numbered to match the legs.
- **total** — the whole round trip, measured directly on the source's clock.
  Legs plus holds reconstruct it exactly.
- **mdev** — mean absolute deviation from the mean, iputils `ping`'s definition,
  so the figures compare directly with a plain ping run.
- **n** — samples behind each row. Timed-out probes are counted as loss and
  excluded from the statistics; a reply that arrives after its deadline is
  reported as late and also excluded. Interrupting a run drops the in-flight
  probe from the totals, so Ctrl-C leaves the loss figure alone.

Times print to 0.01 ms, matching a measurement floor set by host scheduling
jitter of roughly half a millisecond.

### Which number answers which question

| you want to know | read |
|---|---|
| how good is this path | the **legs** — wire time only |
| what real traffic experiences | **total** |
| is it the network or a loaded box | **hold** against the legs |

## How it works

Every node measures intervals on its **own** monotonic clock. A leg is the
difference between two adjacent intervals, so each node's clock offset appears
twice inside one subtraction and cancels. No clock synchronisation is required,
and because the cancellation is local to each pair it holds at any depth.

```
I0    = t3 - t0            round trip, source clock
W(k)  = t_out - t_in       full window at hop k, hop k's clock
W'(k) = t_rcv - t_fwd      wire-only window at hop k, hop k's clock

leg 0   = I0     - W(0)        source <-> first hop
leg k   = W'(k-1) - W(k)       hop k-1 <-> hop k
hold k  = W(k)   - W'(k)       time hop k held the packet
```

Each relay takes **four** timestamps: arrival and send outbound, arrival and
send on the return. The inner pair excludes the relay's own processing, so legs
carry wire time and holds are reported on their own. The destination turns the
packet around, so its full window is its hold.

`sum(legs) + sum(holds) = total` — a closed accounting of where the time went,
asserted by the test suite at one, two, and three hops.

### Stateless relays

The route and the timestamps travel **inside the packet**. A relay holds no
configuration, no session table, and nothing per probe: it reads its role off
the packet — more hops after me means forward, last hop means turn around.

Relays and the destination run the identical binary with identical flags, so
adding a hop means starting an agent on the new box and naming it on the
source's command line.

Each hop also appends the address it received the packet from, which is how the
return path unwinds with no relay state, and what carries a NAT'd source's
observable address — visible only to its first hop.

### Names are resolved once

The source resolves every hop at startup and puts addresses on the wire, the way
`ping` resolves once and prints `PING host (ip)`. Two things follow: DNS stays
outside the measured intervals, and the chain is pinned to the hosts resolved at
startup even under GeoDNS or round-robin. Agents resolve nothing.

## Authentication

Every packet carries a truncated HMAC-SHA-256 tag, and packets that fail
verification are dropped silently.

The reason is structural. A `ping` responder replies only to the packet's own
source address, so forging one gains an attacker nothing. A boomerang agent
sends to addresses named *inside* the packet, and the MAC is what binds that
routing to a holder of the shared key. It also means a forged reply cannot
inject fabricated measurements.

The key is 32 bytes, hex-encoded, read from a file — keeping it out of argv, the
environment, `ps`, and shell history. boomerang requires the file be unreadable
to other users.

```bash
openssl rand -hex 32 > /etc/boomerang.key && chmod 600 /etc/boomerang.key
```

The same key goes on every node in the chain, including the source.

## Deploying an agent

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -o boomerang .
bash deploy/deploy-host.sh root@relay.example ./boomerang /path/to/fleet.key
```

`deploy/deploy-host.sh` compares the remote SHA-256 against the local one and
retries up to three times, installing only on a match. A 3.9 MB push over a
lossy long-haul link truncated at 2,088,960 bytes during development, and a
partial binary installs quietly and then segfaults, which reads like a corrupt
build or the wrong architecture.

The systemd unit runs the agent unprivileged with `DynamicUser=yes` and passes
the key with `LoadCredential=`, so the key stays root-owned:

```ini
LoadCredential=key:/etc/boomerang.key
ExecStart=/usr/local/bin/boomerang --agent --port 8888 --key-file %d/key
DynamicUser=yes
```

Port 8888 is unprivileged, so the agent binds with no capabilities at all.

## Limits

Worth knowing before trusting a number:

- **A lost probe cannot be attributed to a leg.** Its timestamps went with it.
  You learn that a probe timed out, not where.
- **Each leg is a round trip**, so an outbound path that differs from the return
  path reads as one figure.
- **The source's scheduling jitter lands in leg 0.** From a laptop on WiFi, leg 0
  showed mdev 10.63 ms against the same relay where a wired host showed 0.36 ms.
  That is the source host being reported faithfully.
- **UDP and ICMP can measure the same path differently.** Cross-checked against
  `ping`, boomerang agreed to 0.08 ms on one leg and read 2.88 ms faster on
  another whose far end was a cloud VM — with larger packets, so not a size
  effect. Some networks handle ICMP on a slower path than UDP to an open port.
  For "how good is this path for real traffic" the UDP figure is the more
  representative one; treat a boomerang number and a historical ping number as
  measurements of different things.
- **A firewall scoped to the relay blocks direct probes.** When the destination
  accepts UDP only from the relay's address, probing it directly reports 100%
  loss while ICMP still answers.

## Development

```bash
go test ./... -count=1 -race     # 49 tests
hermes verify --skip-start       # build + vet + race tests
```

The suite pins the property the design rests on: per-node clock offsets of up to
a full day, in both directions, leave every derived leg bit-identical to the
synchronised baseline. A companion test keeps that fixture honest by confirming
the offsets reach the stamps and that cross-clock arithmetic fails by exactly
the injected skew.
