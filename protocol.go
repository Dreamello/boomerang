package main

// Wire format for boomerang probes.
//
// A probe carries its own route (chain) and an append-only array of timestamps
// (stamps). Each node stamps its own monotonic clock only, so clock offsets
// between nodes cancel when legs are derived by differencing adjacent
// intervals (see legs.go).
//
// Every packet is authenticated with a truncated HMAC-SHA-256 tag, because an
// agent sends to addresses named INSIDE the packet: the MAC is what binds that
// routing to a holder of the shared key. Bad MACs are dropped silently.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// ProtoVersion is bumped only on an incompatible wire change.
	ProtoVersion = 1

	// TagLen is the number of HMAC-SHA-256 bytes kept. 16 bytes (128 bits) is
	// far beyond what an online forgery attempt against a UDP probe needs.
	TagLen = 16

	// MaxPacket bounds a read buffer. A 2-relay probe is ~500 B; this leaves
	// room for long chains without inviting memory games.
	MaxPacket = 4096

	// KeyLen is the pre-shared key length in bytes (hex-encoded on disk).
	KeyLen = 32
)

// Phase tells an agent which direction a probe is travelling.
type Phase string

const (
	PhaseOut  Phase = "out"  // heading away from the source
	PhaseBack Phase = "back" // unwinding toward the source
)

// Stamp holds one node's four monotonic readings, in nanoseconds on that
// node's own clock. Zero means "not taken yet".
//
//	TIn  - packet arrived, outbound
//	TFwd - packet handed to the socket, outbound
//	TRcv - packet arrived, return
//	TOut - packet handed to the socket, return
//
// (TFwd, TRcv) is the wire-only window: it excludes this node's own
// processing, so derived legs are pure wire time. (TIn, TOut) is the full
// window; the difference of the two is this node's processing cost.
type Stamp struct {
	N    int   `json:"n"`               // hop index this stamp belongs to
	TIn  int64 `json:"t_in,omitempty"`  // ns, own monotonic clock
	TFwd int64 `json:"t_fwd,omitempty"` // ns
	TRcv int64 `json:"t_rcv,omitempty"` // ns
	TOut int64 `json:"t_out,omitempty"` // ns
}

// Packet is the on-wire probe.
//
// Timestamps are int64 nanoseconds: ns since process start passes float64's
// 53-bit exact-integer range after ~104 days of uptime, which a long-lived
// relay reaches. Typed structs keep encoding/json exact.
type Packet struct {
	V      int      `json:"v"`
	Seq    int      `json:"seq"`
	Chain  []string `json:"chain"` // ordered hops; last entry is the destination
	Phase  Phase    `json:"phase"`
	Hop    int      `json:"hop"`   // index into Chain of the node handling this packet
	Reply  []string `json:"reply"` // observed previous-hop addrs, appended per hop
	Stamps []Stamp  `json:"stamps"`
}

// signed is the envelope actually written to the wire: a packet plus a MAC
// over its encoded bytes.
type signed struct {
	P   json.RawMessage `json:"p"`
	Tag string          `json:"tag"` // hex, TagLen bytes
}

var (
	ErrShort     = errors.New("packet too short")
	ErrVersion   = errors.New("unsupported protocol version")
	ErrBadMAC    = errors.New("authentication failed")
	ErrMalformed = errors.New("malformed packet")
	ErrChain     = errors.New("invalid chain")
	ErrHop       = errors.New("hop out of range")
	ErrKeyPerms  = errors.New("key file must not be world-accessible")
	ErrKeyLen    = errors.New("key must be 32 bytes hex-encoded")
)

// LoadKey reads a hex-encoded pre-shared key from disk.
//
// The key comes from a file, keeping it out of argv, the environment, `ps`, and
// shell history.
//
// The permission check requires the file be unreadable to other users, catching
// what a bare `openssl rand -hex 32 > key` leaves under a default umask. Group
// bits are an explicit administrative grant and are allowed: systemd's
// LoadCredential= hands the agent a 0440 root:root file inside a 0550 root:root
// directory, reachable only by root and the service's own user.
func LoadKey(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o007 != 0 {
		return nil, fmt.Errorf("%w (have %#o): chmod 600 %s", ErrKeyPerms, perm, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("key file: %w: %v", ErrKeyLen, err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w (have %d bytes)", ErrKeyLen, len(key))
	}
	return key, nil
}

// tag computes the truncated HMAC over body.
func tag(key, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return m.Sum(nil)[:TagLen]
}

// Encode marshals and authenticates a packet.
func Encode(p *Packet, key []byte) ([]byte, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return json.Marshal(signed{P: body, Tag: hex.EncodeToString(tag(key, body))})
}

// Decode authenticates and unmarshals a packet. Anything failing verification,
// including truncated or unrelated traffic, returns ErrBadMAC so callers can
// drop it on one condition.
func Decode(buf []byte, key []byte) (*Packet, error) {
	if len(buf) < 2 {
		return nil, ErrShort
	}
	var env signed
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, ErrBadMAC
	}
	if len(env.P) == 0 || env.Tag == "" {
		return nil, ErrBadMAC
	}
	want, err := hex.DecodeString(env.Tag)
	if err != nil || len(want) != TagLen {
		return nil, ErrBadMAC
	}
	// Constant-time compare: never leak tag bytes through timing.
	if !hmac.Equal(want, tag(key, env.P)) {
		return nil, ErrBadMAC
	}
	var p Packet
	if err := json.Unmarshal(env.P, &p); err != nil {
		return nil, ErrMalformed
	}
	if p.V != ProtoVersion {
		return nil, ErrVersion
	}
	if len(p.Chain) == 0 {
		return nil, ErrChain
	}
	if p.Hop < 0 || p.Hop >= len(p.Chain) {
		return nil, ErrHop
	}
	if p.Phase != PhaseOut && p.Phase != PhaseBack {
		return nil, ErrMalformed
	}
	return &p, nil
}

// IsLast reports whether the packet's current hop is the destination.
func (p *Packet) IsLast() bool { return p.Hop == len(p.Chain)-1 }

// stampAt returns a pointer to hop n's stamp, creating it if absent.
func (p *Packet) stampAt(n int) *Stamp {
	for i := range p.Stamps {
		if p.Stamps[i].N == n {
			return &p.Stamps[i]
		}
	}
	p.Stamps = append(p.Stamps, Stamp{N: n})
	return &p.Stamps[len(p.Stamps)-1]
}

// StampIn records arrival at hop n on the outbound trip.
func (p *Packet) StampIn(n int, ns int64) { p.stampAt(n).TIn = ns }

// StampFwd records the outbound send at hop n.
func (p *Packet) StampFwd(n int, ns int64) { p.stampAt(n).TFwd = ns }

// StampRcv records arrival at hop n on the return trip.
func (p *Packet) StampRcv(n int, ns int64) { p.stampAt(n).TRcv = ns }

// StampOut records the return send at hop n.
func (p *Packet) StampOut(n int, ns int64) { p.stampAt(n).TOut = ns }

// Stamp returns hop n's stamp and whether it exists.
func (p *Packet) Stamp(n int) (Stamp, bool) {
	for _, s := range p.Stamps {
		if s.N == n {
			return s, true
		}
	}
	return Stamp{}, false
}

// AppendReply records the address this packet was received from, so the return
// path unwinds without any relay holding state. This is what carries a NAT'd
// source's observable address, which only its first hop can see.
func (p *Packet) AppendReply(addr string) { p.Reply = append(p.Reply, addr) }

// ReturnAddr pops the address to send back to while unwinding.
func (p *Packet) ReturnAddr() (string, bool) {
	if len(p.Reply) == 0 {
		return "", false
	}
	last := len(p.Reply) - 1
	addr := p.Reply[last]
	p.Reply = p.Reply[:last]
	return addr, true
}
