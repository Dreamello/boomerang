package main

// boomerang measures end-to-end and per-leg RTT through a chain of relays from
// a single probe.
//
//	boomerang 192.0.2.101 198.51.100.20    source: last address is the destination
//	boomerang 198.51.100.20                direct probe, no relays
//	boomerang --agent                      relay or destination daemon
//
// The pre-shared key is read from a file, never from the command line, so it
// cannot leak through ps output or shell history.

import (
	"flag"
	"fmt"
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
	runSource(targets, *port, key, *interval, *count, *wait)
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

func runSource(targets []string, port int, key []byte, interval time.Duration, count int, wait time.Duration) {
	chain := make([]string, 0, len(targets))
	for _, t := range targets {
		chain = append(chain, withPort(t, port))
	}

	src, err := NewSource(chain, key, wait, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boomerang: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()

	dest := chain[len(chain)-1]
	if len(chain) == 1 {
		fmt.Printf("BOOMERANG %s direct\n", dest)
	} else {
		fmt.Printf("BOOMERANG %s via %s\n", dest, strings.Join(chain[:len(chain)-1], ", "))
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	src.Run(count, interval, stop)
	fmt.Print(src.Stats().Summary(chain))

	// Exit non-zero when nothing came back, matching ping's convention so this
	// is usable in a shell conditional.
	if src.Stats().recv == 0 {
		os.Exit(1)
	}
}

// withPort appends the default port unless the target already carries one.
func withPort(target string, port int) string {
	if _, _, err := net.SplitHostPort(target); err == nil {
		return target
	}
	return net.JoinHostPort(target, fmt.Sprint(port))
}
