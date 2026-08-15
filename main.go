package main

// boomerang measures end-to-end and per-leg RTT through a chain of relays from
// a single probe.
//
//	boomerang 192.0.2.101 198.51.100.20    source: last address is the destination
//	boomerang 198.51.100.20                direct probe, no relays
//	boomerang --agent                      relay or destination daemon
//
// The pre-shared key is read from a file, keeping it out of ps output and shell
// history.

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// DefaultPort is unprivileged, above nothing the OS hands out for ephemeral
// sockets, and unassigned for UDP.
const DefaultPort = 8888

func defaultKeyFile() string {
	if runtime.GOOS == "linux" {
		return "/etc/boomerang.key"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "boomerang.key"
	}
	return filepath.Join(home, ".config", "boomerang", "key")
}

func main() {
	var (
		agentMode = flag.Bool("agent", false, "run as a relay/destination agent")
		port      = flag.Int("port", DefaultPort, "UDP port")
		keyFile   = flag.String("key-file", defaultKeyFile(), "file holding the hex pre-shared key")
		interval  = flag.Duration("i", time.Second, "interval between probes")
		count     = flag.Int("c", 0, "stop after this many probes (0 = until interrupted)")
		wait      = flag.Duration("W", 2*time.Second, "per-probe reply timeout")
		bind      = flag.String("bind", "0.0.0.0", "agent bind address")
		dropRate  = flag.Float64("debug-drop", 0, "agent: drop this fraction of packets (testing)")
		verbose   = flag.Bool("v", false, "log dropped/unauthenticated packets")
		perHold   = flag.Bool("holds", false, "show hold time per node instead of one total")
		asJSON    = flag.Bool("json", false, "print one JSON object at exit instead of the text summary")
	)
	flag.Usage = usage
	flag.Parse()

	key, err := LoadKey(*keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		fmt.Fprintf(os.Stderr, "generate one with: openssl rand -hex 32 > %s && chmod 600 %s\n",
			*keyFile, *keyFile)
		os.Exit(2)
	}

	if *agentMode {
		runAgent(*bind, *port, key, *dropRate, *verbose)
		return
	}

	targets := flag.Args()
	if len(targets) == 0 {
		usage()
		os.Exit(2)
	}
	runSource(targets, *port, key, *interval, *count, *wait, *perHold, *asJSON)
}

func usage() {
	fmt.Fprint(os.Stderr, `boomerang - per-leg RTT through a chain of relays

  boomerang [options] HOP [HOP ...]   measure; the last HOP is the destination
  boomerang --agent [options]         run the relay/destination daemon

Each HOP is an address, optionally with :port. Any number of relays is allowed;
with none, boomerang is a plain UDP ping.

Options:
`)
	flag.PrintDefaults()
}

func runAgent(bind string, port int, key []byte, dropRate float64, verbose bool) {
	addr := net.JoinHostPort(bind, fmt.Sprint(port))
	a, err := NewAgent(addr, key, dropRate, verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()
	fmt.Printf("boomerang agent listening on udp/%s\n", addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Printf("\nboomerang agent stopping (%s)\n", a.Stats())
		_ = a.Close()
	}()
	if err := a.Serve(); err != nil && !strings.Contains(err.Error(), "use of closed") {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
}

func runSource(targets []string, port int, key []byte, interval time.Duration, count int, wait time.Duration, perHold, asJSON bool) {
	// Resolve every hop once, here, and put addresses on the wire: DNS stays
	// outside the measured intervals, and the chain is pinned to the hosts
	// resolved at startup even under GeoDNS or round-robin.
	chain := make([]string, 0, len(targets))
	labels := make([]string, 0, len(targets))
	for _, t := range targets {
		addr, label, err := resolveHop(t, port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
			os.Exit(1)
		}
		chain = append(chain, addr)
		labels = append(labels, label)
	}

	// In JSON mode stdout carries the payload and nothing else, so per-probe
	// lines go to stderr where a redirect to a file leaves them behind.
	probeOut := io.Writer(os.Stdout)
	if asJSON {
		probeOut = os.Stderr
	}
	src, err := NewSource(chain, key, wait, probeOut, perHold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()

	// Show what each name resolved to, like ping's "PING host (ip)".
	dest := labels[len(labels)-1]
	if len(labels) == 1 {
		fmt.Fprintf(probeOut, "BOOMERANG %s direct\n", dest)
	} else {
		fmt.Fprintf(probeOut, "BOOMERANG %s via %s\n", dest, strings.Join(labels[:len(labels)-1], ", "))
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	src.Run(count, interval, stop)
	if asJSON {
		payload, err := src.Stats().JSON(targets, chain, src.Local(), port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(payload))
	} else {
		fmt.Print(src.Stats().Summary(chain, perHold))
	}

	// Exit non-zero when nothing came back, matching ping so this works in a
	// shell conditional.
	if src.Stats().recv == 0 {
		os.Exit(1)
	}
}

// resolveHop turns a user-supplied target into a wire address plus a display
// label. Resolution happens here, once per run; addresses go on the wire.
//
// The label follows ping's "host (ip)" convention when a name was given, so the
// user sees what it resolved to. A literal address is its own label.
func resolveHop(target string, port int) (addr, label string, err error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host, portStr = target, fmt.Sprint(port)
	}
	ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, portStr))
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", target, err)
	}
	addr = net.JoinHostPort(ua.IP.String(), portStr)
	if net.ParseIP(host) != nil {
		// A literal address: nothing was resolved, so nothing to disclose.
		return addr, shortAddr(addr), nil
	}
	return addr, fmt.Sprintf("%s (%s)", host, ua.IP), nil
}
