package main

// Statistics and output rendering.
//
// All formatting lives here so a later --json or Nezha feed is an additional
// renderer rather than a rewrite.

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// series accumulates samples for one measured quantity, in ms.
type series struct {
	samples []float64
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

// mdev is mean absolute deviation from the mean, in milliseconds.
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
	sent      int
	recv      int
	late      int
	cancelled int
	total     series
	legs      map[string]*series
	holds     map[string]*series
	order     []string // leg keys, in chain order
	hOrd      []string // hold keys, in chain order
	// ends maps a leg key to its {from, to} endpoints, kept apart so the table
	// can pad each side into its own column and line the arrows up.
	ends map[string][2]string
	// holdAll aggregates every node's hold per probe, for the default view.
	holdAll series
	start   time.Time
}

func NewStats() *Stats {
	return &Stats{
		legs:  map[string]*series{},
		holds: map[string]*series{},
		ends:  map[string][2]string{},
		start: time.Now(),
	}
}

func (st *Stats) Sent() { st.sent++ }
func (st *Stats) Late() { st.late++ }

// Cancelled removes interrupted probes from the run's totals, so loss reflects
// only probes that ran their full deadline.
func (st *Stats) Cancelled(n int) {
	st.sent -= n
	if st.sent < 0 {
		st.sent = 0
	}
	st.cancelled += n
}

// Add folds one returned probe into the running statistics.
func (st *Stats) Add(res *Result) {
	st.recv++
	st.total.add(res.E2E)
	st.holdAll.add(res.HoldTotal())
	for _, l := range res.Legs {
		key := l.From + " -> " + l.To
		s, ok := st.legs[key]
		if !ok {
			s = &series{}
			st.legs[key] = s
			st.order = append(st.order, key)
			label := shortAddr(l.To)
			if l.ICMP {
				label += " (icmp)"
			}
			st.ends[key] = [2]string{shortAddr(l.From), label}
		}
		s.add(l.RTT)
	}
	for _, h := range res.Holds {
		s, ok := st.holds[h.At]
		if !ok {
			s = &series{}
			st.holds[h.At] = s
			st.hOrd = append(st.hOrd, h.At)
		}
		s.add(h.Cost)
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
// Leg indices follow the chain; the header and summary table carry the
// addresses they refer to.
//
// perHold (--holds) splits the hold figure per node. The default aggregate is a
// sum, so the line stays arithmetically closed: legs + hold = total.
//
// Precision is 0.01 ms, matching the measurement floor set by host scheduling
// jitter of roughly half a millisecond.
func ProbeLine(res *Result, perHold bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "seq=%d", res.Seq)
	for i, l := range res.Legs {
		if l.ICMP {
			fmt.Fprintf(&b, " leg%d(icmp)=%.2fms", i, msOf(l.RTT))
		} else {
			fmt.Fprintf(&b, " leg%d=%.2fms", i, msOf(l.RTT))
		}
	}
	if perHold {
		for i, h := range res.Holds {
			fmt.Fprintf(&b, " hold%d=%.2fms", i, msOf(h.Cost))
		}
	} else {
		fmt.Fprintf(&b, " hold=%.2fms", msOf(res.HoldTotal()))
	}
	fmt.Fprintf(&b, " total=%.2fms", msOf(res.E2E))
	return b.String()
}

// Summary renders the ping-style block printed at exit.
func (st *Stats) Summary(chain []string, perHold bool) string {
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

	rows := [][]string{{"", "", "", "", "min", "avg", "max", "mdev", "n"}}
	for i, k := range st.order {
		e := st.ends[k]
		rows = append(rows, row(fmt.Sprintf("leg%d", i), e[0], "->", e[1], st.legs[k]))
	}
	if perHold {
		for i, k := range st.hOrd {
			rows = append(rows, row(fmt.Sprintf("hold%d", i), shortAddr(k), "", "", st.holds[k]))
		}
	} else {
		rows = append(rows, row("hold", "", "", "", &st.holdAll))
	}
	// total last: it is the whole of which every row above is a part.
	rows = append(rows, row("total", "", "", "", &st.total))
	b.WriteString(renderTable(rows))
	return b.String()
}

func row(name, from, arrow, to string, s *series) []string {
	return []string{
		name, from, arrow, to,
		fmt.Sprintf("%.2f", s.min()),
		fmt.Sprintf("%.2f", s.avg()),
		fmt.Sprintf("%.2f", s.max()),
		fmt.Sprintf("%.2f", s.mdev()),
		fmt.Sprintf("%d", s.n()),
	}
}

// labelCols is how many leading columns are text: name, from, arrow, to.
const labelCols = 4

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
		var line strings.Builder
		for i, c := range r {
			switch {
			case i == 0:
				fmt.Fprintf(&line, "%-*s", w[i], c)
			case i < labelCols:
				// Endpoint columns are padded independently, which is what puts
				// every arrow in the same screen column.
				fmt.Fprintf(&line, " %-*s", w[i], c)
			default:
				fmt.Fprintf(&line, "  %*s", w[i], c)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
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
