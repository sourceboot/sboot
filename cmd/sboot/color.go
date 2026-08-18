// Terminal color, rationed (ux-plan §12.2 rule 4).
//
// The rules, in the order they veto: `--no-color` (any command) · the NO_COLOR
// convention (any non-empty value) · TERM=dumb · and the stream itself must be a
// terminal — piped output is always clean, so `sboot | grep` never sees an escape
// byte. Semantics are fixed: green marks a pass/verified, red marks a FAILURE and
// nothing else, amber is the attention accent and is rationed to the few moments
// the design names (the ▸ current lab, the grading header). Everything else is
// plain or dim.
package main

import (
	"os"
	"strings"
)

// noColorFlag is set by `--no-color` during argument parsing, before any output.
var noColorFlag bool

// colorEnabled answers for ONE stream, because stdout piped into a file with
// stderr still on the terminal is an everyday shape (`sboot > log`), and each
// side must decide for itself.
func colorEnabled(f *os.File) bool {
	if noColorFlag || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(f)
}

// isTerminal — no termios, no dependency: a character device is a terminal for
// every purpose this binary has (deciding whether to color, spin, or prompt).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// interactiveTTY is the PROMPT gate (§12.2 rule 7): a human is plausibly present
// only when stdin AND stderr are both terminals. The stricter conjunction exists
// because /dev/null is itself a character device, so stdin alone cannot tell
// `sboot < /dev/null` in a script from a person — but such a script has stderr
// on a pipe or file, which is what this catches. The residual case (stdin from
// /dev/null on a live terminal) degrades safely: the prompt's read sees EOF and
// declines.
func interactiveTTY() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stderr)
}

const (
	ansiReset = "\x1b[0m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiAmber = "\x1b[33m"
	ansiDim   = "\x1b[2m"
)

// painter returns a paint function bound to one stream's color decision, so a
// renderer can be a pure string builder and still honor the stream it will be
// written to.
func painter(f *os.File) func(code, s string) string {
	if !colorEnabled(f) {
		return func(_, s string) string { return s }
	}
	return func(code, s string) string {
		if s == "" {
			return s
		}
		return code + s + ansiReset
	}
}

// stripFlag removes every occurrence of `flag` from args, stopping at a literal
// `--` (everything after it is positional by POSIX rule 9). Returns the remaining
// args and whether the flag was present.
func stripFlag(args []string, flag string) ([]string, bool) {
	out := args[:0:0]
	found := false
	terminated := false
	for _, a := range args {
		if !terminated && a == "--" {
			terminated = true
		}
		if !terminated && a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// hasFlagBefore reports whether `flag` appears before any `--` terminator.
func hasFlagBefore(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == flag {
			return true
		}
	}
	return false
}

// padTo right-pads s with spaces to width (in runes, since stage slugs and
// titles are what land in these columns).
func padTo(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
