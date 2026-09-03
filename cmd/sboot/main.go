// sboot — the learner's CLI. Two-tier grading (the content-protection rules):
//
//	sboot test [stage]    practice — builds locally, then execs the grading engine
//	                    (grade.go) against the published checks; fast,
//	                    offline-friendly, recorded as a practice run, never
//	                    completes a stage.
//	sboot submit [stage]  official — gates on a local run, then uploads that run's
//	                    capture and the os/ source tree; the platform judges the
//	                    capture against the full private rubric, inside the request
//	                    that carries it, and answers with the verdict. Since D8
//	                    nothing on our side builds or boots. Only a passing
//	                    submission completes.
//
// The stage is OPTIONAL since v0.4 (ux-plan §11.c): it defaults to the CURRENT
// lab — the first live stage the server has not verified (status.go currentLab)
// — and the defaulted choice is always printed. Bare `sboot` is the orientation
// screen (§11.b), `sboot help` is where usage moved, and status.go's file
// comment is the map of the orientation machinery.
//
// ── THE SUBMIT GATE (D3, the grading design) ──────────────────
//
//	build once → boot once → capture
//	judge locally (published checks) → instant feedback
//	  FAIL → stop. No submission is created, nothing is uploaded.
//	  PASS → upload that same capture; the server judges and owns the verdict.
//
// Three properties, and each one is a property of this ordering rather than of any
// single step:
//
//  1. NO DOUBLE WORK. One local run scores the boot AND drops the machine state of
//     that same boot (grade.go), so a submit costs what a `sboot test` costs plus an
//     upload. Before D3 the local capture was a SECOND build and a SECOND boot after
//     the upload.
//  2. test ⇒ submit, BY CONSTRUCTION on the client side. The bytes the server
//     judges are the bytes the local judge just judged — not a re-run that ought
//     to agree.
//  3. INSTANT FEEDBACK, LOCALLY FIRST. A failing run is answered by the grader in
//     the learner's own terminal, with the full guidance ladder, instead of by a
//     round trip. What the upload adds is the official record, and since 2026-08-03
//     it adds it in one request: the platform judges inline, so there is no queue
//     to wait behind and no verdict to poll for.
//
// The server does NOT build or boot (D8). What it still does independently is
// JUDGE, from the same compiled engine, which is what makes the cross-check in
// grader.ClientVerdict meaningful.
//
// ── TWO DIRECTORIES, ONE COMMAND (the workspace split, 2026-07-26) ──────────────
//
// A learner has a git repo (their code) and a cache (our tests + grading engine),
// and `sboot` is what keeps them from ever having to think about that. repo.go owns
// the first, cache.go owns the second, and `sboot where` prints both — see
// the workspace-split design "Resolved (2026-07-26): three homes".
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Where the CLI talks to unless SBOOT_API_URL says otherwise.
//
// This MUST be a real deployment, not localhost. A released binary defaulting to
// localhost fails on a learner's very first command with "connection refused" — which
// is exactly what happened on 2026-07-27, on the first `sboot start` anyone ran. The
// override is for developers pointing at a local platform; the default is for the
// people who did not build this.
//
// Currently the Vercel deployment rather than sourceboot.com: the domain is not pointed
// at Vercel yet (plan.md M3). Move this to defaultSite in the same change that lands the
// domain — until then a "nicer" default would simply be wrong.
const defaultAPI = "https://sourceboot.com"
const defaultCourse = "os-rust"
const brandName = "SourceBoot"

// Where the public site is, for anything a learner might READ later — chiefly the
// README `sboot start` writes into a repo they may publish. Deliberately not
// `defaultAPI`: a README that links to http://localhost:3000 is worse than no link.
const defaultSite = "https://sourceboot.com"

// Stamped by the release workflow (-ldflags "-X main.version=vX.Y.Z").
var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// stageRe is the explicit-stage validation: an argument that names a stage must
// start with its lab number. It is also how `sboot hint` tells a stage argument
// from a check id (`serial.burst`) — stages always lead with digits, check ids
// never do.
var stageRe = regexp.MustCompile(`^\d+`)

// gradedArgs is one graded command's parsed invocation.
type gradedArgs struct {
	stage     string // "" until defaulted
	check     string // hint, explain
	defaulted bool   // the stage came from the current-lab rule, not the learner
	force     bool
	jsonOut   bool
	yes       bool
	here      bool   // explain: answer in this terminal instead of opening the chat
	message   string // explain: the learner's own question
	dir       string // start: unpack here instead of the course's own default name
	name      string // repo: create this repo instead of the course's own default
}

// parseCommon walks a command's arguments: positionals in order, the flags the
// command declared, `--` ending options (POSIX), unknown flags refused with the
// help pointer. Returns the positionals.
func parseCommon(cmd string, args []string, maxPos int, opts *gradedArgs) []string {
	var pos []string
	terminated := false
	// Indexed rather than ranged since 2026-08-23: `--message <text>` is the one
	// flag here that takes a value, and a learner's question is the one thing that
	// must be spellable the way every other CLI spells it.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !terminated && a == "--":
			terminated = true
		case !terminated && (a == "--force" || a == "-f"):
			if cmd != "submit" {
				usageError("--force only means something for `sboot submit`")
			}
			opts.force = true
		case !terminated && a == "--json":
			if cmd != "test" && cmd != "submit" {
				usageError("--json is not available on `sboot %s` (it is on `sboot`, test and submit)", cmd)
			}
			opts.jsonOut = true
		case !terminated && (a == "--yes" || a == "-y"):
			if cmd != "start" && cmd != "repo" && cmd != "reveal" {
				usageError("--yes only means something for `sboot start`, `sboot repo` and `sboot reveal`")
			}
			opts.yes = true
		case !terminated && a == "--here":
			if cmd != "explain" {
				usageError("--here only means something for `sboot explain`")
			}
			opts.here = true
		case !terminated && (a == "--message" || a == "-m" || strings.HasPrefix(a, "--message=")):
			if cmd != "explain" {
				usageError("--message only means something for `sboot explain`")
			}
			if v, found := strings.CutPrefix(a, "--message="); found {
				opts.message = v
				break
			}
			if i+1 >= len(args) {
				usageError("%s needs your question after it: `sboot explain --here -m \"why is this failing?\"`", a)
			}
			i++
			opts.message = args[i]
		case !terminated && (a == "--dir" || strings.HasPrefix(a, "--dir=")):
			// `sboot start` names the folder after the ARTIFACT since 2026-09-02
			// (`word-game-sb`), so the override that was implicit — the folder was
			// always the course id — has to become explicit.
			if cmd != "start" {
				usageError("--dir only means something for `sboot start`")
			}
			if v, found := strings.CutPrefix(a, "--dir="); found {
				opts.dir = v
				break
			}
			if i+1 >= len(args) {
				usageError("--dir needs a folder name after it: `sboot start <course> --dir my-game`")
			}
			i++
			opts.dir = args[i]
		case !terminated && (a == "--name" || strings.HasPrefix(a, "--name=")):
			if cmd != "repo" {
				usageError("--name only means something for `sboot repo`")
			}
			if v, found := strings.CutPrefix(a, "--name="); found {
				opts.name = v
				break
			}
			if i+1 >= len(args) {
				usageError("--name needs a repo name after it: `sboot repo --name my-word-game`")
			}
			i++
			opts.name = args[i]
		case !terminated && a == "--no-color":
			noColorFlag = true
		case !terminated && (a == "--help" || a == "-h"):
			printHelp(os.Stdout, false)
			exitWith(0)
		case !terminated && strings.HasPrefix(a, "-") && a != "-":
			usageError("unknown flag %q", a)
		default:
			if len(pos) >= maxPos {
				usageError("unexpected argument %q", a)
			}
			pos = append(pos, a)
		}
	}
	return pos
}

// requireRepo locates the workspace or refuses with the copy every graded verb
// shares (the ux-v2 error tile): what is missing, then the next move.
func requireRepo(doing string) repo {
	r, err := findRepo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: no course workspace here — nothing to %s.\n", doing)
		fmt.Fprintln(os.Stderr, "sboot: `sboot courses` lists them; `sboot start <id>` begins one.")
		exitWith(2)
	}
	return r
}

// resolveGradedStage settles which stage a graded verb runs against: an explicit
// argument wins (validated, cosmetically resolved to the manifest's id); no
// argument means the CURRENT lab — first live stage the server has not verified,
// server truth online, cache offline (§11.c).
//
// The frontier ("every live lab is verified") is a normal state, not an error:
// `onFrontier` renders it and its return becomes the exit code.
func resolveGradedStage(r repo, ga *gradedArgs, verb string, onFrontier func(*frontierInfo) int) string {
	if ga.stage != "" {
		if stageRe.FindString(ga.stage) == "" {
			fmt.Fprintf(os.Stderr, "sboot: stage %q must start with its lab number (e.g. 01-boot)\n", ga.stage)
			exitWith(2)
		}
		resolved, _ := resolveStageArg(r.course, ga.stage)
		return resolved
	}
	ga.defaulted = true
	st := loadState()
	lab, fr, _, err := currentLab(st, r.course)
	saveQuietly(st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		fmt.Fprintf(os.Stderr, "sboot: name the lab instead: `sboot %s <stage>` — or reconnect (`sboot login`) and retry.\n", verb)
		exitWith(2)
	}
	if fr != nil {
		exitWith(onFrontier(fr))
	}
	return lab.Stage
}

// hintStage is which lab a `sboot hint` is about — the learner's word if they gave
// one, else the current-lab rule, with ONE exception that comes before it:
//
// THE LAST FAILED RUN OUTRANKS THE FRONTIER (2026-09-02 review round, D-WIN-4).
// Bare `sboot hint` otherwise asks the ACCOUNT which lab is next, and on an account
// with every live lab verified the honest answer is "all live labs are verified —
// nothing to hint. Nice work" — printed, on a Windows machine that had just told
// the learner `stuck? sboot hint`, seconds after a build that could not link. Lab
// 00's own promise ("a new machine, months from now proves itself by running the
// same test") creates exactly that learner. A local run that produced no verdict at
// all is fresher than any server frontier and is cleared the moment anything
// grades, so preferring it cannot strand a working learner on a stale lab.
//
// A function rather than inline in main() so the preference is pinned against the
// frontier it beats (onboarding_test.go, G142): the frontier path exits the
// process, which is exactly what a regression would do inside a test.
func hintStage(r repo, ga *gradedArgs) string {
	if ga.stage == "" {
		if blocked := loadState().blockedStage(r.course); blocked != "" {
			ga.stage, ga.defaulted = blocked, true
		}
	}
	return resolveGradedStage(r, ga, "hint", func(fr *frontierInfo) int {
		fmt.Println("all live labs are verified — nothing to hint. Nice work.")
		return 0
	})
}

// frontierMessage is the S5 tile: every live lab verified, said plainly, exit 0.
func frontierMessage(course string, fr *frontierInfo) int {
	fmt.Printf("all live labs in %s are verified — nothing new to grade.\n", course)
	if fr.next != nil {
		fmt.Printf("lab %s (%s) is next and not yet published; labs open in order as they land.\n",
			labNumber(fr.next.Stage), labSlug(fr.next.Stage))
	} else {
		fmt.Println("new labs open in order as they land.")
	}
	fmt.Println("meanwhile: `sboot courses` — the rest of your path.")
	return 0
}

// gradedHeader prints the always-visible stage line — `grading 02-glass-cockpit
// — "The glass cockpit"` — on stderr (messaging; the verdict on stdout must
// survive `2>/dev/null`). When the stage was defaulted, the line says so and
// names the override, which is what makes the default legible (§11.c).
func gradedHeader(verbing, course, stage, verb string, defaulted bool) {
	p := painter(os.Stderr)
	line := verbing + " " + stage
	if t := labTitle(course, stage); t != "" {
		line += fmt.Sprintf(" — %q", t)
	}
	line = p(ansiAmber, line)
	if defaulted {
		line += p(ansiDim, fmt.Sprintf("   # your current lab; sboot %s <stage> overrides", verb))
	}
	fmt.Fprintln(os.Stderr, line)
}

func main() {
	// FIRST, before a single byte is written: on Windows the console renders this
	// binary's UTF-8 as cp437 mojibake unless it is told otherwise, and the runes
	// affected (`·`, `▸`) are on the very first screen (console_windows.go, D-WIN-3).
	// A no-op everywhere else.
	enableUTF8Console()

	args := os.Args[1:]
	if next, found := stripFlag(args, "--no-color"); found {
		noColorFlag = true
		args = next
	}
	if hasFlagBefore(args, "--version") {
		fmt.Println("sboot", version)
		exitWith(0)
	}

	// Bare `sboot` is the orientation screen — where am I, what's next, how —
	// exit 0 (§11.b). Usage lives in `sboot help`.
	if len(args) == 0 {
		exitWith(runStatus(false))
	}
	if len(args) == 1 && args[0] == "--json" {
		exitWith(runStatus(true))
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printHelp(os.Stdout, hasFlagBefore(args[1:], "--all"))
		exitWith(0)
	}

	cmd, rest := args[0], args[1:]
	var ga gradedArgs

	switch cmd {
	case "version":
		fmt.Println("sboot", version)
		exitWith(0)

	case "courses":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runCourses())

	case "login":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runLogin())

	case "logout":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runLogout())

	case "whoami":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runWhoami())

	case "repo":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runRepo(ga.yes, ga.name))

	case "start":
		pos := parseCommon(cmd, rest, 1, &ga)
		course := env("SBOOT_COURSE", "")
		if len(pos) == 1 {
			course = pos[0]
		}
		if course == "" {
			exitWith(startNoArg())
		}
		runStart(course, ga.dir, ga.yes)
		exitWith(0)

	case "resume":
		// One positional, and it is required: resume is about a workspace that
		// already exists somewhere, and guessing which one is exactly the kind of
		// help that picks the wrong repo.
		pos := parseCommon(cmd, rest, 1, &ga)
		if len(pos) != 1 {
			usageError("`sboot resume` needs a folder or a git URL: `sboot resume ~/word-game-sb`")
		}
		exitWith(runResume(pos[0]))

	case "where":
		parseCommon(cmd, rest, 0, &ga)
		exitWith(runWhere())

	case "fetch":
		pos := parseCommon(cmd, rest, 1, &ga)
		course := ""
		if len(pos) == 1 {
			course = pos[0]
		}
		exitWith(runFetch(course))

	case "debug":
		// Wraps the course grader's own `debug` (the workspace split moved xtask/
		// out of the learner's repo, so `cargo xtask debug` is inert there).
		pos := parseCommon(cmd, rest, 1, &ga)
		if len(pos) == 1 {
			ga.stage = pos[0]
		}
		r := requireRepo("debug")
		stage := resolveGradedStage(r, &ga, "debug", func(fr *frontierInfo) int {
			fmt.Fprintln(os.Stderr, "sboot: all live labs are verified — name the stage to boot: `sboot debug <stage>`.")
			return 2
		})
		gradedHeader("debugging", r.course, stage, "debug", ga.defaulted)
		exitWith(runDebug(r, stage))

	case "hint":
		// Two optional positionals: [stage] [check]. A stage always leads with
		// digits and a check id never does, so one argument is unambiguous.
		pos := parseCommon(cmd, rest, 2, &ga)
		switch len(pos) {
		case 1:
			if stageRe.FindString(pos[0]) != "" {
				ga.stage = pos[0]
			} else {
				ga.check = pos[0]
			}
		case 2:
			ga.stage, ga.check = pos[0], pos[1]
		}
		r := requireRepo("hint")
		stage := hintStage(r, &ga)
		exitWith(runHint(r, stage, ga.check, ga.defaulted))

	case "explain":
		// Same [stage] [check] shape as `hint` — the two verbs are rungs of one
		// ladder and a learner should not have to re-learn the arguments to climb.
		pos := parseCommon(cmd, rest, 2, &ga)
		switch len(pos) {
		case 1:
			if stageRe.FindString(pos[0]) != "" {
				ga.stage = pos[0]
			} else {
				ga.check = pos[0]
			}
		case 2:
			ga.stage, ga.check = pos[0], pos[1]
		}
		r := requireRepo("explain")
		stage := resolveGradedStage(r, &ga, "explain", func(fr *frontierInfo) int {
			fmt.Println("all live labs are verified — nothing to explain. Nice work.")
			return 0
		})
		exitWith(runExplain(r, stage, ga))

	case "reveal":
		// One positional only: reveal is per LAB (the server serves that lab's
		// module skeleton, then its reference), not per check.
		pos := parseCommon(cmd, rest, 1, &ga)
		if len(pos) == 1 {
			ga.stage = pos[0]
		}
		r := requireRepo("reveal")
		stage := resolveGradedStage(r, &ga, "reveal", func(fr *frontierInfo) int {
			fmt.Println("all live labs are verified — nothing to reveal. Nice work.")
			return 0
		})
		exitWith(runReveal(r, stage, ga.yes))

	case "test":
		pos := parseCommon(cmd, rest, 1, &ga)
		if len(pos) == 1 {
			ga.stage = pos[0]
		}
		r := requireRepo("grade")
		stage := resolveGradedStage(r, &ga, "test", func(fr *frontierInfo) int {
			return frontierMessage(r.course, fr)
		})
		runTest(r, stage, ga)

	case "submit":
		pos := parseCommon(cmd, rest, 1, &ga)
		if len(pos) == 1 {
			ga.stage = pos[0]
		}
		r := requireRepo("submit")
		stage := resolveGradedStage(r, &ga, "submit", func(fr *frontierInfo) int {
			return frontierMessage(r.course, fr)
		})
		runSubmit(r, stage, ga)

	default:
		usageError("unknown command %q", cmd)
	}
}

// startNoArg answers a bare `sboot start`: the catalog and how to pick — nothing
// is created (the ux-v2 start tile; dogfood P4/C2).
func startNoArg() int {
	st := loadState()
	cat, _ := catalog(st)
	saveQuietly(st)
	p := painter(os.Stdout)
	fmt.Println("which course? the catalog:")
	fmt.Println()
	if len(cat) == 0 {
		fmt.Printf("  (unreachable right now — browse it at %s/courses)\n", siteURL())
	} else {
		var live []catalogCourse
		for _, c := range cat {
			if c.Live > 0 {
				live = append(live, c)
			}
		}
		idW, titleW := courseColumns(live, nil)
		rows, _ := catalogRows(p, live, nil, idW, titleW)
		for _, line := range rows {
			fmt.Println(line)
		}
	}
	fmt.Println()
	fmt.Printf("pick one:  %s\n", p(ansiGreen, "sboot start "+firstLive(cat)))
	fmt.Println(p(ansiDim, "(nothing was created)"))
	return 2
}

// prepare resolves everything a local grading run needs: the lab's tests and the
// grading engine from the cache, staged next to the learner's os/ tree.
//
// This is where the entitlement check happens, and it happens exactly once — on the
// FETCH. `sboot test` needs the cache, and the cache only ever arrives through an
// authenticated, entitlement-checked download, so someone who never signed in has no
// tests and nothing to run. There is deliberately no periodic re-validation: a
// cached lab must keep grading with no network at all (architecture.md), and a
// client-owned re-check would bake a policy decision into a binary we cannot recall
// while catching only honest, well-connected people
// (the workspace-split design "Entitlement: a server-issued grant").
func prepare(r repo, stage string) string {
	s, err := ensureSpec(r.course, stage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}
	run, err := stageRun(s, r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}
	return run
}

func runTest(r repo, stage string, ga gradedArgs) {
	code := runTestCode(r, stage, ga)
	// The repo nudge (P-21), on the practice loop's own terms: at most once a day,
	// and only when there is a local repo with no remote. `test` runs dozens of
	// times an hour, so anything more often is noise in the one loop that must
	// stay quiet.
	repoNudge(r, true)
	exitWith(code)
}

// runTestCode is `sboot test`, returning its exit code instead of taking it.
// Split out 2026-09-02 so `sboot resume` can end on the first lab's checks — the
// proof that a rebuilt machine really can grade — without a second copy of the
// run, the ladder bookkeeping and the L2a "graded nothing" rule.
func runTestCode(r repo, stage string, ga gradedArgs) int {
	gradedHeader("grading", r.course, stage, "test", ga.defaulted)
	run := prepare(r, stage)

	// Failure guidance: read the per-check consecutive-failure counters BEFORE
	// grading, so we can tell the grader which checks have earned the Layer 2
	// ladder (the failure-guidance spec). The grader itself stays historyless.
	st := loadState()
	jsonMode = ga.jsonOut
	res := runGrader(run, r.course, stage, st.tierSpec(r.course, stage), "", r.tree)
	// Recorded BEFORE the launch-failure exit, because a run that never reached a
	// check is exactly the run `sboot hint` had nothing to say about (F00-5).
	st.noteRunError(r.course, stage, res)
	if res.launchFailed {
		saveQuietly(st)
		reportGraderMissing(r, res.launchErr)
		return 2
	}
	st.record(r.course, stage, res.checks)
	st.recordScore(r.course, stage, res.score, res.max)
	if err := st.save(); err != nil {
		debugf("could not save guidance state: %v", err)
	}

	// A RUN THAT GRADED NOTHING IS NOT A SCORE OF ZERO (ledger L2a/L2b). The build
	// failed, or the engine refused the lab: either way the learner has just read the
	// real diagnosis — their compiler's own output, streamed live — and the honest
	// record of that is NO record. Reporting 0/0 would tell them their work scored
	// nothing, and would write a stage_completions row a broken toolchain and a hard
	// lab are indistinguishable in.
	//
	// The run is not lost: noteRunError above put the REASON in state.json, which is
	// what `sboot hint` answers with (L28), and the "stuck?" line below still points
	// at it. Under --json nothing lands on stdout either, for the same reason and by
	// the same rule the launch-failure path above already followed: no verdict, no
	// verdict object.
	if res.graded() {
		if ga.jsonOut {
			printVerdictJSON("test", r.course, stage, res)
		}
		if err := reportPractice(r.course, stage, res.score, res.max, res.passed, res.detail); err != nil {
			reportPracticeFailure(err)
		}
	}
	if res.exitCode != 0 {
		// The stuck deep link (§11.i): the free ladder in the terminal, the
		// explain chat on the page — never a terminal AI command.
		p := painter(os.Stderr)
		fmt.Fprintf(os.Stderr, "stuck?  %s · %s\n", p(ansiGreen, "sboot hint"),
			stageStuckURL(r.course, stage))
	}
	return res.exitCode
}

// stageStuckURL is the lab page's Stuck? anchor — where the ladder's web mirror
// and the metered explain chat live (§11.i).
func stageStuckURL(course, stage string) string {
	return fmt.Sprintf("%s/courses/%s/stages/%s#stuck", siteURL(), course, stage)
}

// printVerdictJSON is `--json`'s verdict object (§12.2 rule 6), on stdout.
func printVerdictJSON(command, course, stage string, res graderRun) {
	checks := make([]map[string]any, 0, len(res.checks))
	for _, c := range res.checks {
		checks = append(checks, map[string]any{
			"id": c.id, "pass": c.pass, "points": c.points, "desc": c.desc,
		})
	}
	b, _ := json.MarshalIndent(map[string]any{
		"command": command, "course": course, "stage": stage,
		"score": res.score, "max_score": res.max, "passed": res.passed,
		"exit": res.exitCode, "checks": checks,
	}, "", "  ")
	fmt.Println(string(b))
}

// runDebug boots the stage under QEMU frozen for a debugger, by handing off to the
// course grader's own `debug` subcommand.
//
// It exists because of the workspace split: `xtask/` is no longer in the learner's
// repo, so the `cargo xtask` alias in their root `.cargo/config.toml` is inert there
// and `cargo xtask debug` — which 04-debugging tells them to run — cannot work. The
// grader is in the cache, so this stages the run exactly as `sboot test` does and
// invokes it there.
//
// stdio is INHERITED rather than captured: QEMU freezes waiting for gdb on :1234, so
// this is an interactive, long-running foreground process, not something to buffer and
// report on. There is no verdict here and nothing is recorded.
func runDebug(r repo, stage string) int {
	run := prepare(r, stage)
	cmd := exec.Command("cargo", "xtask", "debug", stage)
	cmd.Dir = run
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "sboot: could not start the debugger: %v\n", err)
		return 2
	}
	return 0
}

// reportPracticeFailure explains why the practice run was not recorded.
//
// WHY THIS IS NOT ONE MESSAGE: two very different failures arrive here. If the
// platform never answered (offline, wrong SBOOT_API_URL, platform down) then the
// reassurance is true — practice is local, losing the telemetry costs the learner
// nothing. If the platform answered 401/403 it is false and actively misleading:
// the run was refused, not lost, and no amount of retrying will change it. Until
// 2026-07-25 both printed "could not reach the platform … that's fine, it's
// practice", which sent a learner who had hit an access lock off to debug their
// network. `sboot submit` always got this right ("submission rejected: …").
func reportPracticeFailure(err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusForbidden:
			fmt.Fprintf(os.Stderr, "\nsboot: this lab is not open to you yet: %s\n", ae.msg)
			fmt.Fprintf(os.Stderr, "sboot: nothing is wrong with your setup — open %s to unlock it.\n", apiURL())
			return
		case http.StatusUnauthorized:
			// NOT SIGNED IN is not the same event as a token that stopped working,
			// and `sboot test` is promised as "free, offline, unlimited" — so a
			// learner who never logged in must not have a green run closed by a
			// three-line credentials error that reads as "your run did not count"
			// (dogfood F00-8). resolveToken's "dev" source is exactly "nothing is
			// configured here": one dim line, and what it would buy them.
			if _, source := resolveToken(); source == "dev" {
				p := painter(os.Stderr)
				fmt.Fprintf(os.Stderr, "%s\n", p(ansiDim,
					"not signed in, so this run stayed on your machine — `sboot login` to keep it on your dashboard."))
				return
			}
			fmt.Fprintf(os.Stderr, "\nsboot: the platform did not accept your token: %s\n", ae.msg)
			fmt.Fprintf(os.Stderr, "sboot: reconnect with `sboot login`, or paste a fresh token from\n")
			fmt.Fprintf(os.Stderr, "sboot:   %s/account   into SBOOT_TOKEN.\n", siteURL())
			return
		}
	}
	fmt.Fprintf(os.Stderr, "\nsboot: could not reach the platform (%v)\n", err)
	fmt.Fprintln(os.Stderr, "sboot: your practice run was NOT recorded — that's fine, it's practice.")
}

// reportGraderMissing explains a grader that never started, as opposed to one that
// ran and failed the code. Shared by `sboot test` and `sboot submit` — the missing
// piece is the same one either way, and submit adds the sentence that explains why
// it needs the same tools practice does.
func reportGraderMissing(r repo, err error) {
	tool := missingTool(err)
	if tool == "" {
		fmt.Fprintf(os.Stderr, "sboot: could not run the grader: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "sboot: %s is not installed (or is not on your PATH).\n", tool)
	}
	for _, line := range installHint(tool, rustChannel(r.dir)) {
		fmt.Fprintf(os.Stderr, "sboot:   %s\n", line)
	}
	if tool != "" {
		// The diagnosis this message does NOT make, and it is the common one on
		// Windows: the tool IS installed and this shell's PATH predates it
		// (D-WIN-2 — the lab page says "open a new terminal"; the tool never did).
		fmt.Fprintf(os.Stderr, "sboot:   %s\n", newTerminalNote())
	}
}

// reportSubmitGraderMissing is the submit half, and it teaches the relationship
// rather than reporting a failure.
//
// The learner's likely model is "test runs here, submit runs there", so "missing
// binary" reads as our problem to fix. It is not: since D8 the official grade is
// computed from the run THEIR machine makes (the grading design),
// so a machine that cannot run `sboot test` cannot submit either, and no flag
// changes that. Say what submit actually does, then name the tool and how to get it.
func reportSubmitGraderMissing(r repo, stage string, err error) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "sboot: nothing was submitted.")
	fmt.Fprintln(os.Stderr, "sboot: `sboot submit` runs `sboot test` on your machine and sends the captured")
	fmt.Fprintln(os.Stderr, "sboot: result to us for the official grade, so it needs the same tools")
	fmt.Fprintln(os.Stderr, "sboot: `sboot test` does — and they are not there yet:")
	fmt.Fprintln(os.Stderr)
	reportGraderMissing(r, err)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "sboot: then get `sboot test %s` passing — that is the run submit sends.\n", stage)
	// Deliberately does NOT offer --force. It used to work here, because the server
	// would build and boot for a learner whose machine could not; that is exactly the
	// capability D8 costs, and pointing at a flag that now fails on the server is
	// worse than not mentioning it.
}

// missingTool names the executable that could not be started, from the error
// os/exec returns for it. Empty when the error is not that shape.
func missingTool(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) {
		return ee.Name
	}
	return ""
}

// installHint returns how to get `tool` on this platform.
//
// Keyed on the tool rather than on the course, because the CLI learns the build
// command from the course's own manifest (`grader.build`) and a C course's missing
// piece is `make`, not rustup. An unknown tool still gets the honest generic answer
// instead of advice about the wrong language.
//
// `channel` is what the workspace's own rust-toolchain.toml pins, or "". It used to
// be the word "nightly", hardcoded — true of the OS courses and false of a course
// that pins stable, whose learners were told the opposite of what their own course
// says (dogfood F00-6). The pin is a property of the repo the learner is standing
// in, so it is read from there; unreadable or absent, the line says only that the
// file decides.
func installHint(tool, channel string) []string {
	switch tool {
	case "cargo", "rustc", "rustup":
		pin := "the course's rust-toolchain.toml pins the exact toolchain"
		if channel != "" {
			pin = "this course pins " + channel + " in rust-toolchain.toml"
		}
		if hostOS == "windows" {
			// The Unix one-liner is not merely the wrong spelling here: typed into
			// PowerShell it produces a red error block about `sh` not being a cmdlet
			// (D-WIN-2). rustup's Windows route is a downloaded .exe, and the
			// prerequisites question it asks is the SAME missing linker toolchain.go
			// answers — so it is named here, before the learner picks blind.
			return []string{
				"install Rust (rustup; " + pin + "):",
				"download and run https://win.rustup.rs/x86_64  (rustup-init.exe, from rustup.rs)",
				"it will ask about Visual C++ prerequisites — take option 1 unless you have them",
			}
		}
		return []string{
			"install Rust (rustup; " + pin + "):",
			"curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh",
		}
	case "nasm":
		return []string{pkgInstall("nasm")}
	case "qemu-system-x86_64", "qemu":
		return []string{pkgInstall("qemu-system-x86")}
	case "make", "gcc", "cc", "ld":
		// The C toolchain has a per-OS explanation, not just a per-OS package name
		// (a Mac's is Apple's installer, a Windows box's is Microsoft's), so this
		// case hands over to the file that owns that copy.
		return linkerHint(hostOS)
	case "":
		return []string{"the course's build needs a working toolchain — check `sboot where`."}
	default:
		return []string{pkgInstall(tool)}
	}
}

// pkgInstall renders the package-manager line for this OS.
//
// macOS gets Homebrew; Linux asks which package manager is actually on the PATH
// (toolchain.go) rather than assuming apt, because the PACKAGE NAME differs and
// not only the command; Windows gets winget.
//
// The `default` branch used to serve every non-macOS machine on the reasoning that
// a Fedora learner still reads the package's name. On Windows that produced
// `sudo apt install git` — a package manager, a privilege command and an install
// path that none of them exist (D-WIN-2, measured on Windows 11).
func pkgInstall(pkg string) string {
	switch hostOS {
	case "darwin":
		if pkg == "build-essential" {
			return "xcode-select --install"
		}
		return "brew install " + strings.TrimSuffix(pkg, "-system-x86")
	case "windows":
		return windowsInstallLine(pkg)
	default:
		return linuxInstallLine(pkg)
	}
}

// apiError is a reply we DID get: the platform answered, and the answer was no.
// It carries the status so callers can separate "refused" from "unreachable".
type apiError struct {
	status int
	msg    string
	// The course id this one was RENAMED to, from the platform's `renamed_to`
	// field (410 Gone; lib/api-gate.ts renamedResponse, 2026-09-01). Empty on
	// every other refusal, and empty against any platform that predates the
	// field — which is what makes it safe to branch on: an absent value reads as
	// "not a rename", the behaviour this binary had before.
	renamedTo string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.msg, e.status) }

// ── where: the answer to "so where IS my grader?" ────────────────────────────────
//
// This command exists because two directories is more to explain than one, and if
// that explanation is sloppy the split becomes a support burden. A learner should
// never have to know what XDG means, or go looking under ~/Library on a Mac and
// ~/.local/share on Linux, to answer a question the tool can just answer.
//
// It reads only local state — no network — so it works on a plane and it works when
// the platform is down, which is when someone is most likely to be asking.
func runWhere() int {
	r, err := findRepo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	fmt.Printf("course   %s\n", r.course)
	fmt.Printf("repo     %s\n", r.dir)
	fmt.Printf("         └─ your code. This is the git repo — nothing of ours is in it.\n")

	s, ok := cachedSpec(r.course)
	if !ok {
		fmt.Printf("tests    (none cached yet — run `sboot fetch`)\n")
	} else {
		labs := s.cachedLabs()
		fmt.Printf("tests    %s\n", s.labsDir())
		fmt.Printf("         └─ spec %s, %d lab(s) cached", s.version, len(labs))
		if len(labs) > 0 {
			fmt.Printf(": %s", strings.Join(labs, " "))
		}
		fmt.Println()
		// The grader is a SEPARATE PROGRAM again, and this command's whole job is to
		// answer "where?" honestly — so it names the file, not the fact.
		//
		// It has been three things in a month: a Rust crate in the learner's repo, then
		// compiled into this binary (which is what "grader <path to xtask>" would have
		// been quietly lying about), and since the distribution split a per-platform
		// binary fetched with the tests and exec'd. The last shape is the one a learner
		// can act on: it is a file they can see, delete, and re-fetch, and it is the
		// thing a support question about "which grader am I running" is really about.
		fmt.Printf("grader   %s\n", s.judgePath())
		fmt.Printf("         └─ fetched for %s with the tests, and checked against a published\n", platformKey())
		fmt.Printf("            SHA-256 before it runs. Not part of `sboot` and separately licensed.\n")
		if st, err := os.Stat(s.buildToolDir()); err == nil && st.IsDir() {
			fmt.Printf("build    %s\n", s.buildToolDir())
			fmt.Printf("         └─ this course's build tooling, cached with its tests.\n")
		}
		if run, err := workDir(r.course, r.dir); err == nil {
			fmt.Printf("scratch  %s\n", run)
			// "+ engine" was true only while the engine was a directory staged in here.
			// It is exec'd from the cache now, so naming it would send anyone who went
			// looking to an empty spot.
			fmt.Printf("         └─ where the grader runs: our tests + this course's build\n")
			fmt.Printf("            tooling, staged beside your %s/.\n", r.treeName())
			// Say which way os/ got there, because the answer decides where cargo
			// writes and this command's whole job is to answer "where?" honestly.
			// The symlink is the fast path (edits are picked up with no copy step),
			// and its cost is that target/ lands in the repo rather than here — which
			// is what `sboot start`'s .gitignore is for. Claiming "so your repo stays
			// clean" was the opposite of what the symlink does.
			// `os` is the STAGED name for every course (cache.go linkOSTree), so it
			// is named literally here and the learner's own tree is named beside it.
			if linked, err := os.Readlink(filepath.Join(run, "os")); err == nil && linked == r.osDir() {
				fmt.Printf("            os/ there is a symlink to your %s/, so cargo's target/ dirs land\n", r.treeName())
				fmt.Printf("            in your repo — `sboot start`'s .gitignore already covers them.\n")
			}
		}
	}
	if dir, err := stateDir(); err == nil {
		fmt.Printf("state    %s\n", filepath.Join(dir, "state.json"))
	}
	return 0
}

// ── fetch: download tests deliberately, rather than as a side effect ─────────────
//
// `sboot test` fetches the lab it needs on its own, so this command exists for the one
// case implicit fetching cannot serve: doing it DELIBERATELY — refreshing a stale spec,
// or warming the cache before losing your connection.
//
// There is no `--all` (removed 2026-07-26). It promised the whole course and delivered
// the next lab, because it was bounded by sequential unlock rather than entitlement.
// The lab-boundary trap it was meant to prevent is already covered by the opportunistic
// prefetch when a lab is completed (cache.go).
func runFetch(course string) int {
	if course == "" {
		if r, err := findRepo(); err == nil {
			course = r.course
		} else {
			course = env("SBOOT_COURSE", defaultCourse)
		}
	}
	s, err := ensureSpec(course, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	fmt.Printf("── %s spec %s: %d lab(s) cached\n   %s\n",
		course, s.version, len(s.cachedLabs()), s.labsDir())
	return 0
}

// One check's verdict, as reported by the local grader's protocol output. `id` is
// what makes per-check attempt counting possible — and why the counter is keyed on
// it rather than on `desc`, which is prose and would reset every learner's ladder
// the first time a description got reworded.
type localCheck struct {
	id     string
	pass   bool
	points int
	desc   string
	// LAYER 0 for this check, verbatim as the engine generated it: what the
	// learner's own run did, never a criterion of ours (grader/guidance.go
	// Observe). Empty for a check that passed, and for a verdict from an engine
	// older than the sixth protocol field.
	//
	// It is carried here for ONE consumer — `sboot hint`'s evidence-selected rung
	// (the failure-guidance spec "EVIDENCE-SELECTED"). state.go stores it per
	// failing check and hint.go matches an authored rung's selectors against it,
	// which is how the ladder's last line can name the cause this learner's run
	// points at rather than listing all four. It is NEVER re-rendered: the engine
	// already printed it, and printing it twice would be a second place for the
	// never-echo-the-rubric rule to be got wrong.
	evidence string
}

type graderRun struct {
	checks     []localCheck
	score, max int
	passed     bool
	detail     string
	exitCode   int
	// The build command RAN and exited non-zero: the learner's own compile error, or
	// a tool their build needed and could not launch. runGrader returns before a
	// single check is scored, so this run has NO VERDICT — it is not a score of zero
	// (ledger L2a/L2b), and it is what `sboot hint` needs to tell apart from "you
	// have not run the test yet" (dogfood F00-4/5 — lab 00's whole "Stuck?" section
	// is written around reading a red check, and a failed build reaches none).
	//
	// It was a METHOD sniffing the struct's shape (exit 1, nothing scored) until
	// 2026-08-23, deliberately, so that L2a's reflective pin would keep failing while
	// the practice record was still written as 0/0. That defect is now fixed, so the
	// fact is recorded honestly, at the one place that knows it.
	buildFailed bool
	// The engine ran to a VERDICT and this run carries it: a protocol file was read
	// back and it contained an LBX_SCORE record. False is every other outcome —
	// the build failed, the engine refused the lab, it wrote nothing, it wrote half
	// a file — and false is the DEFAULT, so a path added later that returns early
	// records nothing rather than recording a zero (ledger L2a/L2b).
	//
	// It is a fact set where it is known, not a shape inferred afterwards: 0/0 with
	// no checks is a real verdict for a rubric that has none, and only the code that
	// read the protocol can tell that from a run that never reached one.
	verdict bool
	// The grader could not be STARTED (no cargo on PATH, not a staging root) — which
	// is a different thing from a grader that ran and failed the code, and the two
	// have different consequences on the submit path: `--force` may proceed past
	// the second but never silently past the first.
	launchFailed bool
	launchErr    error
	// What the build itself PRINTED, bounded to its head and tail and stripped of
	// colour (buildlog.go). Not a verdict and never rendered back verbatim: it is
	// the text `sboot hint` classifies, which is how a build that died on a missing
	// LINKER gets an answer instead of "the compiler's own output is the hint"
	// (2026-09-02 review round; ledger G140/G141). Empty when the build never
	// started, because there was nothing to print.
	buildOut string
}

// graded reports whether the ENGINE PRODUCED A VERDICT for this run — the `verdict`
// field above, read through a name that says what the caller is asking.
//
// Everything that records or reports a practice result asks this first, because a
// run that never got a verdict is a failure to grade and not a score of zero.
// "practice run recorded (0/0)" tells a learner whose build just died that their
// work scored nothing, and the row behind it is indistinguishable from "tried the
// lab and failed every check" — a broken install reading as a hard lab in the one
// dataset the dropout curve is built from (ledger L2a/L2b, the testing strategy §1.5).
func (r graderRun) graded() bool { return r.verdict }

// parseVerdict reads the grader's protocol output (grader.EmitProtocol):
//
//	LBX_CHECK\t{PASS|FAIL}\t{points}\t{desc}\t{guidance}\t{id}
//	LBX_SCORE\t{got}\t{total}
//
// Fields are append-only, so accept >= 4 and treat 5 and 6 as optional — the same
// rule (and the same reason) as the server-side parser: a record from a grader
// newer than this binary must still parse, or every check silently vanishes.
//
// The guidance field is not RENDERED here. The grader has already rendered it,
// capped at the tier we asked for; re-rendering it in the CLI would be a second
// place for the "never echo the rubric" rule to be got wrong. Since 2026-08-23 its
// LAYER 0 tier is nonetheless read out of it and kept on the check — not to print,
// but so `sboot hint` can match an authored rung's evidence selectors against what
// this learner's run actually did (the evidence bridge; see localCheck.evidence).
//
// `ok` is whether an LBX_SCORE record was there at all, and it is what `verdict` is
// set from: a protocol the engine only half wrote (killed mid-write, out of disk)
// parses to 0/0 and must not be recorded as a score of zero (ledger L2a). The score
// line is the right anchor because the engine emits exactly one, always, last.
func parseVerdict(out string) (checks []localCheck, score, max int, ok bool) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		switch {
		case len(f) >= 4 && f[0] == "LBX_CHECK":
			c := localCheck{pass: f[1] == "PASS", desc: f[3]}
			c.points, _ = strconv.Atoi(f[2])
			if len(f) >= 5 {
				c.evidence = layer0(f[4])
			}
			if len(f) >= 6 {
				c.id = f[5]
			}
			checks = append(checks, c)
		// >= 3, not == 3: LBX_SCORE is append-only too, and this binary lives on
		// a machine we cannot patch. A grader that ever appends a fourth field
		// would make an == 3 parser skip the line entirely and report 0/0 — a
		// plausible-looking wrong verdict, not an error, permanent on that laptop.
		case len(f) >= 3 && f[0] == "LBX_SCORE":
			score, _ = strconv.Atoi(f[1])
			max, _ = strconv.Atoi(f[2])
			ok = true
		}
	}
	return
}

// The three control bytes grader/protocol.go packs a check's guidance ladder
// with: US after the `L<n>` tag, RS between tiers, VT standing in for a newline
// inside one tier's text. Spelled out here rather than imported because the CLI
// links NOTHING of the engine — harness/go.mod has no requires at all, and that
// emptiness is what keeps the grading engine out of the public MIT repo
// (the grading-engine distribution decision). Three bytes copied is the price; they are part
// of an append-only wire format, so they do not move.
const (
	guidanceUS = "\x1f"
	guidanceRS = "\x1e"
	guidanceVT = "\x0b"
)

// layer0 unpacks the LAYER 0 tier — the learner's own observation — out of a
// packed guidance field, or "" when the record carries none (a check that
// passed, or an engine that predates the field).
//
// Deliberately tolerant of everything else in there: tiers are append-only and a
// newer engine may pack tags this binary has never heard of, so anything that is
// not `L0` is skipped rather than mis-read. Nothing here is printed — see
// localCheck.evidence for the single consumer.
func layer0(guidance string) string {
	for _, part := range strings.Split(guidance, guidanceRS) {
		tag, text, ok := strings.Cut(part, guidanceUS)
		if !ok || tag != "L0" {
			continue
		}
		return strings.ReplaceAll(text, guidanceVT, "\n")
	}
	return ""
}

func apiURL() string {
	return strings.TrimRight(env("SBOOT_API_URL", defaultAPI), "/")
}

// siteURL is the URL a HUMAN should be sent to, which is not always the one this
// binary talks to. A learner running against a local platform still wants their
// repo's README to link somewhere a reader can actually go, so a loopback API URL
// falls back to the public site.
func siteURL() string {
	if s := os.Getenv("SBOOT_SITE_URL"); s != "" {
		return strings.TrimRight(s, "/")
	}
	u := apiURL()
	if strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") {
		return defaultSite
	}
	return u
}

// submissionLink is the ONE way a submit verdict points at its own row's page:
// the server's review_url when it sent one, else the stage page's ?submission=
// deep link built on the API origin. R2R-7 (round-2 dogfood 2026-08-25) — the
// pass and fail paths built this two different ways, and the fail path used
// siteURL(), whose loopback→public-site fallback is right for a README a
// stranger reads and exactly wrong here: against a local platform it printed a
// production URL for a submission that exists only in the local DB. The row
// lives on the deployment this binary just talked to, so the fallback derives
// from apiURL(), the same origin the row was created on.
func submissionLink(reviewURL, course, stage, id string) string {
	if reviewURL != "" {
		return reviewURL
	}
	if id == "" {
		return ""
	}
	return fmt.Sprintf("%s/courses/%s/stages/%s?submission=%s#review", apiURL(), course, stage, id)
}

func authedRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	// The token order is auth.go's resolveToken: SBOOT_TOKEN env → the stored
	// login (site-matched) → the dev default. Until `sboot login` existed this
	// line read the env var directly with the same default, so a machine with
	// no credentials file behaves byte-identically to every earlier release.
	token, _ := resolveToken()
	req.Header.Set("Authorization", "Bearer "+token)
	// WHO IS ASKING (the CLI release policy §1). Two spellings on purpose: the
	// User-Agent is conventional and carries the platform tuple, and the explicit
	// header is what the server keys on because proxies and corporate middleboxes
	// rewrite User-Agent. Sent on every authed request, alongside X-Sboot-Spec
	// below, because "which versions are affected?" is step 0 of the bad-release
	// procedure and cannot be answered retroactively.
	req.Header.Set("User-Agent", fmt.Sprintf("sboot/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH))
	req.Header.Set(hdrCLIVersion, version)
	// On EVERY request, or on none (see vercelBypass). There is no subset that
	// would work: a 302 on the spec fetch is exactly as fatal as a 302 on the
	// submit, and every call this binary makes goes through this function.
	if s := vercelBypass(); s != "" {
		req.Header.Set(hdrVercelBypass, s)
	}
	return req, nil
}

// ── Vercel Deployment Protection: a TESTING affordance, never a learner one ──────
//
// A preview deployment answers 302 to any request carrying no bypass credential, so
// without this the CLI cannot talk to a preview AT ALL — the environment that exists
// to be exercised before production was untestable by the one client that matters.
// `sboot start` got the auth page instead of a workspace and nothing downstream ever
// ran. That is why the header goes on every request rather than on the interesting
// ones.
//
// A LEARNER WILL NEVER SET THIS. It is deliberately absent from `sboot --help`
// (documented in harness/README.md and docs/commands.md instead), production is not
// protected, and nothing anywhere branches on the header being present — so it
// cannot become load-bearing for a normal run. Unset, the request bytes are
// byte-identical to what they were before this existed.
//
// ONE SECRET, ONE PLACE, FOUR READERS. Same two sources as scripts/smoke.sh,
// scripts/canary-grade.sh and e2e/pages_test.go's target mode: the environment
// first, then `<state dir>/vercel-bypass` (chmod 600). Drop it in the file once and
// every probe we point at a deployment — including this binary — picks it up.
// SBOOT_STATE_DIR moves that file with the rest of the CLI's state, which is why a
// runner that relocates the state dir (canary-grade.sh does) passes the value in the
// environment instead.
//
// NEVER PRINTED. Not in --help, not in any error path, not under SBOOT_DEBUG — the
// debug line below says only that one is configured. Go itself is careful here too
// (net/http's validateHeaders: "Don't include the value in the error, because it may
// be sensitive"), and the validity check below means we never hand it a value that
// could reach that path anyway.
const hdrVercelBypass = "x-vercel-protection-bypass"

// bypassFile is read from the state dir, so it sits next to state.json in
// ~/.config/sboot — the path scripts/smoke.sh has always used.
const bypassFile = "vercel-bypass"

func vercelBypass() string {
	if v := strings.TrimSpace(os.Getenv("SBOOT_VERCEL_BYPASS")); v != "" {
		return validBypass(v, "SBOOT_VERCEL_BYPASS")
	}
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, bypassFile))
	if err != nil {
		return "" // the overwhelmingly common case: there is no such file
	}
	return validBypass(strings.TrimSpace(string(b)), filepath.Join(dir, bypassFile))
}

// bypassNoted keeps the debug line to one per run rather than one per request. This
// binary has no goroutines (nothing in harness/ starts one), so a plain bool is the
// whole synchronisation story.
var bypassNoted bool

// validBypass drops anything that is not a plain one-line token. A file with a stray
// control character would otherwise be refused by net/http at send time, turning a
// misconfiguration into "connection error" on every command — and the source is named
// here, where the VALUE never is.
func validBypass(v, source string) string {
	noteOnce := func(format string, args ...any) {
		if !bypassNoted {
			bypassNoted = true
			debugf(format, args...)
		}
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			noteOnce("ignoring the deployment-protection bypass from %s: not a single-line ASCII token", source)
			return ""
		}
	}
	if v != "" {
		noteOnce("sending %s (configured via %s; value not shown)", hdrVercelBypass, source)
	}
	return v
}

// send performs a request and records whatever the platform said about CLI
// versions on the way back (update.go). Every network call in this binary goes
// through it, so discovery costs zero extra round trips — the news rides on the
// request the learner was already making.
func send(c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	noteCLIHeaders(resp)
	return resp, err
}

// exitWith is os.Exit plus the one thing that must happen on EVERY exit path:
// print anything the platform asked us to say. It goes last, so nothing ever
// appears between "── building" and the verdict (§1), and it goes on the failure
// paths too — a learner whose run just broke is exactly who most needs to hear
// "0.3.1 is broken, downgrade".
func exitWith(code int) {
	flushCLINotice()
	os.Exit(code)
}

type practiceResp struct {
	OK   bool   `json:"ok"`
	User string `json:"user"`
	Hint string `json:"hint"`
	Err  string `json:"error"`
}

func reportPractice(course, stage string, score, max int, passed bool, detail string) error {
	body, _ := json.Marshal(map[string]any{
		"course":    course,
		"stage":     stage,
		"score":     score,
		"max_score": max,
		"passed":    passed,
		"detail":    detail,
	})
	req, err := authedRequest("POST", apiURL()+"/api/v1/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var r practiceResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || !r.OK {
		if r.Err != "" {
			return &apiError{status: resp.StatusCode, msg: r.Err}
		}
		return &apiError{status: resp.StatusCode, msg: "unexpected response"}
	}

	// In --json mode stdout is the verdict object and nothing else; the human
	// line moves to stderr with the rest of the messaging.
	w := io.Writer(os.Stdout)
	if jsonMode {
		w = os.Stderr
	}
	if passed {
		fmt.Fprintf(w, "\n→ practice run recorded ✅ — %s\n", r.Hint)
	} else {
		fmt.Fprintf(w, "\n→ practice run recorded (%d/%d) — keep going.\n", score, max)
	}
	return nil
}

// ── submit: the official, server-graded run ─────────────────────────────────────

// submissionResp is what POST /api/v1/submissions answers with, and since
// 2026-08-03 that answer IS THE VERDICT: the platform judges the capture inside
// the request that carries it, so there is nothing to wait for and nothing to poll.
//
// What used to be here: a 202 saying `status: "pending"`, then a loop that GET'd
// /api/v1/submissions/<id> every two seconds for up to five minutes while a
// separate grading worker claimed the job off a queue. The queue, the worker and
// the loop are all gone. The route still answers `pending` in exactly one case —
// the judge could not be reached at all — and that is reported rather than waited
// on, because nothing else will ever grade it.
//
// A `Shadow *shadowAssignment` field lived here too, for the Beta-1 dual run.
// Gone with the executor (D8 phase 4). An unknown JSON field is ignored, so a
// platform that still sent one would simply be un-listened-to.
type submissionResp struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Score     *int   `json:"score"`
	MaxScore  *int   `json:"max_score"`
	Detail    string `json:"detail"`
	NextStage *struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Live  bool   `json:"live"`
	} `json:"next_stage"`
	// ADDITIVE (2026-08-17, the flagship bridge — docs/ai-features.md): present
	// on a pass when the platform created its AI review. Its presence is what
	// switches the pass print to "Submitted → <url>": the lab is finished on
	// that page's Complete button now, not by the submit. Absent from an older
	// platform ⇒ the pre-review verdict print runs unchanged.
	ReviewURL string `json:"review_url"`
	Err       string `json:"error"`
}

// runSubmit is a thin wrapper so the body can `return` an exit code and let its
// defers run — the temp capture file needs cleanup, and `os.Exit` skips defers.
func runSubmit(r repo, stage string, ga gradedArgs) {
	code := submit(r, stage, ga)
	// The repo nudge (P-21), EVERY passing submit — not once a day. A passing
	// submit is the moment there is something worth showing, which is the only
	// moment "put this on GitHub" is an offer rather than an interruption; the
	// daily throttle belongs on `test`, the loop that runs all day.
	if code == 0 {
		repoNudge(r, false)
	}
	exitWith(code)
}

// reportSubmitAuthFailure is the 401 tile: both doors (`sboot login` and the
// copied-token path), and what was NOT lost.
func reportSubmitAuthFailure(msg string) {
	if msg == "" {
		msg = "invalid or missing token"
	}
	fmt.Fprintf(os.Stderr, "sboot: the platform did not accept your token (401: %s).\n", msg)
	fmt.Fprintln(os.Stderr, "sboot: reconnect with `sboot login`, or paste a fresh token from")
	fmt.Fprintf(os.Stderr, "sboot:   %s/account   into SBOOT_TOKEN.\n", siteURL())
	fmt.Fprintln(os.Stderr, "sboot: nothing was submitted — your passing run is safe to resend.")
}

func submit(r repo, stage string, ga gradedArgs) int {
	force := ga.force
	gradedHeader("submitting", r.course, stage, "submit", ga.defaulted)
	osDir := r.osDir()
	if st, err := os.Stat(osDir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "sboot: no %s/ tree at %s\n", r.treeName(), osDir)
		return 2
	}
	course := r.course
	jsonMode = ga.jsonOut

	// Ask whether this stage is even open to us before spending a build and a boot
	// on it. Being told "finish the previous lab first" after a two-minute local
	// run is the one bad outcome the gate creates, and one cheap GET removes it.
	if ae := submitPreflight(course, stage); ae != nil {
		if ae.status == http.StatusUnauthorized {
			reportSubmitAuthFailure(ae.msg)
			return 2
		}
		fmt.Fprintf(os.Stderr, "sboot: submission rejected: %s\n", ae.Error())
		return 2
	}

	// The tests and the grader come from the cache; `os/` stays in the repo. Same
	// staging root `sboot test` uses, so the gate below really is the same run.
	run := prepare(r, stage)

	// ── The gate: one build, one boot, two consumers ───────────────────────────
	var capturePath string
	if f, err := os.CreateTemp("", "sboot-capture-*.json"); err == nil {
		capturePath = f.Name()
		f.Close()
		defer os.Remove(capturePath)
	}

	// "one run", not "one boot": since the command capture (2026-08-13) a course
	// may have nothing to boot — rust-for-kernels's evidence is `cargo test` — and this
	// line is printed for every course. The boot IS the run for the OS courses.
	// Progress narration goes to stderr (§12.2 rule 3): stdout is for the verdict.
	fmt.Fprintf(os.Stderr, "── checking %s locally first (one build, one run)\n", stage)
	st := loadState()

	// R1-7 (round-1 dogfood 2026-08-25): a bare `sboot submit` right after a
	// completion targets the NEXT lab — the current-lab default moved forward
	// the moment the previous one was verified — so a learner acting on
	// "revise and resubmit" advice grades an untouched workspace and would be
	// handed a `--force` suggestion for a lab they never started. Detect that
	// shape NOW, before this very run writes lab state and erases the
	// "untouched" evidence: stage defaulted + no local trace of it + the lab
	// before it verified. The refusal below then names both interpretations
	// instead of suggesting `--force` on starter code.
	freshDefault := ""
	if ga.defaulted && !st.hasLocalWork(course, stage) {
		freshDefault = previousVerifiedLab(st, course, stage)
	}

	res := runGrader(run, course, stage, st.tierSpec(course, stage), capturePath, r.tree)

	// THE LADDER COUNTER (the failure-guidance spec), and the decision behind it.
	//
	// A gated run COUNTS, exactly as `sboot test` does. It is the same grader, on
	// the same machine, against the same published checks, and its failures were
	// just rendered to the learner with the same ladder — so it is the same
	// evidence of being stuck, and the counter measures being stuck now. Not
	// counting it would leave a hole precisely where the gate changes behaviour:
	// a failing submit is now free, so someone will iterate with `submit`, and
	// that learner would never earn Layer 2 at all.
	//
	// A `--force` run is READ-ONLY: the tier caps are still read (guidance renders
	// at the earned tier) and nothing is written. `--force` means "I am not acting
	// on this local verdict" — either the learner disputes it or they are recording
	// a failure deliberately — and writing on that path would let a disputed
	// failure escalate a ladder, or a fluke pass erase a genuine streak. Read-only
	// makes `--force` unable to corrupt the counter by construction rather than by
	// care.
	//
	// Note this is not the rule grader.SubmitMaxTier implements. That one caps what
	// the SERVER's verdict renders, and stays at tier 1: the server has nothing
	// honest to escalate to. Local rendering is local rendering.
	if !force {
		st.record(course, stage, res.checks)
		st.noteRunError(course, stage, res)
		if err := st.save(); err != nil {
			debugf("could not save guidance state: %v", err)
		}
	}

	switch {
	case res.launchFailed:
		// NO CAPTURE, NO SUBMISSION — and --force does not change that (D8).
		//
		// This used to be the one thing `--force` could still do here: submit with no
		// local run at all and let the server build and boot for us. There is no
		// server-side build any more, so there is nothing on the other end to produce
		// a verdict from — a submission with no capture is refused by the platform
		// too (submissions/route.ts), and refusing here saves the round trip and gives
		// a message that can actually name the missing tool.
		//
		// `--force` keeps its real meanings: a local run that FAILED can still be
		// submitted (below), for a learner who disputes the local verdict or wants the
		// failure on the record. What it can no longer do is submit without one.
		reportSubmitGraderMissing(r, stage, res.launchErr)
		return 2
	case res.exitCode != 0 && !force:
		reportGateFailure(stage, freshDefault, res)
		return 1
	case res.exitCode != 0:
		fmt.Fprintf(os.Stderr, "\n── local check FAILED (%s) — submitting anyway (--force)\n", scoreText(res))
	default:
		fmt.Fprintf(os.Stderr, "\n── local check passed (%s)\n", scoreText(res))
	}

	fmt.Fprintf(os.Stderr, "── packaging %s/ (source only)\n", r.treeName())
	archive, err := tarGzDir(osDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: package: %v\n", err)
		return 2
	}

	// THE UPLOAD IS THE EVIDENCE, not a request for one (D8). The capture is the
	// machine state of the boot whose verdict the learner just watched, and the
	// server judges THOSE bytes against the private rubric — so it travels with the
	// submission rather than chasing it afterwards under a nonce. The source goes
	// too, unchanged: 66 of the rubric's checks are read from it.
	body, contentType, err := submitBody(archive, readCapture(capturePath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: package: %v\n", err)
		return 2
	}

	url := fmt.Sprintf("%s/api/v1/submissions?course=%s&stage=%s", apiURL(), course, stage)
	req, err := authedRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	req.Header.Set("Content-Type", contentType)
	// SPEC SKEW (the CLI release policy §6). Which tests produced the verdict the
	// learner is about to see? Under the retired `server` backend the answer could
	// not change the grade — the runner rebuilt and rebooted with the in-repo rubric
	// — but the capture above was produced with THIS spec's script, settle marker and
	// timeout, so a stale one is judged against another run's criteria. The server
	// compares this against lib/spec.ts `specStamp` and refuses a mismatch by name
	// (409), which is the whole reason the header exists.
	if s, ok := cachedSpec(course); ok {
		req.Header.Set("X-Sboot-Spec", course+"@"+s.version)
	}
	resp, err := send(&http.Client{Timeout: 60 * time.Second}, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: could not reach the platform (%v)\n", err)
		return 2
	}
	var created submissionResp
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil || created.ID == "" {
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			reportSubmitAuthFailure(created.Err)
		case created.Err != "":
			fmt.Fprintf(os.Stderr, "sboot: submission rejected: %s (HTTP %d)\n", created.Err, resp.StatusCode)
		default:
			fmt.Fprintf(os.Stderr, "sboot: unexpected response (HTTP %d)\n", resp.StatusCode)
		}
		return 2
	}
	fmt.Fprintf(os.Stderr, "── submitted (%d KB) — graded on the server\n", len(body)/1024)
	if ga.jsonOut {
		out := map[string]any{
			"command": "submit", "course": course, "stage": stage,
			"id": created.ID, "status": created.Status,
			"score": deref(created.Score), "max_score": deref(created.MaxScore),
		}
		// Additive, mirroring the wire: absent when the platform sent none.
		if created.ReviewURL != "" {
			out["review_url"] = created.ReviewURL
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	}

	// THE VERDICT IS ALREADY IN `created`. The platform judges the capture inside
	// the request that carried it, so there is no queue to wait behind and nothing
	// to poll — the five-minute deadline, the `.`/`!` progress dots and the Ctrl-C
	// window they created are all gone with the runner that made them necessary.
	//
	// The verdict is DATA: stdout (except under --json, where stdout is the one
	// JSON object and the human rendering joins the messaging on stderr).
	vw := io.Writer(os.Stdout)
	if ga.jsonOut {
		vw = os.Stderr
	}
	if created.Detail != "" {
		fmt.Fprintln(vw)
		// The server's verdict detail quotes the learner's own run back —
		// including command lines and paths in the STAGING spelling (`os/…`).
		// Same display rule as the local run (retree.go, F00-1): rewrite at the
		// terminal, a no-op for every course whose tree is `os`.
		fmt.Fprintln(vw, indent(retreeText(created.Detail, run, r.tree), "  "))
	}
	switch created.Status {
	case "passed":
		// THE FLAGSHIP BRIDGE (docs/ai-features.md; ux-plan §6): when the
		// platform created a review, the pass is *submitted for review* — the
		// lab is finished by the Complete button ON that page, so this print
		// must not claim completion and the CLI must not mark the stage
		// verified (the server hasn't; the next online status read is truth).
		// An older platform sends no review_url and the pre-review print below
		// runs unchanged — the fallback the append-only rule requires.
		if created.ReviewURL != "" {
			fmt.Fprintf(vw, "\n  ✅ official grade: %d/%d — recorded\n", deref(created.Score), deref(created.MaxScore))
			// Through the same submissionLink the fail path uses (R2R-7): with a
			// ReviewURL present it IS the server's URL — one mechanism, both verdicts.
			fmt.Fprintf(vw, "  Submitted → %s\n", submissionLink(created.ReviewURL, course, stage, created.ID))
			fmt.Fprintln(vw, "  read your review, then press Complete there — that's what finishes the lab")
			return 0
		}
		// Keep the offline orientation truthful without a round trip: this stage
		// is now verified, and the next status screen should say so even on a
		// plane.
		st.markVerified(course, stage, fmt.Sprintf("%d/%d", deref(created.Score), deref(created.MaxScore)))
		if err := st.save(); err != nil {
			debugf("could not save state: %v", err)
		}
		fmt.Fprintf(vw, "\n  ✅ official grade: %d/%d — stage complete!", deref(created.Score), deref(created.MaxScore))
		if created.NextStage != nil {
			// OPPORTUNISTIC PREFETCH. Completing this lab is the moment the next one
			// becomes fetchable (sequential unlock just opened it), and it is also the
			// moment the learner is most likely to close the laptop. Pull its tests now
			// so a per-lab fetch never becomes the reason someone cannot start work on
			// a train. Best-effort and silent: it is an optimisation, and failing it
			// costs one download later.
			if created.NextStage.Live {
				prefetchLab(course, created.NextStage.ID)
				fmt.Fprintf(vw, " Next up: %s\n", created.NextStage.ID)
			} else {
				fmt.Fprintf(vw, " Next up: %s (coming soon)\n", created.NextStage.Title)
			}
		} else {
			fmt.Fprintln(vw, " Course complete!")
		}
		return 0
	case "failed":
		fmt.Fprintf(vw, "\n  ❌ official grade: %d/%d\n", deref(created.Score), deref(created.MaxScore))
		// F06-2 (rust-for-beginners dogfood 2026-08-24): the failing verdict gets the
		// same pointer a passing one gets. The learner who just recorded a
		// failure on purpose — possibly because they suspect the grader — is the
		// one who most needs the web review/Stuck? surface, and was the one who
		// got no link. The row exists (a failed submission is deep-linkable on
		// the stage page's stepper by id); the server only sends `review_url` on
		// a pass, so when it sent none submissionLink builds the same page's URL
		// from what this side already knows — on the API origin, the deployment
		// that holds the row (R2R-7: this used to be siteURL(), which pointed a
		// local dev's failure at a production page that could not exist).
		if link := submissionLink(created.ReviewURL, course, stage, created.ID); link != "" {
			fmt.Fprintf(vw, "  Recorded → %s\n", link)
			fmt.Fprintln(vw, "  this run and its evidence are on that page — its Stuck? panel picks up from here")
		}
		// This line used to read "the server rubric includes hidden checks; make it
		// work, not just print". D1 removed the hidden checks, so blaming a stricter
		// rubric would now be false — and D3 makes the honest reading exact. If the
		// local gate went green and the server did not, the two runs graded the SAME
		// checks, so what differs is the two machines.
		if res.exitCode == 0 && !res.launchFailed {
			fmt.Fprintln(vw, "     Your local run passed, so the difference is between your machine and")
			fmt.Fprintln(vw, "     ours, not between two sets of checks. Please report it.")
		}
		return 1
	case "error":
		fmt.Fprintln(vw, "\n  ⚠ grading error on the server — please retry; if it persists, report it.")
		return 2
	default:
		// `pending` or `running`: the platform accepted the upload and could not
		// grade it — the judge was unreachable, errored, or this deployment has none
		// configured. NOTHING WILL PICK IT UP LATER; there is no queue and no worker.
		// So this says so immediately instead of implying a wait, which is the whole
		// difference from the old poll loop: that one printed dots for five minutes
		// and then "still pending — check the dashboard later".
		//
		// Re-running the command is the right advice and costs nothing extra: the
		// same work carries the same idempotency key, so a retry lands on this same
		// submission rather than creating a second one.
		fmt.Fprintln(vw, "\n  ⚠ your work was uploaded, but nothing graded it. This is a problem on our")
		fmt.Fprintln(vw, "     side, not with your code — nothing about your submission was judged.")
		fmt.Fprintf(vw, "     Run `sboot submit %s` again in a few minutes. If it happens twice in a\n", stage)
		fmt.Fprintln(vw, "     row the grading service is down, and re-submitting will not help.")
		return 2
	}
}

// scoreText renders the local run's score, or says so when there is none — an
// xtask too old for `--protocol` whose human output we also failed to scrape.
func scoreText(res graderRun) string {
	if res.max <= 0 {
		return "no score reported"
	}
	return fmt.Sprintf("%d/%d", res.score, res.max)
}

// reportGateFailure explains a submit that was stopped by the local check.
//
// It deliberately adds almost nothing: the grader has already streamed every
// failing check and its guidance to this terminal, and repeating that would be a
// second place for the never-echo-the-rubric rule to be got wrong. The only facts
// the grader cannot know are that nothing was submitted, and how to override.
//
// freshDefault is non-empty when this run graded a DEFAULTED stage the learner
// has never touched on this machine, right after completing the lab before it
// (R1-7 — main.go's submit gate computes it before the run writes state). In
// that shape the failing checks are the starter stubs, "fix these first" is
// wrong, and a `--force` suggestion would invite recording a forced 0/N on an
// unstarted lab — so the copy names both interpretations, explicitly, instead.
func reportGateFailure(stage, freshDefault string, res graderRun) {
	fmt.Fprintf(os.Stderr, "\n── not submitted: the local check did not pass (%s)\n", scoreText(res))
	if freshDefault != "" {
		fmt.Fprintf(os.Stderr, "   This graded %s — your current lab, which has no work on this machine\n", stage)
		fmt.Fprintf(os.Stderr, "   yet: a bare `sboot submit` moved on when %s completed.\n", freshDefault)
		fmt.Fprintln(os.Stderr, "   No submission was created and nothing was sent. Say which you meant:")
		fmt.Fprintf(os.Stderr, "     resubmit lab %s:  sboot submit %s\n", labNumber(freshDefault), freshDefault)
		fmt.Fprintf(os.Stderr, "     start lab %s:     work through its brief, then sboot test %s\n", labNumber(stage), stage)
		return
	}
	fmt.Fprintln(os.Stderr, "   `sboot submit` grades the same checks you just ran, so fix these first —")
	fmt.Fprintln(os.Stderr, "   no submission was created and nothing was sent.")
	fmt.Fprintf(os.Stderr, "   Think the local grader is wrong, or want this failure on the record?\n")
	fmt.Fprintf(os.Stderr, "     sboot submit %s --force\n", stage)
}

// submitPreflight asks the platform whether it would accept a submission for this
// stage, before the gate spends a build and a boot on one it will refuse.
//
// DELIBERATELY PERMISSIVE: this is an optimisation, not a gate. Only an explicit
// refusal counts; offline, a 5xx, or a platform too old to answer GET here all
// return nil and the normal path continues, where the POST is still what decides.
// `sboot submit` must never start refusing because of a network blip, and the
// local check itself works with no network at all.
func submitPreflight(course, stage string) *apiError {
	// SBOOT_OFFLINE promises "skip every network call", and this call is the
	// easiest one to honor it with: it is an optimisation, and skipping it just
	// means the POST decides — which offline it will, by failing to send.
	if offline() {
		return nil
	}
	url := fmt.Sprintf("%s/api/v1/submissions?course=%s&stage=%s", apiURL(), course, stage)
	req, err := authedRequest("GET", url, nil)
	if err != nil {
		debugf("preflight: %v", err)
		return nil
	}
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		debugf("preflight: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Err       string `json:"error"`
		RenamedTo string `json:"renamed_to"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		if r.Err == "" {
			r.Err = "refused"
		}
		return &apiError{status: resp.StatusCode, msg: r.Err}
	case http.StatusGone:
		// A RENAMED COURSE (410 Gone, lib/api-gate.ts renamedResponse). On this route
		// it can mean nothing else: the workspace's `course = "…"` names an id that
		// was retired, and the platform has named its successor. Refusing here is the
		// whole value of the preflight — the POST says the same sentence, but only
		// after the gate has spent a build and a boot on tests that are frozen.
		//
		// Structural like harness/cache.go's branch: `renamed_to` is what decides, so
		// the two ends are never coupled through prose. An older platform sends no
		// such body on this route and cannot reach this case with either field set,
		// which keeps the "deliberately permissive" promise above intact.
		if r.RenamedTo != "" || r.Err != "" {
			msg := r.Err
			if msg == "" {
				msg = fmt.Sprintf("this course was renamed to %q — edit course = %q in your workspace's sboot.toml", r.RenamedTo, r.RenamedTo)
			}
			return &apiError{status: resp.StatusCode, msg: msg, renamedTo: r.RenamedTo}
		}
	case http.StatusNotFound:
		// 404 is ambiguous in a way the others are not: it is our "unknown course or
		// stage", but it is also what some deployments answer for a route that has no
		// GET handler. Refuse only when the body says why — an empty 404 falls through
		// to the POST rather than making a working submit stop working.
		if r.Err != "" {
			return &apiError{status: resp.StatusCode, msg: r.Err}
		}
	}
	return nil
}

// submitBody packs a submission: the source tarball, and the capture of the boot
// the gate just judged.
//
// MULTIPART, and only when there is a capture. The tarball is byte for byte the one
// this CLI has always uploaded — it is now a named part next to the capture, because
// the server judges the capture and reads the source, and one request that either
// wholly succeeds or wholly fails leaves no half-submitted state to reason about.
// With no capture (nothing but `--force` past a failed local run can produce that,
// and the platform refuses it) the body is the raw tarball, exactly as before.
func submitBody(archive, capture []byte) (body []byte, contentType string, err error) {
	if capture == nil {
		return archive, "application/gzip", nil
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, part := range []struct {
		field, filename string
		data            []byte
	}{
		{"archive", "os.tar.gz", archive},
		{"capture", "capture.json", capture},
	} {
		f, err := w.CreateFormFile(part.field, part.filename)
		if err != nil {
			return nil, "", err
		}
		if _, err := f.Write(part.data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// readCapture returns the gate's capture, or nil if there isn't a usable one.
func readCapture(path string) []byte {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// ── start: create the learner's workspace ───────────────────────────────────────
//
// What lands here is ONLY the learner's half — their source tree, the build config,
// and the four files this command writes itself (sboot.toml, LICENSE, .gitignore,
// README.md). The tests and the grading engine go to the cache in the same run, so
// the learner ends up ready to grade without ever having had our harness inside
// their repo.
//
// Both destinations are printed, because two directories is only a support burden if
// nobody says where they are.
//
// ── LOCAL ONLY, SINCE 2026-09-02 (P-21) ────────────────────────────────────────
//
// This command no longer touches GitHub. It unpacks, runs `git init` INSIDE the
// folder, settles the git identity, makes the first commit, and prints one line
// naming `sboot repo`. Everything that can fail on a network, a missing gh, a
// browser login or a password prompt now lives in that command, which nobody has
// to run — see concierge.go's header for why the boundary moved.
//
// ── AND IT REPAIRS RATHER THAN REFUSING (P-16) ─────────────────────────────────
//
// A workspace that already has a valid sboot.toml is RE-FETCHED, not rejected: the
// missing pieces come back, a learner file is never overwritten, and the command
// says what it repaired. That is the answer to a download that died half way, which
// used to leave a half-tree the learner could only fix with `rm -rf` — a command
// nobody should have to type over their own work. A non-empty directory with NO
// sboot.toml is still refused: it is not ours, and we do not know what is in it.
func runStart(course, dirFlag string, yes bool) {
	// The folder's NAME comes from the course's manifest (`artifact:` → the
	// `<artifact>-sb` a learner shows people), so ask before anything lands on
	// disk. The cache answers first and offline; the network refreshes it. A
	// failure here is not fatal — the fallback is the course id, which is what
	// every workspace made before 2026-09-02 is called.
	artifact := courseArtifact(course)
	title, firstStage, firstTitle := course, "", ""
	specTree := ""
	if m, err := fetchManifest(course); err == nil {
		if m.Artifact != "" {
			artifact = m.Artifact
		}
		if m.Title != "" {
			title = m.Title
		}
		specTree = m.Tree
		for _, l := range m.Labs {
			if l.Live {
				firstStage, firstTitle = l.Stage, l.Title
				break
			}
		}
	} else if e, ok := err.(*apiError); ok && e.status == http.StatusUnauthorized {
		// `sboot start` is the FIRST command anyone runs, so this is the expected
		// first failure rather than an edge case, and it is answered here — before a
		// directory is created — instead of after a failed download.
		fmt.Fprintf(os.Stderr, "sboot: %s\n", e.msg)
		reportSignedOut("sboot start " + course)
		exitWith(2)
	} else {
		debugf("manifest not fetched before start: %v", err)
	}

	dest := dirFlag
	if dest == "" {
		dest = workspaceName(course, artifact)
	}
	// Standing IN a workspace for this course is the same question as pointing at
	// one: repair it, rather than nesting a second copy inside it.
	if dirFlag == "" && courseFromManifest(".") == course {
		dest = "."
	}

	switch {
	case courseFromManifest(dest) != "":
		if have := courseFromManifest(dest); have != course {
			fmt.Fprintf(os.Stderr, "sboot: ./%s is a workspace for %q, not %q.\n", dest, have, course)
			fmt.Fprintf(os.Stderr, "sboot: pick another folder: sboot start %s --dir <name>\n", course)
			exitWith(2)
		}
		repairStart(course, dest, title, firstStage, firstTitle, specTree, yes)
		return
	case dirNonEmpty(dest):
		fmt.Fprintf(os.Stderr, "sboot: ./%s already exists and is not empty — and it is not a %s workspace.\n", dest, brandName)
		fmt.Fprintf(os.Stderr, "sboot: unpack somewhere else:  sboot start %s --dir <name>\n", course)
		fmt.Fprintf(os.Stderr, "sboot: already have this course somewhere? cd into it, or `sboot resume <path>`.\n")
		exitWith(2)
	}

	fmt.Printf("── fetching the %s starter tree\n", course)
	// UNPACK BESIDE THE DESTINATION, THEN MOVE IT IN. A download that dies half way
	// used to leave a partial tree in ./<course> that the NEXT `sboot start` then
	// refused as "not empty" — ledger C5, i.e. one flaky connection between a
	// learner and an unrecoverable first five minutes. Staging makes the failure
	// leave nothing at all behind, which is a better answer than any message.
	stagedDir, err := os.MkdirTemp(filepath.Dir(absOr(dest)), ".sboot-start-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: could not create a working directory next to ./%s (%v)\n", dest, err)
		exitWith(2)
	}
	if err := fetchWorkspace(course, stagedDir); err != nil {
		os.RemoveAll(stagedDir)
		reportWorkspaceFailure(course, err)
		exitWith(2)
	}
	if err := moveInto(stagedDir, dest); err != nil {
		os.RemoveAll(stagedDir)
		fmt.Fprintf(os.Stderr, "sboot: could not put the starter tree in ./%s (%v)\n", dest, err)
		exitWith(2)
	}

	// The four generated files BEFORE the tests are fetched (skeptic, 2026-09-03):
	// sboot.toml is what makes this directory a workspace, and a repair (P-16) is
	// keyed on it. The spec download below is the one network wait left in this
	// command, so a Ctrl-C there used to leave a full tree with no manifest —
	// exactly the "not one of ours, refusing" shape P-16 exists to end.
	if err := writeRepoFiles(dest, course, title, firstStage, specTree); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}

	// The tests + grader, into the cache. Deliberately AFTER the scaffold: if this
	// half fails (offline mid-setup) the learner still has their repo and a later
	// `sboot test` will finish the job, rather than being left with nothing.
	fetchTests(course, firstStage)

	abs, _ := filepath.Abs(dest)
	fmt.Printf("\n── your workspace is ready: %s\n", abs)
	// The tree name and what it holds both come from the course (dogfood F00-1):
	// `rust-for-systems` unpacks a `db/` holding a SQLite reader, and was told `os/ is
	// yours` and that publishing it publishes a kernel it does not have.
	fmt.Printf("   %s/ is yours. sboot.toml says which course this is.\n", treeOr(specTree))
	fmt.Printf("   Our tests and grader are NOT in here — they live in the %s data dir,\n", brandName)
	fmt.Printf("   so publishing this repo publishes %s and nothing else.\n", treeSubject(course))
	if s, ok := cachedSpec(course); ok {
		fmt.Printf("   tests + grader: %s\n", s.dir)
	}

	// Seed the progress cache: a fresh start means nothing is verified yet, and
	// writing that down is what lets `sboot test` default its stage offline
	// immediately after (currentLab needs SOME progress source).
	st := loadState()
	if st.Sync[course] == nil {
		st.setSync(course, &courseSync{Verified: map[string]string{},
			SyncedAt: time.Now().UTC().Format(time.RFC3339)})
	}
	saveQuietly(st)

	// The LOCAL repo — git init, identity, first commit. Nothing here can reach
	// GitHub, and the one line below is the only thing that names it.
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "── your history starts here")
	if err := ensureLocalRepo(os.Stderr, abs, course, yes); err != nil {
		fmt.Fprintf(os.Stderr, "   %v\n", err)
		fmt.Fprintln(os.Stderr, "   nothing here blocks the course — the files are on disk either way.")
	}
	fmt.Fprintf(os.Stderr, "   when you want it on GitHub: %s\n", painter(os.Stderr)(ansiGreen, "sboot repo"))

	printFirstLab(course, dest, firstStage, firstTitle)
}

// repairStart is `sboot start` run against a workspace that already exists: put
// back what is missing, touch nothing that is there, and say which it was.
//
// NEVER OVERWRITES A LEARNER FILE, and that is a property of the mechanism rather
// than a promise: the starter tree is unpacked to a staging directory and only the
// paths ABSENT from the workspace are copied across, so a file the learner has
// edited is not compared, not backed up, and not read. writeRepoFiles has the same
// rule for the four files it generates.
func repairStart(course, dest, title, firstStage, firstTitle, specTree string, yes bool) {
	fmt.Printf("── %s is already here — checking what is missing\n", dest)
	var repaired []string

	stagedDir, err := os.MkdirTemp(filepath.Dir(absOr(dest)), ".sboot-start-")
	if err == nil {
		defer os.RemoveAll(stagedDir)
		if err := fetchWorkspace(course, stagedDir); err != nil {
			// A repair that cannot reach the platform still repairs the cache half
			// below; say so once and carry on.
			fmt.Fprintf(os.Stderr, "sboot: could not re-fetch the starter tree (%v)\n", err)
		} else {
			restored, err := copyMissing(stagedDir, dest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sboot: could not restore missing files (%v)\n", err)
			}
			repaired = append(repaired, restored...)
		}
	}

	before, _ := cachedSpec(course)
	fetchTests(course, firstStage)
	if after, ok := cachedSpec(course); ok && after.dir != before.dir {
		repaired = append(repaired, "the course's tests + grading engine")
	}

	// The four generated files, restored one by one — writeRepoFiles skips what
	// exists, so this only has to notice what did not.
	for _, f := range []string{manifestName, "LICENSE", ".gitignore", "README.md"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			repaired = append(repaired, f)
		}
	}
	if err := writeRepoFiles(dest, course, title, firstStage, specTree); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}

	abs, _ := filepath.Abs(dest)
	if len(repaired) == 0 {
		fmt.Printf("\n── nothing was missing: %s is complete.\n", abs)
	} else {
		fmt.Printf("\n── repaired %s:\n", abs)
		for _, r := range repaired {
			fmt.Printf("   + %s\n", r)
		}
		fmt.Println("   your own files were not touched — only what was absent came back.")
	}
	if err := ensureLocalRepo(os.Stderr, abs, course, yes); err != nil {
		fmt.Fprintf(os.Stderr, "   %v\n", err)
	}
	if _, ok := existingRemote(abs); !ok {
		fmt.Fprintf(os.Stderr, "   when you want it on GitHub: %s\n", painter(os.Stderr)(ansiGreen, "sboot repo"))
	}
	printFirstLab(course, dest, firstStage, firstTitle)
}

// printFirstLab is the handoff both paths end on: the first live lab, the command
// that grades it, and its lesson URL (P6 — a URL at the decision moment).
func printFirstLab(course, dest, firstStage, firstTitle string) {
	firstTest := firstStage
	if firstTest == "" {
		firstTest = "01-boot"
	}
	headline := firstTitle
	if headline == "" {
		headline = labSlug(firstTest)
	}
	p := painter(os.Stdout)
	fmt.Printf("\nbegin at lab %s — %s:\n", labNumber(firstTest), p(ansiAmber, headline))
	if dest != "." {
		fmt.Printf("  cd %s\n", dest)
	}
	fmt.Printf("  %s                  %s\n", p(ansiGreen, "sboot test"), p(ansiDim, "# grades "+firstTest))
	fmt.Printf("  %s/courses/%s/stages/%s\n", siteURL(), course, firstTest)
}

// fetchWorkspace downloads the course's starter tree into `dest`.
func fetchWorkspace(course, dest string) error {
	req, err := authedRequest("GET", apiURL()+"/api/v1/courses/"+course+"/workspace", nil)
	if err != nil {
		return err
	}
	resp, err := send(&http.Client{Timeout: 120 * time.Second}, req)
	if err != nil {
		return fmt.Errorf("could not reach the platform (%w)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Err string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		if e.Err == "" {
			e.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &apiError{status: resp.StatusCode, msg: e.Err}
	}
	return extractTarGz(resp.Body, dest)
}

func reportWorkspaceFailure(course string, err error) {
	fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
	if e, ok := err.(*apiError); ok {
		if e.status == http.StatusUnauthorized {
			reportSignedOut("sboot start " + course)
			return
		}
		// The platform ANSWERED — a renamed course (410, naming the new id), a
		// course it does not have, a lab this account cannot open. Nothing is
		// "coming back"; the message above already says what to do instead.
		if e.status >= 400 && e.status < 500 {
			fmt.Fprintln(os.Stderr, "sboot: nothing was left on disk.")
			return
		}
	}
	fmt.Fprintln(os.Stderr, "sboot: nothing was left on disk — re-run `sboot start` when it is back.")
}

// reportSignedOut answers a 401 on the first command anyone runs.
//
// `sboot login` FIRST, the token second (P-6, 2026-09-02). A bare "invalid or
// missing token" is a dead end — the reader has no idea a token exists — and the
// old answer sent them to a web page to copy one into an env var, which is the
// fallback, not the path the lesson teaches. `sboot login` connects the machine
// in one command with a browser approval and stores the credential; the export
// stays for CI and for anyone whose browser cannot be reached.
//
// `retry` is the command the reader was running, printed verbatim as the second
// step — `sboot start <course>` or `sboot resume <what they typed>` — because the
// point of naming it is that they can paste it.
func reportSignedOut(retry string) {
	p := painter(os.Stderr)
	fmt.Fprintf(os.Stderr, "\n  This machine is not connected to your account yet:\n")
	fmt.Fprintf(os.Stderr, "    %s\n", p(ansiGreen, "sboot login"))
	fmt.Fprintf(os.Stderr, "    %s\n", retry)
	fmt.Fprintf(os.Stderr, "\n  No browser here? Take a token from %s/account instead:\n", siteURL())
	fmt.Fprintf(os.Stderr, "    export SBOOT_TOKEN=<the token>\n")
}

// fetchTests puts the course's tests and grading engine in the cache, and the
// first live lab's checks beside them. Never fatal: the workspace is on disk and
// the next `sboot test` finishes the job.
func fetchTests(course, firstStage string) {
	s, err := ensureSpec(course, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: note — the tests are not cached yet (%v)\n", err)
		fmt.Fprintln(os.Stderr, "sboot: run `sboot fetch` when you're back online.")
		return
	}
	// Only the FIRST lab's tests. Per-lab download is the point: tests for labs
	// this learner has not reached never touch their disk
	// (the workspace-split design "Per-lab test download, not per-course").
	if firstStage != "" {
		if err := fetchLab(course, firstStage, s.dir); err != nil {
			debugf("first lab not fetched at start: %v", err)
		}
	}
}

// ── the small filesystem half of start ──────────────────────────────────────────

func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func dirNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// moveInto puts a staged tree at `dest`, which is either absent or an empty
// directory somebody made in advance.
func moveInto(staged, dest string) error {
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		// An empty directory the learner created for us: replace it, so the rename
		// below works the same way on every OS (Windows refuses to rename onto an
		// existing directory even when it is empty).
		if err := os.Remove(dest); err != nil {
			// Not removable — fall back to copying the contents across.
			_, cerr := copyMissing(staged, dest)
			if cerr != nil {
				return cerr
			}
			return os.RemoveAll(staged)
		}
	}
	if err := os.Rename(staged, dest); err == nil {
		return nil
	}
	// Different filesystems (a TMPDIR override, a mounted home): copy instead.
	if _, err := copyMissing(staged, dest); err != nil {
		return err
	}
	return os.RemoveAll(staged)
}

// copyMissing copies every file under `src` to `dst` that is NOT already there,
// and returns the workspace-relative names of the ones it wrote.
//
// The never-overwrite rule is the whole function: an existing path is skipped
// before it is opened, so a file the learner has edited is not read, not compared
// and not replaced.
func copyMissing(src, dst string) ([]string, error) {
	var written []string
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, p)
		if relErr != nil || rel == "." {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, err := os.Stat(target); err == nil {
			return nil // theirs — leave it alone
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, err := d.Info(); err == nil {
			mode = info.Mode().Perm()
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, b, mode); err != nil {
			return err
		}
		written = append(written, filepath.ToSlash(rel))
		return nil
	})
	return written, err
}

// tarGzDir packages a directory (source only: build artifacts excluded).
func tarGzDir(dir string) ([]byte, error) { return tarGzDirSep(dir, filepath.Separator) }

// tarGzDirSep is tarGzDir with the host's path separator injected, which is what makes
// the "/"-separated entry-name rule below provable on a host whose separator is already
// "/" — i.e. on every machine this is developed and per-PR-tested on. Nothing but a test
// passes anything other than filepath.Separator.
func tarGzDirSep(dir string, sep rune) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		// Tar entry names are "/"-separated BY SPEC, and the server's extractor reads
		// them that way (grader.ExtractSourceTar). A Windows walk hands us
		// `kernel\main.rs`, which the extractor takes
		// for a FILENAME, not a path: the learner's whole tree would land on the server
		// as flat files with backslashes in their names, and grading would score an
		// empty kernel (L11 — confirmed on a windows-latest runner 2026-07-27).
		name := rel
		if sep != '/' {
			name = strings.ReplaceAll(name, string(sep), "/")
		}
		if d.IsDir() {
			if d.Name() == "target" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return tw.WriteHeader(&tar.Header{
				Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755,
			})
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks etc.; the server rejects them anyway
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)),
		}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix)
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
