package main

// The source: sends probes on a schedule, matches replies, derives legs.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Source drives a measurement run.
type Source struct {
	conn    *net.UDPConn
	key     []byte
	chain   []string
	first   *net.UDPAddr
	timeout time.Duration
	out     io.Writer
	// perHold splits the hold figure per node instead of one aggregate.
	perHold bool
	// local is the address the kernel chose for this route, used to label leg0.
	local string
	// icmpDest, when set, makes the last agent in chain terminate via ICMP to
	// this address instead of turning around.
	icmpDest string

	mu       sync.Mutex
	inFlight map[int]time.Time // seq -> send time, on this process's clock
	stats    *Stats
}

// NewSource dials the first hop and prepares a run.
//
// sport pins the source UDP port when non-zero; 0 lets the kernel choose an
// ephemeral one. Pinning makes a run repeatable on a path that assigns latency
// per flow (5-tuple), so two runs can be compared rather than each drawing a
// different path.
func NewSource(chain []string, key []byte, timeout time.Duration, out io.Writer, perHold bool, icmpDest string, sport int) (*Source, error) {
	if len(chain) == 0 {
		return nil, errors.New("need at least one target")
	}
	// The chain arrives already resolved, so this parses and enforces that
	// invariant here, in one place.
	first, err := parseAddr(chain[0])
	if err != nil {
		return nil, fmt.Errorf("first hop %q must be a literal address: %w", chain[0], err)
	}
	var laddr *net.UDPAddr
	if sport != 0 {
		laddr = &net.UDPAddr{Port: sport}
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		if sport != 0 {
			return nil, fmt.Errorf("bind source port %d: %w", sport, err)
		}
		return nil, err
	}
	// Label leg0 with the local address the kernel picks for this route. An
	// unbound socket has no address until it has a destination, so ask which
	// source a connection to the first hop would use: on a multi-homed host
	// that is per-route, so this is the address the probes actually leave by.
	local := "src"
	if probe, err := net.DialUDP("udp", nil, first); err == nil {
		if ua, ok := probe.LocalAddr().(*net.UDPAddr); ok {
			local = ua.IP.String()
		}
		_ = probe.Close()
	}
	return &Source{
		conn:     conn,
		key:      key,
		chain:    chain,
		first:    first,
		timeout:  timeout,
		out:      out,
		perHold:  perHold,
		local:    local,
		icmpDest: icmpDest,
		inFlight: map[int]time.Time{},
		stats:    NewStats(),
	}, nil
}

func (s *Source) Close() error { return s.conn.Close() }

func (s *Source) Stats() *Stats { return s.stats }

// Local reports the address the kernel chose for this route, which is what leg 0
// starts from.
func (s *Source) Local() string { return s.local }

// SourcePort reports the UDP port probes actually left from. On a path that
// assigns latency per flow, this identifies WHICH flow a run measured, so a
// saved run is reproducible with --sport.
func (s *Source) SourcePort() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

// send transmits one probe.
func (s *Source) send(seq int) error {
	p := &Packet{
		V:        ProtoVersion,
		Seq:      seq,
		Chain:    s.chain,
		Phase:    PhaseOut,
		Hop:      0,
		ICMPDest: s.icmpDest,
	}
	buf, err := Encode(p, s.key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.inFlight[seq] = time.Now()
	s.stats.Sent()
	s.mu.Unlock()

	if _, err := s.conn.WriteToUDP(buf, s.first); err != nil {
		return err
	}
	return nil
}

// receive reads replies until the socket closes, printing each as it lands.
func (s *Source) receive() {
	buf := make([]byte, MaxPacket)
	for {
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed: run over
		}
		arrived := time.Now()
		p, err := Decode(buf[:n], s.key)
		if err != nil {
			continue // not ours, or forged: ignore silently
		}

		s.mu.Lock()
		sentAt, known := s.inFlight[p.Seq]
		if known {
			delete(s.inFlight, p.Seq)
		}
		s.mu.Unlock()

		if !known {
			// Already timed out, or a duplicate: counted separately so the
			// statistics stay over probes that met their deadline.
			s.mu.Lock()
			s.stats.Late()
			s.mu.Unlock()
			fmt.Fprintf(s.out, "seq=%d late (arrived after timeout, excluded)\n", p.Seq)
			continue
		}

		e2e := int64(arrived.Sub(sentAt))
		res, err := DeriveLegs(p, e2e)
		if err != nil {
			fmt.Fprintf(s.out, "seq=%d unusable: %v\n", p.Seq, err)
			continue
		}
		// DeriveLegs labels the first leg "src"; replace it with the real
		// local address now that one is known.
		if len(res.Legs) > 0 && res.Legs[0].From == "src" {
			res.Legs[0].From = s.local
		}
		s.mu.Lock()
		s.stats.Add(res)
		s.mu.Unlock()
		fmt.Fprintln(s.out, ProbeLine(res, s.perHold))
	}
}

// sweep expires probes whose deadline has passed. Which leg dropped a lost
// probe is unknowable here: the stamps died with the packet.
func (s *Source) sweep(now time.Time) {
	s.mu.Lock()
	var lost []int
	for seq, sentAt := range s.inFlight {
		if now.Sub(sentAt) > s.timeout {
			lost = append(lost, seq)
			delete(s.inFlight, seq)
		}
	}
	s.mu.Unlock()
	for _, seq := range lost {
		fmt.Fprintf(s.out, "seq=%d timeout\n", seq)
	}
}

// cancel discards probes that were still within their deadline when the user
// interrupted the run.
//
// A cancelled probe leaves both totals, so loss is computed only over probes
// that were given their full deadline.
func (s *Source) cancel() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.inFlight)
	s.inFlight = map[int]time.Time{}
	s.stats.Cancelled(n)
	return n
}

// Run sends count probes at the given interval (count <= 0 runs until stop is
// closed), then drains in-flight probes up to one timeout before returning.
func (s *Source) Run(count int, interval time.Duration, stop <-chan struct{}) {
	done := make(chan struct{})
	go func() {
		s.receive()
		close(done)
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sweeper := time.NewTicker(100 * time.Millisecond)
	defer sweeper.Stop()

	seq := 0
	if err := s.send(seq); err != nil {
		fmt.Fprintf(s.out, "send: %v\n", err)
	}
	seq++

	// Interrupted runs cancel probes still inside their deadline; a run that
	// ends on its own terms lets them expire as loss.
	interrupted := false

sendLoop:
	for count <= 0 || seq < count {
		select {
		case <-stop:
			interrupted = true
			break sendLoop
		case <-sweeper.C:
			s.sweep(time.Now())
		case <-ticker.C:
			if err := s.send(seq); err != nil {
				fmt.Fprintf(s.out, "send: %v\n", err)
			}
			seq++
		}
	}

	// Give outstanding probes their full deadline before declaring loss.
	deadline := time.After(s.timeout + 100*time.Millisecond)
drain:
	for {
		select {
		case <-stop:
			interrupted = true
			break drain
		case <-sweeper.C:
			s.sweep(time.Now())
			s.mu.Lock()
			empty := len(s.inFlight) == 0
			s.mu.Unlock()
			if empty {
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if interrupted {
		// Cancelled: these probes were cut short of their deadline.
		s.cancel()
	} else {
		// The run finished on its own terms: anything still outstanding has
		// genuinely exceeded its deadline.
		s.sweep(time.Now().Add(s.timeout + time.Second))
	}
	_ = s.conn.Close()
	<-done
}
