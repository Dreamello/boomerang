package main

// Name resolution happens here, once per run.

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveHopLiteralAddress(t *testing.T) {
	addr, label, err := resolveHop("192.0.2.101", 8888)
	if err != nil {
		t.Fatalf("resolveHop: %v", err)
	}
	if addr != "192.0.2.101:8888" {
		t.Errorf("addr: got %q want 192.0.2.101:8888", addr)
	}
	// Nothing was resolved, so there is nothing to disclose in parentheses.
	if label != "192.0.2.101" {
		t.Errorf("label: got %q want the bare address", label)
	}
}

func TestResolveHopKeepsExplicitPort(t *testing.T) {
	addr, _, err := resolveHop("192.0.2.1:9999", 8888)
	if err != nil {
		t.Fatalf("resolveHop: %v", err)
	}
	if addr != "192.0.2.1:9999" {
		t.Errorf("explicit port lost: got %q", addr)
	}
}

// A name must come back as an address for the wire, with the name kept only for

// display -- ping's "host (ip)" convention.

func TestResolveHopName(t *testing.T) {
	addr, label, err := resolveHop("localhost", 8888)
	if err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("resolveHop returned an unparseable address %q: %v", addr, err)
	}
	if host == "localhost" {
		t.Errorf("wire address still carries a name: %q", addr)
	}
	if !strings.HasPrefix(label, "localhost (") || !strings.HasSuffix(label, ")") {
		t.Errorf("label should read \"localhost (ip)\", got %q", label)
	}
	if !strings.Contains(label, host) {
		t.Errorf("label %q does not disclose the resolved address %q", label, host)
	}
}

func TestResolveHopRejectsGarbage(t *testing.T) {
	if _, _, err := resolveHop("no-such-host.invalid", 8888); err == nil {
		t.Error("accepted a name that cannot resolve")
	}
}

// The agent must refuse to send anywhere it would have to resolve. A lookup on

// a relay lands between its two timestamps and is charged to wire time, which

// silently inflates a leg; and an agent that resolves on demand can be made to

// issue DNS queries by anyone who can craft a packet.

// A source given a hostname on the wire is a bug in the caller, not something to

// paper over by resolving late.

// The default key path has to be usable by whoever is running. On a relay the
// system key is 0600 root:root (the agent reads it via LoadCredential), so a
// normal login needs its own copy to be picked up without a flag.
func TestDefaultKeyFilePrefersAUserOwnedCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No user copy: fall back to a path root can use.
	first := defaultKeyFile()
	if runtime.GOOS == "linux" && first != "/etc/boomerang.key" {
		t.Errorf("with no user key, linux should fall back to the system path, got %q", first)
	}

	// Once a user copy exists it wins, so `boomerang <chain>` works unflagged.
	userKey := filepath.Join(home, ".config", "boomerang", "key")
	if err := os.MkdirAll(filepath.Dir(userKey), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userKey, []byte(strings.Repeat("ab", KeyLen)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultKeyFile(); got != userKey {
		t.Errorf("user-owned key should win: got %q want %q", got, userKey)
	}
}
