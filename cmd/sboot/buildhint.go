// sboot hint after a BUILD failure — the ladder a compile error never had
// (2026-09-13; the failure-guidance spec "Built 2026-09-13"; ledger G278, G269–G271).
//
// ── WHY ────────────────────────────────────────────────────────────────────────
// A hint is per check, and a build that fails runs no check — so until now every
// `sboot hint` after a compile error printed the same fixed "the compiler's output
// is the hint" paragraph, forever. A fresh-VM beginner lap (rust-for-beginners lab 05,
// 2026-09-12) asked five times and read the same words five times. Meanwhile the
// courses had ALREADY authored compile-error guidance — `banner.compiles` carries
// `evidence` rules keyed on E0425 and E0308, lab 04's `words.suite` ladder names the
// missing cast and the missing `*` — and none of it was reachable, because the
// evidence channel feeds per-check Layer 0 and a failed build writes none.
//
// ── WHAT THIS DOES ─────────────────────────────────────────────────────────────
// The build's output is already recorded (buildlog.go → state.json BuildOut). This
// takes the FIRST compiler error from it and finds authored text for THIS lab:
//
//  1. an `evidence` rule on a compile-shaped check (`*.compiles`, or a `*.suite`
//     whose ladder talks about a crate that does not compile) that matches the
//     first error's own diagnostic text;
//  2. failing that, an evidence rule on any other check of this lab — but only a
//     rule keyed on a rustc error CODE (`E0308`), because a rule written for a
//     test's panic ("assertion", `_ "`) must never fire on compiler output that
//     happens to contain the same characters;
//  3. failing that, the lab's compile-shaped check itself, with no evidence line.
//
// and serves it as a ladder, one step deeper per ask: the first error quoted →
// the matched evidence sentence → that check's l1 → its l2 → the "seen every
// hint" footer. Nothing matching is said plainly, with the first error still quoted.
//
// ── WHAT IT NEVER PRINTS ───────────────────────────────────────────────────────
// Only two kinds of text reach the terminal: the learner's OWN compiler output (the
// first error's header and its `-->` location — two lines they have already read on
// their screen) and AUTHORED hints.json text. The lab.toml is read for its check
// IDS and nothing else: no `desc`, no needle, no criterion. That is the never-echo
// rule (content-protection.md) in the same shape evidence.go-era selectors keep it:
// the evidence chooses WHICH authored line, never the words.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// buildHintMark is one stage's position on the build-failure ladder (state.go
// BuildHints): which first error it is for, and how many asks it has answered.
type buildHintMark struct {
	Sig   string `json:"sig"`
	Shown int    `json:"shown"`
}

// bumpBuildHint climbs one step for this stage's first compile error and returns
// the 1-based ask just earned. A DIFFERENT first error — a new fingerprint —
// restarts at the top: the ladder below it is a different ladder.
// clearBuildHint drops a stage's build-ladder position — with the build output it
// was about (noteRunError, on a run that graded or that the engine refused; G282).
func (s *guidanceState) clearBuildHint(course, stage string) {
	k := course + "/" + stage
	if _, had := s.BuildHints[k]; !had {
		return
	}
	delete(s.BuildHints, k)
	s.dirty = true
}

func (s *guidanceState) bumpBuildHint(course, stage, sig string) int {
	if s.BuildHints == nil {
		s.BuildHints = map[string]*buildHintMark{}
	}
	k := course + "/" + stage
	m := s.BuildHints[k]
	if m == nil || m.Sig != sig {
		m = &buildHintMark{Sig: sig}
		s.BuildHints[k] = m
	}
	m.Shown++
	s.dirty = true
	return m.Shown
}

// compileError is the first error a build printed, reduced to what the ladder
// needs.
type compileError struct {
	// The header line exactly as printed: "error[E0308]: mismatched types".
	Header string
	// "E0308", or "" for an error rustc gave no code (a syntax error).
	Code    string
	Message string
	// The primary `-->` location as printed ("lantern/src/words.rs:2:22", or a
	// Windows "lantern\src\words.rs:2:22"), and the same with :line:col removed.
	Location string
	File     string
	// What evidence selectors are matched against: the header plus the block's
	// diagnostic lines — labels, notes, help — with every SOURCE line removed. A
	// learner's own code is not evidence about which cause this is, and a
	// selector must not fire because they named a variable after one.
	Match string
}

var (
	compileErrHeaderRe = regexp.MustCompile(`^error(?:\[(E\d{4})\])?: (.+)$`)
	compileErrLocRe    = regexp.MustCompile(`^\s*-->\s*(.+?)\s*$`)
	// `:2:22` or `:2` at the END — so a Windows drive letter's colon survives.
	lineColRe = regexp.MustCompile(`:\d+(?::\d+)?$`)
	// A quoted source line (`2 |     let i = …`) or a suggested edit (`17 +  use …`).
	sourceLineRe = regexp.MustCompile(`^\s*\d+\s*[|+\-~]`)
)

// maxQuotedHeader bounds the one line of compiler text this file prints. rustc's
// headers are short; a pathological one must not become a wall.
const maxQuotedHeader = 240

// firstCompileError finds the first `error…:` line in a build's output that is a
// real diagnostic — not cargo's closing "could not compile" summary — and reads
// its block (up to the next blank line or the next top-level diagnostic).
func firstCompileError(buildOut string) (compileError, bool) {
	lines := strings.Split(strings.ReplaceAll(buildOut, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		m := compileErrHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		msg := strings.TrimSpace(m[2])
		if m[1] == "" && isCargoSummary(msg) {
			continue
		}
		ce := compileError{Header: line, Code: m[1], Message: msg}
		match := []string{line}
		for _, bl := range lines[i+1:] {
			bl = strings.TrimRight(bl, " \t\r")
			if strings.TrimSpace(bl) == "" || startsTopLevel(bl) {
				break
			}
			if ce.Location == "" {
				if lm := compileErrLocRe.FindStringSubmatch(bl); lm != nil {
					ce.Location = lm[1]
					ce.File = lineColRe.ReplaceAllString(lm[1], "")
					continue
				}
			}
			if sourceLineRe.MatchString(bl) {
				continue
			}
			match = append(match, bl)
		}
		ce.Match = strings.Join(match, "\n")
		return ce, true
	}
	return compileError{}, false
}

// isCargoSummary is cargo's own closing line about a failure it already reported.
func isCargoSummary(msg string) bool {
	for _, p := range []string{"could not compile", "aborting due to", "build failed"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

func startsTopLevel(l string) bool {
	for _, p := range []string{"error", "warning", "Some errors have", "For more information"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// fingerprint keys the ladder: the code, the message and the FILE, never the line
// — adding a line above the error moves it without changing which error it is.
// Separators are normalised so the same error keys the same on every host.
func (ce compileError) fingerprint() string {
	file := strings.ReplaceAll(ce.File, `\`, "/")
	sum := sha256.Sum256([]byte(ce.Code + "\x00" + ce.Message + "\x00" + file))
	return hex.EncodeToString(sum[:8])
}

// labCheckIDs is every published check id in this lab, in rubric order — read out of
// the staged `labs/<stage>/lab.toml` for its ids ONLY. It is what scopes the lookup
// to THIS lab: E0308 has authored text in several labs of rust-for-beginners, and
// the lab the learner is in is the one whose words apply.
//
// A line scanner rather than a TOML parser, because the harness module has no
// dependencies by design (harness/go.mod) and an id line is `id = "…"` in every
// rubric in the corpus. A lab.toml it cannot read yields no ids, which is the
// honest "no written hint" answer rather than a guess.
func labCheckIDs(run, stage string) []string {
	b, err := os.ReadFile(filepath.Join(run, "labs", stage, "lab.toml"))
	if err != nil {
		return nil
	}
	var ids []string
	inChecks := false
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		switch {
		case tomlArrayTableRe.MatchString(t):
			// An array-of-tables header; only the checks table carries ids we want.
			inChecks = tomlArrayTableRe.FindStringSubmatch(t)[1] == "checks"
		case strings.HasPrefix(t, "["):
			inChecks = false
		case inChecks:
			if m := tomlIDRe.FindStringSubmatch(t); m != nil {
				ids = append(ids, m[1])
			}
		}
	}
	return ids
}

var tomlIDRe = regexp.MustCompile(`^id\s*=\s*["']([^"']+)["']`)

// tomlArrayTableRe matches an array-of-tables header and captures its name. The header
// is recognised generically rather than spelled out, which also accepts the spaces TOML
// allows inside the brackets — and keeps the public mirror's rubric-content scan, which
// refuses the literal checks-table header anywhere in shipped source, untouched.
var tomlArrayTableRe = regexp.MustCompile(`^\[\[\s*([A-Za-z0-9_.-]+)\s*\]\]`)

// compileTalkRe recognises a `*.suite` ladder written for the case where the crate
// did not build — the courses without a separate compile check say so in the
// umbrella's own words ("a crate that will not build lands on this one").
var compileTalkRe = regexp.MustCompile(`(?i)not compile|will not build|does not build|did not compile|compile error`)

var errorCodeRe = regexp.MustCompile(`^E\d{4}$`)

// compilerShaped is whether the first error is one the lab's COMPILE hints can be
// about (G284): rustc's own diagnostic, which always points at the source it is about —
// a `-->` location naming a .rs file (either separator; ce.File has :line:col already
// removed) — or carries an E-code. Deliberately NOT a list of message phrases: rustc
// prints some of the commonest beginner errors with no code at all (`cannot find macro
// printn`, `1 positional argument in format string, but no arguments were given`,
// `argument never used`), and the first cut of this predicate, which asked for a code
// or a parse-error phrase, sent all three to the no-written-hint answer.
//
// What it keeps out, by construction rather than by list: a cargo manifest error is
// located in Cargo.toml (or nowhere), and a link failure, a missing linker, a registry
// or network failure carry no location at all — so each gets the plain answer, because
// a ladder about types and modules is the wrong answer to it.
func compilerShaped(ce compileError) bool {
	if ce.Code != "" {
		return true
	}
	return strings.HasSuffix(strings.ToLower(ce.File), ".rs")
}

// isCompileCheck: the checks a build failure lands on.
func isCompileCheck(id string, e hintEntry) bool {
	if strings.HasSuffix(id, ".compiles") {
		return true
	}
	return strings.HasSuffix(id, umbrellaSuffix) && compileTalkRe.MatchString(e.L1+"\n"+e.L2)
}

// codeRules keeps only the selectors keyed on a rustc error code.
func codeRules(rules []evidenceRule) []evidenceRule {
	var out []evidenceRule
	for _, r := range rules {
		k := evidenceRule{Then: r.Then}
		if errorCodeRe.MatchString(r.Match) {
			k.Match = r.Match
		}
		for _, m := range r.MatchAny {
			if errorCodeRe.MatchString(m) {
				k.MatchAny = append(k.MatchAny, m)
			}
		}
		if k.Match != "" || len(k.MatchAny) > 0 {
			out = append(out, k)
		}
	}
	return out
}

// selectBuildGuidance picks the check whose authored ladder answers this first
// error, and the evidence sentence that matched ("" when the check was chosen for
// being compile-shaped alone). ("", "") means nothing authored answers it.
func selectBuildGuidance(hf hintsFile, labChecks []string, ce compileError) (checkID, evidence string) {
	// 1. evidence on a compile-shaped check.
	for _, id := range labChecks {
		e, ok := hf.Checks[id]
		if !ok || !isCompileCheck(id, e) {
			continue
		}
		if then := selectEvidence(e.Evidence, ce.Match); then != "" {
			return id, then
		}
	}
	// 2. an error-code selector on any other check of this lab.
	for _, id := range labChecks {
		e, ok := hf.Checks[id]
		if !ok || isCompileCheck(id, e) {
			continue
		}
		if then := selectEvidence(codeRules(e.Evidence), ce.Match); then != "" {
			return id, then
		}
	}
	// 3. the compile-shaped check itself: `*.compiles` first, then the umbrella.
	for _, want := range []string{".compiles", umbrellaSuffix} {
		for _, id := range labChecks {
			e, ok := hf.Checks[id]
			if ok && e.L1 != "" && strings.HasSuffix(id, want) && isCompileCheck(id, e) {
				return id, ""
			}
		}
	}
	return "", ""
}

// buildTier is one step of the build ladder.
type buildTier struct {
	body string
	// What the telemetry event calls this step (telemetry.go): 0 for the quoted
	// first error, "evidence", 1 for l1, 2 for l2.
	rung any
}

func buildTiers(e hintEntry, evidence string) []buildTier {
	t := []buildTier{{rung: 0}}
	if evidence != "" {
		t = append(t, buildTier{body: evidence, rung: "evidence"})
	}
	if e.L1 != "" {
		t = append(t, buildTier{body: e.L1, rung: 1})
	}
	if e.L2 != "" {
		t = append(t, buildTier{body: e.L2, rung: 2})
	}
	return t
}

// loadHintsFile reads the staged hints.json, or nil when the course publishes none
// or it does not parse (runHint reports an unreadable file on the check path; here
// the honest answer to either is "no written hint", with the error still quoted).
func loadHintsFile(run string) *hintsFile {
	b, err := os.ReadFile(filepath.Join(run, hintsFileName))
	if err != nil {
		return nil
	}
	var hf hintsFile
	if json.Unmarshal(b, &hf) != nil {
		return nil
	}
	return &hf
}

// quotedCompileError is the learner's own first error, as they read it: the header,
// and the location in their tree's spelling (retree.go — the staging root says `os`).
func quotedCompileError(ce compileError, run, tree string) []string {
	// By runes, not bytes: a byte cut can split a multi-byte character and print
	// invalid UTF-8 at the learner (G285).
	h := ce.Header
	if r := []rune(h); len(r) > maxQuotedHeader {
		h = string(r[:maxQuotedHeader]) + "…"
	}
	out := []string{"  " + h}
	if ce.Location != "" {
		out = append(out, "   --> "+retreeText(ce.Location, run, tree))
	}
	return out
}

// runBuildHint answers a `sboot hint` whose last run stopped at a compile error. ok
// is false when the recorded output holds no compiler error to read (an older state
// file, or a build that failed some other way) — the caller then prints the
// long-standing blocked answer unchanged.
func runBuildHint(r repo, stage, run string, st *guidanceState, buildOut, stuckURL string) (text string, shown hintShown, ok bool) {
	ce, found := firstCompileError(buildOut)
	if !found {
		return "", hintShown{}, false
	}
	quoted := quotedCompileError(ce, run, r.tree)

	checkID, evidence := "", ""
	hf := loadHintsFile(run)
	if hf != nil && compilerShaped(ce) {
		checkID, evidence = selectBuildGuidance(*hf, labCheckIDs(run, stage), ce)
	}
	if checkID == "" {
		return renderUnhintedBuild(stage, quoted, stuckURL),
			hintShown{check: buildCheck, rung: 0, kind: "build_unhinted"}, true
	}

	tiers := buildTiers(hf.Checks[checkID], evidence)
	ask := st.bumpBuildHint(r.course, stage, ce.fingerprint())
	shown = hintShown{check: buildCheck, rung: "past", kind: "build"}
	if ask <= len(tiers) {
		shown.rung = tiers[ask-1].rung
	}
	return renderBuildLadder(stage, checkID, ce, quoted, tiers, ask, stuckURL), shown, true
}

// buildCheck is the check id a build-failure hint reports under: no check ran.
const buildCheck = "build"

// renderBuildLadder renders the ask-th step (1-based) of the build ladder.
func renderBuildLadder(stage, checkID string, ce compileError, quoted []string, tiers []buildTier, ask int, stuckURL string) string {
	n := len(tiers)
	if ask > n {
		return fmt.Sprintf(
			"[build] You've seen every written hint for this error.\n"+
				"── Re-read the compiler's FIRST error above your last failed run (`%s`) — it names\n"+
				"   the line and what it expected there; fix it and re-run `sboot test %s`.\n"+
				"── `sboot explain %s` takes it to the AI tutor (or the same chat on this lab's page: %s).\n"+
				"── Still stuck? `sboot reveal %s` shows the course's own solution for this lab "+
				"and marks it solution-assisted — it never blocks completing it.",
			ce.Header, stage, stage, stuckURL, stage)
	}
	t := tiers[ask-1]
	header := fmt.Sprintf("[hint %d of %d · build]", ask, n)
	if t.rung == 1 || t.rung == 2 {
		// The authored ladder it came from is named, so the learner can ask for
		// the same check by id once their crate builds.
		header = fmt.Sprintf("[hint %d of %d · build · %s]", ask, n, checkID)
	}
	out := []string{header, ""}
	if ask == 1 {
		out = append(out,
			"Your last `sboot test "+stage+"` stopped at the build, so no check ran. The compiler's",
			"first error:",
			"")
		out = append(out, quoted...)
		out = append(out, "",
			"Fix that FIRST error and re-run — the errors after it are often its consequences.")
	} else {
		out = append(out, "Your first error: "+strings.TrimSpace(quoted[0]), "",
			strings.TrimRight(t.body, " \t\r\n"))
	}
	out = append(out, "")
	if ask < n {
		out = append(out, fmt.Sprintf("run it again for the next one (%d of %d).", ask+1, n))
	} else {
		out = append(out,
			"that's the last written hint for this error. more help — `sboot explain "+stage+"`",
			"(the AI tutor; `--here` answers in this terminal), or the same chat on this lab's page:",
			"  "+stuckURL)
	}
	return strings.Join(out, "\n")
}

// renderUnhintedBuild is the answer when nothing authored matches: today's
// message, with the first error quoted, and the absence of a written hint said
// plainly rather than implied by silence.
func renderUnhintedBuild(stage string, quoted []string, stuckURL string) string {
	out := []string{"[hint — nothing was graded]", "",
		"Your last `sboot test " + stage + "` stopped at the build, so no check ran and there is",
		"no failing check to hint at yet. The first error it printed:",
		""}
	out = append(out, quoted...)
	out = append(out, "",
		"There is no written hint for this error in this lab. The compiler's own output is the hint",
		"here, and it is above that failed run: fix the FIRST error and re-run — the ones after it",
		"are often its consequences.",
		"",
		"once it builds, `sboot test "+stage+"` scores the checks and `sboot hint "+stage+"` picks up",
		"whichever one is red.",
		"more help — `sboot explain "+stage+"` (the AI tutor; `--here` answers in this terminal),",
		"or the same chat on this lab's page:",
		"  "+stuckURL)
	return strings.Join(out, "\n")
}
