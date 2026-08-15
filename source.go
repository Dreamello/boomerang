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

	mu       sync.Mutex
	inFlight map[int]time.Time // seq -> send time, on this process's clock
	stats    *Stats
}

// NewSource dials the first hop and prepares a run.
func NewSource(chain []string, key []byte, timeout time.Duration, out io.Writer) (*Source, error) {
	if len(chain) == 0 {
		return nil, errors.New("need at least one target")
	}
	first, err := net.ResolveUDPAddr("udp", chain[0])
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", chain[0], err)
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	return &Source{
		conn:     conn,
		key:      key,
		chain:    chain,
		first:    first,
		timeout:  timeout,
		out:      out,
		inFlight: map[int]time.Time{},
		stats:    NewStats(),
	}, nil
}

func (s *Source) Close() error { return s.conn.Close() }

func (s *Source) Stats() *Stats { return s.stats }

// send transmits one probe.
func (s *Source) send(seq int) error {
	p := &Packet{
		V:     ProtoVersion,
		Seq:   seq,
		Chain: s.chain,
		Phase: PhaseOut,
		Hop:   0,
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
			// Already timed out, or a duplicate. Counted, never folded into
			// the statistics -- a reply that missed its deadline would bias
			// the numbers toward the slow tail.
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
		s.mu.Lock()
		s.stats.Add(res)
		s.mu.Unlock()
		fmt.Fprintln(s.out, ProbeLine(res))
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

sendLoop:
	for count <= 0 || seq < count {
		select {
		case <-stop:
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
	s.sweep(time.Now().Add(s.timeout + time.Second)) // expire whatever is left
	_ = s.conn.Close()
	<-done
}
