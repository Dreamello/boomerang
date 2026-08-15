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

	out := st.Summary([]string{"a"}, false)
	if !strings.Contains(out, "1 late") {
		t.Errorf("summary hides late replies:\n%s", out)
	}
	if st.total.n() != 1 {
		t.Errorf("late reply folded into stats: n=%d want 1", st.total.n())
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
		Holds: []Hold{
			{At: "192.0.2.101:8888", Cost: 40000},
			{At: "198.51.100.20:8888", Cost: 20000},
		},
	}

	// Default: one aggregate hold, total last.
	line := ProbeLine(res, false)
	for _, want := range []string{
		"seq=41", "leg0=86.92ms", "leg1=33.35ms", "hold=0.06ms", "total=120.31ms",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%s", want, line)
		}
	}
	// Addresses are the summary table's job; repeating them per probe would
	// cost most of the line width to say nothing new.
	if strings.Contains(line, "192.0.2.101") || strings.Contains(line, "<->") {
		t.Errorf("probe line should carry no addresses:\n%s", line)
	}
	// total reads last, after the parts it is the sum of.
	if !strings.HasSuffix(line, "total=120.31ms") {
		t.Errorf("total should end the line:\n%s", line)
	}
	// The old jargon is gone.
	if strings.Contains(line, "e2e") || strings.Contains(line, "proc") {
		t.Errorf("stale naming in:\n%s", line)
	}

	// --holds: one figure per node, still ending with total.
	per := ProbeLine(res, true)
	for _, want := range []string{"hold0=0.04ms", "hold1=0.02ms", "total=120.31ms"} {
		if !strings.Contains(per, want) {
			t.Errorf("missing %q in:\n%s", want, per)
		}
	}
	if strings.Contains(per, " hold=") {
		t.Errorf("--holds should replace the aggregate, not add to it:\n%s", per)
	}
}

// The displayed numbers must add up to the displayed total, or the line invites
// the reader to hunt for time that is not missing.
func TestProbeLineAddsUp(t *testing.T) {
	res := &Result{
		Seq:   1,
		Legs:  []Leg{{RTT: 87 * ms}, {RTT: 32 * ms}},
		Holds: []Hold{{Cost: 150000}, {Cost: 60000}},
	}
	res.E2E = 87*ms + 32*ms + 150000 + 60000

	line := ProbeLine(res, false)
	// 87.00 + 32.00 + 0.21 = 119.21
	for _, want := range []string{"leg0=87.00ms", "leg1=32.00ms", "hold=0.21ms", "total=119.21ms"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%s", want, line)
		}
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
				res.Holds = append(res.Holds, Hold{At: to, Cost: 40000})
			}
			from = to
		}
		st.Add(res)
		return st
	}

	direct := mk(1).Summary([]string{"192.0.2.1:8888"}, false)
	if !strings.Contains(direct, "direct") {
		t.Errorf("a chain of one should read as direct:\n%s", direct)
	}
	one := mk(2).Summary([]string{"a", "b"}, false)
	if !strings.Contains(one, "via 1 relay") || strings.Contains(one, "1 relays") {
		t.Errorf("singular relay wording:\n%s", one)
	}
	two := mk(3).Summary([]string{"a", "b", "c"}, false)
	if !strings.Contains(two, "via 2 relays") {
		t.Errorf("plural relay wording:\n%s", two)
	}
	// Every measured quantity needs a visible sample count.
	if !strings.Contains(two, "mdev") || !strings.Contains(two, "n") {
		t.Errorf("summary table missing mdev/n columns:\n%s", two)
	}
	for _, want := range []string{"leg0", "leg1", "leg2", "hold", "total"} {
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
	out := st.Summary([]string{"a"}, false)
	if !strings.Contains(out, "100.0% loss") {
		t.Errorf("expected total loss:\n%s", out)
	}
	if strings.Contains(out, "mdev") {
		t.Errorf("no statistics table should print with zero samples:\n%s", out)
	}
}

// --holds splits the summary per node; the default collapses it to one row.
func TestSummaryHoldModes(t *testing.T) {
	st := NewStats()
	st.Sent()
	st.Add(&Result{
		Seq:   0,
		E2E:   120 * ms,
		Legs:  []Leg{{From: "src", To: "192.0.2.1:8888", RTT: 87 * ms}, {From: "192.0.2.1:8888", To: "192.0.2.2:8888", RTT: 32 * ms}},
		Holds: []Hold{{At: "192.0.2.1:8888", Cost: 150000}, {At: "192.0.2.2:8888", Cost: 60000}},
	})

	def := st.Summary([]string{"192.0.2.1:8888", "192.0.2.2:8888"}, false)
	if strings.Contains(def, "hold0") || strings.Contains(def, "hold1") {
		t.Errorf("default summary should collapse holds:\n%s", def)
	}
	if !strings.Contains(def, "hold ") {
		t.Errorf("default summary missing the aggregate hold row:\n%s", def)
	}

	per := st.Summary([]string{"192.0.2.1:8888", "192.0.2.2:8888"}, true)
	for _, want := range []string{"hold0 192.0.2.1", "hold1 192.0.2.2"} {
		if !strings.Contains(per, want) {
			t.Errorf("--holds summary missing %q:\n%s", want, per)
		}
	}
	// total stays the final row in both modes.
	for name, out := range map[string]string{"default": def, "--holds": per} {
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if !strings.HasPrefix(lines[len(lines)-1], "total") {
			t.Errorf("%s: total should be the last row, got %q", name, lines[len(lines)-1])
		}
	}
}

// The summary table's arrows must line up, which is what makes a multi-hop chain
// scannable. Nothing else asserts column geometry.

func TestSummaryArrowsAlign(t *testing.T) {
	st := NewStats()
	st.Sent()
	// Endpoints of deliberately different widths: a short address, a long one,
	// and a hostname-length label.
	st.Add(&Result{
		Seq: 0,
		E2E: 186 * ms,
		Legs: []Leg{
			{From: "192.0.2.36", To: "192.0.2.101:8888", RTT: 89 * ms},
			{From: "192.0.2.101:8888", To: "192.0.2.9:8888", RTT: 49 * ms},
			{From: "192.0.2.9:8888", To: "198.51.100.20:8888", RTT: 36 * ms},
			{From: "198.51.100.20:8888", To: "203.0.113.11:8888", RTT: 10 * ms},
		},
		Holds: []Hold{
			{At: "192.0.2.101:8888", Cost: 100000},
			{At: "192.0.2.9:8888", Cost: 100000},
			{At: "198.51.100.20:8888", Cost: 100000},
			{At: "203.0.113.11:8888", Cost: 100000},
		},
	})

	out := st.Summary([]string{"a", "b", "c", "d"}, false)

	var cols []int
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "->"); i >= 0 {
			cols = append(cols, i)
		}
	}
	if len(cols) != 4 {
		t.Fatalf("expected 4 leg rows with arrows, got %d:\n%s", len(cols), out)
	}
	for i, c := range cols {
		if c != cols[0] {
			t.Errorf("leg%d arrow at column %d, leg0 at %d -- arrows must align:\n%s",
				i, c, cols[0], out)
		}
	}

	// The numeric columns must stay right-aligned behind the labels.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("row has trailing whitespace: %q", line)
		}
	}

	// The default port is noise here exactly as it is in the header.
	if strings.Contains(out, ":8888") {
		t.Errorf("default port should be elided in the table:\n%s", out)
	}
}
