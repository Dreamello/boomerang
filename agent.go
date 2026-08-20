package main

// The agent: one UDP socket, one event loop, no state between packets.
//
// An agent decides what to do from the packet alone:
//
//	outbound, more hops after me -> stamp in/fwd, forward to the next hop
//	outbound, I am the last hop  -> stamp in/out, turn the packet around
//	returning                    -> stamp rcv/out, pass it back upstream
//
// Because the route and the stamps travel in the packet, relays and the
// destination run the identical binary with identical flags, and adding a hop
// only means starting an agent there and naming it at the source.

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"strconv"
	"time"
)

// Agent serves probes on one UDP socket.
type Agent struct {
	conn *net.UDPConn
	key  []byte
	// start anchors this process's monotonic clock. Stamps are ns since start,
	// so they are immune to wall-clock steps (NTP, manual sets, DST).
	start time.Time
	// dropRate injects loss for testing. Zero in production.
	dropRate float64
	verbose  bool

	// ICMP last-hop termination. The socket is opened lazily on the first
	// probe that needs it; a box that never terminates pays nothing.
	icmp        icmpConn
	icmpTimeout time.Duration

	// Counters, for the health line only.
	served  uint64
	dropped uint64
}

// NewAgent binds the UDP socket.
func NewAgent(addr string, key []byte, dropRate float64, verbose bool, icmpTimeout time.Duration) (*Agent, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	return &Agent{
		conn:        conn,
		key:         key,
		start:       time.Now(),
		dropRate:    dropRate,
		verbose:     verbose,
		icmpTimeout: icmpTimeout,
	}, nil
}

// LocalAddr reports the bound address, useful when the port was chosen by the
// kernel (tests).
func (a *Agent) LocalAddr() *net.UDPAddr { return a.conn.LocalAddr().(*net.UDPAddr) }

// Close releases the socket.
func (a *Agent) Close() error { return a.conn.Close() }

// now returns this process's monotonic nanoseconds.
func (a *Agent) now() int64 { return int64(time.Since(a.start)) }

// Serve runs the event loop until the socket is closed.
func (a *Agent) Serve() error {
	buf := make([]byte, MaxPacket)
	for {
		n, src, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			return err // closed, or unrecoverable
		}
		// Stamp arrival before parsing, so decode cost lands in this node's
		// hold figure.
		arrived := a.now()
		a.handle(buf[:n], src, arrived)
	}
}

// handle processes one datagram. Unauthenticated or malformed traffic is
// dropped silently, so a scanner gets no response and leaves no journal noise.
func (a *Agent) handle(raw []byte, src *net.UDPAddr, arrived int64) {
	p, err := Decode(raw, a.key)
	if err != nil {
		a.dropped++
		if a.verbose {
			log.Printf("drop from %s: %v", src, err)
		}
		return
	}
	if a.dropRate > 0 && rand.Float64() < a.dropRate {
		if a.verbose {
			log.Printf("injected drop: seq=%d phase=%s hop=%d", p.Seq, p.Phase, p.Hop)
		}
		return
	}

	switch p.Phase {
	case PhaseOut:
		a.forwardOrTurn(p, src, arrived)
	case PhaseBack:
		a.unwind(p, arrived)
	}
}

// forwardOrTurn handles an outbound probe.
//
// Invariant on Hop, maintained by every node: p.Hop is the index of the node
// that should handle this packet NEXT. Outbound increments it; turning around
// and unwinding decrements it, so a returning packet stamps against the hop
// that handled it.
func (a *Agent) forwardOrTurn(p *Packet, src *net.UDPAddr, arrived int64) {
	hop := p.Hop
	p.StampIn(hop, arrived)

	// Record who handed us this packet, so the return path needs no state
	// here. A NAT'd source's observable address is only visible to this hop.
	p.AppendReply(src.String())

	if p.IsLast() {
		// ICMP termination: this agent is the last in the chain and the probe
		// has an ICMP target. Spawn the ping in its own goroutine so the main
		// event loop isn't blocked by ICMP RTT.
		if p.ICMPDest != "" {
			go a.terminate(p, hop)
			return
		}

		// Destination: turn it around, stamping TOut as late as possible so the
		// turnaround reflects the real dwell time.
		p.Phase = PhaseBack
		back, ok := p.ReturnAddr()
		if !ok {
			return
		}
		p.Hop = prevHop(hop)
		p.StampOut(hop, a.now())
		a.send(p, back)
		a.served++
		return
	}

	// Relay: forward to the next hop.
	p.Hop = hop + 1
	next := p.Chain[p.Hop]
	p.StampFwd(hop, a.now())
	a.send(p, next)
	a.served++
}

// prevHop steps one hop back toward the source, clamping at 0 so Hop stays a
// valid index into Chain on the final leg home.
func prevHop(hop int) int {
	if hop <= 0 {
		return 0
	}
	return hop - 1
}

// unwind handles a returning probe.
func (a *Agent) unwind(p *Packet, arrived int64) {
	hop := p.Hop
	p.StampRcv(hop, arrived)
	back, ok := p.ReturnAddr()
	if !ok {
		return
	}
	p.Hop = prevHop(hop)
	p.StampOut(hop, a.now())
	a.send(p, back)
}

// send encodes and transmits to a literal IP:port.
//
// Agents resolve nothing. The source resolves the whole chain once and puts
// addresses on the wire, which keeps DNS out of the interval between this
// node's two timestamps and out of reach of anyone crafting a packet.
func (a *Agent) send(p *Packet, addr string) {
	ua, err := parseAddr(addr)
	if err != nil {
		if a.verbose {
			log.Printf("refusing to send to %q: %v", addr, err)
		}
		return
	}
	buf, err := Encode(p, a.key)
	if err != nil {
		if a.verbose {
			log.Printf("encode: %v", err)
		}
		return
	}
	if _, err := a.conn.WriteToUDP(buf, ua); err != nil && a.verbose {
		log.Printf("send to %s: %v", addr, err)
	}
}

// parseAddr accepts a literal IP:port, keeping every relay send path free of
// name lookups.
func parseAddr(addr string) (*net.UDPAddr, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("%q is not a literal address", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad port %q: %w", portStr, err)
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

// Stats renders a one-line health summary.
func (a *Agent) Stats() string {
	return fmt.Sprintf("served=%d dropped=%d", a.served, a.dropped)
}
