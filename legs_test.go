package main

import (
	"strings"
	"testing"
)

const ms = int64(1_000_000) // ns per millisecond

// chainOf builds a chain of the given length: N relays plus a destination.
func chainOf(n int) []string {
	c := make([]string, 0, n)
	for i := 0; i < n; i++ {
		c = append(c, "10.0.0."+string(rune('1'+i))+":8888")
	}
	return c
}

// synth builds a returned probe for a chain, given per-leg wire RTTs, per-relay
// processing costs, the destination turnaround, and a per-node clock offset.
//
// Offsets are what the test is really about: each node's clock reads whatever
// it likes, and the derived legs must not move.
func synth(t *testing.T, legRTT []int64, proc []int64, destTurn int64, offset []int64) (*Packet, int64) {
	t.Helper()
	hops := len(legRTT)
	if len(proc) != hops-1 {
		t.Fatalf("need %d proc values, got %d", hops-1, len(proc))
	}
	if len(offset) != hops {
		t.Fatalf("need %d offsets, got %d", hops, len(offset))
	}

	// Build the timeline on one imaginary global clock, then translate each
	// node's own stamps into its local clock by adding that node's offset.
	p := &Packet{V: ProtoVersion, Seq: 7, Chain: chainOf(hops), Phase: PhaseBack}

	// Outbound: half of each leg's wire time, then the relay's outbound half of
	// processing.
	globalNow := int64(5 * 1000 * ms) // arbitrary start
	e2eStart := globalNow
	arrival := make([]int64, hops)
	for n := 0; n < hops; n++ {
		globalNow += legRTT[n] / 2 // wire, outbound
		arrival[n] = globalNow
		p.StampIn(n, globalNow+offset[n])
		if n == hops-1 {
			break
		}
		globalNow += proc[n] / 2 // this relay's outbound processing
		p.StampFwd(n, globalNow+offset[n])
	}

	// Destination turns the packet around.
	globalNow += destTurn
	p.StampOut(hops-1, globalNow+offset[hops-1])

	// Return: unwind, mirroring wire and processing halves.
	for n := hops - 2; n >= 0; n-- {
		globalNow += legRTT[n+1] / 2 // wire, return
		p.StampRcv(n, globalNow+offset[n])
		globalNow += proc[n] / 2 // this relay's return processing
		p.StampOut(n, globalNow+offset[n])
	}
	globalNow += legRTT[0] / 2 // final wire hop back to the source
	e2e := globalNow - e2eStart
	return p, e2e
}

func TestDeriveLegsSingleRelay(t *testing.T) {
	// Example topology: source -> relay -> destination.
	legs := []int64{87 * ms, 35 * ms}
	p, e2e := synth(t, legs, []int64{40_000}, 2*ms, []int64{0, 0})

	res, err := DeriveLegs(p, e2e)
	if err != nil {
		t.Fatalf("DeriveLegs: %v", err)
	}
	if len(res.Legs) != 2 {
		t.Fatalf("got %d legs want 2", len(res.Legs))
	}
	if res.Legs[0].RTT != legs[0] || res.Legs[1].RTT != legs[1] {
		t.Errorf("legs: got %v/%v want %v/%v",
			res.Legs[0].RTT, res.Legs[1].RTT, legs[0], legs[1])
	}
	if res.Legs[0].From != "src" {
		t.Errorf("first leg should start at src, got %q", res.Legs[0].From)
	}
	if len(res.Holds) != 2 || res.Holds[0].Cost != 40_000 {
		t.Errorf("holds: got %v want first entry 40000ns", res.Holds)
	}
	// The destination's hold is the turnaround: the last hold entry.
	if got := res.Holds[len(res.Holds)-1].Cost; got != 2*ms {
		t.Errorf("dest turnaround: got %d want %d", got, 2*ms)
	}
}

// N=0: a direct probe. The destination is the only hop, and the single leg is
// the whole path.
func TestDeriveLegsDirect(t *testing.T) {
	p, e2e := synth(t, []int64{151 * ms}, nil, 1*ms, []int64{0})
	res, err := DeriveLegs(p, e2e)
	if err != nil {
		t.Fatalf("DeriveLegs: %v", err)
	}
	if len(res.Legs) != 1 || res.Legs[0].RTT != 151*ms {
		t.Errorf("got %v want one leg of 151ms", res.Legs)
	}
	// A direct probe still has one hold: the destination's turnaround.
	if len(res.Holds) != 1 {
		t.Errorf("direct probe should have exactly one hold, got %v", res.Holds)
	}
}

// Arbitrary chain length is the whole point: the same arithmetic must hold at
// any depth.
func TestDeriveLegsArbitraryLength(t *testing.T) {
	for hops := 1; hops <= 6; hops++ {
		legs := make([]int64, hops)
		for i := range legs {
			legs[i] = int64(10+i*7) * ms
		}
		proc := make([]int64, hops-1)
		for i := range proc {
			proc[i] = int64(20_000 + i*5_000)
		}
		p, e2e := synth(t, legs, proc, 3*ms, make([]int64, hops))
		res, err := DeriveLegs(p, e2e)
		if err != nil {
			t.Fatalf("hops=%d: %v", hops, err)
		}
		if len(res.Legs) != hops {
			t.Fatalf("hops=%d: got %d legs", hops, len(res.Legs))
		}
		for i := range legs {
			if res.Legs[i].RTT != legs[i] {
				t.Errorf("hops=%d leg %d: got %d want %d",
					hops, i, res.Legs[i].RTT, legs[i])
			}
		}
		if len(res.Holds) != hops {
			t.Fatalf("hops=%d: got %d holds want %d", hops, len(res.Holds), hops)
		}
		for i := range proc {
			if res.Holds[i].Cost != proc[i] {
				t.Errorf("hops=%d hold %d: got %d want %d",
					hops, i, res.Holds[i].Cost, proc[i])
			}
		}
	}
}

// THE PROOF: clock offsets cancel. Nodes are given wildly wrong clocks -- hours
// apart, in both directions -- and every derived leg must be identical to the
// synchronised case. If this fails, the whole design is unsound.
func TestClockOffsetsCancel(t *testing.T) {
	legs := []int64{87 * ms, 35 * ms, 12 * ms}
	proc := []int64{40_000, 55_000}
	const sec = 1000 * ms

	offsets := [][]int64{
		{0, 0, 0},                       // synchronised baseline
		{0, 3 * sec, 0},                 // one relay 3s ahead
		{0, -3 * sec, 0},                // one relay 3s behind
		{0, 7200 * sec, -3600 * sec},    // two hours ahead, one hour behind
		{5 * sec, -11 * sec, 999 * sec}, // every node disagrees
		{0, 0, 86400 * sec},             // destination a day out
	}

	var baseline []int64
	for i, off := range offsets {
		p, e2e := synth(t, legs, proc, 2*ms, off)
		res, err := DeriveLegs(p, e2e)
		if err != nil {
			t.Fatalf("offsets %v: %v", off, err)
		}
		got := make([]int64, len(res.Legs))
		for j, l := range res.Legs {
			got[j] = l.RTT
		}
		if i == 0 {
			baseline = got
			for j := range legs {
				if got[j] != legs[j] {
					t.Fatalf("baseline leg %d wrong: got %d want %d", j, got[j], legs[j])
				}
			}
			continue
		}
		for j := range baseline {
			if got[j] != baseline[j] {
				t.Errorf("offsets %v: leg %d moved to %d (baseline %d) -- "+
					"clock offsets did NOT cancel", off, j, got[j], baseline[j])
			}
		}
		// Holds are also single-clock differences, so they must cancel too.
		for j := range proc {
			if res.Holds[j].Cost != proc[j] {
				t.Errorf("offsets %v: hold %d got %d want %d",
					off, j, res.Holds[j].Cost, proc[j])
			}
		}
	}
}

// Sum of legs, processing, and the destination's turnaround must reconstruct
// the E2E measurement -- a closed accounting of where the time went.
func TestTimeAccountingCloses(t *testing.T) {
	legs := []int64{87 * ms, 35 * ms, 12 * ms}
	proc := []int64{40_000, 55_000}
	destTurn := 2 * ms
	p, e2e := synth(t, legs, proc, destTurn, []int64{0, 500 * ms, -700 * ms})
	res, err := DeriveLegs(p, e2e)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, l := range res.Legs {
		sum += l.RTT
	}
	// Holds already include the destination's turnaround, so legs + holds is
	// the complete accounting.
	for _, h := range res.Holds {
		sum += h.Cost
	}
	if sum != e2e {
		t.Errorf("accounting does not close: legs+holds=%d e2e=%d (diff %d)",
			sum, e2e, e2e-sum)
	}
}

func TestDeriveLegsRejectsIncompleteStamps(t *testing.T) {
	legs := []int64{87 * ms, 35 * ms}

	t.Run("relay never stamped", func(t *testing.T) {
		p, e2e := synth(t, legs, []int64{40_000}, 2*ms, []int64{0, 0})
		p.Stamps = p.Stamps[1:] // drop hop 0
		if _, err := DeriveLegs(p, e2e); err == nil {
			t.Error("accepted a probe with a missing stamp")
		}
	})

	t.Run("relay missing wire window", func(t *testing.T) {
		p, e2e := synth(t, legs, []int64{40_000}, 2*ms, []int64{0, 0})
		for i := range p.Stamps {
			if p.Stamps[i].N == 0 {
				p.Stamps[i].TRcv = 0
			}
		}
		if _, err := DeriveLegs(p, e2e); err == nil {
			t.Error("accepted a relay with no return stamp")
		}
	})

	t.Run("empty chain", func(t *testing.T) {
		if _, err := DeriveLegs(&Packet{V: ProtoVersion}, 1); err == nil {
			t.Error("accepted an empty chain")
		}
	})
}

// A forged or buggy packet can imply a negative leg. That must surface as an
// error, never as a plausible-looking measurement.
func TestDeriveLegsRejectsNegativeLeg(t *testing.T) {
	p, e2e := synth(t, []int64{87 * ms, 35 * ms}, []int64{40_000}, 2*ms, []int64{0, 0})
	// Claim the relay held the packet far longer than the source's whole E2E.
	for i := range p.Stamps {
		if p.Stamps[i].N == 0 {
			p.Stamps[i].TOut = p.Stamps[i].TIn + 10*e2e
		}
	}
	_, err := DeriveLegs(p, e2e)
	if err == nil {
		t.Fatal("accepted stamps implying a negative leg")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("unclear error: %v", err)
	}
}

// Guard against a vacuous offset test: prove the synthesised stamps really do
// carry the per-node offsets, and that the naive cross-clock arithmetic these
// tests are meant to rule out would in fact be broken by them.
func TestOffsetTestHasTeeth(t *testing.T) {
	legs := []int64{87 * ms, 35 * ms}
	proc := []int64{40_000}
	const sec = 1000 * ms

	sync, _ := synth(t, legs, proc, 2*ms, []int64{0, 0})
	skew, _ := synth(t, legs, proc, 2*ms, []int64{0, 3600 * sec})

	sSync, _ := sync.Stamp(1)
	sSkew, _ := skew.Stamp(1)
	if sSync.TIn == sSkew.TIn {
		t.Fatal("offsets are not reaching the stamps; the cancellation test is vacuous")
	}
	if got := sSkew.TIn - sSync.TIn; got != 3600*sec {
		t.Errorf("destination offset applied as %d ns, want %d", got, 3600*sec)
	}

	// The naive approach boomerang deliberately avoids: subtracting timestamps
	// taken on DIFFERENT nodes' clocks. It happens to work when clocks agree
	// and blows up by exactly the offset when they do not.
	naive := func(p *Packet) int64 {
		relay, _ := p.Stamp(0)
		dest, _ := p.Stamp(1)
		return (dest.TIn - relay.TFwd) * 2 // cross-clock: unsound
	}
	if naive(sync) == naive(skew) {
		t.Fatal("cross-clock arithmetic survived a 1h skew; the fixture cannot " +
			"distinguish sound from unsound math")
	}
	if got := naive(skew) - naive(sync); got != 2*3600*sec {
		t.Errorf("cross-clock error was %d ns, want %d", got, 2*3600*sec)
	}
}
