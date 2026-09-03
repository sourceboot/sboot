// The build's own output, bounded and kept — the evidence channel a failed BUILD
// never had (rust-for-beginners review round, 2026-09-02; ledger G140).
//
// ── WHY THIS EXISTS ────────────────────────────────────────────────────────────
// Until now the build streamed straight to the terminal and nowhere else, so a run
// that died before a single check ran left the CLI knowing only THAT it failed. On
// three fresh machines that was the first thing a learner met — `linker cc not
// found` (Linux), `link.exe not found` (Windows), `No developer tools were found`
// (macOS) — and `sboot hint` could only hand them back to "the compiler's own
// output", which in that case is the operating system's error and not theirs.
// Answering it needs one thing: the text. So the build is teed.
//
// ── WHAT IS KEPT, AND WHY IT IS BOUNDED AT BOTH ENDS ───────────────────────────
// The head, because the first error is the one to fix and cargo puts it near the
// top; the tail, because a large course compiles dozens of crates before it fails
// and the head would be forty `Compiling …` lines. Everything between is dropped
// with a marker. Bytes are capped as well as lines: this text lands in state.json,
// which must stay small enough to rewrite on every run.
//
// It is stored RAW (minus ANSI colour, which is ours — see stripANSI), the same
// rule retree.go states for every non-display channel: matchers are written
// against what actually ran, never against what the terminal was shown.
package main

import (
	"bytes"
	"strings"
	"sync"
)

const (
	// ~40 lines of head is what the review round asked for; the tail is half that.
	buildLogHeadLines = 40
	buildLogTailLines = 20
	// The byte caps bound one pathological line (a linker printing a 200 KB
	// command line) rather than the line count.
	buildLogHeadBytes = 6 << 10
	buildLogTailBytes = 3 << 10
	// What a dropped middle looks like in the stored text.
	buildLogElision = "\n… [output trimmed] …\n"
)

// headTailCapture is an io.Writer that keeps the first N lines and the last M
// lines of what passes through it and forgets the rest. It never blocks, never
// grows, and never affects what the terminal sees — it is always the second half
// of an io.MultiWriter, so a failure to record cannot fail a build.
//
// The mutex is belt-and-braces: this binary has no goroutines today, but exec
// wires the child's stdout and stderr to two writers and a future change that
// splits them would otherwise be a data race nobody could reproduce.
type headTailCapture struct {
	mu   sync.Mutex
	head []byte
	// How many lines the head actually took. The head fills by LINES or by
	// BYTES, whichever first, and the elision marker has to be computed against
	// what it took rather than against the line cap — a build whose first lines
	// are long fills the head at line 3 and then drops middle lines like any
	// other, and comparing against 40 would hide that drop.
	headLines int
	tail      []string
	lines     int
	buf       []byte // the current partial line
}

func newHeadTailCapture() *headTailCapture { return &headTailCapture{} }

func (c *headTailCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		c.addLine(string(c.buf[:i+1]))
		c.buf = c.buf[i+1:]
	}
	// A producer writing kilobytes with no newline (a progress bar redrawing
	// itself) must not grow the partial buffer without bound.
	if len(c.buf) > buildLogHeadBytes {
		c.addLine(string(c.buf))
		c.buf = c.buf[:0]
	}
	return len(p), nil
}

func (c *headTailCapture) addLine(l string) {
	c.lines++
	if c.lines <= buildLogHeadLines && len(c.head) < buildLogHeadBytes {
		c.head = append(c.head, l...)
		c.headLines++
		return
	}
	c.tail = append(c.tail, l)
	if len(c.tail) > buildLogTailLines {
		c.tail = c.tail[1:]
	}
}

// text is the recorded output: head, an elision marker when anything was dropped,
// then the tail. ANSI colour is stripped — we may have asked the build for colour
// (grade.go) precisely because the terminal is a terminal, and an escape sequence
// in the middle of `error: linker \`cc\` not found` is how a signature match
// silently stops firing.
func (c *headTailCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) > 0 {
		c.addLine(string(c.buf))
		c.buf = nil
	}
	out := string(c.head)
	if len(c.tail) > 0 {
		if c.lines > c.headLines+len(c.tail) {
			out += buildLogElision
		}
		tail := strings.Join(c.tail, "")
		if len(tail) > buildLogTailBytes {
			tail = tail[len(tail)-buildLogTailBytes:]
		}
		out += tail
	}
	return out
}

// stripANSI removes CSI/OSC escape sequences. Deliberately conservative: it drops
// only what it recognises as a sequence and leaves every other byte alone, so a
// build that prints no colour comes through byte-identical.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1b || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch s[i+1] {
		case '[': // CSI … final byte in @-~
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j + 1
		case ']': // OSC … BEL or ST
			j := i + 2
			for j < len(s) && s[j] != 0x07 {
				if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j + 1
		default:
			i += 2 // a two-byte escape (ESC c, ESC =) — dropped whole
		}
	}
	return b.String()
}
