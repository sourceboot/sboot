// sboot — the learner's CLI. Two-tier grading (the content-protection rules):
//
//	sboot test <stage>    practice — builds locally, then execs the grading engine
//	                    (grade.go) against the published checks; fast,
//	                    offline-friendly, recorded as a practice run, never
//	                    completes a stage.
//	sboot submit <stage>  official — gates on a local run, then uploads that run's
//	                    capture and the os/ source tree; the platform judges the
//	                    capture against the full private rubric, inside the request
//	                    that carries it, and answers with the verdict. Since D8
//	                    nothing on our side builds or boots. Only a passing
//	                    submission completes.
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

func usage() {
	fmt.Fprintf(os.Stderr, `sboot — learn by building

usage:
  sboot start [course]   create the course repo in ./<course>/ (run once):
                       your os/ tree, build config, README, LICENSE
  sboot test <stage>     practice: run the lab's checks locally
                       (fast loop; does not complete the stage)
  sboot hint <stage> [check]  a hint for a failing check — each time you run
                       it, the hint goes one rung deeper
  sboot submit <stage>   official: check locally first, then upload for the
                       server-side grade (completes the stage)
  sboot where            print where your repo, tests and grader live
  sboot debug <stage>    boot this stage under QEMU frozen for a debugger on :1234
  sboot fetch [course]   download the current lab's tests on purpose (refresh, or
                       pre-cache before losing your connection)

flags:
  --force, -f          submit even if the local check fails — for when you think
                       the local grader is wrong, or you want the failed attempt
                       on the record

environment:
  SBOOT_API_URL      platform URL           (default %s)
  SBOOT_TOKEN        your API token         (default the local dev token)
  SBOOT_COURSE       course id              (default: read from sboot.toml)
  SBOOT_COURSE_DIR   your course repo       (default: walk up from cwd)
  SBOOT_CACHE_DIR    tests + grader cache   (default: the OS data dir)
  SBOOT_OFFLINE      set to skip every network call (cached tests still grade)
`, defaultAPI)
	exitWith(2)
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("sboot", version)
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "start" {
		course := env("SBOOT_COURSE", defaultCourse)
		if len(os.Args) >= 3 {
			course = os.Args[2]
		}
		runStart(course)
		exitWith(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == "where" {
		exitWith(runWhere())
	}
	if len(os.Args) >= 2 && os.Args[1] == "debug" {
		// Added 2026-07-26 to repair a regression the workspace split caused. The
		// split moved xtask/ out of the learner's repo into the cache, which left
		// `cargo xtask debug` — an instruction 04-debugging's brief and lesson both
		// give — failing with "manifest path does not exist" in their own repo.
		// The grader lives where `sboot where` says it does; wrapping is what every
		// other xtask invocation already does.
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sboot debug <stage>")
			exitWith(2)
		}
		r, err := findRepo()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
			exitWith(2)
		}
		exitWith(runDebug(r, os.Args[2]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "hint" {
		// Its own dispatch (like debug) because it takes an OPTIONAL second
		// positional — the check id — which the generic stage-plus-flags parser
		// below would reject as "unexpected argument".
		if len(os.Args) < 3 || len(os.Args) > 4 {
			fmt.Fprintln(os.Stderr, "usage: sboot hint <stage> [check-id]")
			exitWith(2)
		}
		stage, check := os.Args[2], ""
		if len(os.Args) == 4 {
			check = os.Args[3]
		}
		if regexp.MustCompile(`^\d+`).FindString(stage) == "" {
			fmt.Fprintf(os.Stderr, "sboot: stage %q must start with its lab number (e.g. 01-boot)\n", stage)
			exitWith(2)
		}
		r, err := findRepo()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
			exitWith(2)
		}
		exitWith(runHint(r, stage, check))
	}
	if len(os.Args) >= 2 && os.Args[1] == "fetch" {
		// No `--all`: removed 2026-07-26. It read as "cache the whole course before a
		// flight" but was bounded by SEQUENTIAL UNLOCK, not entitlement — on a fresh
		// account it cached 2 labs and reported "16 not open to you yet". A command
		// whose name promises the course and delivers the next lab is worse than no
		// command. The lab-boundary case it was meant to cover is already handled by
		// the opportunistic prefetch on completion (see cache.go).
		course := ""
		for _, a := range os.Args[2:] {
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "sboot: unknown flag %q\n", a)
				usage()
			}
			course = a
		}
		exitWith(runFetch(course))
	}
	if len(os.Args) < 3 {
		usage()
	}
	command := os.Args[1]

	// Hand-rolled rather than `flag`: the stage is positional and must be accepted
	// on either side of the flags, because `sboot submit 01-boot --force` is what a
	// learner will actually type and stdlib `flag` stops parsing at the first
	// positional.
	var stage string
	var force bool
	for _, a := range os.Args[2:] {
		switch {
		case a == "--force" || a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "sboot: unknown flag %q\n", a)
			usage()
		case stage == "":
			stage = a
		default:
			fmt.Fprintf(os.Stderr, "sboot: unexpected argument %q\n", a)
			usage()
		}
	}
	if stage == "" {
		usage()
	}
	if regexp.MustCompile(`^\d+`).FindString(stage) == "" {
		fmt.Fprintf(os.Stderr, "sboot: stage %q must start with its lab number (e.g. 01-boot)\n", stage)
		exitWith(2)
	}
	if force && command != "submit" {
		fmt.Fprintf(os.Stderr, "sboot: --force only means something for `sboot submit`\n")
		exitWith(2)
	}

	r, err := findRepo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}

	switch command {
	case "test":
		runTest(r, stage)
	case "submit":
		runSubmit(r, stage, force)
	default:
		usage()
	}
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

func runTest(r repo, stage string) {
	run := prepare(r, stage)

	// Failure guidance: read the per-check consecutive-failure counters BEFORE
	// grading, so we can tell the grader which checks have earned the Layer 2
	// ladder (the failure-guidance spec). The grader itself stays historyless.
	st := loadState()
	res := runGrader(run, r.course, stage, st.tierSpec(r.course, stage), "")
	if res.launchFailed {
		reportGraderMissing(res.launchErr)
		exitWith(2)
	}
	st.record(r.course, stage, res.checks)
	if err := st.save(); err != nil {
		debugf("could not save guidance state: %v", err)
	}

	if err := reportPractice(r.course, stage, res.score, res.max, res.passed, res.detail); err != nil {
		reportPracticeFailure(err)
	}
	exitWith(res.exitCode)
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
			fmt.Fprintf(os.Stderr, "\nsboot: the platform did not accept your token: %s\n", ae.msg)
			fmt.Fprintf(os.Stderr, "sboot: get a fresh one at %s/account and set SBOOT_TOKEN.\n", apiURL())
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
func reportGraderMissing(err error) {
	tool := missingTool(err)
	if tool == "" {
		fmt.Fprintf(os.Stderr, "sboot: could not run the grader: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "sboot: %s is not installed (or is not on your PATH).\n", tool)
	}
	for _, line := range installHint(tool) {
		fmt.Fprintf(os.Stderr, "sboot:   %s\n", line)
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
func reportSubmitGraderMissing(stage string, err error) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "sboot: nothing was submitted.")
	fmt.Fprintln(os.Stderr, "sboot: `sboot submit` runs `sboot test` on your machine and sends the captured")
	fmt.Fprintln(os.Stderr, "sboot: result to us for the official grade, so it needs the same tools")
	fmt.Fprintln(os.Stderr, "sboot: `sboot test` does — and they are not there yet:")
	fmt.Fprintln(os.Stderr)
	reportGraderMissing(err)
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
func installHint(tool string) []string {
	switch tool {
	case "cargo", "rustc", "rustup":
		return []string{
			"install Rust (nightly is pinned by the course's rust-toolchain.toml):",
			"curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh",
		}
	case "nasm":
		return []string{pkgInstall("nasm")}
	case "qemu-system-x86_64", "qemu":
		return []string{pkgInstall("qemu-system-x86")}
	case "make", "gcc", "cc", "ld":
		return []string{pkgInstall("build-essential")}
	case "":
		return []string{"the course's build needs a working toolchain — check `sboot where`."}
	default:
		return []string{pkgInstall(tool)}
	}
}

// pkgInstall renders the package-manager line for this OS. macOS gets Homebrew and
// Linux gets apt: both are the overwhelmingly common case, and a learner on neither
// still reads a line that names the package they need.
func pkgInstall(pkg string) string {
	switch runtime.GOOS {
	case "darwin":
		if pkg == "build-essential" {
			return "xcode-select --install"
		}
		return "brew install " + strings.TrimSuffix(pkg, "-system-x86")
	default:
		return "sudo apt install " + pkg
	}
}

// apiError is a reply we DID get: the platform answered, and the answer was no.
// It carries the status so callers can separate "refused" from "unreachable".
type apiError struct {
	status int
	msg    string
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
			fmt.Printf("            tooling, staged beside your os/.\n")
			// Say which way os/ got there, because the answer decides where cargo
			// writes and this command's whole job is to answer "where?" honestly.
			// The symlink is the fast path (edits are picked up with no copy step),
			// and its cost is that target/ lands in the repo rather than here — which
			// is what `sboot start`'s .gitignore is for. Claiming "so your repo stays
			// clean" was the opposite of what the symlink does.
			if linked, err := os.Readlink(filepath.Join(run, "os")); err == nil && linked == r.osDir() {
				fmt.Printf("            os/ there is a symlink to yours, so cargo's target/ dirs land\n")
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
}

type graderRun struct {
	checks     []localCheck
	score, max int
	passed     bool
	detail     string
	exitCode   int
	// The grader could not be STARTED (no cargo on PATH, not a staging root) — which
	// is a different thing from a grader that ran and failed the code, and the two
	// have different consequences on the submit path: `--force` may proceed past
	// the second but never silently past the first.
	launchFailed bool
	launchErr    error
}

// parseVerdict reads the grader's protocol output (grader.EmitProtocol):
//
//	LBX_CHECK\t{PASS|FAIL}\t{points}\t{desc}\t{guidance}\t{id}
//	LBX_SCORE\t{got}\t{total}
//
// Fields are append-only, so accept >= 4 and treat 5 and 6 as optional — the same
// rule (and the same reason) as the server-side parser: a record from a grader
// newer than this binary must still parse, or every check silently vanishes.
//
// The guidance field is ignored here. The grader has already rendered it, capped at
// the tier we asked for; re-rendering it in the CLI would be a second place for the
// "never echo the rubric" rule to be got wrong.
func parseVerdict(out string) (checks []localCheck, score, max int) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		switch {
		case len(f) >= 4 && f[0] == "LBX_CHECK":
			c := localCheck{pass: f[1] == "PASS", desc: f[3]}
			c.points, _ = strconv.Atoi(f[2])
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
		}
	}
	return
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

func authedRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+env("SBOOT_TOKEN", "sboot-dev-token"))
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

	if passed {
		fmt.Printf("\n→ practice run recorded ✅ — %s\n", r.Hint)
	} else {
		fmt.Printf("\n→ practice run recorded (%d/%d) — keep going.\n", score, max)
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
	Err string `json:"error"`
}

// runSubmit is a thin wrapper so the body can `return` an exit code and let its
// defers run — the temp capture file needs cleanup, and `os.Exit` skips defers.
func runSubmit(r repo, stage string, force bool) {
	exitWith(submit(r, stage, force))
}

func submit(r repo, stage string, force bool) int {
	osDir := r.osDir()
	if st, err := os.Stat(osDir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "sboot: no os/ tree at %s\n", osDir)
		return 2
	}
	course := r.course

	// Ask whether this stage is even open to us before spending a build and a boot
	// on it. Being told "finish the previous lab first" after a two-minute local
	// run is the one bad outcome the gate creates, and one cheap GET removes it.
	if ae := submitPreflight(course, stage); ae != nil {
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

	fmt.Printf("── checking %s locally first (one build, one boot)\n", stage)
	st := loadState()
	res := runGrader(run, course, stage, st.tierSpec(course, stage), capturePath)

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
		reportSubmitGraderMissing(stage, res.launchErr)
		return 2
	case res.exitCode != 0 && !force:
		reportGateFailure(stage, res)
		return 1
	case res.exitCode != 0:
		fmt.Printf("\n── local check FAILED (%s) — submitting anyway (--force)\n", scoreText(res))
	default:
		fmt.Printf("\n── local check passed (%s)\n", scoreText(res))
	}

	fmt.Println("── packaging os/ (source only)")
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
		if created.Err != "" {
			fmt.Fprintf(os.Stderr, "sboot: submission rejected: %s (HTTP %d)\n", created.Err, resp.StatusCode)
		} else {
			fmt.Fprintf(os.Stderr, "sboot: unexpected response (HTTP %d)\n", resp.StatusCode)
		}
		return 2
	}
	fmt.Printf("── submitted (%d KB) — graded on the server\n", len(body)/1024)

	// THE VERDICT IS ALREADY IN `created`. The platform judges the capture inside
	// the request that carried it, so there is no queue to wait behind and nothing
	// to poll — the five-minute deadline, the `.`/`!` progress dots and the Ctrl-C
	// window they created are all gone with the runner that made them necessary.
	if created.Detail != "" {
		fmt.Println()
		fmt.Println(indent(created.Detail, "  "))
	}
	switch created.Status {
	case "passed":
		fmt.Printf("\n  ✅ official grade: %d/%d — stage complete!", deref(created.Score), deref(created.MaxScore))
		if created.NextStage != nil {
			// OPPORTUNISTIC PREFETCH. Completing this lab is the moment the next one
			// becomes fetchable (sequential unlock just opened it), and it is also the
			// moment the learner is most likely to close the laptop. Pull its tests now
			// so a per-lab fetch never becomes the reason someone cannot start work on
			// a train. Best-effort and silent: it is an optimisation, and failing it
			// costs one download later.
			if created.NextStage.Live {
				prefetchLab(course, created.NextStage.ID)
				fmt.Printf(" Next up: %s\n", created.NextStage.ID)
			} else {
				fmt.Printf(" Next up: %s (coming soon)\n", created.NextStage.Title)
			}
		} else {
			fmt.Println(" Course complete!")
		}
		return 0
	case "failed":
		fmt.Printf("\n  ❌ official grade: %d/%d\n", deref(created.Score), deref(created.MaxScore))
		// This line used to read "the server rubric includes hidden checks; make it
		// work, not just print". D1 removed the hidden checks, so blaming a stricter
		// rubric would now be false — and D3 makes the honest reading exact. If the
		// local gate went green and the server did not, the two runs graded the SAME
		// checks, so what differs is the two machines.
		if res.exitCode == 0 && !res.launchFailed {
			fmt.Println("     Your local run passed, so the difference is between your machine and")
			fmt.Println("     ours, not between two sets of checks. Please report it.")
		}
		return 1
	case "error":
		fmt.Println("\n  ⚠ grading error on the server — please retry; if it persists, report it.")
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
		fmt.Println("\n  ⚠ your work was uploaded, but nothing graded it. This is a problem on our")
		fmt.Println("     side, not with your code — nothing about your submission was judged.")
		fmt.Printf("     Run `sboot submit %s` again in a few minutes. If it happens twice in a\n", stage)
		fmt.Println("     row the grading service is down, and re-submitting will not help.")
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
func reportGateFailure(stage string, res graderRun) {
	fmt.Printf("\n── not submitted: the local check did not pass (%s)\n", scoreText(res))
	fmt.Println("   `sboot submit` grades the same checks you just ran, so fix these first —")
	fmt.Println("   no submission was created and nothing was sent.")
	fmt.Printf("   Think the local grader is wrong, or want this failure on the record?\n")
	fmt.Printf("     sboot submit %s --force\n", stage)
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
		Err string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		if r.Err == "" {
			r.Err = "refused"
		}
		return &apiError{status: resp.StatusCode, msg: r.Err}
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

// ── start: create the learner's repo ────────────────────────────────────────────
//
// What lands here is ONLY the learner's half — `os/`, the build config, and the four
// files this command writes itself (sboot.toml, LICENSE, .gitignore, README.md). The
// tests and the grading engine go to the cache in the same run, so the learner ends
// up ready to grade without ever having had our harness inside their repo.
//
// Both destinations are printed, because two directories is only a support burden if
// nobody says where they are.
func runStart(course string) {
	dest := course
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		fmt.Fprintf(os.Stderr, "sboot: ./%s already exists and is not empty — refusing to overwrite\n", dest)
		exitWith(2)
	}

	fmt.Printf("── fetching the %s starter tree\n", course)
	req, err := authedRequest("GET", apiURL()+"/api/v1/courses/"+course+"/workspace", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}
	resp, err := send(&http.Client{Timeout: 120 * time.Second}, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: could not reach the platform (%v)\n", err)
		exitWith(2)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Err string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Err == "" {
			e.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		fmt.Fprintf(os.Stderr, "sboot: %s\n", e.Err)
		// `sboot start` is the FIRST command anyone runs, so a bare "invalid or missing
		// token" is a dead end — the reader has no idea a token exists or where to get
		// one. The default SBOOT_TOKEN is a local dev value that can never work against
		// a real deployment, so this is the expected first failure, not an edge case.
		if resp.StatusCode == http.StatusUnauthorized {
			fmt.Fprintf(os.Stderr, "\n  Get your token at %s/account, then:\n", siteURL())
			fmt.Fprintf(os.Stderr, "    export SBOOT_TOKEN=<the token>\n")
			fmt.Fprintf(os.Stderr, "    sboot start %s\n", course)
		}
		exitWith(2)
	}

	if err := extractTarGz(resp.Body, dest); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: extract: %v\n", err)
		exitWith(2)
	}

	// The tests + grader, into the cache. Deliberately AFTER the scaffold: if this
	// half fails (offline mid-setup) the learner still has their repo and a later
	// `sboot test` will finish the job, rather than being left with nothing.
	title, firstStage := course, ""
	if s, err := ensureSpec(course, ""); err == nil {
		if m, err := fetchManifest(course); err == nil {
			if m.Title != "" {
				title = m.Title
			}
			for _, l := range m.Labs {
				if l.Live {
					firstStage = l.Stage
					break
				}
			}
		}
		// Only the FIRST lab's tests. Per-lab download is the point: tests for labs
		// this learner has not reached never touch their disk
		// (the workspace-split design "Per-lab test download, not per-course").
		if firstStage != "" {
			if err := fetchLab(course, firstStage, s.dir); err != nil {
				debugf("first lab not fetched at start: %v", err)
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "sboot: note — the tests are not cached yet (%v)\n", err)
		fmt.Fprintln(os.Stderr, "sboot: run `sboot fetch` when you're back online.")
	}

	if err := writeRepoFiles(dest, course, title, firstStage); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		exitWith(2)
	}

	abs, _ := filepath.Abs(dest)
	fmt.Printf("\n── your repo is ready: %s\n", abs)
	fmt.Printf("   os/ is yours. sboot.toml says which course this is.\n")
	fmt.Printf("   Our tests and grader are NOT in here — they live in the %s data dir,\n", brandName)
	fmt.Printf("   so publishing this repo publishes your kernel and nothing else.\n")
	if s, ok := cachedSpec(course); ok {
		fmt.Printf("   tests + grader: %s\n", s.dir)
	}
	firstTest := firstStage
	if firstTest == "" {
		firstTest = "01-boot"
	}
	fmt.Printf(`
  cd %s
  git init && git add . && git commit -m "start %s"
  sboot where              # both paths, any time
  sboot test %s        # first practice run
`, dest, course, firstTest)
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
