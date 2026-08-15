package main

// Statistics and output rendering.
//
// All formatting lives here so a later --json or Nezha feed is an additional
// renderer rather than a rewrite.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// series accumulates samples for one measured quantity.
type series struct {
	name    string
	samples []float64 // ms
}

func (s *series) add(ns int64) { s.samples = append(s.samples, float64(ns)/1e6) }

func (s *series) n() int { return len(s.samples) }

func (s *series) min() float64 {
	m := math.Inf(1)
	for _, v := range s.samples {
		if v < m {
			m = v
		}
	}
	return m
}

func (s *series) max() float64 {
	m := math.Inf(-1)
	for _, v := range s.samples {
		if v > m {
			m = v
		}
	}
	return m
}

func (s *series) avg() float64 {
	var sum float64
	for _, v := range s.samples {
		sum += v
	}
	return sum / float64(len(s.samples))
}

// mdev is mean absolute deviation from the mean -- the same definition iputils
// ping reports, so the numbers are comparable with a plain ping run.
func (s *series) mdev() float64 {
	if len(s.samples) == 0 {
		return 0
	}
	avg := s.avg()
	var sum float64
	for _, v := range s.samples {
		sum += math.Abs(v - avg)
	}
	return sum / float64(len(s.samples))
}

// Stats collects every series across a run.
type Stats struct {
	sent  int
	recv  int
	late  int
	e2e   series
	legs  map[string]*series
	procs map[string]*series
	order []string // leg keys, in chain order
	pOrd  []string // proc keys, in chain order
	start time.Time
}

func NewStats() *Stats {
	return &Stats{
		legs:  map[string]*series{},
		procs: map[string]*series{},
		start: time.Now(),
	}
}

func (st *Stats) Sent() { st.sent++ }
func (st *Stats) Late() { st.late++ }

// Add folds one returned probe into the running statistics.
func (st *Stats) Add(res *Result) {
	st.recv++
	st.e2e.add(res.E2E)
	for _, l := range res.Legs {
		key := l.From + " -> " + l.To
		s, ok := st.legs[key]
		if !ok {
			s = &series{name: key}
			st.legs[key] = s
			st.order = append(st.order, key)
		}
		s.add(l.RTT)
	}
	for _, p := range res.Procs {
		s, ok := st.procs[p.At]
		if !ok {
			s = &series{name: p.At}
			st.procs[p.At] = s
			st.pOrd = append(st.pOrd, p.At)
		}
		s.add(p.Cost)
	}
}

// Loss is the percentage of probes that never came back.
func (st *Stats) Loss() float64 {
	if st.sent == 0 {
		return 0
	}
	return float64(st.sent-st.recv) / float64(st.sent) * 100
}

// ProbeLine renders the per-probe line printed as each reply lands.
//
// Precision is fixed at 0.01 ms deliberately: the measurement floor on this
// path is macOS scheduling jitter of roughly +/-0.5 ms, so more digits would
// imply accuracy that is not there.
func ProbeLine(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "seq=%d e2e=%.2fms", res.Seq, msOf(res.E2E))
	for i, l := range res.Legs {
		fmt.Fprintf(&b, "  leg%d[%s]=%.2fms", i, shortPair(l.From, l.To), msOf(l.RTT))
	}
	for _, p := range res.Procs {
		fmt.Fprintf(&b, "  proc[%s]=%.2fms", shortAddr(p.At), msOf(p.Cost))
	}
	return b.String()
}

// Summary renders the ping-style block printed at exit.
func (st *Stats) Summary(chain []string) string {
	var b strings.Builder
	target := "direct"
	if len(chain) > 1 {
		target = fmt.Sprintf("via %d relay", len(chain)-1)
		if len(chain) > 2 {
			target += "s"
		}
	}
	fmt.Fprintf(&b, "\n--- boomerang statistics (%s) ---\n", target)
	fmt.Fprintf(&b, "%d probes sent, %d returned, %.1f%% loss, time %s\n",
		st.sent, st.recv, st.Loss(), time.Since(st.start).Round(time.Millisecond))
	if st.late > 0 {
		fmt.Fprintf(&b, "%d late replies excluded from statistics\n", st.late)
	}
	if st.recv == 0 {
		return b.String()
	}

	rows := [][]string{{"", "min", "avg", "max", "mdev", "n"}}
	rows = append(rows, row("e2e", &st.e2e))
	for i, k := range st.order {
		rows = append(rows, row(fmt.Sprintf("leg%d %s", i, k), st.legs[k]))
	}
	for _, k := range st.pOrd {
		rows = append(rows, row("proc "+shortAddr(k), st.procs[k]))
	}
	b.WriteString(renderTable(rows))
	return b.String()
}

func row(label string, s *series) []string {
	return []string{
		label,
		fmt.Sprintf("%.2f", s.min()),
		fmt.Sprintf("%.2f", s.avg()),
		fmt.Sprintf("%.2f", s.max()),
		fmt.Sprintf("%.2f", s.mdev()),
		fmt.Sprintf("%d", s.n()),
	}
}

// renderTable pads columns so the numbers line up in a terminal.
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	w := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if len(c) > w[i] {
				w[i] = len(c)
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, c := range r {
			if i == 0 {
				fmt.Fprintf(&b, "%-*s", w[i], c)
			} else {
				fmt.Fprintf(&b, "  %*s", w[i], c)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func msOf(ns int64) float64 { return float64(ns) / 1e6 }

// shortAddr drops the port when it is the default, keeping lines readable.
func shortAddr(a string) string {
	if i := strings.LastIndex(a, ":"); i > 0 {
		if a[i+1:] == fmt.Sprint(DefaultPort) {
			return a[:i]
		}
	}
	return a
}

func shortPair(from, to string) string {
	return shortAddr(from) + "<->" + shortAddr(to)
}

// sortedKeys is used only by tests that need deterministic iteration.
func sortedKeys(m map[string]*series) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
