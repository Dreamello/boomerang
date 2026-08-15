package main

// Tests for the source: how a run ends, and what it reports when it does.

import (
	"strings"
	"testing"
	"time"
)

// Ctrl-C used to force-expire every in-flight probe, so a probe that had been
// out for milliseconds of its 2s budget was reported as a timeout. With eight
// probes that reads as 12.5% loss on a path with none.
func TestInterruptDoesNotCountAsLoss(t *testing.T) {
	key := testKey(t)
	// A destination that never answers: every probe stays in flight.
	silent := startAgent(t, key, 1.0)

	out := &syncBuf{}
	// Deadline far longer than the run, so nothing legitimately times out.
	src, err := NewSource([]string{silent}, key, 30*time.Second, out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	stop := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(stop)
	}()
	src.Run(0, 20*time.Millisecond, stop)

	st := src.Stats()
	text := out.String() + st.Summary([]string{silent}, false)

	if st.cancelled == 0 {
		t.Fatal("interrupted probes were not recorded as cancelled")
	}
	if got := st.Loss(); got != 0 {
		t.Errorf("interrupt reported as %.1f%% loss; cancelled probes never got "+
			"their deadline so they are not loss", got)
	}
	if strings.Contains(text, "timeout") {
		t.Errorf("interrupted probes reported as timeouts:\n%s", text)
	}
	// Interrupting is a deliberate user action, so it needs no commentary: the
	// probe simply leaves the totals rather than being explained.
	if strings.Contains(text, "cancel") {
		t.Errorf("interrupt should be silent, got:\n%s", text)
	}
	// Nothing returned, so there is no sample to report either way.
	if st.recv != 0 {
		t.Errorf("a silent destination returned %d replies", st.recv)
	}
}

// A run that ends on its own terms must still call an unanswered probe loss --
// the fix above must not suppress real loss.
func TestCountedRunStillReportsLoss(t *testing.T) {
	key := testKey(t)
	silent := startAgent(t, key, 1.0)

	out := &syncBuf{}
	src, err := NewSource([]string{silent}, key, 200*time.Millisecond, out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	// -c 3, no interrupt: every probe exceeds its deadline for real.
	src.Run(3, 20*time.Millisecond, make(chan struct{}))

	st := src.Stats()
	if st.cancelled != 0 {
		t.Errorf("a completed run marked %d probes cancelled", st.cancelled)
	}
	if got := st.Loss(); got != 100 {
		t.Errorf("loss: got %.1f%% want 100%%", got)
	}
	if !strings.Contains(out.String(), "timeout") {
		t.Errorf("real timeouts should be reported:\n%s", out.String())
	}
}

// leg0 used to read "src", which says nothing about which host or interface the
// probe left from.
func TestLeg0NamesTheLocalAddress(t *testing.T) {
	key := testKey(t)
	dest := startAgent(t, key, 0)

	out := &syncBuf{}
	src, err := NewSource([]string{dest}, key, 2*time.Second, out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Run(2, 20*time.Millisecond, make(chan struct{}))

	st := src.Stats()
	if st.recv == 0 {
		t.Fatalf("no replies:\n%s", out.String())
	}
	if len(st.order) == 0 {
		t.Fatal("no leg series recorded")
	}
	leg0 := st.order[0]
	if strings.HasPrefix(leg0, "src ") {
		t.Errorf("leg0 still labelled src: %q", leg0)
	}
	// Loopback destination, so the chosen local address must be loopback too.
	if !strings.HasPrefix(leg0, "127.0.0.1") {
		t.Errorf("leg0 should start with the local address, got %q", leg0)
	}
	if !strings.Contains(st.Summary([]string{dest}, false), "127.0.0.1") {
		t.Error("summary does not name the local address")
	}
}
