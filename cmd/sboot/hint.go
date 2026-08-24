// sboot hint — the pull-based failure-guidance ladder
// (the failure-guidance spec "Failure guidance v2", ratified 2026-08-08).
//
// v1 PUSHED help: it counted consecutive failures and auto-escalated to Layer 2
// on the third strike. v2 PULLS: the learner asks, and each ask climbs one rung.
// Asking is itself the genuine-effort gate, so the strike machinery is not
// needed here — what remains is one monotonic per-check marker ("highest rung
// revealed"), which doubles as the `[hint N of M]` display. The live os-rust
// course keeps its v1 mechanism untouched; this verb is the v2 surface every
// new course (os2-rust first) uses.
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
// Ported from content/courses/os2-rust/proto/hint.py, with three deliberate
// divergences: (1) the failing set comes from the recorded verdict rather than
// a KTAP transcript — the CLI already has the engine's per-check results, so
// re-parsing serial output would be a second opinion about what failed; (2) the
// ladder's last rung and its past-the-ladder copy hand off to the lab page's
// `#stuck` anchor (ux-plan §11.i: the AI tier is web-only — there is no
// terminal `sboot explain`, by design, ever); (3) the prototype's `--observed`
// evidence line is not wired — the render supports it, but the capture is not
// retained between runs, so the command passes "" (the evidence-selected L2
// line arrives with the evidence bridge, not before).
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

	st := loadState()
	failing, graded := st.lastFailed(r.course, stage)

	target := checkID
	checkDefaulted := false
	if target == "" {
		// A run that produced no verdict at all is the freshest thing we know —
		// noteRunError clears it the moment anything grades — and it is the case
		// `sboot test` sends here with "stuck? sboot hint" only for hint to answer
		// "run sboot test first" (dogfood F00-4/5). Answer the run they actually had.
		if reason := st.lastRunError(r.course, stage); reason != "" {
			fmt.Println(renderBlockedHint(r, stage, reason, stageStuckURL(r.course, stage)))
			return 0
		}
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

	fmt.Println(renderHint(stage, target, entry, rung, "", stageStuckURL(r.course, stage)))
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
// that always points somewhere: the next rung while one exists, then the lab
// page's `#stuck` anchor (`stuckURL`), where the ladder's web mirror and the
// metered explain chat live. The ladder never dead-ends on "no more hints",
// and it NEVER names a terminal AI command — the AI tier is web-only by design
// (ux-plan §11.i; `sboot explain`/`reveal` are retired, not pending).
func renderHint(stage, checkID string, e hintEntry, rung int, observed, stuckURL string) string {
	tiers := hintTiers(e)
	n := len(tiers)
	if rung <= n {
		out := []string{
			fmt.Sprintf("[hint %d of %d · %s]", rung, n, checkID),
			"",
			strings.TrimRight(tiers[rung-1], " \t\r\n"),
		}
		// The evidence-selected line, from the learner's own capture — the one
		// part of L2 that is not authored ahead of time. Unused until the
		// evidence bridge lands; see the package comment.
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
				"run it again for the next rung (%d of %d%s).", rung+1, n, suffix))
		} else {
			out = append(out,
				"that's the last authored rung. more help — the explain chat, on this lab's page:",
				"  "+stuckURL)
		}
		return strings.Join(out, "\n")
	}
	// Past the authored ladder: keep pointing at real next moves — the learner's
	// own evidence, then the same #stuck page.
	// COURSE-NEUTRAL, deliberately: "the first byte or pixel that diverged" was
	// os2-rust's framebuffer talking, printed to every course by a message that
	// belongs to none of them (dogfood F01-6 — `rust-core` has no pixels, and its
	// evidence is an assert_eq! diff). What every check's evidence does have is a
	// thing it looked for and a thing it found.
	return fmt.Sprintf(
		"[%s] You've seen every written hint for this check.\n"+
			"── Re-run `sboot test %s` and read the failure evidence closely — "+
			"it names what the check looked for and what it found instead.\n"+
			"── more help — the explain chat, on this lab's page: %s",
		checkID, stage, stuckURL)
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
func renderBlockedHint(r repo, stage, reason, stuckURL string) string {
	out := []string{"[hint — nothing was graded]", ""}
	once := "once it builds"
	if reason == "toolchain" || strings.HasPrefix(reason, "toolchain:") {
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
	} else {
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
		"more help — the explain chat, on this lab's page:",
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
