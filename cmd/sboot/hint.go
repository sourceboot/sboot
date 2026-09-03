// sboot hint — the pull-based failure-guidance ladder
// (the failure-guidance spec "Failure guidance v2", ratified 2026-08-08).
//
// v1 PUSHED help: it counted consecutive failures and auto-escalated to Layer 2
// on the third strike. v2 PULLS: the learner asks, and each ask climbs one rung.
// Asking is itself the genuine-effort gate, so the strike machinery is not
// needed here — what remains is one monotonic per-check marker ("highest rung
// revealed"), which doubles as the `[hint N of M]` display. The live os-rust
// course keeps its v1 mechanism untouched; this verb is the v2 surface every
// new course (kernel-in-rust first) uses.
//
//	sboot hint <stage>              the first currently-failing check's next rung
//	sboot hint <stage> <check-id>   a specific check's next rung
//
// WHERE THE PIECES LIVE. The authored text ships as `hints.json` at the spec
// root (staged into the run dir by stageRun, like everything else in the spec
// bundle); the failing set is what the last local grade recorded in state.json
// (state.go LastFailed — "test writes, hint reads"); the rung marker is
// state.json's hint_rungs. Fully offline: nothing here talks to any server.
//
// Ported from content/courses/kernel-in-rust/proto/hint.py, with three deliberate
// divergences: (1) the failing set comes from the recorded verdict rather than
// a KTAP transcript — the CLI already has the engine's per-check results, so
// re-parsing serial output would be a second opinion about what failed; (2) the
// ladder's last rung and its past-the-ladder copy hand off to the NEXT RUNG —
// `sboot explain`, then `sboot reveal` — and to the same chat on the lab page's
// `#stuck` anchor. (This copy named only the web surface between 2026-08-13 and
// 2026-08-23, while the terminal verbs were unbuilt: "a hint must never name a
// command that doesn't exist". They exist now — explain.go, reveal.go — so the
// spec's own hand-off wording is restored; the web surface stays named beside
// them, because the chat there keeps context across turns.); (3) the prototype's
// `--observed` evidence line is not a FLAG — it is wired (2026-08-23), and the
// CLI supplies it rather than the learner. `sboot test` records each failing
// check's Layer 0 into state.json, and the deepest authored rung matches its
// `evidence` selectors against that to name the cause THIS run points at
// (the failure-guidance spec "EVIDENCE-SELECTED"). A rung with no selectors, and
// a selector that matches nothing, render exactly as they did before.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const hintsFileName = "hints.json"

// hintEntry is one check's authored rungs: L1 always, L2 when the check
// warranted a full debug ladder. A check with no l2 shows `[hint 1 of 1]` and
// hands straight off to the lab page's #stuck anchor.
type hintEntry struct {
	L1 string `json:"l1"`
	L2 string `json:"l2"`
	// The EVIDENCE-SELECTED last line, optional and per check
	// (the failure-guidance spec: "the last line names the cause the learner's own
	// capture points at"). Rules are tried in order and the FIRST match wins, so
	// they are authored specific-before-general. A check with none — every check
	// in every course until 2026-08-23 — renders exactly as it always did.
	Evidence []evidenceRule `json:"evidence,omitempty"`
}

// evidenceRule is one authored selector: a pattern to look for in what the
// learner's own run did, and the line to add when it is there.
//
// The pattern side deliberately matches only the LEARNER'S evidence, never their
// source or the rubric — the whole point is to say which of the ladder's causes
// this run points at, and the ladder is already written.
type evidenceRule struct {
	// Substring, case-insensitive. Case folding is not a nicety: the strings
	// worth keying on are compiler and libtest output ("attempt to subtract with
	// overflow", "range end index"), and an author who capitalises one has
	// written a rule that silently never fires.
	Match string `json:"match,omitempty"`
	// Any-of, for one cause with several spellings ("index out of bounds" and
	// "range end index" are the same bug reported by two different panics).
	MatchAny []string `json:"match_any,omitempty"`
	// The authored line, appended to the deepest rung. Prose, like every other
	// string in this file, and under the same rule: name the cause, never the fix.
	Then string `json:"then"`
}

// selectEvidence picks the authored line this run's evidence points at, or ""
// when nothing matches.
//
// "" IS THE DESIGNED OUTCOME, not a failure: a selector that matches nothing
// falls back to the static rung, which is the whole ladder and is already
// complete on its own. So a rule may be added for a cause that is merely likely,
// and an author is never tempted to write a catch-all that would guess.
func selectEvidence(rules []evidenceRule, observed string) string {
	if observed == "" {
		return ""
	}
	hay := strings.ToLower(observed)
	for _, r := range rules {
		if r.Then == "" {
			continue
		}
		if r.Match != "" && strings.Contains(hay, strings.ToLower(r.Match)) {
			return r.Then
		}
		for _, m := range r.MatchAny {
			if m != "" && strings.Contains(hay, strings.ToLower(m)) {
				return r.Then
			}
		}
	}
	return ""
}

// hintsFile is hints.json: authored, shipped with the spec, never generated.
// `_comment` and any future keys are ignored by construction.
type hintsFile struct {
	Checks map[string]hintEntry `json:"checks"`
}

// runHint prints the next rung for one failing check. Exit codes: 0 for a hint
// (or an honest "nothing to hint"), 1 for a check with no authored hint, 2 for
// "run sboot test first" and setup problems.
//
// `stageDefaulted` says the stage came from the current-lab rule rather than the
// learner — the header then names both the stage and the chosen check, which is
// how the defaults stay visible (§11.c) and how the learner discovers they can
// target other checks.
func runHint(r repo, stage, checkID string, stageDefaulted bool) int {
	// Same prepare path as `sboot test`: the spec (and with it hints.json) is
	// fetched and staged, offline falling back to the cache.
	run := prepare(r, stage)

	st := loadState()

	// A run that produced no verdict at all is the freshest thing we know —
	// noteRunError clears it the moment anything grades — and it is the case
	// `sboot test` sends here with "stuck? sboot hint" only for hint to answer
	// "run sboot test first" (dogfood F00-4/5). Answer the run they actually had.
	//
	// BEFORE hints.json is looked for (2026-09-02, G141). This answer is code, not
	// an authored hint, and it needs no file: a course that publishes no hints
	// meets a missing linker on a fresh machine exactly like one that does, and
	// "no hints published" is the wrong thing to say to someone whose build just
	// died in the operating system's own output.
	if checkID == "" {
		if reason := st.lastRunError(r.course, stage); reason != "" {
			fmt.Println(renderBlockedHint(r, stage, reason, st.buildOut(r.course, stage),
				stageStuckURL(r.course, stage)))
			return 0
		}
	}

	b, err := os.ReadFile(filepath.Join(run, hintsFileName))
	if err != nil {
		// Not an error: a course simply may not publish hints (os-rust's v1
		// guidance lives in lab.toml instead). Exit 0 — nothing is wrong.
		fmt.Printf("no hints published for %s.\n", r.course)
		return 0
	}
	var hf hintsFile
	if err := json.Unmarshal(b, &hf); err != nil {
		// A hints.json we shipped that does not parse is OUR bug, same posture as
		// an unreadable rubric in resolve.
		fmt.Fprintf(os.Stderr, "sboot: the hints for %s are unreadable (%v) — run `sboot fetch %s` "+
			"to refresh them, and report it if it persists.\n", r.course, err, r.course)
		return 2
	}
	if len(hf.Checks) == 0 {
		fmt.Printf("no hints published for %s.\n", r.course)
		return 0
	}

	failing, graded := st.lastFailed(r.course, stage)

	target := checkID
	checkDefaulted := false
	if target == "" {
		if !graded {
			fmt.Fprintf(os.Stderr, "Run `sboot test %s` first, then `sboot hint` picks up what failed "+
				"— or name a check: `sboot hint %s <check-id>`.\n", stage, stage)
			return 2
		}
		if len(failing) == 0 {
			fmt.Println("✓ all checks pass — nothing to hint. Nice work.")
			return 0
		}
		// Bare `sboot hint` targets one failing check and prints its id (in the
		// header below), which is how the learner discovers they can target others.
		target = defaultHintTarget(failing)
		checkDefaulted = true
	} else if graded && !containsString(failing, target) {
		// A passing check cannot be the thing to hint — and refusing here is what
		// lets the rung marker stay monotonic-forever without ever gating a
		// passing learner (failure-guidance.md v2). If the stage has never been
		// graded we take the learner's word that it is failing, as the prototype
		// does.
		fmt.Printf("✓ %s is passing — nothing to hint. Run `sboot hint %s` on a failing check.\n",
			target, stage)
		return 0
	}

	entry, ok := hf.Checks[target]
	if !ok {
		fmt.Fprintf(os.Stderr, "No authored hint for `%s` yet. Re-run `sboot test %s` and read its failure evidence — it names what the check looked for and what it found instead.\n", target, stage)
		return 1
	}

	// The defaulted-target header (stderr — it is framing, not the hint): what
	// this run is about, and how to aim elsewhere.
	if stageDefaulted || checkDefaulted {
		p := painter(os.Stderr)
		line := p(ansiAmber, fmt.Sprintf("hint — %s · %s", stage, target))
		if checkDefaulted {
			line += p(ansiDim, " # your failing check; sboot hint <check> targets another")
		}
		fmt.Fprintln(os.Stderr, line)
		fmt.Fprintln(os.Stderr)
	}

	// Advance the rung by one on every ask; monotonic per (course, stage, check).
	rung := st.bumpRung(r.course, stage, target)
	if err := st.save(); err != nil {
		debugf("could not save hint state: %v", err)
	}

	// The evidence bridge: what the last graded run OBSERVED about this check
	// (state.go Evidence — `sboot test` writes it, this reads it), reduced to the
	// one authored line its selectors point at. "" whenever there is nothing
	// recorded or nothing matches, which renders the rung unchanged.
	observed := selectEvidence(entry.Evidence, st.evidence(r.course, stage, target))

	fmt.Println(renderHint(stage, target, entry, rung, observed, stageStuckURL(r.course, stage)))
	return 0
}

// hintTiers is the authored ladder for one check, in order: L1 always, L2 when
// authored. The length is the learner-facing denominator — a check with only L1
// shows `[hint 1 of 1]` and never promises a rung that does not exist.
func hintTiers(e hintEntry) []string {
	t := []string{e.L1}
	if e.L2 != "" {
		t = append(t, e.L2)
	}
	return t
}

// renderHint renders the rung-th ask (1-based) for a check — the faithful port
// of proto/hint.py's render(): a position header with a dynamic denominator, the
// authored body, an optional evidence line on the deepest rung, and a footer
// that always points somewhere: the next rung while one exists, then the next
// TOOL — `sboot explain`, and past the ladder `sboot reveal` — plus the lab
// page's `#stuck` anchor (`stuckURL`), where the same chat lives with its
// context kept across turns. The ladder never dead-ends on "no more hints".
func renderHint(stage, checkID string, e hintEntry, rung int, observed, stuckURL string) string {
	tiers := hintTiers(e)
	n := len(tiers)
	if rung <= n {
		out := []string{
			fmt.Sprintf("[hint %d of %d · %s]", rung, n, checkID),
			"",
			strings.TrimRight(tiers[rung-1], " \t\r\n"),
		}
		// The evidence-selected line: the one part of the deepest rung that is
		// chosen per run rather than fixed. The TEXT is still authored — what the
		// learner's evidence decides is WHICH authored line, never the words —
		// so the never-echo-the-rubric rule is enforced at authoring time here,
		// exactly as it is for the rung above it. See selectEvidence.
		if rung == n && observed != "" {
			out = append(out, "  → Your evidence: "+observed)
		}
		out = append(out, "")
		if rung < n {
			suffix := ""
			if rung+1 == n && e.L2 != "" {
				suffix = " — the debug ladder"
			}
			out = append(out, fmt.Sprintf(
				"run it again for the next one (%d of %d%s).", rung+1, n, suffix))
		} else {
			out = append(out,
				"that's the last written hint. next: `sboot explain "+checkID+"` — the AI tutor,",
				"with this run's evidence attached (`--here` answers in this terminal).",
				"or the same chat on this lab's page:",
				"  "+stuckURL)
		}
		return strings.Join(out, "\n")
	}
	// Past the authored ladder: keep pointing at real next moves — the learner's
	// own evidence, then the same #stuck page.
	// COURSE-NEUTRAL, deliberately: "the first byte or pixel that diverged" was
	// kernel-in-rust's framebuffer talking, printed to every course by a message that
	// belongs to none of them (dogfood F01-6 — `rust-for-systems` has no pixels, and its
	// evidence is an assert_eq! diff). What every check's evidence does have is a
	// thing it looked for and a thing it found.
	return fmt.Sprintf(
		"[%s] You've seen every written hint for this check.\n"+
			"── Re-run `sboot test %s` and read the failure evidence closely — "+
			"it names what the check looked for and what it found instead.\n"+
			"── `sboot explain %s` takes it to the AI tutor with that evidence attached "+
			"(or the same chat on this lab's page: %s).\n"+
			"── Still stuck? `sboot reveal %s` shows the course's own solution for this lab "+
			"and marks it solution-assisted — it never blocks completing it.",
		checkID, stage, checkID, stuckURL, stage)
}

// umbrellaSuffix marks a lab's WHOLE-SUITE check — `<lab>.suite` by the convention
// every rubric in the corpus follows ("the whole suite is green", authored with a
// has_all that names the other checks' subjects).
//
// It fails whenever any narrower check does, and it sorts early in every rubric, so
// before 2026-08-23 a bare `sboot hint` picked it essentially every time (dogfood
// F01-5). Its authored hint can only be about the SUITE — are the tests named right,
// did they all run — which is almost never what the learner got wrong, while the
// specific check's hint is about the actual failure and was one command away.
const umbrellaSuffix = ".suite"

// defaultHintTarget picks which failing check a bare `sboot hint` answers: the first
// SPECIFIC one, falling back to verdict order when every failing check is an
// umbrella (a suite that did not run at all, which is then the honest answer).
func defaultHintTarget(failing []string) string {
	for _, id := range failing {
		if !strings.HasSuffix(id, umbrellaSuffix) {
			return id
		}
	}
	return failing[0]
}

// renderBlockedHint answers a `sboot hint` whose last run produced no verdict: the
// build failed, or the toolchain could not start it. There is no authored ladder for
// either (a hint is per check, and no check ran), so this points at the only evidence
// that exists — the compiler's own output, or the missing tool.
//
// `buildOut` is what the build itself printed (state.go BuildOut), and it splits the
// build-failed branch in two. "Fix the FIRST error" is the right answer to a compile
// error and the WRONG one to `linker cc not found`: the learner of lab 00 has written
// no code, and the first error is the operating system's. So a build whose output
// carries a missing-linker signature is answered as the toolchain problem it is
// (toolchain.go) — the same shape the `toolchain:` reason already used, because it is
// the same fact discovered one classifier later.
func renderBlockedHint(r repo, stage, reason, buildOut, stuckURL string) string {
	out := []string{"[hint — nothing was graded]", ""}
	once := "once it builds"
	switch {
	case reason == "build" && missingLinker(hostOS, buildOut):
		once = "once that is installed"
		out = append(out,
			"Your last `sboot test "+stage+"` never reached a check: your machine has no linker,",
			"so nothing in this lab has been scored yet — this is your machine, not your code.")
		out = append(out, "")
		out = append(out, linkerHint(hostOS)...)
	case reason == "toolchain" || strings.HasPrefix(reason, "toolchain:"):
		once = "once that is installed"
		// "" when the launch error did not name an executable — installHint has an
		// honest answer for that case too, so it is passed through unchanged.
		tool := strings.TrimPrefix(strings.TrimPrefix(reason, "toolchain"), ":")
		what := "the build could not be started"
		if tool != "" {
			what = "`" + tool + "` could not be started"
		}
		out = append(out,
			"Your last `sboot test "+stage+"` never reached a check: "+what+", so nothing",
			"in this lab has been scored yet — this is your machine, not your code.")
		for _, l := range installHint(tool, rustChannel(r.dir)) {
			out = append(out, "  "+l)
		}
		out = append(out, "  "+newTerminalNote())
	default:
		out = append(out,
			"Your last `sboot test "+stage+"` stopped at the build, so no check ran and there is",
			"no failing check to hint at yet.",
			"",
			"The compiler's own output is the hint here, and it is above that failed run: fix the",
			"FIRST error and re-run — the ones after it are often its consequences.")
	}
	out = append(out, "",
		once+", `sboot test "+stage+"` scores the checks and `sboot hint "+stage+"` picks up",
		"whichever one is red.",
		"more help — `sboot explain "+stage+"` (the AI tutor; `--here` answers in this terminal),",
		"or the same chat on this lab's page:",
		"  "+stuckURL)
	return strings.Join(out, "\n")
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
