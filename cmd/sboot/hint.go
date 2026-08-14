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
// re-run command in the footer names the stage (`sboot hint <stage> <check>`),
// because unlike the prototype this CLI needs the stage argument; (3) the
// prototype's `--observed` evidence line is not wired — the render supports it,
// but the capture is not retained between runs, so the command passes "" (the
// evidence-selected L2 line arrives with the evidence bridge, not before).
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
// hands straight off to `sboot explain`.
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
func runHint(r repo, stage, checkID string) int {
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
		// Bare `sboot hint` targets the FIRST failing check and prints its id (in
		// the header below), which is how the learner discovers they can target
		// others.
		target = failing[0]
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
		fmt.Fprintf(os.Stderr, "No authored hint for `%s` yet. Re-run `sboot test %s` and read its failure evidence — it names the first byte or pixel that diverged.\n", target, stage)
		return 1
	}

	// Advance the rung by one on every ask; monotonic per (course, stage, check).
	rung := st.bumpRung(r.course, stage, target)
	if err := st.save(); err != nil {
		debugf("could not save hint state: %v", err)
	}

	fmt.Println(renderHint(stage, target, entry, rung, ""))
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
// that always points somewhere: the next rung while one exists, then back to
// the learner's own evidence and the lesson. The ladder never dead-ends on
// "no more hints". (The v2 spec hands off to `sboot explain`/`sboot reveal`
// here; those commands are not built yet, so the copy must not name them —
// re-point the footers when the AI tutor lands.)
func renderHint(stage, checkID string, e hintEntry, rung int, observed string) string {
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
			out = append(out, fmt.Sprintf(
				"── Run `sboot hint %s %s` again for a step-by-step debug ladder.", stage, checkID))
		} else {
			out = append(out, fmt.Sprintf(
				"── That's the deepest written hint. Next: re-run `sboot test %s` and hold "+
					"its failure evidence against the lesson's Stuck? section.", stage))
		}
		return strings.Join(out, "\n")
	}
	// Past the authored ladder: keep pointing at real next moves.
	return fmt.Sprintf(
		"[%s] You've seen every written hint for this check.\n"+
			"── Re-run `sboot test %s` and read the failure evidence closely — "+
			"it names the first byte or pixel that diverged.\n"+
			"── The lesson's Stuck? section maps this check to its usual causes.",
		checkID, stage)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
