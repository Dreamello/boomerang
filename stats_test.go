package main

import (
	"math"
	"strings"
	"testing"
)

// mdev must match iputils ping's definition (mean absolute deviation from the
// mean), so boomerang's numbers are comparable with a plain ping run.
func TestMdevMatchesIputilsDefinition(t *testing.T) {
	s := &series{}
	for _, v := range []int64{10 * ms, 12 * ms, 14 * ms, 16 * ms} {
		s.add(v)
	}
	if got := s.avg(); math.Abs(got-13) > 1e-9 {
		t.Errorf("avg: got %v want 13", got)
	}
	// |10-13| + |12-13| + |14-13| + |16-13| = 3+1+1+3 = 8; 8/4 = 2
	if got := s.mdev(); math.Abs(got-2) > 1e-9 {
		t.Errorf("mdev: got %v want 2", got)
	}
	if got := s.min(); math.Abs(got-10) > 1e-9 {
		t.Errorf("min: got %v want 10", got)
	}
	if got := s.max(); math.Abs(got-16) > 1e-9 {
		t.Errorf("max: got %v want 16", got)
	}
	if s.n() != 4 {
		t.Errorf("n: got %d want 4", s.n())
	}
}

func TestMdevZeroForConstantSeries(t *testing.T) {
	s := &series{}
	for i := 0; i < 5; i++ {
		s.add(87 * ms)
	}
	if got := s.mdev(); got != 0 {
		t.Errorf("mdev of a constant series: got %v want 0", got)
	}
}

func TestLossAccounting(t *testing.T) {
	st := NewStats()
	for i := 0; i < 10; i++ {
		st.Sent()
	}
	for i := 0; i < 7; i++ {
		st.Add(&Result{Seq: i, E2E: 100 * ms, Legs: []Leg{{From: "src", To: "a", RTT: 100 * ms}}})
	}
	if got := st.Loss(); math.Abs(got-30) > 1e-9 {
		t.Errorf("loss: got %v%% want 30%%", got)
	}
	if st.recv != 7 {
		t.Errorf("recv: got %d want 7", st.recv)
	}
}

func TestLossZeroWhenNothingSent(t *testing.T) {
	if got := NewStats().Loss(); got != 0 {
		t.Errorf("got %v want 0", got)
	}
}

// A run's visible count must reflect what was folded in, so late replies can
// never silently pad the statistics.
func TestLateRepliesAreCountedNotFolded(t *testing.T) {
	st := NewStats()
	st.Sent()
	st.Sent()
	st.Add(&Result{Seq: 0, E2E: 100 * ms, Legs: []Leg{{From: "src", To: "a", RTT: 100 * ms}}})
	st.Late()

	out := st.Summary([]string{"a"})
	if !strings.Contains(out, "1 late") {
		t.Errorf("summary hides late replies:\n%s", out)
	}
	if st.e2e.n() != 1 {
		t.Errorf("late reply folded into stats: n=%d want 1", st.e2e.n())
	}
	if !strings.Contains(out, "50.0% loss") {
		t.Errorf("expected 50%% loss in:\n%s", out)
	}
}

func TestProbeLineFormat(t *testing.T) {
	res := &Result{
		Seq: 41,
		E2E: 120310000,
		Legs: []Leg{
			{From: "src", To: "192.0.2.101:8888", RTT: 86920000},
			{From: "192.0.2.101:8888", To: "198.51.100.20:8888", RTT: 33350000},
		},
		Procs: []Proc{{At: "192.0.2.101:8888", Cost: 40000}},
	}
	line := ProbeLine(res)
	for _, want := range []string{
		"seq=41", "e2e=120.31ms",
		"leg0[src<->192.0.2.101]=86.92ms",
		"leg1[192.0.2.101<->198.51.100.20]=33.35ms",
		"proc[192.0.2.101]=0.04ms",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%s", want, line)
		}
	}
	// The default port is noise; a non-default one is information.
	if strings.Contains(line, ":8888") {
		t.Errorf("default port should be elided:\n%s", line)
	}
	odd := ProbeLine(&Result{Seq: 1, E2E: ms,
		Legs: []Leg{{From: "src", To: "192.0.2.1:9999", RTT: ms}}})
	if !strings.Contains(odd, ":9999") {
		t.Errorf("non-default port should be shown:\n%s", odd)
	}
}

func TestSummaryShapes(t *testing.T) {
	mk := func(hops int) *Stats {
		st := NewStats()
		st.Sent()
		res := &Result{Seq: 0, E2E: 120 * ms}
		from := "src"
		for i := 0; i < hops; i++ {
			to := "10.0.0." + string(rune('1'+i)) + ":8888"
			res.Legs = append(res.Legs, Leg{From: from, To: to, RTT: int64(30+i) * ms})
			if i < hops-1 {
				res.Procs = append(res.Procs, Proc{At: to, Cost: 40000})
			}
			from = to
		}
		st.Add(res)
		return st
	}

	direct := mk(1).Summary([]string{"192.0.2.1:8888"})
	if !strings.Contains(direct, "direct") {
		t.Errorf("a chain of one should read as direct:\n%s", direct)
	}
	one := mk(2).Summary([]string{"a", "b"})
	if !strings.Contains(one, "via 1 relay") || strings.Contains(one, "1 relays") {
		t.Errorf("singular relay wording:\n%s", one)
	}
	two := mk(3).Summary([]string{"a", "b", "c"})
	if !strings.Contains(two, "via 2 relays") {
		t.Errorf("plural relay wording:\n%s", two)
	}
	// Every measured quantity needs a visible sample count.
	if !strings.Contains(two, "mdev") || !strings.Contains(two, "n") {
		t.Errorf("summary table missing mdev/n columns:\n%s", two)
	}
	for _, want := range []string{"leg0", "leg1", "leg2", "proc"} {
		if !strings.Contains(two, want) {
			t.Errorf("missing %q row:\n%s", want, two)
		}
	}
}

func TestSummaryWithNoReplies(t *testing.T) {
	st := NewStats()
	for i := 0; i < 3; i++ {
		st.Sent()
	}
	out := st.Summary([]string{"a"})
	if !strings.Contains(out, "100.0% loss") {
		t.Errorf("expected total loss:\n%s", out)
	}
	if strings.Contains(out, "mdev") {
		t.Errorf("no statistics table should print with zero samples:\n%s", out)
	}
}

func TestSortedKeysDeterministic(t *testing.T) {
	m := map[string]*series{"c": {}, "a": {}, "b": {}}
	got := sortedKeys(m)
	if strings.Join(got, "") != "abc" {
		t.Errorf("got %v", got)
	}
}
