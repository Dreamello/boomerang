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
//
//	leg 0        = I0     - W(0)      source <-> first hop
//	leg k        = W'(k-1) - W(k)     hop k-1 <-> hop k
//	hold k       = W(k)   - W'(k)     time hop k held the packet
//
// The destination turns the packet around, so its full window is its hold and
// terminates the chain. Hold is therefore uniform: one per hop, covering two
// transits at a relay and one at the destination.
//
// Legs and holds together account for the measured round trip exactly:
//
//	sum(legs) + sum(holds) = e2e

import "fmt"

// Leg is one measured hop-to-hop wire time.
type Leg struct {
	From string // "src" or an address
	To   string
	RTT  int64 // ns, round trip across this leg only
}

// Hold is time a node spent holding the packet: inbound plus outbound
// processing at a relay, the turnaround at the destination.
type Hold struct {
	At   string
	Cost int64 // ns
}

// Result is everything derivable from a single returned probe.
type Result struct {
	Seq   int
	E2E   int64 // ns, source clock -- measured directly, not summed
	Legs  []Leg
	Holds []Hold // one per hop, same order and length as Legs
}

// HoldTotal is the combined time every node held the packet.
func (r *Result) HoldTotal() int64 {
	var sum int64
	for _, h := range r.Holds {
		sum += h.Cost
	}
	return sum
}

// DeriveLegs computes per-leg RTTs from a returned probe.
//
// e2e is measured on the source's clock; every other input comes from the
// packet's stamps. A negative leg means the arithmetic was fed inconsistent
// stamps (a bug or a forged packet), so it is reported as an error.
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
			// Destination: its full window is the turnaround.
			res.Holds = append(res.Holds, Hold{At: p.Chain[n], Cost: full})
			break
		}
		wire := s.TRcv - s.TFwd
		if wire < 0 {
			return nil, fmt.Errorf("relay %d (%s): rcv before fwd", n, p.Chain[n])
		}
		res.Holds = append(res.Holds, Hold{At: p.Chain[n], Cost: full - wire})
		upstream = wire
		from = p.Chain[n]
	}
	return res, nil
}
