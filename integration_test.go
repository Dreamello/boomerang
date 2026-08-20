package main

// End-to-end test: a real source and real agents over real UDP sockets on
// loopback, exercising the whole path the production binary takes.

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is an io.Writer safe for the source's concurrent send/receive paths.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runChain measures through hops agents on loopback and returns the output plus
// the run's statistics.
func runChain(t *testing.T, hops, count int, dropRate float64) (*Stats, string, []string) {
	t.Helper()
	key := testKey(t)
	chain := make([]string, hops)
	for i := range chain {
		chain[i] = startAgent(t, key, dropRate)
	}
	out := &syncBuf{}
	src, err := NewSource(chain, key, 500*time.Millisecond, out, false, "")
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	defer src.Close()
	src.Run(count, 20*time.Millisecond, make(chan struct{}))
	return src.Stats(), out.String(), chain
}

// The headline claim: every leg is measured, they are all non-negative, and the
// accounting closes against the source's own E2E figure.
func TestIntegrationTimeAccountingCloses(t *testing.T) {
	for hops := 1; hops <= 3; hops++ {
		st, out, _ := runChain(t, hops, 5, 0)
		if st.recv == 0 {
			t.Fatalf("hops=%d: no replies at all\n%s", hops, out)
		}
		if st.Loss() != 0 {
			t.Errorf("hops=%d: %.1f%% loss on loopback\n%s", hops, st.Loss(), out)
		}
		if len(st.order) != hops {
			t.Errorf("hops=%d: measured %d legs", hops, len(st.order))
		}
		// One hold per hop: every relay plus the destination.
		if len(st.hOrd) != hops {
			t.Errorf("hops=%d: measured %d hold series want %d",
				hops, len(st.hOrd), hops)
		}

		// legs + processing + destination turnaround must reconstruct e2e.
		// Tolerance is 0.5 ms per hop: measured macOS scheduling jitter.
		var sum float64
		for _, k := range st.order {
			sum += st.legs[k].avg()
		}
		for _, k := range st.hOrd {
			sum += st.holds[k].avg()
		}
		tol := 0.5 * float64(hops)
		if diff := st.total.avg() - sum; diff < -tol || diff > tol+0.5 {
			t.Errorf("hops=%d: accounting off by %.3f ms (legs+proc=%.3f e2e=%.3f)",
				hops, diff, sum, st.total.avg())
		}

		// Every sample must be non-negative and physically plausible.
		for _, k := range st.order {
			if st.legs[k].min() < 0 {
				t.Errorf("hops=%d: negative leg on %s: %.3f", hops, k, st.legs[k].min())
			}
		}
		for _, k := range st.hOrd {
			if st.holds[k].min() < 0 {
				t.Errorf("hops=%d: negative hold at %s", hops, k)
			}
		}
	}
}

// -c must stop after exactly that many probes.
func TestIntegrationCountFlag(t *testing.T) {
	st, out, _ := runChain(t, 2, 4, 0)
	if st.sent != 4 {
		t.Errorf("sent %d probes, want 4\n%s", st.sent, out)
	}
	if strings.Count(out, "seq=") < 4 {
		t.Errorf("expected a line per probe:\n%s", out)
	}
}

// Loss must be counted honestly and must not hang the run.
func TestIntegrationLossCounted(t *testing.T) {
	st, out, chain := runChain(t, 2, 6, 1.0) // agents drop everything
	if st.recv != 0 {
		t.Errorf("got %d replies through agents dropping 100%%", st.recv)
	}
	if st.Loss() != 100 {
		t.Errorf("loss: got %.1f%% want 100%%", st.Loss())
	}
	if !strings.Contains(out, "timeout") {
		t.Errorf("timeouts should be visible:\n%s", out)
	}
	summary := st.Summary(chain, false)
	if !strings.Contains(summary, "100.0% loss") {
		t.Errorf("summary must state the loss:\n%s", summary)
	}
}

// Partial loss: the run continues and the surviving probes still measure.
func TestIntegrationSurvivesPartialLoss(t *testing.T) {
	key := testKey(t)
	dest := startAgent(t, key, 0)
	lossy := startAgent(t, key, 0.5) // relay drops half
	out := &syncBuf{}
	src, err := NewSource([]string{lossy, dest}, key, 300*time.Millisecond, out, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Run(20, 10*time.Millisecond, make(chan struct{}))

	st := src.Stats()
	if st.sent != 20 {
		t.Errorf("sent %d want 20", st.sent)
	}
	if st.recv == 0 {
		t.Skip("all 20 probes hit the 50% drop; rerun (statistically unlikely)")
	}
	if st.recv == st.sent {
		t.Skip("no probes dropped despite a 50% rate; rerun")
	}
	// Whatever survived must still be internally consistent. Look the series up
	// by position rather than by label: leg0's name is the local address the
	// kernel chose, which the test cannot predict.
	if len(st.order) == 0 {
		t.Fatal("no leg series recorded")
	}
	if st.legs[st.order[0]].min() < 0 {
		t.Error("negative leg among surviving probes")
	}
	if st.Loss() <= 0 || st.Loss() >= 100 {
		t.Errorf("expected partial loss, got %.1f%%", st.Loss())
	}
}

// A signal must end the run promptly and still print a usable summary.
func TestIntegrationStopSignal(t *testing.T) {
	key := testKey(t)
	dest := startAgent(t, key, 0)
	relay := startAgent(t, key, 0)
	out := &syncBuf{}
	src, err := NewSource([]string{relay, dest}, key, time.Second, out, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	stop := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		close(stop)
	}()
	start := time.Now()
	src.Run(0, 20*time.Millisecond, stop) // count=0: runs until stopped
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("stop took %v; should be prompt", elapsed)
	}
	st := src.Stats()
	if st.sent == 0 {
		t.Error("no probes sent before stop")
	}
	if !strings.Contains(st.Summary([]string{relay, dest}, false), "probes sent") {
		t.Error("summary missing after stop")
	}
}

// A source must ignore replies it cannot authenticate, so a forged packet
// cannot inject a fabricated measurement.
func TestIntegrationSourceIgnoresForgedReply(t *testing.T) {
	key := testKey(t)
	other := make([]byte, KeyLen)
	dest := startAgent(t, key, 0)

	out := &syncBuf{}
	src, err := NewSource([]string{dest}, other, 200*time.Millisecond, out, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	// Source and agent hold different keys: nothing should be measurable.
	src.Run(3, 10*time.Millisecond, make(chan struct{}))
	if src.Stats().recv != 0 {
		t.Errorf("source accepted %d replies it could not authenticate", src.Stats().recv)
	}
}

// N=0 is a first-class case: with no relays boomerang is a plain UDP ping.
func TestIntegrationDirectProbe(t *testing.T) {
	st, out, chain := runChain(t, 1, 3, 0)
	if st.recv == 0 {
		t.Fatalf("direct probe returned nothing:\n%s", out)
	}
	if len(st.order) != 1 {
		t.Errorf("direct probe should measure exactly one leg, got %d", len(st.order))
	}
	// A direct probe has one hold series: the destination's turnaround.
	if len(st.hOrd) != 1 {
		t.Errorf("direct probe should have one hold series, got %v", st.hOrd)
	}
	if !strings.Contains(st.Summary(chain, false), "direct") {
		t.Error("summary should say direct")
	}
}
