package main

// Leg derivation.
//
// Every node measures only intervals on its OWN monotonic clock. A leg is the
// difference between two adjacent intervals, so each node's unknown clock
// offset appears twice within one subtraction and cancels. No clock
// synchronisation is needed anywhere, and the chain can be any length.
//
// For a chain of N relays plus a destination, with the source as node -1:
//
//	I0    = t3 - t0                E2E RTT, source clock
//	W(k)  = t_out - t_in           full window at hop k, hop k's clock
//	W'(k) = t_rcv - t_fwd          wire-only window at hop k, hop k's clock
//	W(D)  = t_out - t_in           destination turnaround, destination's clock
//
//	leg 0        = I0     - W(0)      source <-> first hop
//	leg k        = W'(k-1) - W(k)     hop k-1 <-> hop k
//	proc k       = W(k)   - W'(k)     hop k's own processing cost
//
// The destination has no wire-only window (it turns the packet around rather
// than forwarding), so its full window terminates the chain.

import "fmt"

// Leg is one measured hop-to-hop wire time.
type Leg struct {
	From string // "src" or an address
	To   string
	RTT  int64 // ns, round trip across this leg only
}

// Proc is one relay's own processing cost, in and out combined.
type Proc struct {
	At   string
	Cost int64 // ns
}

// Result is everything derivable from a single returned probe.
type Result struct {
	Seq   int
	E2E   int64 // ns, source clock
	Legs  []Leg
	Procs []Proc
	// DestTurnaround is the destination's own in-to-out time: real work that
	// belongs to neither adjacent leg.
	DestTurnaround int64
}

// DeriveLegs computes per-leg RTTs from a returned probe.
//
// e2e is measured on the source's clock; every other input comes from the
// packet's stamps. A negative leg means the arithmetic was fed inconsistent
// stamps (a bug or a forged packet), so it is reported as an error rather than
// silently clamped.
func DeriveLegs(p *Packet, e2e int64) (*Result, error) {
	if len(p.Chain) == 0 {
		return nil, ErrChain
	}
	res := &Result{Seq: p.Seq, E2E: e2e}
	last := len(p.Chain) - 1

	// Every hop must have stamped, or the packet did not travel the chain.
	for n := 0; n <= last; n++ {
		s, ok := p.Stamp(n)
		if !ok {
			return nil, fmt.Errorf("hop %d (%s) did not stamp", n, p.Chain[n])
		}
		if s.TIn == 0 || s.TOut == 0 {
			return nil, fmt.Errorf("hop %d (%s) has an incomplete stamp", n, p.Chain[n])
		}
		if n != last && (s.TFwd == 0 || s.TRcv == 0) {
			return nil, fmt.Errorf("relay %d (%s) has an incomplete stamp", n, p.Chain[n])
		}
	}

	dest, _ := p.Stamp(last)
	res.DestTurnaround = dest.TOut - dest.TIn

	// upstream is the interval measured by the previous node: the source's E2E
	// for leg 0, then each relay's wire-only window.
	upstream := e2e
	from := "src"
	for n := 0; n <= last; n++ {
		s, _ := p.Stamp(n)
		full := s.TOut - s.TIn
		if full < 0 {
			return nil, fmt.Errorf("hop %d (%s): out before in", n, p.Chain[n])
		}
		leg := upstream - full
		if leg < 0 {
			return nil, fmt.Errorf("hop %d (%s): derived leg is negative (%d ns); "+
				"stamps are inconsistent", n, p.Chain[n], leg)
		}
		res.Legs = append(res.Legs, Leg{From: from, To: p.Chain[n], RTT: leg})

		if n == last {
			break
		}
		wire := s.TRcv - s.TFwd
		if wire < 0 {
			return nil, fmt.Errorf("relay %d (%s): rcv before fwd", n, p.Chain[n])
		}
		res.Procs = append(res.Procs, Proc{At: p.Chain[n], Cost: full - wire})
		upstream = wire
		from = p.Chain[n]
	}
	return res, nil
}
