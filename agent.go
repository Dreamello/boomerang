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
// never reconfigures an existing node.

import (
	"fmt"
	"log"
	"math/rand"
	"net"
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

	// Counters, for the health line only.
	served  uint64
	dropped uint64
}

// NewAgent binds the UDP socket.
func NewAgent(addr string, key []byte, dropRate float64, verbose bool) (*Agent, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	return &Agent{
		conn:     conn,
		key:      key,
		start:    time.Now(),
		dropRate: dropRate,
		verbose:  verbose,
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
		// Stamp arrival before any parsing, so decode cost lands in this
		// node's processing time rather than inflating a leg.
		arrived := a.now()
		a.handle(buf[:n], src, arrived)
	}
}

// handle processes one datagram. Unauthenticated or malformed traffic is
// dropped silently: no reply, no log line, so a scanner learns nothing and
// cannot fill the journal.
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
// that should handle this packet NEXT. Outbound that means incrementing;
// turning around and unwinding it means decrementing, so a returning packet
// stamps against the hop that actually handled it.
func (a *Agent) forwardOrTurn(p *Packet, src *net.UDPAddr, arrived int64) {
	hop := p.Hop
	p.StampIn(hop, arrived)

	// Record who handed us this packet, so the return path needs no state
	// here. A NAT'd source's observable address is only visible to this hop.
	p.AppendReply(src.String())

	if p.IsLast() {
		// Destination: turn it around. TOut is taken as late as possible so the
		// turnaround reflects real work, not our own bookkeeping.
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

// prevHop steps one hop back toward the source, clamped so the packet stays
// decodable: Hop must remain a valid index into Chain even on the final leg
// home, where there is no previous hop.
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

// send encodes and transmits, resolving the destination each time. Resolution
// stays per-send so an agent holds no cached routing state.
func (a *Agent) send(p *Packet, addr string) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		if a.verbose {
			log.Printf("resolve %s: %v", addr, err)
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

// Stats renders a one-line health summary.
func (a *Agent) Stats() string {
	return fmt.Sprintf("served=%d dropped=%d", a.served, a.dropped)
}
