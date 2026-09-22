package main

// Argument permutation.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `boomerang relay.example -c 5` treats "-c" as a hostname and fails with a
// confusing DNS error. Reordering allows ordinary flags between hop arguments.
//
// permuteArgs reorders argv so flags come first, then hands the result to
// flag.Parse(). It must know which flags take a separate value, or
// `-c 5 host` would be read as flag "-c" plus operands "5" and "host".

import (
	"flag"
	"strings"
)

// permuteArgs moves flags ahead of operands, preserving the relative order of
// each group. fs supplies the flag definitions, so a flag's arity comes from
// the actual FlagSet rather than a hand-maintained list that would drift.
//
// Recognised forms:
//
//	-flag=value  / --flag=value    self-contained, one token
//	-flag value  / --flag value    two tokens, when flag is not boolean
//	-bool        / --bool          one token, when flag IS boolean
//	--                             stops this scan; the delimiter is not retained
//
// An unknown flag is passed through as a single token so flag.Parse() reports
// it with its own error message rather than this code guessing at arity.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	operands := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Stop scanning at "--". The downstream parser may still interpret
		// flag-like operands because this transformation drops the delimiter.
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}

		// Not a flag: an operand, or a bare "-".
		if len(a) < 2 || a[0] != '-' {
			operands = append(operands, a)
			continue
		}

		flags = append(flags, a)

		// -flag=value is self-contained.
		if strings.Contains(a, "=") {
			continue
		}

		name := strings.TrimLeft(a, "-")
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag: let flag.Parse() produce the error. Consuming a
			// following token here could swallow a real operand.
			continue
		}
		if isBoolFlag(f) {
			continue // takes no value
		}
		// Takes a value: pull the next token along with it.
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, operands...)
}

// isBoolFlag reports whether f is a boolean flag, which takes no separate
// value. The flag package marks these with an IsBoolFlag() method.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
