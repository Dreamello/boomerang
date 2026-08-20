package main

// Tests for argument permutation: flags must work in any position.

import (
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testFlagSet mirrors the real flag shapes: bool, int, duration, string.
func testFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(new(strings.Builder)) // silence usage on error
	fs.Int("c", 0, "count")
	fs.Duration("i", time.Second, "interval")
	fs.Duration("W", 2*time.Second, "wait")
	fs.Int("sport", 0, "source port")
	fs.String("key-file", "", "key file")
	fs.Bool("json", false, "json output")
	fs.Bool("agent", false, "agent mode")
	fs.Bool("holds", false, "per-node holds")
	return fs
}

func TestPermuteArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"already flags-first is unchanged",
			[]string{"-c", "5", "relay", "target"},
			[]string{"-c", "5", "relay", "target"},
		},
		{
			"trailing flag moves ahead of hops",
			[]string{"relay", "target", "-c", "5"},
			[]string{"-c", "5", "relay", "target"},
		},
		{
			"flag interleaved between hops",
			[]string{"relay", "-c", "5", "target"},
			[]string{"-c", "5", "relay", "target"},
		},
		{
			"bool flag takes no value, hop after it is preserved",
			[]string{"relay", "-json", "target"},
			[]string{"-json", "relay", "target"},
		},
		{
			"equals form is one token",
			[]string{"relay", "-c=5", "target"},
			[]string{"-c=5", "relay", "target"},
		},
		{
			"double dash long form",
			[]string{"relay", "--sport", "43000"},
			[]string{"--sport", "43000", "relay"},
		},
		{
			"several flags in mixed positions keep their order",
			[]string{"relay", "-c", "5", "target", "-i", "0.5s", "-json"},
			[]string{"-c", "5", "-i", "0.5s", "-json", "relay", "target"},
		},
		{
			"hop order is preserved -- chain order is semantic",
			[]string{"relay1", "-c", "3", "relay2", "dest"},
			[]string{"-c", "3", "relay1", "relay2", "dest"},
		},
		{
			"-- ends flag processing",
			[]string{"-c", "5", "--", "-weird-host"},
			[]string{"-c", "5", "-weird-host"},
		},
		{
			"unknown flag is passed through without eating an operand",
			[]string{"relay", "-nope"},
			[]string{"-nope", "relay"},
		},
		{
			"no args",
			[]string{},
			[]string{},
		},
		{
			"only hops",
			[]string{"relay", "target"},
			[]string{"relay", "target"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := permuteArgs(testFlagSet(), c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("permuteArgs(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// The permutation is only useful if the FlagSet then parses it correctly and
// the operands survive as the chain.
func TestPermutedArgsParse(t *testing.T) {
	cases := []struct {
		name      string
		in        []string
		wantCount int
		wantHops  []string
		wantJSON  bool
	}{
		{"flags last", []string{"relay", "target", "-c", "7"}, 7, []string{"relay", "target"}, false},
		{"flags first", []string{"-c", "7", "relay", "target"}, 7, []string{"relay", "target"}, false},
		{"flag in middle", []string{"relay", "-c", "7", "target"}, 7, []string{"relay", "target"}, false},
		{"bool last", []string{"relay", "-json"}, 0, []string{"relay"}, true},
		{"bool between", []string{"relay", "-json", "target"}, 0, []string{"relay", "target"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := testFlagSet()
			count := fs.Lookup("c").Value.(flag.Getter)
			asJSON := fs.Lookup("json").Value.(flag.Getter)
			if err := fs.Parse(permuteArgs(fs, c.in)); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := count.Get().(int); got != c.wantCount {
				t.Errorf("-c: got %d want %d", got, c.wantCount)
			}
			if got := asJSON.Get().(bool); got != c.wantJSON {
				t.Errorf("-json: got %v want %v", got, c.wantJSON)
			}
			if got := fs.Args(); !reflect.DeepEqual(got, c.wantHops) {
				t.Errorf("hops: got %q want %q", got, c.wantHops)
			}
		})
	}
}

// A value that looks like a flag must still be consumed as that flag's value:
// a negative number, or a hostname beginning with a dash after "--".
func TestPermuteArgsFlagLikeValues(t *testing.T) {
	fs := testFlagSet()
	// -c -1 : the "-1" belongs to -c, not the operand list.
	got := permuteArgs(fs, []string{"relay", "-c", "-1"})
	want := []string{"-c", "-1", "relay"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("negative value\n got %q\nwant %q", got, want)
	}
}

// Regression guard for the original bug: a trailing flag was parsed as a
// hostname, producing a DNS error that said nothing about the real problem.
func TestTrailingFlagIsNotTreatedAsHop(t *testing.T) {
	fs := testFlagSet()
	if err := fs.Parse(permuteArgs(fs, []string{"127.0.0.1", "-c", "2"})); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "-") {
			t.Errorf("flag %q leaked into the hop list %q", a, fs.Args())
		}
	}
	if n := fs.Lookup("c").Value.(flag.Getter).Get().(int); n != 2 {
		t.Errorf("-c was not applied: got %d want 2", n)
	}
}
