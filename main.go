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
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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

// defaultKeyFile picks the key a run should use when none was named.
//
// A user-owned copy under $HOME wins when it exists, so a human on a relay box
// gets a working default: the system key is 0600 root:root because the agent
// reads it through systemd's LoadCredential, and a normal login cannot open it.
// Falling back to the system path keeps root and the unit working unchanged.
func defaultKeyFile() string {
	if home, err := os.UserHomeDir(); err == nil {
		user := filepath.Join(home, ".config", "boomerang", "key")
		if _, err := os.Stat(user); err == nil {
			return user
		}
		if runtime.GOOS != "linux" {
			return user
		}
	}
	if runtime.GOOS == "linux" {
		return "/etc/boomerang.key"
	}
	return "boomerang.key"
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
		icmpLast  = flag.Bool("icmp-last", false, "reach the final target via ICMP echo (no agent needed there)")
		icmpWait  = flag.Duration("icmp-wait", time.Second, "ICMP echo reply timeout at the terminator")
		sport     = flag.Int("sport", 0, "pin the source UDP port (0 = kernel picks); makes runs comparable on per-flow paths")
	)
	flag.Usage = usage
	// Permute so flags are accepted in any position, like ping/curl/ssh:
	// `boomerang relay.example -c 5` works, not just `-c 5 relay.example`.
	flag.CommandLine.Parse(permuteArgs(flag.CommandLine, os.Args[1:]))

	key, err := LoadKey(*keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		// Tailor the hint: telling someone to generate a key over a file they
		// merely cannot READ would overwrite a working fleet key and break every
		// agent sharing it.
		switch {
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintf(os.Stderr,
				"the key exists but this user cannot read it. Either run with sudo, or\n"+
					"copy it somewhere you own:\n"+
					"  sudo install -m600 -o $USER %s ~/.config/boomerang/key\n"+
					"  boomerang --key-file ~/.config/boomerang/key ...\n", *keyFile)
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(os.Stderr,
				"no key there yet. Copy the one the rest of the chain uses, or for a new\n"+
					"fleet generate one and install it on every node:\n"+
					"  openssl rand -hex 32 > %s && chmod 600 %s\n", *keyFile, *keyFile)
		}
		os.Exit(2)
	}

	if *agentMode {
		runAgent(*bind, *port, key, *dropRate, *verbose, *icmpWait)
		return
	}

	targets := flag.Args()
	if len(targets) == 0 {
		usage()
		os.Exit(2)
	}
	if *icmpLast && len(targets) < 2 {
		fmt.Fprintf(os.Stderr, "boomerang: --icmp-last requires at least one relay and an ICMP target\n")
		os.Exit(2)
	}
	runSource(targets, *port, key, *interval, *count, *wait, *perHold, *asJSON, *icmpLast, *sport)
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

func runAgent(bind string, port int, key []byte, dropRate float64, verbose bool, icmpTimeout time.Duration) {
	addr := net.JoinHostPort(bind, fmt.Sprint(port))
	a, err := NewAgent(addr, key, dropRate, verbose, icmpTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
	defer a.Close()
	defer a.icmp.close()
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

func runSource(targets []string, port int, key []byte, interval time.Duration, count int, wait time.Duration, perHold, asJSON bool, icmpLast bool, sport int) {
	// When --icmp-last is set, the final target is an ICMP destination (bare
	// IP, no agent needed), and only the preceding targets are UDP agents.
	var icmpDest string
	udpTargets := targets
	if icmpLast {
		icmpDest = targets[len(targets)-1]
		udpTargets = targets[:len(targets)-1]
	}

	// Resolve every hop once, here, and put addresses on the wire: DNS stays
	// outside the measured intervals, and the chain is pinned to the hosts
	// resolved at startup even under GeoDNS or round-robin.
	chain := make([]string, 0, len(udpTargets))
	labels := make([]string, 0, len(udpTargets))
	for _, t := range udpTargets {
		addr, label, err := resolveHop(t, port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
			os.Exit(1)
		}
		chain = append(chain, addr)
		labels = append(labels, label)
	}

	// Resolve the ICMP dest (if any) for the header line.
	var icmpLabel string
	if icmpLast {
		ip := net.ParseIP(icmpDest)
		if ip == nil {
			// Try to resolve it.
			addrs, err := net.LookupIP(icmpDest)
			if err != nil || len(addrs) == 0 {
				fmt.Fprintf(os.Stderr, "boomerang: resolve icmp target %q: %v\n", icmpDest, err)
				os.Exit(1)
			}
			ip = addrs[0]
			icmpLabel = fmt.Sprintf("%s (%s)", icmpDest, ip)
			icmpDest = ip.String()
		} else {
			icmpLabel = icmpDest
		}
	}

	// In JSON mode stdout carries the payload and nothing else, so per-probe
	// lines go to stderr where a redirect to a file leaves them behind.
	probeOut := io.Writer(os.Stdout)
	if asJSON {
		probeOut = os.Stderr
	}
	src, err := NewSource(chain, key, wait, probeOut, perHold, icmpDest, sport)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()

	// Show what each name resolved to, like ping's "PING host (ip)".
	// The source port is reported because on a per-flow path it identifies which
	// path this run measured; --sport <that port> reproduces it.
	if icmpLast {
		fmt.Fprintf(probeOut, "BOOMERANG %s via %s (icmp-last) sport=%d\n",
			icmpLabel, strings.Join(labels, ", "), src.SourcePort())
	} else {
		dest := labels[len(labels)-1]
		if len(labels) == 1 {
			fmt.Fprintf(probeOut, "BOOMERANG %s direct sport=%d\n", dest, src.SourcePort())
		} else {
			fmt.Fprintf(probeOut, "BOOMERANG %s via %s sport=%d\n",
				dest, strings.Join(labels[:len(labels)-1], ", "), src.SourcePort())
		}
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
		payload, err := src.Stats().JSON(targets, chain, src.Local(), port, src.SourcePort())
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
