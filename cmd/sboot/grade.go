// The local grade: resolve the lab, build once, then hand the staging root to the
// grading engine.
//
// ── WHAT CHANGED, AND WHY (the grading-engine distribution decision, 2026-08-02) ─
//
// Phase 4 of the Rust→Go port made `sboot test` judge IN PROCESS: harness/ imported
// platform/grader and scored the rubric itself. That was right about the drift
// problem and wrong about distribution — this binary is mirrored to a PUBLIC MIT
// repo, so linking the engine would have published the rubric evaluator, the
// guidance ladder and the lab schema under a licence that cannot be taken back.
//
// So the engine is now a separate program, `sboot-judge`, fetched per platform with
// the course spec and exec'd from the cache (cache.go, judgeBinary). Phase 4's real
// win is untouched: it is still ONE implementation, compiled once, linked by the
// server-side judge (platform/judgehttp) and shipped to the learner — a practice
// verdict and an official one cannot disagree by reimplementation. Only the call
// boundary moved, from a function call to a process.
//
// EVERYTHING A LEARNER SEES IS UNCHANGED, deliberately and down to the byte: the
// `── grading <lab>` header on stderr, the build output streamed live, the blank
// line, `  [PASS] desc (N pt)` per check with its guidance ladder underneath,
// `  score: got/total`, and the ✅/❌ closer. So are the exit codes: 2 for "no such
// lab" and "no os/ tree", 1 for a failed build and for anything short of full marks,
// 0 for full marks.
//
// ── THE THREE STEPS, AND WHY THE BUILD IS IN THE MIDDLE ────────────────────────
//
//	sboot-judge resolve   which rubric, and does it parse?   ← before a build is spent
//	<course build cmd>    the course's own business          ← this file's one subprocess
//	sboot-judge grade     boot, score, render, record
//
// The build stays here rather than moving into the engine because the CLI is the only
// place that can tell a build which never STARTED (no cargo on PATH — a broken
// install) from one that ran and failed (the learner's own compile error). That
// distinction is what keeps a toolchain problem out of the practice record as a 0/0
// (ledger L2a), and it is why `launchFailed` exists.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The grader manifest a course.yaml did not override. These two are the Rust/QEMU
// shape every OS course uses; a course whose toolchain differs ships its own
// `grader:` block (the multi-language design) and it arrives with the spec.
// The artifact half of that pair now lives in the engine, which is what joins it to
// a path and therefore what must clamp it (sboot-judge grade, artifactPath).
const defaultBuildCmd = "cargo xtask build"

// runGrader builds the staged workspace and grades this lab against its PUBLISHED
// checks, returning everything the CLI needs to report and record the run.
//
// `runDir` is the STAGING ROOT (cache.go stageRun), not the learner's repo: it holds
// `labs/`, the build config, `build/`, and an `os` symlink to their real tree. The
// build command runs with that as its working directory, which is what the course's
// own build expects.
//
// `tierSpec` (possibly "") names the checks that consecutive local failures have
// escalated to Layer 2. The CLI owns that policy and the engine stays historyless —
// the same separation that lets the judge run next to untrusted code on the server.
//
// `captureOut` (possibly "") asks for the machine state of the boot to be written
// there as well, which is how `sboot submit` gets its evidence without a second
// build and a second boot (D3).
func runGrader(runDir, course, labID, tierSpec, captureOut string) graderRun {
	run := graderRun{}

	// ── The engine ─────────────────────────────────────────────────────────────
	// Resolved once, up front: a missing or corrupt engine must be reported as a
	// grader that could not START, never as a lab that scored nothing.
	engine, err := judgeBinary(course)
	if err != nil {
		run.launchFailed = true
		run.launchErr = err
		return run
	}

	// ── Which rubric ───────────────────────────────────────────────────────────
	// The engine owns FindLab's prefix rule and the lab.toml parse, so that WHICH
	// rubric gets graded has exactly one implementation. It prints its own refusal
	// on stderr; exit 2 is a usage error, not a grade.
	//
	// `--grader` (2026-08-13) additionally asks for the lab's OWN build/artifact
	// (os2-rust builds a different image per lab). An older cached engine rejects
	// the flag with exit 2, so on any exit failure the resolve is retried without
	// it: a real refusal (unknown lab, unreadable rubric) fails identically both
	// times and its message reaches the learner once, from the retry, while an
	// old engine's "unknown argument" complaint is swallowed with the first run.
	labDir, labGrader, err := resolveLab(engine, runDir, course, labID)
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			run.launchFailed = true
			run.launchErr = err
			return run
		}
		run.exitCode = ee.ExitCode()
		return run
	}
	if labDir == "" {
		run.launchFailed = true
		run.launchErr = fmt.Errorf("%s resolve printed no lab directory", filepath.Base(engine))
		return run
	}

	fmt.Fprintf(os.Stderr, "── grading %s\n", filepath.Base(labDir))

	// ── Build (the one subprocess this file owns, and the course's own) ────────
	// Precedence: the LAB's own build command (lab.toml, via resolve --grader),
	// then the course-level manifest (course.yaml `grader:`), then the compiled-in
	// Rust/QEMU default. Same rule for the artifact below. NO SHELL either way —
	// argv is strings.Fields, and the command comes only from server-side content
	// we author, never from anything a learner writes.
	gm := courseGrader(course)
	buildCmd := labGrader.build
	if buildCmd == "" {
		buildCmd = gm.Build
	}
	build := argv(buildCmd, defaultBuildCmd)
	cmd := exec.Command(build[0], build[1:]...)
	cmd.Dir = runDir
	// INHERITED, not captured. The build is the slow part of a practice run and its
	// output is the learner's own compiler errors: they have to arrive as they
	// happen, and they must keep whatever colour and interleaving cargo gave them.
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			// The build command could not be STARTED (no cargo on PATH). Different
			// from a build that ran and failed, and the difference matters on the
			// submit path: `--force` may proceed past the second, never silently past
			// the first. Reported by the caller, not here.
			run.launchFailed = true
			run.launchErr = err
			return run
		}
		fmt.Fprintln(os.Stderr, "\nBUILD FAILED — fix the errors above and try again.")
		run.exitCode = 1
		return run
	}

	// ── Judge ──────────────────────────────────────────────────────────────────
	// The structured verdict travels in a FILE, not on stdout: stdout is the
	// learner's own reading material, which is teed to the terminal and to the
	// practice record. Written into the staging root's build/ — scratch that already
	// exists, is already gitignored in every course, and is per repo, so two
	// concurrent runs of different repos cannot collide.
	protocolPath := filepath.Join(runDir, "build", "verdict.lbx")
	if err := os.MkdirAll(filepath.Dir(protocolPath), 0o755); err != nil {
		run.launchFailed = true
		run.launchErr = err
		return run
	}
	// Removed first so a stale verdict from a previous run can never be read back as
	// this one's. That is the failure this file is most exposed to: an engine that
	// dies before writing would otherwise report the LAST run's score as if it were
	// now's, which is a wrong number rather than an error.
	_ = os.Remove(protocolPath)

	// The resolved artifact travels on the command line even though `grade` would
	// fall back to the lab.toml value on its own — passing it keeps the CLI's idea
	// and the engine's idea visibly the same value, resolved by one rule.
	artifact := labGrader.artifact
	if artifact == "" {
		artifact = gm.Artifact
	}
	args := []string{"grade", "--root", runDir, "--lab-dir", labDir, "--protocol", protocolPath}
	if artifact != "" {
		args = append(args, "--artifact", artifact)
	}
	if tierSpec != "" {
		args = append(args, "--tier", tierSpec)
	}
	if captureOut != "" {
		args = append(args, "--capture-out", captureOut)
	}

	var captured bytes.Buffer
	judge := exec.Command(engine, args...)
	judge.Dir = runDir
	judge.Stdout = io.MultiWriter(os.Stdout, &captured)
	judge.Stderr = os.Stderr
	err = judge.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			run.launchFailed = true
			run.launchErr = err
			return run
		}
		run.exitCode = ee.ExitCode()
	}
	// 2 and above is the engine REFUSING (no os/ tree, an unwritable verdict file):
	// it has already said why on stderr, and there is no verdict to read. 0 and 1 are
	// grades, and a grade without a protocol file is the failure handled below.
	if run.exitCode >= 2 {
		return run
	}

	protocol, readErr := os.ReadFile(protocolPath)
	if readErr != nil {
		// The engine ran and produced no verdict — it crashed, was killed, or could
		// not write. There is nothing to record, and recording it as 0/0 is precisely
		// the lie L2a is about, so this is a launch failure: no verdict, not a score.
		run.launchFailed = true
		run.launchErr = fmt.Errorf("the grading engine produced no verdict (%v)", readErr)
		return run
	}
	_ = os.Remove(protocolPath)
	run.checks, run.score, run.max = parseVerdict(string(protocol))
	run.passed = run.exitCode == 0 && run.max > 0 && run.score == run.max

	// Keep just the verdict lines as the recorded detail. Guidance is deliberately
	// NOT recorded: it is derived from this run's output, useful in the terminal,
	// and noise in a stored practice record.
	var lines []string
	for _, l := range strings.Split(captured.String(), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[PASS]") || strings.HasPrefix(t, "[FAIL]") || strings.HasPrefix(t, "score:") {
			lines = append(lines, t)
		}
	}
	run.detail = strings.Join(lines, "\n")
	return run
}

// labGraderManifest is what `resolve --grader` reported about the lab itself:
// its own build command and artifact path, both possibly empty ("use the course
// default"). See grader/lab.go — the build command is split with strings.Fields,
// no shell.
type labGraderManifest struct {
	build    string
	artifact string
}

// resolveLab runs `sboot-judge resolve --grader` and parses the lab directory
// plus the lab's own grader manifest off its stdout:
//
//	<lab dir>
//	build=<cmd or empty>
//	artifact=<rel or empty>
//
// An engine cached before the flag existed exits 2 on it, so ANY exit failure is
// retried once without the flag — course-level values then apply, which is
// exactly what that engine's rubrics could express. A genuine refusal (unknown
// lab, unreadable rubric) fails both runs; only the retry's stderr is shown, so
// the learner reads the real message exactly once and never the old engine's
// "unknown argument" complaint. A launch failure (the binary cannot start) is
// not retried: the second run cannot start either.
func resolveLab(engine, runDir, course, labID string) (labDir string, lg labGraderManifest, err error) {
	base := []string{"resolve", "--root", runDir, "--lab", labID, "--course", course}

	var out, errBuf bytes.Buffer
	first := exec.Command(engine, append(base, "--grader")...)
	first.Stdout, first.Stderr = &out, &errBuf
	if runErr := first.Run(); runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return "", lg, runErr
		}
		out.Reset()
		retry := exec.Command(engine, base...)
		retry.Stdout, retry.Stderr = &out, os.Stderr
		if runErr := retry.Run(); runErr != nil {
			return "", lg, runErr
		}
		return strings.TrimSpace(out.String()), lg, nil
	}
	// Success: forward anything resolve said on stderr (today: nothing).
	if errBuf.Len() > 0 {
		fmt.Fprint(os.Stderr, errBuf.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) > 0 {
		labDir = strings.TrimSpace(lines[0])
	}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "build="):
			lg.build = strings.TrimSpace(strings.TrimPrefix(l, "build="))
		case strings.HasPrefix(l, "artifact="):
			lg.artifact = strings.TrimSpace(strings.TrimPrefix(l, "artifact="))
		}
		// Unknown lines are ignored on purpose: the record list is append-only,
		// and a CLI on a learner's machine must keep parsing a newer engine's
		// output (the same rule as parseVerdict).
	}
	return labDir, lg, nil
}

// argv splits a command string into argv — whitespace, no shell, no quoting — falling
// back to `def` when the manifest says nothing. The rule that makes it safe to
// execute: commands come only from server-side config, never from anything a
// learner writes.
func argv(cmd, def string) []string {
	if f := strings.Fields(cmd); len(f) > 0 {
		return f
	}
	return strings.Fields(def)
}
