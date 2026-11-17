// sboot hint while a check's run stopped at a `todo!()` — the stub-state ladder
// (2026-10-19, sprint 3 #34; plan.md "Hint routing for build failures and `todo!()`
// stubs", requirements 2–5; ledger G1171, G1173; the TPM's Q3 ruling (a)).
//
// ── WHY ────────────────────────────────────────────────────────────────────────
// A just-wired lab has every body still `todo!()`, so every row is red and a bare
// `sboot hint` served the first row's test-failure ladder: rung 1 of a ladder that
// debugs byte order, for readers that do not exist yet (rust-for-systems 2026-10-14
// review, D-linux-3 / D-win-2). Wave 7 patched it in the course — a `not yet
// implemented` evidence rule on 52 rows and a state sentence on each lab's first
// L1 — which is the right words in the wrong place: the learner still climbed the
// byte-order ladder to reach them, and spent its rungs doing it.
//
// ── WHAT THIS DOES ─────────────────────────────────────────────────────────────
// classifyStub reads ONE thing: the target check's recorded Layer 0 (state.json
// `last_evidence`), which is already the learner's own failure block — the engine
// quotes it (grader/cmdrun.go observeCmd, ≤ 6 lines / 600 bytes) and the CLI caps
// it at 2 KB. It fires only on the shape `todo!()` prints: a `panicked at <loc>:`
// line whose very NEXT line is `not yet implemented` (or `not yet implemented: …`,
// a `todo!("…")` with a message), or the one-line form toolchains before 1.73
// printed (`panicked at 'not yet implemented', <loc>`). `<loc>` must be a relative
// `.rs` path with no `..`: the engine has already cut a COURSE crate's site down to
// a bare `panicked:` (stripCourseLocations), and an absolute site is not in the
// learner's tree, so neither can qualify. It also keeps the panic's column (never
// printed; rustc's DISPLAY column, a tab counting 4: todoAt), which is how the
// owner read below finds the `todo!` the panic names.
//
// When it fires AND this lab authors a `labs.<stage>.stub` entry in hints.json AND
// the `todo!()` is not the learner's own (below), `sboot hint` serves THAT ladder
// instead of the check's: the learner's own `file:line` of the panic, then the
// authored l1, then l2, then the "seen every written hint" footer. The check's own rung is NOT climbed meanwhile, so once the
// body is written its ladder starts at the top. A lab with no stub entry is
// answered exactly as before — the classifier only CHOOSES which written hint to
// show (requirement 3); it never writes one.
//
// ── WHOSE `todo!()` IT IS (2026-11-16, sprint 9 #132; WK1116-1, ledger G2497) ────
// A stub ladder is written about the bodies a lab SHIPS as `todo!()` ("That line is
// one of the six test bodies this lab ships"), but the panic looks the same when the
// `todo!()` is in a function the learner wrote and the test calls it — a natural
// first draft — and the ladder then told them the red was expected and to move on.
// Neither the file (rust-for-systems ships its stubs in the same module as the
// learner's code) nor the thread (it names the test either way) nor the column tells
// the two apart; the learner's own source does. So once the evidence has fired,
// stubPanic.owner opens the panic's file in the learner's tree and finds the
// innermost `fn` around the `todo!` at the panic's line and column (enclosingFn, a
// small lexer that skips comments, strings and char literals and counts braces):
//   - a `#[test]` fn, or `main` in `src/bin/*.rs` or `src/main.rs` (a binary's
//     entry body; keyed by the file, since a course row's thread is cut): the
//     shape a lab ships (shippedStubShape) — the stub ladder answers, as before;
//   - any other fn: the learner's own code — the CHECK's ladder answers, from its
//     own rung, under one harness sentence (ownStubNote) naming their file:line;
//   - no answer (the file is gone or over 1 MiB, the panic's line no longer holds
//     the `todo!` its column names because they edited since the run, or no fn
//     encloses it): the stub ladder, as before. Only a positive read moves the learner off it.
//
// G2497's census reads the starter template of EVERY course that authors a stub
// ladder (rust-for-systems ships 47 `todo!()`) and holds each `todo!()` to
// shippedStubShape. A course whose starter ships one in a library fn would have it
// called "your own code" here, and the census fails on it. It also fails a starter
// file shipping fewer than its starter overlay (lab-src/starter/) declares for that
// file, the floor read from the overlay, never typed (G2559, #154). rust-for-beginners (labs 02 to 07's
// stubs, 2026-11-17, #153 and #154) ships none: its pages have the learner TYPE each
// test's frame with a `todo!()` body, so G2556 holds every bare `todo!()` on a stub
// course's pages to the same shape, and a page that types one authors a stub ladder.
//
// ── WHAT IT NEVER DOES ─────────────────────────────────────────────────────────
//   - fire on a build failure: a build that failed ran no test, so there is no
//     check evidence to read, and runHint answers a `build` run error before it
//     ever reaches a check (buildhint.go). Pinned anyway (G1171);
//   - print anything of the evidence but the location's file and line — no message,
//     no test name, and never a check's desc or needle (lab.toml is not read here);
//     the learner's source is read only to find the enclosing fn, and nothing of it
//     is printed (not even that fn's name, which may be a lab.toml needle);
//   - answer a lab with no stub entry differently: the owner read happens only when
//     the lab authors one;
//   - outlive the panic: its counter (state.go StubHints) is dropped by the graded
//     run whose evidence for that check no longer carries one (G1173).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// stubEntry is a lab's authored stub-state ladder: hints.json `labs.<stage>.stub`.
type stubEntry struct {
	L1 string `json:"l1"`
	L2 string `json:"l2,omitempty"`
}

// stubPanic is a `todo!()` panic the classifier found in the learner's own output.
type stubPanic struct {
	// As the engine recorded it (relative to the learner's workspace), any
	// separator: "sqlkit/src/bytes.rs", or "sqlkit\src\bytes.rs" from Windows.
	File string
	Line string
	// The 1-based column the panic reports (where the `todo!` invocation starts),
	// in rustc's display width, not bytes (todoAt); "" when it cannot be read. Read by owner to find the `todo!` the panic
	// names; never printed.
	Col string
}

var (
	// `thread 'x' panicked at sqlkit/src/bytes.rs:12:5:` (or, under a course row, the
	// head already cut to `panicked at …`). The path runs to the first :line:col:,
	// so a path holding spaces survives.
	stubPanicAtRe = regexp.MustCompile(`panicked at (.+?):(\d+):(\d+):\s*$`)
	// The message line under it: exactly what `todo!()` and `todo!("…")` print.
	stubMessageRe = regexp.MustCompile(`^not yet implemented(?::\s.*)?$`)
	// Rust before 1.73 printed both on one line.
	stubOneLineRe = regexp.MustCompile(`panicked at 'not yet implemented(?::[^']*)?', (.+?):(\d+):(\d+)\s*$`)
)

// classifyStub reports the learner's `todo!()` panic in one check's recorded
// evidence, or false. Reads nothing else.
func classifyStub(evidence string) (stubPanic, bool) {
	if evidence == "" {
		return stubPanic{}, false
	}
	lines := strings.Split(strings.ReplaceAll(evidence, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if m := stubOneLineRe.FindStringSubmatch(l); m != nil {
			if p, ok := learnerStubSite(m[1], m[2]); ok {
				p.Col = m[3]
				return p, true
			}
			continue
		}
		m := stubPanicAtRe.FindStringSubmatch(l)
		if m == nil || i+1 >= len(lines) {
			continue
		}
		if !stubMessageRe.MatchString(strings.TrimSpace(lines[i+1])) {
			continue
		}
		if p, ok := learnerStubSite(m[1], m[2]); ok {
			p.Col = m[3]
			return p, true
		}
	}
	return stubPanic{}, false
}

// stubOwner is whose `todo!()` a classified panic is (see "WHOSE `todo!()` IT IS").
type stubOwner int

const (
	stubOwnerUnknown stubOwner = iota // the source could not answer: the stub ladder, as before
	stubOwnerShipped                  // a test body, or a binary's main: the stub ladder
	stubOwnerLearner                  // a function of the learner's own: the check's ladder
)

// maxStubSourceBytes bounds the one source file owner reads.
const maxStubSourceBytes = 1 << 20

// owner reads the panic's file in the learner's tree (treeDir: the directory the
// engine's relative paths start from, repo.osDir) and says whose `todo!()` it is.
func (p stubPanic) owner(treeDir string) stubOwner {
	n, err := strconv.Atoi(p.Line)
	if treeDir == "" || err != nil {
		return stubOwnerUnknown
	}
	path := filepath.Join(treeDir, filepath.FromSlash(strings.ReplaceAll(p.File, `\`, "/")))
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxStubSourceBytes {
		return stubOwnerUnknown
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return stubOwnerUnknown
	}
	col, _ := strconv.Atoi(p.Col) // 0 when the panic carried none: the line's first `todo!`
	site, ok := enclosingFn(string(b), n, col)
	if !ok {
		return stubOwnerUnknown
	}
	if shippedStubShape(site, p.File) {
		return stubOwnerShipped
	}
	return stubOwnerLearner
}

// shippedStubShape is the shape of a body a lab ships as `todo!()`: a `#[test]`
// fn, or `main` in a binary's entry file (`src/bin/*.rs`, `src/main.rs`). Keyed
// by the file rather than the panicking thread, because a course row's evidence
// has its thread cut (stripCourseTestHead). The census in G2497 holds every
// stub-ladder course's starter to it.
func shippedStubShape(site fnSite, file string) bool {
	f := "/" + strings.ReplaceAll(file, `\`, "/")
	return site.Test || site.Name == "main" && (strings.Contains(f, "/src/bin/") || strings.HasSuffix(f, "/src/main.rs"))
}

// todoPathPrefixRe is what may stand between the panic's column and `todo!`: a
// path to the macro (`std::todo!`, `::core::todo!`).
var todoPathPrefixRe = regexp.MustCompile(`^(?:::)?(?:[A-Za-z_][A-Za-z0-9_]*::)+$`)

// fnSite is the fn whose body holds a source position.
type fnSite struct {
	Name string
	Test bool // carries `#[test]` (or a `…::test` attribute, e.g. `#[tokio::test]`)
}

// enclosingFn names the innermost `fn` whose body holds the `todo!` at the 1-based
// line and column of src. col is the panic's 1-based DISPLAY column, as rustc
// prints it (a tab counts 4, a wide character 2: todoAt); col 0 means the line's
// first `todo!`. False when the line is out of range, the column opens no `todo!`
// invocation (a path such as `std::` may stand before it) while the line holds
// none or several, or no fn encloses it.
//
// Not a parser: a lexer that skips comments (nested block comments too), string,
// raw-string, byte and char literals, tells a lifetime from a char literal, reads
// attributes whole, and counts braces. A `{` opens a fn's frame when a `fn <name>`
// has been read since the last `;` (outside parentheses and brackets, so a
// `-> [u8; 4]` return type keeps it), `{` or `}`.
func enclosingFn(src string, line, col int) (fnSite, bool) {
	lines := strings.SplitAfter(src, "\n")
	if line < 1 || line > len(lines) {
		return fnSite{}, false
	}
	at := todoAt(lines[line-1], col)
	if at < 0 {
		return fnSite{}, false
	}
	end := at
	for _, l := range lines[:line-1] {
		end += len(l)
	}

	var stack []*fnSite // one entry per open brace; nil for a non-fn block
	pendingName, pendingTest, depth := "", false, 0
	for i := 0; i < end; {
		c := src[i]
		next := byte(0)
		if i+1 < len(src) {
			next = src[i+1]
		}
		switch {
		case c == '/' && next == '/':
			if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(src)
			}
			continue
		case c == '/' && next == '*':
			i += 2
			for d := 1; d > 0 && i < len(src); {
				switch {
				case strings.HasPrefix(src[i:], "/*"):
					d, i = d+1, i+2
				case strings.HasPrefix(src[i:], "*/"):
					d, i = d-1, i+2
				default:
					i++
				}
			}
			continue
		case c == '"':
			i = skipRustString(src, i+1)
			continue
		case c == '\'':
			i = skipRustQuote(src, i)
			continue
		case c == '#' && (next == '[' || next == '!' && i+2 < len(src) && src[i+2] == '['):
			open := i + 1
			if next == '!' {
				open++
			}
			j := skipRustBrackets(src, open)
			if next == '[' {
				a := strings.Join(strings.Fields(src[open+1:max(open+1, j-1)]), "")
				if a == "test" || strings.HasSuffix(a, "::test") {
					pendingTest = true
				}
			}
			i = j
			continue
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && isRustIdentByte(src[j]) {
				j++
			}
			word := src[i:j]
			switch {
			case (word == "r" || word == "br") && j < len(src) && (src[j] == '"' || src[j] == '#'):
				i = skipRustRawString(src, j)
				continue
			case word == "b" && j < len(src) && src[j] == '\'':
				i = skipRustQuote(src, j)
				continue
			case word == "fn":
				k := j
				for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r') {
					k++
				}
				m := k
				for m < len(src) && isRustIdentByte(src[m]) {
					m++
				}
				if m > k {
					pendingName = src[k:m]
					j = m
				}
			}
			i = j
			continue
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			if depth > 0 {
				depth--
			}
		case c == ';':
			if depth == 0 {
				pendingName, pendingTest = "", false
			}
		case c == '{':
			var f *fnSite
			if pendingName != "" {
				f = &fnSite{Name: pendingName, Test: pendingTest}
			}
			stack = append(stack, f)
			pendingName, pendingTest, depth = "", false, 0
		case c == '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			pendingName, pendingTest = "", false
		}
		i++
	}
	for k := len(stack) - 1; k >= 0; k-- {
		if stack[k] != nil {
			return *stack[k], true
		}
	}
	return fnSite{}, false
}

// todoAt is the byte offset in one source line of the `todo!` a panic at col
// names (col 0: the first `todo!` on the line), or -1. col is rustc's DISPLAY
// column (rustcColumnByte); when it opens no `todo!` (a character whose width
// panicColumnWidth guesses wrong) and the line holds exactly one, that one is it.
func todoAt(line string, col int) int {
	if col <= 0 {
		return strings.Index(line, "todo!")
	}
	if off := rustcColumnByte(line, col); off >= 0 {
		rest := line[off:]
		if i := strings.Index(rest, "todo!"); i == 0 || i > 0 && todoPathPrefixRe.MatchString(rest[:i]) {
			return off + i
		}
	}
	if strings.Count(line, "todo!") == 1 {
		return strings.Index(line, "todo!")
	}
	return -1
}

// rustcColumnByte is the byte offset in line of rustc's 1-based panic column col,
// or -1 when no character starts there. rustc counts DISPLAY width, not bytes or
// characters (measured with rustc 1.98.1, 2026-11-16: `\ttodo!()` panics at :5,
// `\t\ttodo!()` at :9, ` \ttodo!()` at :6 — a tab is 4 wherever it falls, not a
// tab stop; `"人人"; todo!()` and `"가"` count 2 a character, `"éé"` and a
// halfwidth `ｱ` 1, a combining U+0301 0).
func rustcColumnByte(line string, col int) int {
	at := 1
	for off, r := range line {
		if at == col {
			return off
		}
		if at > col {
			return -1
		}
		at += panicColumnWidth(r)
	}
	return -1
}

// panicColumnWidth is the width rustc gives one character in a panic's column:
// a tab 4; a combining mark or format character 0; an East Asian wide or
// fullwidth character (and the wide emoji blocks) 2; anything else 1. A short
// table of the wide ranges, written here because harness/go.mod has no requires;
// a character it misjudges costs only the column, and todoAt's one-`todo!` rule
// still finds the macro on any line with a single one.
func panicColumnWidth(r rune) int {
	switch {
	case r == '\t':
		return 4
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	}
	for _, w := range wideRanges {
		if r < w[0] {
			return 1
		}
		if r <= w[1] {
			return 2
		}
	}
	return 1
}

// wideRanges are the East Asian Wide and Fullwidth blocks, sorted.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0},
	{0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F},
	{0x2693, 0x2693}, {0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5},
	{0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5},
	{0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728},
	{0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF},
	{0xA960, 0xA97F}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F},
	{0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x16FE0, 0x16FE4}, {0x17000, 0x18CFF}, {0x1B000, 0x1B2FF},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F200, 0x1F251},
	{0x1F300, 0x1F320}, {0x1F32D, 0x1F335}, {0x1F337, 0x1F37C}, {0x1F37E, 0x1F393}, {0x1F3A0, 0x1F3CA},
	{0x1F3CF, 0x1F3D3}, {0x1F3E0, 0x1F3F0}, {0x1F3F4, 0x1F3F4}, {0x1F3F8, 0x1F43E}, {0x1F440, 0x1F440},
	{0x1F442, 0x1F4FC}, {0x1F4FF, 0x1F53D}, {0x1F54B, 0x1F54E}, {0x1F550, 0x1F567}, {0x1F57A, 0x1F57A},
	{0x1F595, 0x1F596}, {0x1F5A4, 0x1F5A4}, {0x1F5FB, 0x1F64F}, {0x1F680, 0x1F6C5}, {0x1F6CC, 0x1F6CC},
	{0x1F6D0, 0x1F6D2}, {0x1F6D5, 0x1F6D7}, {0x1F6DC, 0x1F6DF}, {0x1F6EB, 0x1F6EC}, {0x1F6F4, 0x1F6FC},
	{0x1F7E0, 0x1F7EB}, {0x1F7F0, 0x1F7F0}, {0x1F90C, 0x1F93A}, {0x1F93C, 0x1F945}, {0x1F947, 0x1F9FF},
	{0x1FA70, 0x1FAFF}, {0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

func isRustIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// skipRustString returns the index after the `"` closing a string whose body
// starts at i (escapes honoured).
func skipRustString(src string, i int) int {
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return len(src)
}

// skipRustRawString skips `#…#"…"#…#` starting at the first `#` or `"` after r/br.
func skipRustRawString(src string, i int) int {
	hashes := 0
	for i < len(src) && src[i] == '#' {
		hashes++
		i++
	}
	if i >= len(src) || src[i] != '"' {
		return i // `r#ident`, a raw identifier: nothing to skip
	}
	closing := "\"" + strings.Repeat("#", hashes)
	if j := strings.Index(src[i+1:], closing); j >= 0 {
		return i + 1 + j + len(closing)
	}
	return len(src)
}

// skipRustQuote skips a char literal at i (`'x'`, `'\n'`, `'\u{1F600}'`, `'é'`), or
// just the `'` of a lifetime or label (`'a`, `'outer:`).
func skipRustQuote(src string, i int) int {
	if i+1 >= len(src) {
		return len(src)
	}
	if src[i+1] == '\\' {
		// The escaped character itself may be a `'` (`'\''`), so the close is
		// looked for after it.
		if i+3 <= len(src) {
			if j := strings.IndexByte(src[i+3:], '\''); j >= 0 && j <= 10 {
				return i + 3 + j + 1
			}
		}
		return i + 1
	}
	_, size := utf8.DecodeRuneInString(src[i+1:])
	if i+1+size < len(src) && src[i+1+size] == '\'' {
		return i + 2 + size
	}
	return i + 1
}

// skipRustBrackets returns the index after the `]` matching the `[` at open,
// skipping strings inside an attribute (`#[doc = "]"]`).
func skipRustBrackets(src string, open int) int {
	d := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '"':
			i = skipRustString(src, i+1) - 1
		case '[':
			d++
		case ']':
			d--
			if d == 0 {
				return i + 1
			}
		}
	}
	return len(src)
}

// learnerStubSite accepts a panic location only when it can be the learner's own
// source file: relative, a `.rs` file, and never climbing out with `..`.
func learnerStubSite(file, line string) (stubPanic, bool) {
	file = strings.TrimSpace(file)
	if file == "" || isAbsPathAnyOS(file) || !strings.HasSuffix(strings.ToLower(file), ".rs") {
		return stubPanic{}, false
	}
	for _, seg := range strings.FieldsFunc(file, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return stubPanic{}, false
		}
	}
	return stubPanic{File: file, Line: line}, true
}

// isAbsPathAnyOS: an absolute path in either OS's spelling (the recorded evidence
// may have been produced on Windows and read on Unix, or the reverse — the
// state file never moves between machines, but the test fixtures do).
func isAbsPathAnyOS(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// sig keys the stub ladder: the FILE, never the line — writing one body moves
// the next `todo!()` in the same file up or down without it being a new state.
// Separators are normalised so the same file keys the same on every host.
func (p stubPanic) sig() string {
	return strings.ReplaceAll(p.File, `\`, "/")
}

// where is the learner's own file:line, in their tree's spelling.
func (p stubPanic) where(run, tree string) string {
	return retreeText(p.File, run, tree) + ":" + p.Line
}

// stubTiers is the authored stub ladder: l1, then l2 when authored.
func stubTiers(e stubEntry) []string {
	t := []string{e.L1}
	if e.L2 != "" {
		t = append(t, e.L2)
	}
	return t
}

// labStub is this lab's authored stub ladder, or nil when it authors none (the
// check's own ladder then answers, exactly as before).
func (hf hintsFile) labStub(stage string) *stubEntry {
	if e := hf.Labs[stage].Stub; e != nil && strings.TrimSpace(e.L1) != "" {
		return e
	}
	return nil
}

// bumpStubHint climbs one step of a check's stub ladder for the `todo!()` in this
// file and returns the 1-based ask just earned. A stub in a DIFFERENT file restarts
// at the top.
func (s *guidanceState) bumpStubHint(course, stage, checkID, sig string) int {
	if s.StubHints == nil {
		s.StubHints = map[string]*buildHintMark{}
	}
	k := stateKey(course, stage, checkID)
	m := s.StubHints[k]
	if m == nil || m.Sig != sig {
		m = &buildHintMark{Sig: sig}
		s.StubHints[k] = m
	}
	m.Shown++
	s.dirty = true
	return m.Shown
}

// clearStubHint drops one check's stub-ladder position: the panic it was about is
// gone (the check passed, or its run no longer stops at a `todo!()`).
func (s *guidanceState) clearStubHint(key string) {
	if _, had := s.StubHints[key]; !had {
		return
	}
	delete(s.StubHints, key)
	s.dirty = true
}

// ownStubNote is the harness's one sentence over the check's own ladder when the
// `todo!()` is the learner's own (G2497): their file:line, and nothing else of the
// evidence or of their source.
func ownStubNote(stage, where string) string {
	return "Your last `sboot test " + stage + "` stopped at a `todo!()` in your own code, not in this check's test:\n" +
		"`not yet implemented` at " + where + ", in a function the test calls, so this check has nothing to grade\n" +
		"until you write that body."
}

// withOwnStubNote puts note under a rendered hint's first line (its `[hint …]` or
// `[<check>]` header).
func withOwnStubNote(text, note string) string {
	head, rest, _ := strings.Cut(text, "\n")
	return head + "\n\n" + note + "\n\n" + strings.TrimLeft(rest, "\n")
}

// renderStubHint renders the ask-th step (1-based) of a check's stub ladder.
func renderStubHint(stage, checkID, where string, e stubEntry, ask int, stuckURL string) string {
	tiers := stubTiers(e)
	n := len(tiers)
	if ask > n {
		return fmt.Sprintf(
			"[%s · stub] You've seen every written hint for a body that is still `todo!()`.\n"+
				"── Your last run of this check stopped at `not yet implemented` in %s: write that body,\n"+
				"   then `sboot test %s` grades the check and `sboot hint %s %s` picks up its own hints.\n"+
				"── `sboot explain %s` takes it to the AI tutor (or the same chat on this lab's page: %s).\n"+
				"── Still stuck? `sboot reveal %s` shows the course's own solution for this lab "+
				"and marks it solution-assisted — it never blocks completing it.",
			checkID, where, stage, stage, checkID, checkID, stuckURL, stage)
	}
	out := []string{fmt.Sprintf("[hint %d of %d · stub · %s]", ask, n, checkID), ""}
	if ask == 1 {
		out = append(out,
			"Your last `sboot test "+stage+"` ran this check and it stopped at a `todo!()`:",
			"`not yet implemented` at "+where+".",
			"")
	} else {
		out = append(out, "Still a `todo!()` at "+where+".", "")
	}
	out = append(out, strings.TrimRight(tiers[ask-1], " \t\r\n"), "")
	if ask < n {
		out = append(out, fmt.Sprintf("run it again for the next one (%d of %d).", ask+1, n))
	} else {
		out = append(out,
			"that's the last written hint while that body is unwritten. once it is, `sboot test "+stage+"`",
			"and `sboot hint "+stage+" "+checkID+"` start this check's own hints from the top.",
			"more help — `sboot explain "+checkID+"` (the AI tutor; `--here` answers in this terminal),",
			"or the same chat on this lab's page:",
			"  "+stuckURL)
	}
	return strings.Join(out, "\n")
}
