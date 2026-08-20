package main

// ICMP last-hop termination.
//
// When ICMPDest is set and this agent is the last in the chain, it becomes a
// terminator: instead of turning the packet around, it pings the ICMP target
// and folds the round-trip into the derivation as the final leg.
//
// The terminator stamps all four fields like a relay:
//   TIn  = arrival from the previous hop (set by forwardOrTurn)
//   TFwd = just before sending the ICMP echo
//   TRcv = on receiving the ICMP reply
//   TOut = just before sending the UDP reply back upstream
//
// Uses "udp4" network in x/net/icmp, which opens a SOCK_DGRAM ICMP socket —
// the unprivileged path that works when the process GID falls inside
// net.ipv4.ping_group_range. Check this setting on each Linux host.
// If the socket cannot be opened, the agent logs once and refuses to terminate.

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// icmpConn wraps the unprivileged ICMP socket, opened lazily on first use.
// All ICMP termination is serialized through sendRecv.
type icmpConn struct {
	mu   sync.Mutex // guards conn/err init AND serializes sendRecv
	conn *icmp.PacketConn
	err  error // sticky: once the socket fails to open, never retried
	seq  atomic.Uint32
}

// open returns the ICMP socket, creating it on first call.
func (ic *icmpConn) open() (*icmp.PacketConn, error) {
	if ic.err != nil {
		return nil, ic.err
	}
	if ic.conn != nil {
		return ic.conn, nil
	}
	// "udp4" = SOCK_DGRAM + IPPROTO_ICMP: unprivileged, no CAP_NET_RAW.
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		ic.err = err
		return nil, err
	}
	ic.conn = conn
	return conn, nil
}

func (ic *icmpConn) close() {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if ic.conn != nil {
		_ = ic.conn.Close()
	}
}

// nextSeq returns the next ICMP echo sequence number (wraps at 65535).
func (ic *icmpConn) nextSeq() uint16 {
	return uint16(ic.seq.Add(1))
}

// sendRecv sends an ICMP echo and waits for the matching reply. Serialized
// by ic.mu so concurrent terminates don't race on ReadFrom.
//
// Returns the reply's source address and nil on success.
func (ic *icmpConn) sendRecv(dst net.Addr, id, seq uint16, payload []byte, timeout time.Duration) (string, error) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	conn, err := ic.open()
	if err != nil {
		return "", err
	}

	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   int(id),
			Seq:  int(seq),
			Data: payload,
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return "", err
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	if _, err := conn.WriteTo(wb, dst); err != nil {
		return "", err
	}

	// Read replies until we match our id+seq or timeout.
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return "", err // timeout
		}
		rm, err := icmp.ParseMessage(1, buf[:n]) // 1 = ICMPv4
		if err != nil {
			continue
		}
		if rm.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if uint16(echo.Seq) == seq {
			addr := ""
			if from != nil {
				addr = from.String()
			}
			return addr, nil
		}
	}
}

// terminate handles an ICMP-terminated probe. Called in its own goroutine so
// the main event loop isn't blocked by ICMP round-trip time.
//
// Preconditions set by forwardOrTurn:
//   - TIn already stamped at this hop
//   - AppendReply already called with the UDP sender's address
//   - p.IsLast() is true and p.ICMPDest != ""
//
// This function stamps TFwd, does the ICMP echo/reply, stamps TRcv and TOut,
// then sends the probe back via UDP.
func (a *Agent) terminate(p *Packet, hop int) {
	// For "udp4" ICMP, the address is just "ip:0" (port is ignored but needed).
	dst := &net.UDPAddr{IP: net.ParseIP(p.ICMPDest)}
	if dst.IP == nil {
		if a.verbose {
			log.Printf("icmp: %q is not a valid IP", p.ICMPDest)
		}
		return
	}

	id := uint16(p.Seq & 0xffff)
	seq := a.icmp.nextSeq()
	// Minimal payload — just enough to correlate. The full probe lives in
	// this goroutine's stack, so it doesn't need to survive the round trip.
	payload := []byte("boom")

	// Stamp TFwd just before sending.
	p.StampFwd(hop, a.now())

	replyAddr, err := a.icmp.sendRecv(dst, id, seq, payload, a.icmpTimeout)
	if err != nil {
		if a.verbose {
			log.Printf("icmp %s: %v", p.ICMPDest, err)
		}
		return // probe lost — source times it out
	}

	// Stamp TRcv on reply.
	p.StampRcv(hop, a.now())

	// Record the observed reply source for attribution.
	// Strip the ":0" port that UDPAddr.String() appends for ICMP.
	if h, _, err := net.SplitHostPort(replyAddr); err == nil {
		replyAddr = h
	}
	p.ICMPReply = replyAddr

	// Turn the packet around and send back via UDP.
	p.Phase = PhaseBack
	back, ok := p.ReturnAddr()
	if !ok {
		return
	}
	p.Hop = prevHop(hop)
	p.StampOut(hop, a.now())
	a.send(p, back)
	a.served++
}
