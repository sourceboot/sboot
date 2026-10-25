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
// learner's tree, so neither can qualify.
//
// When it fires AND this lab authors a `labs.<stage>.stub` entry in hints.json,
// `sboot hint` serves THAT ladder instead of the check's: the learner's own
// `file:line` of the panic, then the authored l1, then l2, then the "seen every
// written hint" footer. The check's own rung is NOT climbed meanwhile, so once the
// body is written its ladder starts at the top. A lab with no stub entry is
// answered exactly as before — the classifier only CHOOSES which written hint to
// show (requirement 3); it never writes one.
//
// ── WHAT IT NEVER DOES ─────────────────────────────────────────────────────────
//   - fire on a build failure: a build that failed ran no test, so there is no
//     check evidence to read, and runHint answers a `build` run error before it
//     ever reaches a check (buildhint.go). Pinned anyway (G1171);
//   - print anything of the evidence but the location's file and line — no message,
//     no test name, and never a check's desc or needle (lab.toml is not read here);
//   - outlive the panic: its counter (state.go StubHints) is dropped by the graded
//     run whose evidence for that check no longer carries one (G1173).
package main

import (
	"fmt"
	"regexp"
	"strings"
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
}

var (
	// `thread 'x' panicked at sqlkit/src/bytes.rs:12:5:` (or, under a course row, the
	// head already cut to `panicked at …`). The path runs to the first :line:col:,
	// so a path holding spaces survives.
	stubPanicAtRe = regexp.MustCompile(`panicked at (.+?):(\d+):\d+:\s*$`)
	// The message line under it: exactly what `todo!()` and `todo!("…")` print.
	stubMessageRe = regexp.MustCompile(`^not yet implemented(?::\s.*)?$`)
	// Rust before 1.73 printed both on one line.
	stubOneLineRe = regexp.MustCompile(`panicked at 'not yet implemented(?::[^']*)?', (.+?):(\d+):\d+\s*$`)
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
			return p, true
		}
	}
	return stubPanic{}, false
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
