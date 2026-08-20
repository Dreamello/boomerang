package main

import (
	"net"
	"testing"
	"time"
)

// startAgent brings up an agent on a kernel-chosen loopback port and returns
// its address. The agent is closed when the test ends.
func startAgent(t *testing.T, key []byte, dropRate float64) string {
	t.Helper()
	a, err := NewAgent("127.0.0.1:0", key, dropRate, false, 2*time.Second)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	go func() { _ = a.Serve() }()
	t.Cleanup(func() { _ = a.Close() })
	return a.LocalAddr().String()
}

// sendProbe sends one outbound probe down a chain and waits for the return.
func sendProbe(t *testing.T, key []byte, chain []string, timeout time.Duration) (*Packet, int64, error) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	first, err := net.ResolveUDPAddr("udp", chain[0])
	if err != nil {
		t.Fatal(err)
	}
	p := &Packet{V: ProtoVersion, Seq: 1, Chain: chain, Phase: PhaseOut, Hop: 0}
	buf, err := Encode(p, key)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := conn.WriteToUDP(buf, first); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	in := make([]byte, MaxPacket)
	n, _, err := conn.ReadFromUDP(in)
	if err != nil {
		return nil, 0, err
	}
	e2e := int64(time.Since(start))
	got, err := Decode(in[:n], key)
	if err != nil {
		return nil, 0, err
	}
	return got, e2e, nil
}

// A relay forwards and a destination turns around, with no configuration
// telling either which it is.
func TestAgentChainOneRelay(t *testing.T) {
	key := testKey(t)
	dest := startAgent(t, key, 0)
	relay := startAgent(t, key, 0)

	got, e2e, err := sendProbe(t, key, []string{relay, dest}, 2*time.Second)
	if err != nil {
		t.Fatalf("no return: %v", err)
	}
	if got.Phase != PhaseBack {
		t.Errorf("phase: got %s want back", got.Phase)
	}
	if len(got.Stamps) != 2 {
		t.Fatalf("got %d stamps want 2: %+v", len(got.Stamps), got.Stamps)
	}
	// The relay must have all four stamps; the destination only in/out.
	relayStamp, ok := got.Stamp(0)
	if !ok {
		t.Fatal("relay did not stamp")
	}
	if relayStamp.TIn == 0 || relayStamp.TFwd == 0 || relayStamp.TRcv == 0 || relayStamp.TOut == 0 {
		t.Errorf("relay stamp incomplete: %+v", relayStamp)
	}
	destStamp, ok := got.Stamp(1)
	if !ok {
		t.Fatal("destination did not stamp")
	}
	if destStamp.TIn == 0 || destStamp.TOut == 0 {
		t.Errorf("destination stamp incomplete: %+v", destStamp)
	}
	if destStamp.TFwd != 0 || destStamp.TRcv != 0 {
		t.Errorf("destination should not have wire-window stamps: %+v", destStamp)
	}
	// The reply stack must be fully unwound by the time it reaches the source.
	if len(got.Reply) != 0 {
		t.Errorf("reply stack not unwound: %v", got.Reply)
	}
	// And the whole thing must be derivable.
	res, err := DeriveLegs(got, e2e)
	if err != nil {
		t.Fatalf("DeriveLegs on a live probe: %v", err)
	}
	if len(res.Legs) != 2 || len(res.Holds) != 2 {
		t.Errorf("got %d legs %d holds want 2/2", len(res.Legs), len(res.Holds))
	}
}

// Chain length is arbitrary: the same binary handles any depth with no
// per-node configuration.
func TestAgentArbitraryChainLength(t *testing.T) {
	key := testKey(t)
	for hops := 1; hops <= 5; hops++ {
		chain := make([]string, hops)
		for i := range chain {
			chain[i] = startAgent(t, key, 0)
		}
		got, e2e, err := sendProbe(t, key, chain, 3*time.Second)
		if err != nil {
			t.Fatalf("hops=%d: no return: %v", hops, err)
		}
		if len(got.Stamps) != hops {
			t.Errorf("hops=%d: got %d stamps", hops, len(got.Stamps))
		}
		res, err := DeriveLegs(got, e2e)
		if err != nil {
			t.Fatalf("hops=%d: DeriveLegs: %v", hops, err)
		}
		if len(res.Legs) != hops {
			t.Errorf("hops=%d: got %d legs", hops, len(res.Legs))
		}
		for _, l := range res.Legs {
			if l.RTT < 0 {
				t.Errorf("hops=%d: negative leg %v", hops, l)
			}
		}
	}
}

// An agent must not answer traffic it cannot authenticate, and must not die on
// it either.
func TestAgentIgnoresUnauthenticated(t *testing.T) {
	key := testKey(t)
	other := make([]byte, KeyLen)
	for i := range other {
		other[i] = 0x5a
	}
	dest := startAgent(t, key, 0)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	da, _ := net.ResolveUDPAddr("udp", dest)

	// Wrong key, then outright garbage.
	wrong, err := Encode(&Packet{V: ProtoVersion, Chain: []string{dest}, Phase: PhaseOut}, other)
	if err != nil {
		t.Fatal(err)
	}
	for _, junk := range [][]byte{wrong, []byte("hello"), {0, 1, 2, 3}, {}} {
		if _, err := conn.WriteToUDP(junk, da); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, MaxPacket)
	if n, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Errorf("agent replied to unauthenticated traffic (%d bytes)", n)
	}

	// It must still serve a valid probe afterwards.
	if _, _, err := sendProbe(t, key, []string{dest}, 2*time.Second); err != nil {
		t.Errorf("agent stopped serving after junk: %v", err)
	}
}

// Loss injection is how the source's timeout path gets exercised, so it must
// actually drop.
func TestAgentDropInjection(t *testing.T) {
	key := testKey(t)
	dest := startAgent(t, key, 1.0) // drop everything
	if _, _, err := sendProbe(t, key, []string{dest}, 400*time.Millisecond); err == nil {
		t.Error("probe returned from an agent set to drop 100%")
	}
}

// Stamps come from a monotonic clock, so they must be immune to wall-clock
// steps and always advance.
func TestAgentStampsAreMonotonic(t *testing.T) {
	a, err := NewAgent("127.0.0.1:0", testKey(t), 0, false, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	prev := a.now()
	for i := 0; i < 100; i++ {
		n := a.now()
		if n < prev {
			t.Fatalf("clock went backwards: %d then %d", prev, n)
		}
		prev = n
	}
}

func TestAgentRefusesHostnames(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8888", true},
		{"[::1]:8888", true},
		{"localhost:8888", false},
		{"example.com:8888", false},
		{"127.0.0.1", false}, // no port
		{"127.0.0.1:http", false},
		{"", false},
	}
	for _, c := range cases {
		_, err := parseAddr(c.addr)
		if c.ok && err != nil {
			t.Errorf("parseAddr(%q) rejected a literal address: %v", c.addr, err)
		}
		if !c.ok && err == nil {
			t.Errorf("parseAddr(%q) accepted something a relay would have to resolve", c.addr)
		}
	}
}

func TestNewSourceRejectsHostnameChain(t *testing.T) {
	if _, err := NewSource([]string{"localhost:8888"}, testKey(t), 0, nil, false, ""); err == nil {
		t.Error("NewSource accepted an unresolved chain")
	}
}
