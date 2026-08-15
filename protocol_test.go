package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := hex.DecodeString(strings.Repeat("ab", KeyLen))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func samplePacket() *Packet {
	return &Packet{
		V:     ProtoVersion,
		Seq:   41,
		Chain: []string{"192.0.2.101:8888", "198.51.100.20:8888"},
		Phase: PhaseOut,
		Hop:   0,
		Reply: []string{"203.0.113.100:53210"},
		Stamps: []Stamp{
			{N: 0, TIn: 881234120000, TFwd: 881234161000},
		},
	}
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	key := testKey(t)
	in := samplePacket()
	buf, err := Encode(in, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(buf, key)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Seq != in.Seq || out.Phase != in.Phase || out.Hop != in.Hop {
		t.Errorf("header mismatch: got %+v want %+v", out, in)
	}
	if len(out.Chain) != 2 || out.Chain[1] != in.Chain[1] {
		t.Errorf("chain mismatch: got %v", out.Chain)
	}
	if len(out.Reply) != 1 || out.Reply[0] != in.Reply[0] {
		t.Errorf("reply mismatch: got %v", out.Reply)
	}
	if len(out.Stamps) != 1 || out.Stamps[0] != in.Stamps[0] {
		t.Errorf("stamps mismatch: got %v want %v", out.Stamps, in.Stamps)
	}
}

// Nanosecond timestamps must survive encoding exactly. A float64 round-trip
// silently corrupts values past 2^53, which a relay reaches after ~104 days of
// uptime.
func TestTimestampExactnessBeyondFloat64(t *testing.T) {
	key := testKey(t)
	// 2^53 + 1 is the smallest integer float64 cannot represent.
	const beyond = int64(1)<<53 + 1
	// ~292 years in ns: the practical top of the int64 range.
	const huge = int64(9223372036854775807)

	for _, ns := range []int64{beyond, beyond + 2, huge - 1} {
		p := samplePacket()
		p.Stamps = []Stamp{{N: 0, TIn: ns, TFwd: ns + 1, TRcv: ns + 2, TOut: ns + 3}}
		buf, err := Encode(p, key)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out, err := Decode(buf, key)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		got := out.Stamps[0]
		if got.TIn != ns || got.TFwd != ns+1 || got.TRcv != ns+2 || got.TOut != ns+3 {
			t.Errorf("ns %d corrupted: got %+v", ns, got)
		}
		// Prove the float64 path would have failed, so this test is meaningful.
		if ns == beyond && int64(float64(ns)) == ns {
			t.Fatal("float64 round-trip did not lose precision; test is vacuous")
		}
	}
}

func TestDecodeRejectsBadMAC(t *testing.T) {
	key := testKey(t)
	other, _ := hex.DecodeString(strings.Repeat("cd", KeyLen))

	buf, err := Encode(samplePacket(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(buf, other); !errors.Is(err, ErrBadMAC) {
		t.Errorf("wrong key: got %v want ErrBadMAC", err)
	}
}

// The MAC must cover the stamps, not just the header: a relay that could edit
// timestamps in flight could fabricate measurements.
func TestDecodeDetectsTamperedStamp(t *testing.T) {
	key := testKey(t)
	buf, err := Encode(samplePacket(), key)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		P   json.RawMessage `json:"p"`
		Tag string          `json:"tag"`
	}
	if err := json.Unmarshal(buf, &env); err != nil {
		t.Fatal(err)
	}
	var p Packet
	if err := json.Unmarshal(env.P, &p); err != nil {
		t.Fatal(err)
	}
	p.Stamps[0].TIn = 1 // forge a faster leg
	forged, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	env.P = forged
	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(tampered, key); !errors.Is(err, ErrBadMAC) {
		t.Errorf("tampered stamp: got %v want ErrBadMAC", err)
	}
}

// Anything a scanner or stray process might send must be rejected without a
// panic, since an agent decodes whatever arrives on an open UDP port.
func TestDecodeRejectsGarbageWithoutPanic(t *testing.T) {
	key := testKey(t)
	good, err := Encode(samplePacket(), key)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":            {},
		"one byte":         {0x7b},
		"not json":         []byte("hello world"),
		"json but not env": []byte(`{"foo":"bar"}`),
		"no tag":           []byte(`{"p":{"v":1}}`),
		"tag wrong len":    []byte(`{"p":{"v":1},"tag":"aabb"}`),
		"tag not hex":      []byte(`{"p":{"v":1},"tag":"zzzz"}`),
		"truncated":        good[:len(good)/2],
		"nul bytes":        {0, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(buf, key); err == nil {
				t.Errorf("accepted %q", name)
			}
		})
	}
}

func TestDecodeRejectsBadFields(t *testing.T) {
	key := testKey(t)
	cases := []struct {
		name string
		mut  func(*Packet)
		want error
	}{
		{"wrong version", func(p *Packet) { p.V = 99 }, ErrVersion},
		{"empty chain", func(p *Packet) { p.Chain = nil }, ErrChain},
		{"hop negative", func(p *Packet) { p.Hop = -1 }, ErrHop},
		{"hop past end", func(p *Packet) { p.Hop = 7 }, ErrHop},
		{"bad phase", func(p *Packet) { p.Phase = "sideways" }, ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := samplePacket()
			c.mut(p)
			buf, err := Encode(p, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(buf, key); !errors.Is(err, c.want) {
				t.Errorf("got %v want %v", err, c.want)
			}
		})
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	valid := strings.Repeat("ab", KeyLen)

	t.Run("good", func(t *testing.T) {
		p := filepath.Join(dir, "good.key")
		if err := os.WriteFile(p, []byte(valid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		key, err := LoadKey(p)
		if err != nil {
			t.Fatalf("LoadKey: %v", err)
		}
		if len(key) != KeyLen {
			t.Errorf("got %d bytes want %d", len(key), KeyLen)
		}
	})

	// A key any user on the box can read is not a secret. This is what a bare
	// `openssl rand -hex 32 > key` leaves under a default umask.
	t.Run("world-accessible refused", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o644, 0o604, 0o666, 0o777, 0o606} {
			p := filepath.Join(dir, fmt.Sprintf("perm-%o.key", mode))
			if err := os.WriteFile(p, []byte(valid), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadKey(p); !errors.Is(err, ErrKeyPerms) {
				t.Errorf("mode %#o: got %v want ErrKeyPerms", mode, err)
			}
		}
	})

	// Modes with no world bits must be accepted. 0440 is what systemd's
	// LoadCredential= actually hands the agent (verified on Debian 13 /
	// systemd 257: 0440 root:root inside a 0550 root:root directory), so
	// refusing it would crash-loop the service on every relay.
	t.Run("non-world-accessible accepted", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o600, 0o400, 0o440, 0o640} {
			// A fresh path per mode: WriteFile does not re-apply perms to an
			// existing file, so reusing one would hit the read-only leftover.
			p := filepath.Join(dir, fmt.Sprintf("ok-%o.key", mode))
			if err := os.WriteFile(p, []byte(valid), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadKey(p); err != nil {
				t.Errorf("mode %#o refused: %v", mode, err)
			}
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadKey(filepath.Join(dir, "nope.key")); err == nil {
			t.Error("accepted missing file")
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		p := filepath.Join(dir, "short.key")
		if err := os.WriteFile(p, []byte("abcd"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(p); !errors.Is(err, ErrKeyLen) {
			t.Errorf("got %v want ErrKeyLen", err)
		}
	})

	t.Run("not hex", func(t *testing.T) {
		p := filepath.Join(dir, "nothex.key")
		if err := os.WriteFile(p, []byte(strings.Repeat("zz", KeyLen)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(p); !errors.Is(err, ErrKeyLen) {
			t.Errorf("got %v want ErrKeyLen", err)
		}
	})
}

func TestStampHelpers(t *testing.T) {
	p := samplePacket()
	p.StampRcv(0, 500)
	p.StampOut(0, 600)
	s, ok := p.Stamp(0)
	if !ok {
		t.Fatal("stamp 0 missing")
	}
	if s.TRcv != 500 || s.TOut != 600 {
		t.Errorf("got %+v", s)
	}
	if s.TIn != 881234120000 {
		t.Errorf("existing field clobbered: %+v", s)
	}
	// A new hop index appends rather than overwriting.
	p.StampIn(1, 700)
	if len(p.Stamps) != 2 {
		t.Fatalf("got %d stamps want 2", len(p.Stamps))
	}
	if s1, _ := p.Stamp(1); s1.TIn != 700 {
		t.Errorf("hop 1: got %+v", s1)
	}
	if _, ok := p.Stamp(9); ok {
		t.Error("reported a stamp that was never taken")
	}
}

func TestReplyStack(t *testing.T) {
	p := &Packet{}
	p.AppendReply("a:1")
	p.AppendReply("b:2")
	if addr, ok := p.ReturnAddr(); !ok || addr != "b:2" {
		t.Errorf("got %q %v want b:2", addr, ok)
	}
	if addr, ok := p.ReturnAddr(); !ok || addr != "a:1" {
		t.Errorf("got %q %v want a:1", addr, ok)
	}
	if _, ok := p.ReturnAddr(); ok {
		t.Error("popped from an empty stack")
	}
}

func TestIsLast(t *testing.T) {
	p := samplePacket()
	if p.IsLast() {
		t.Error("hop 0 of 2 reported as last")
	}
	p.Hop = 1
	if !p.IsLast() {
		t.Error("final hop not reported as last")
	}
	// N=0: a direct probe, where the only hop is also the destination.
	direct := &Packet{V: ProtoVersion, Chain: []string{"192.0.2.4:8888"}, Phase: PhaseOut}
	if !direct.IsLast() {
		t.Error("single-hop chain: destination not reported as last")
	}
}
