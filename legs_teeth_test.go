package main

import "testing"

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
