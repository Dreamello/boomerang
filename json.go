package main

// JSON rendering of a finished run.
//
// A second renderer over the same Stats the text summary uses, so the two report
// identical numbers by construction. Consumers (a panel, a scraper) get every
// series with its sample count rather than a pre-chosen headline figure.

import (
	"encoding/json"
	"math"
	"net"
	"time"
)

// JSONSchemaVersion is bumped when the payload shape changes incompatibly, so a
// consumer can detect it instead of silently misreading fields.
const JSONSchemaVersion = 1

// jsonSeries is one measured quantity. Values are milliseconds; the wire format
// is nanoseconds, but a consumer would only re-derive this rounding.
type jsonSeries struct {
	Min  float64 `json:"min"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
	Mdev float64 `json:"mdev"`
	N    int     `json:"n"`
}

type jsonLeg struct {
	// Index is the hop position; the sample count is jsonSeries.N as "n".
	Index int    `json:"leg"`
	From  string `json:"from"`
	To    string `json:"to"`
	jsonSeries
}

type jsonHold struct {
	Index int    `json:"hold"`
	At    string `json:"at"`
	jsonSeries
}

// jsonRun is the whole payload.
type jsonRun struct {
	V           int    `json:"v"`
	GeneratedAt string `json:"generated_at"`
	// Chain is what the user asked for; Resolved is what it resolved to at
	// startup. They differ when hostnames were given, and the pair is what makes
	// a stale or unexpected resolution visible.
	Chain    []string `json:"chain"`
	Resolved []string `json:"resolved"`
	Source   string   `json:"source"`
	Port     int      `json:"port"`

	DurationMs float64 `json:"duration_ms"`
	Sent       int     `json:"sent"`
	Returned   int     `json:"returned"`
	Late       int     `json:"late"`
	Cancelled  int     `json:"cancelled"`
	LossPct    float64 `json:"loss_pct"`

	// Omitted when nothing returned. A series with no samples has no min or
	// mean, and a zero would read as an instant path; absent is the honest
	// encoding, and it also keeps Inf/NaN away from Marshal.
	Legs  []jsonLeg   `json:"legs,omitempty"`
	Holds []jsonHold  `json:"holds,omitempty"`
	Total *jsonSeries `json:"total,omitempty"`
}

// shortAddrPort drops the ":port" suffix, keeping the bare address.
func shortAddrPort(a string) string {
	if host, _, err := net.SplitHostPort(a); err == nil {
		return host
	}
	return a
}

// round2 keeps the payload at the same 0.01 ms precision the text output uses,
// which is the floor set by host scheduling jitter.
func round2(ms float64) float64 { return math.Round(ms*100) / 100 }

// seriesJSON converts a series.
//
// The zero-sample branch is a belt: min/max on an empty series are +/-Inf and avg
// is NaN, and encoding/json refuses all three, so reaching Marshal with one
// yields no payload at all. Callers already omit empty series, which is what
// keeps a lossy run valid -- this makes the function safe on its own terms.
func seriesJSON(s *series) jsonSeries {
	if s.n() == 0 {
		return jsonSeries{}
	}
	return jsonSeries{
		Min:  round2(s.min()),
		Avg:  round2(s.avg()),
		Max:  round2(s.max()),
		Mdev: round2(s.mdev()),
		N:    s.n(),
	}
}

// JSON renders the run as a single object.
//
// chain is the user-supplied hop list and resolved the addresses actually used,
// both in path order; source is the local address leg 0 started from.
func (st *Stats) JSON(chain, resolved []string, source string, port int) ([]byte, error) {
	// resolved arrives as ip:port; the port is one run-level fact, so report it
	// once rather than repeating it on every hop.
	bare := make([]string, 0, len(resolved))
	for _, r := range resolved {
		bare = append(bare, shortAddrPort(r))
	}
	run := jsonRun{
		V:           JSONSchemaVersion,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Chain:       chain,
		Resolved:    bare,
		Port:        port,
		Source:      source,
		DurationMs:  round2(float64(time.Since(st.start).Nanoseconds()) / 1e6),
		Sent:        st.sent,
		Returned:    st.recv,
		Late:        st.late,
		Cancelled:   st.cancelled,
		LossPct:     round2(st.Loss()),
	}

	for i, k := range st.order {
		e := st.ends[k]
		run.Legs = append(run.Legs, jsonLeg{
			Index: i, From: e[0], To: e[1], jsonSeries: seriesJSON(st.legs[k]),
		})
	}
	// Holds are always per node here. The collapsed view is a display choice for
	// a narrow terminal; a consumer can sum them and cannot recover the split.
	for i, k := range st.hOrd {
		run.Holds = append(run.Holds, jsonHold{
			Index: i, At: shortAddr(k), jsonSeries: seriesJSON(st.holds[k]),
		})
	}
	if st.total.n() > 0 {
		t := seriesJSON(&st.total)
		run.Total = &t
	}
	return json.MarshalIndent(run, "", "  ")
}
