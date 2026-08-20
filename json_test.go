package main

// JSON payload: a second renderer over the same Stats as the text summary, so
// the two must agree by construction.

import (
	"encoding/json"
	"strings"
	"testing"
)

// sampleStats builds a finished two-hop run.
func sampleStats(t *testing.T) *Stats {
	t.Helper()
	st := NewStats()
	for i := 0; i < 4; i++ {
		st.Sent()
	}
	for i := 0; i < 4; i++ {
		st.Add(&Result{
			Seq: i,
			E2E: 120*ms + int64(i)*100000,
			Legs: []Leg{
				{From: "192.0.2.36", To: "192.0.2.101:8888", RTT: 87 * ms},
				{From: "192.0.2.101:8888", To: "198.51.100.20:8888", RTT: 32 * ms},
			},
			Holds: []Hold{
				{At: "192.0.2.101:8888", Cost: 90000},
				{At: "198.51.100.20:8888", Cost: 50000},
			},
		})
	}
	return st
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestJSONShape(t *testing.T) {
	st := sampleStats(t)
	out, err := st.JSON(
		[]string{"relay.example", "target.example"},
		[]string{"192.0.2.101:8888", "198.51.100.20:8888"},
		"192.0.2.36", 8888, 40001)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	m := decode(t, out)

	if m["v"].(float64) != JSONSchemaVersion {
		t.Errorf("v: got %v want %d", m["v"], JSONSchemaVersion)
	}
	// The port is a run-level fact; repeating it per hop is noise.
	if m["port"].(float64) != 8888 {
		t.Errorf("port: got %v", m["port"])
	}
	for _, r := range m["resolved"].([]any) {
		if strings.Contains(r.(string), ":") {
			t.Errorf("resolved should carry bare addresses, got %q", r)
		}
	}
	// chain keeps what the user typed, so a hostname run stays traceable.
	if got := m["chain"].([]any)[0].(string); got != "relay.example" {
		t.Errorf("chain[0]: got %q want the user-supplied name", got)
	}
	if m["sent"].(float64) != 4 || m["returned"].(float64) != 4 {
		t.Errorf("counts: sent=%v returned=%v", m["sent"], m["returned"])
	}
	if m["loss_pct"].(float64) != 0 {
		t.Errorf("loss_pct: got %v want 0", m["loss_pct"])
	}
	// generated_at must be UTC: a consumer converts client-side, and an earlier
	// timestamps must be independent of the viewer's timezone.
	if ts := m["generated_at"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("generated_at should be UTC (Z-suffixed), got %q", ts)
	}
}

// Every series must carry its sample count. An embedded-struct field named "n"
// on both the outer and inner type silently drops one of them.
func TestJSONEverySeriesHasSampleCount(t *testing.T) {
	st := sampleStats(t)
	out, _ := st.JSON([]string{"a", "b"},
		[]string{"192.0.2.101:8888", "198.51.100.20:8888"}, "192.0.2.36", 8888, 40001)
	m := decode(t, out)

	legs := m["legs"].([]any)
	if len(legs) != 2 {
		t.Fatalf("got %d legs want 2", len(legs))
	}
	for i, raw := range legs {
		l := raw.(map[string]any)
		if _, ok := l["n"]; !ok {
			t.Errorf("leg %d has no sample count: %v", i, l)
		}
		if l["n"].(float64) != 4 {
			t.Errorf("leg %d: n=%v want 4", i, l["n"])
		}
		if l["leg"].(float64) != float64(i) {
			t.Errorf("leg index: got %v want %d", l["leg"], i)
		}
		for _, k := range []string{"from", "to", "min", "avg", "max", "mdev"} {
			if _, ok := l[k]; !ok {
				t.Errorf("leg %d missing %q", i, k)
			}
		}
	}
	for i, raw := range m["holds"].([]any) {
		h := raw.(map[string]any)
		if _, ok := h["n"]; !ok {
			t.Errorf("hold %d has no sample count: %v", i, h)
		}
		if h["hold"].(float64) != float64(i) {
			t.Errorf("hold index: got %v want %d", h["hold"], i)
		}
	}
	if _, ok := m["total"].(map[string]any)["n"]; !ok {
		t.Error("total has no sample count")
	}
}

// The payload must report the same numbers as the text summary, since a
// consumer comparing the two would otherwise see a discrepancy that is not real.
func TestJSONAgreesWithTextSummary(t *testing.T) {
	st := sampleStats(t)
	chain := []string{"192.0.2.101:8888", "198.51.100.20:8888"}

	out, _ := st.JSON([]string{"a", "b"}, chain, "192.0.2.36", 8888, 40001)
	m := decode(t, out)
	text := st.Summary(chain, true)

	// 87.00 leg0, 32.00 leg1, 0.09/0.05 holds -- each must appear in both.
	for _, want := range []string{"87.00", "32.00", "0.09", "0.05"} {
		if !strings.Contains(text, want) {
			t.Errorf("text summary missing %q:\n%s", want, text)
		}
	}
	if got := m["legs"].([]any)[0].(map[string]any)["avg"].(float64); got != 87.00 {
		t.Errorf("json leg0 avg %v disagrees with the text summary's 87.00", got)
	}
	if got := m["legs"].([]any)[1].(map[string]any)["avg"].(float64); got != 32.00 {
		t.Errorf("json leg1 avg %v disagrees with the text summary's 32.00", got)
	}
}

// A run where nothing came back has no min, mean, or deviation. min/max on an
// empty series are +/-Inf and avg is NaN, none of which encoding/json can
// represent -- unguarded, the call returns an error and emits nothing at all.
func TestJSONTotalLossStaysValid(t *testing.T) {
	st := NewStats()
	for i := 0; i < 3; i++ {
		st.Sent()
	}

	out, err := st.JSON([]string{"a"}, []string{"192.0.2.4:8888"}, "192.0.2.36", 8888, 40001)
	if err != nil {
		t.Fatalf("total loss produced no payload: %v", err)
	}
	m := decode(t, out)

	if m["loss_pct"].(float64) != 100 {
		t.Errorf("loss_pct: got %v want 100", m["loss_pct"])
	}
	if m["returned"].(float64) != 0 {
		t.Errorf("returned: got %v want 0", m["returned"])
	}
	// Absent beats a fabricated zero: a 0 ms leg would read as an instant path.
	for _, k := range []string{"legs", "holds", "total"} {
		if _, present := m[k]; present {
			t.Errorf("%q should be omitted when nothing returned, got %v", k, m[k])
		}
	}
	if strings.Contains(string(out), "Inf") || strings.Contains(string(out), "NaN") {
		t.Errorf("payload contains a non-representable float:\n%s", out)
	}
}

func TestJSONCountsLateAndCancelled(t *testing.T) {
	st := sampleStats(t)
	st.Late()
	st.Cancelled(1)

	out, _ := st.JSON([]string{"a"}, []string{"192.0.2.4:8888"}, "192.0.2.36", 8888, 40001)
	m := decode(t, out)
	if m["late"].(float64) != 1 {
		t.Errorf("late: got %v want 1", m["late"])
	}
	if m["cancelled"].(float64) != 1 {
		t.Errorf("cancelled: got %v want 1", m["cancelled"])
	}
}

func TestRound2(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{87.194999, 87.19}, {87.195001, 87.2}, {0.004, 0}, {0.005, 0.01}, {120, 120},
	} {
		if got := round2(c.in); got != c.want {
			t.Errorf("round2(%v): got %v want %v", c.in, got, c.want)
		}
	}
}

// The JSON payload must record which source port the run used. A stored
// measurement without it cannot be reproduced on a path that assigns latency
// per 5-tuple, which is the whole reason --sport exists.
func TestJSONRecordsSourcePort(t *testing.T) {
	st := NewStats()
	st.Add(&Result{
		Seq: 0, E2E: 26_000_000,
		Legs:  []Leg{{From: "198.51.100.1", To: "192.0.2.8:8888", RTT: 25_900_000}},
		Holds: []Hold{{At: "192.0.2.8:8888", Cost: 100_000}},
	})
	raw, err := st.JSON([]string{"192.0.2.8"}, []string{"192.0.2.8:8888"},
		"198.51.100.1", DefaultPort, 43000)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// Spelled out in JSON: a machine consumer has no --sport flag for context,
	// and the file's other keys are full words (duration_ms, loss_pct).
	v, ok := got["source_port"]
	if !ok {
		t.Fatalf("source_port absent; a stored run cannot be reproduced:\n%s", raw)
	}
	if n, _ := v.(float64); int(n) != 43000 {
		t.Errorf("source_port: got %v want 43000", v)
	}
	// The destination port is a different field and must not be confused with it.
	if p, _ := got["port"].(float64); int(p) != DefaultPort {
		t.Errorf("port (destination): got %v want %d", got["port"], DefaultPort)
	}
}
