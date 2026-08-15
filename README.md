# Boomerang

Measures end-to-end **and per-leg** round-trip time through a chain of relays,
from a single probe.

`ping` tells you a path is slow. Running it on each hop separately measures
different paths at different moments. Boomerang sends one probe that collects
timestamps out and back, so every leg comes from the same round trip.

```
$ boomerang relay.example target.example
BOOMERANG target.example (198.51.100.20) via relay.example (192.0.2.101)
seq=0 leg0=89.20ms leg1=32.06ms hold=0.14ms total=121.41ms
seq=1 leg0=90.28ms leg1=32.00ms hold=0.12ms total=122.40ms
^C
--- boomerang statistics (via 1 relay) ---
6 probes sent, 6 returned, 0.0% loss, time 1.702s
                                       min     avg     max  mdev  n
leg0  192.0.2.36   -> 192.0.2.101     89.20   89.98   90.93  0.48  6
leg1  192.0.2.101 -> 198.51.100.20   32.00   32.07   32.11  0.03  6
hold                                  0.12    0.14    0.17  0.01  6
total                               121.41  122.19  123.18  0.48  6
```

- **legN** — wire time for one hop pair. `leg0` starts at the local address the
  kernel selected for the route.
- **hold** — time inside the boxes: a relay's inbound plus outbound processing,
  and the destination's turnaround. `--holds` splits it per node.
- **total** — the whole round trip, measured on the source's clock. Legs plus
  holds reconstruct it exactly.
- **mdev** — mean absolute deviation, iputils `ping`'s definition, so the figures
  compare directly with a plain ping run.

Read the **legs** to judge the path, **total** for what real traffic sees, and
**hold** against the legs to tell a loaded box from a slow network.

## Usage

```
boomerang [options] HOP [HOP ...]   measure; the last HOP is the destination
boomerang --agent [options]         run the relay/destination daemon
```

Any number of relays works. With none, boomerang is a plain UDP ping.

```bash
boomerang 192.0.2.101 198.51.100.20        # via one relay, 1/sec until Ctrl-C
boomerang -c 20 192.0.2.101 198.51.100.20  # exactly 20 probes
boomerang a.example b.example c.example    # two relays
```

| flag | meaning |
|---|---|
| `-c N` | stop after N probes (default: until interrupted) |
| `-i 1s` | interval between probes |
| `-W 2s` | per-probe reply timeout |
| `--holds` | hold time per node instead of one total |
| `--port 8888` | UDP port |
| `--key-file PATH` | pre-shared key (default `/etc/boomerang.key` on Linux, `~/.config/boomerang/key` elsewhere) |
| `--json` | print one JSON object at exit; probe lines go to stderr |
| `-v` | log dropped/unauthenticated packets |

Exit status is 0 when at least one probe returned, 1 when none did.

Times print to 0.01 ms, matching a measurement floor set by host scheduling
jitter of roughly half a millisecond. Timed-out probes count as loss; a reply
arriving after its deadline is reported as late and excluded. Interrupting drops
the in-flight probe from the totals, so Ctrl-C leaves the loss figure alone.

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

Each relay takes four timestamps: arrival and send outbound, arrival and send on
the return. The inner pair excludes the relay's own processing, so legs carry
wire time and holds are reported on their own. The destination turns the packet
around, so its full window is its hold.

`sum(legs) + sum(holds) = total`, asserted by the test suite at one, two, and
three hops.

**Relays are stateless.** The route and the timestamps travel inside the packet,
so a relay holds no configuration and nothing per probe: it reads its role off
the packet — more hops after me means forward, last hop means turn around. Every
node runs the identical binary with identical flags, so adding a hop means
starting an agent there and naming it on the source's command line. Each hop also
appends the address it received the packet from, which unwinds the return path
and carries a NAT'd source's observable address, visible only to its first hop.

**Names resolve once**, at the source, which puts addresses on the wire the way
`ping` resolves at startup and prints `PING host (ip)`. DNS therefore stays
outside the measured intervals, and the chain is pinned even under GeoDNS.

## Authentication

Every packet carries a truncated HMAC-SHA-256 tag; packets that fail
verification are dropped silently.

A `ping` responder replies only to the packet's own source address, so forging
one gains an attacker nothing. A boomerang agent sends to addresses named
*inside* the packet, and the MAC is what binds that routing to a holder of the
shared key. It also stops a forged reply injecting fabricated measurements.

The key is 32 bytes hex, read from a file — keeping it out of argv, the
environment, `ps`, and shell history. The file must be unreadable to other users.

```bash
openssl rand -hex 32 > /etc/boomerang.key && chmod 600 /etc/boomerang.key
```

The same key goes on every node, including the source.

## Deploying an agent

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -o boomerang .
bash deploy/deploy-host.sh root@relay.example ./boomerang /path/to/fleet.key
```

The script compares the remote SHA-256 against the local one and installs only on
a match — a 3.9 MB push over a lossy long-haul link truncated at 2,088,960 bytes
during development, and a partial binary installs quietly and then segfaults.

The systemd unit runs the agent unprivileged with `DynamicUser=yes` and passes
the key via `LoadCredential=`, so the key stays root-owned. Port 8888 is
unprivileged, so the agent binds with no capabilities.

## Limits

- **A lost probe cannot be attributed to a leg** — its timestamps went with it.
- **Each leg is a round trip**, so an asymmetric path reads as one figure.
- **The source's scheduling jitter lands in leg 0.** From a laptop on WiFi, leg 0
  showed mdev 10.63 ms where a wired host showed 0.36 ms on the same relay.
- **UDP and ICMP can measure the same path differently.** Cross-checked against
  `ping`, boomerang agreed to 0.08 ms on one leg and read 2.88 ms faster on
  another whose far end was a cloud VM — with larger packets, so not a size
  effect. Treat a boomerang figure and a historical ping figure as different
  measurements.
- **A firewall scoped to the relay blocks direct probes**, reporting 100% loss
  while ICMP still answers.

## Development

```bash
go test ./... -count=1 -race
```

The suite pins the property the design rests on: per-node clock offsets of up to
a full day, in both directions, leave every derived leg bit-identical to the
synchronised baseline. A companion test keeps that fixture honest by confirming
the offsets reach the stamps and that cross-clock arithmetic fails by exactly the
injected skew.