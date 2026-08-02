// sboot — the learner's CLI. Two-tier grading (the content-protection rules):
//
//	sboot test <stage>    practice — builds locally, then execs the grading engine
//	                    (grade.go) against the published checks; fast,
//	                    offline-friendly, recorded as a practice run, never
//	                    completes a stage.
//	sboot submit <stage>  official — gates on a local run, then uploads the os/
//	                    source tree; the platform's runner builds it with the pinned
//	                    toolchain, boots it in QEMU server-side, and grades the full
//	                    rubric. Only a passing submission completes.
//
// ── THE SUBMIT GATE (D3, the grading design) ──────────────────
//
//	build once → boot once → capture
//	judge locally (published checks) → instant feedback
//	  FAIL → stop. No submission is created, no queue wait.
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
//  3. INSTANT FEEDBACK BEFORE THE QUEUE. A failing run is answered by the grader
//     in the learner's own terminal, with the full guidance ladder, instead of
//     minutes later by a server.
//
// The server still builds and boots independently — that is what measures
// environment divergence between machines, and removing it would collapse the two
// dual-run measurements into one (see grader.ClientVerdict).
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
// ran and failed the code.
func reportGraderMissing(err error) {
	fmt.Fprintf(os.Stderr, "sboot: could not run the grader: %v\n", err)
	fmt.Fprintln(os.Stderr, "sboot: is the Rust toolchain installed? (rustup, nasm, qemu)")
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
// rule (and the same reason) as the runner's parser: a record from a grader newer
// than this binary must still parse, or every check silently vanishes.
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
	return req, nil
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
	// Beta-1 dual-run: when present, the server asks us to ALSO upload the machine
	// state from our local run. The authoritative verdict is still the server's;
	// this only feeds the client-vs-server comparison.
	Shadow *shadowAssignment `json:"shadow"`
	Err    string            `json:"error"`
}

type shadowAssignment struct {
	Nonce string `json:"nonce"`
	// UNREAD by this binary, and kept only because the wire shape is append-only
	// and the platform still sends it. The capture the gate uploads is bounded by
	// the stage's own `timeout_secs` and settles on the stage's own marker — the
	// same window the server's judge uses, which is the point — so a server-supplied
	// timeout has nothing left to govern. It was last read by the pre-D3 fallback
	// capture, deleted with the shell-out in Phase 4.
	CaptureTimeoutSecs int `json:"capture_timeout_secs"`
	TTLSecs            int `json:"ttl_secs"`
}

// runSubmit is a thin wrapper so the body can `return` an exit code and let its
// defers run — the temp capture file and the bounded wait for the capture upload
// both need cleanup, and `os.Exit` skips defers.
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
	// Note this is not the rule the runner's `maxTier` implements. That one caps
	// what the SERVER's verdict renders, and stays at tier 1: the server has
	// nothing honest to escalate to. Local rendering is local rendering.
	if !force {
		st.record(course, stage, res.checks)
		if err := st.save(); err != nil {
			debugf("could not save guidance state: %v", err)
		}
	}

	switch {
	case res.launchFailed && !force:
		reportGraderMissing(res.launchErr)
		fmt.Fprintf(os.Stderr, "sboot: to submit without the local check: sboot submit %s --force\n", stage)
		return 2
	case res.launchFailed:
		fmt.Fprintf(os.Stderr, "\nsboot: the local check could not run (%v) — submitting anyway (--force).\n",
			res.launchErr)
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

	url := fmt.Sprintf("%s/api/v1/submissions?course=%s&stage=%s", apiURL(), course, stage)
	req, err := authedRequest("POST", url, bytes.NewReader(archive))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	req.Header.Set("Content-Type", "application/gzip")
	// SPEC-SKEW SEAM (the CLI release policy §6). Which tests produced the verdict the
	// learner is about to see? Under the `server` backend the answer cannot change
	// the grade — the runner grades with the in-repo template — but under the probe
	// backend the capture is produced with THIS spec's script, settle marker and
	// timeout, so a stale spec must be refused with a message that names the cause
	// rather than silently judged. The server ignores this header today; the refusal
	// belongs in platform/app/api/v1/submissions/route.ts once a backend consumes a
	// client capture, and lib/spec.ts `specStamp` is the value to compare against.
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
	fmt.Printf("── submitted (%d KB) — grading on the server", len(archive)/1024)

	// Beta-1 dual-run: hand the server the machine state from the run above, so its
	// shadow judgement acts on the same bytes our local judge just scored. Nothing
	// is built or booted again. Best-effort in a goroutine: the upload is small, but
	// an old cached spec can still fall back to a real capture, and no failure here
	// may affect the official verdict or the exit code.
	var shadowDone chan struct{}
	if created.Shadow != nil {
		shadowDone = make(chan struct{})
		go func() {
			defer close(shadowDone)
			uploadCapture(run, created.ID, capturePath, created.Shadow)
		}()
	}
	// Wait (bounded) for the capture upload before exiting, so a fast server verdict
	// doesn't kill it mid-flight. Everything it does has its own timeout, so this
	// always returns.
	defer func() {
		if shadowDone != nil {
			select {
			case <-shadowDone:
			case <-time.After(90 * time.Second):
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Minute)
	last := "pending"
	for {
		if time.Now().After(deadline) {
			fmt.Printf("\nsboot: still %s after 5 minutes — check the dashboard later.\n", last)
			return 2
		}
		time.Sleep(2 * time.Second)
		s, err := fetchSubmission(created.ID)
		if err != nil {
			fmt.Print("!")
			continue
		}
		if s.Status == last || s.Status == "pending" || s.Status == "running" {
			fmt.Print(".")
			last = s.Status
			continue
		}
		fmt.Println()
		if s.Detail != "" {
			fmt.Println()
			fmt.Println(indent(s.Detail, "  "))
		}
		switch s.Status {
		case "passed":
			fmt.Printf("\n  ✅ official grade: %d/%d — stage complete!", deref(s.Score), deref(s.MaxScore))
			if s.NextStage != nil {
				// OPPORTUNISTIC PREFETCH. Completing this lab is the moment the next
				// one becomes fetchable (sequential unlock just opened it), and it is
				// also the moment the learner is most likely to close the laptop. Pull
				// its tests now so a per-lab fetch never becomes the reason someone
				// cannot start work on a train. Best-effort and silent: it is an
				// optimisation, and failing it costs one download later.
				if s.NextStage.Live {
					prefetchLab(course, s.NextStage.ID)
					fmt.Printf(" Next up: %s\n", s.NextStage.ID)
				} else {
					fmt.Printf(" Next up: %s (coming soon)\n", s.NextStage.Title)
				}
			} else {
				fmt.Println(" Course complete!")
			}
			return 0
		case "failed":
			fmt.Printf("\n  ❌ official grade: %d/%d\n", deref(s.Score), deref(s.MaxScore))
			// This line used to read "the server rubric includes hidden checks; make
			// it work, not just print". D1 removed the hidden checks, so blaming a
			// stricter rubric would now be false — and D3 makes the honest reading
			// exact. If the local gate went green and the server did not, the two
			// runs graded the SAME checks, so what differs is the two machines.
			if res.exitCode == 0 && !res.launchFailed {
				fmt.Println("     Your local run passed, so the difference is between your machine and")
				fmt.Println("     ours, not between two sets of checks. Please report it.")
			}
			return 1
		default: // error
			fmt.Println("\n  ⚠ grading error on the server — please retry; if it persists, report it.")
			return 2
		}
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

// uploadCapture hands the server the machine state for the dual-run comparison —
// inert bytes only (serial + VGA + reset: NO source, NO rubric) — under the
// single-use nonce the server issued.
//
// `prepared` is the capture the GATE already wrote: the very boot whose verdict the
// learner just saw. That is what makes the server's shadow judgement act on the
// same evidence rather than on a re-run, and it is why a submit no longer costs a
// second build and a second boot.
//
// Entirely best-effort: every failure is logged (only under SBOOT_DEBUG) and
// swallowed, because the authoritative verdict is the server's regardless.
func uploadCapture(run, id, prepared string, a *shadowAssignment) {
	dbg := os.Getenv("SBOOT_DEBUG") != ""
	logf := func(format string, args ...any) {
		if dbg {
			fmt.Fprintf(os.Stderr, "\nsboot: shadow: "+format+"\n", args...)
		}
	}

	blob := readCapture(prepared)
	if blob == nil {
		// Nothing to compare. There is no longer a "the workspace pinned an xtask too
		// old for --capture-out" case to fall back from — that fallback, a second
		// build and a second boot via `cargo xtask capture`, was deleted with the
		// shell-out in Phase 4, and the engine's capture flag is not optional now that
		// the CLI fetches the engine with the tests. What is left is a real local
		// failure: the gate could not run, or the blob could not be written. Either
		// way the authoritative verdict is the server's, so this costs the shadow
		// comparison and nothing else.
		logf("the local check produced no capture; skipping the dual-run comparison")
		return
	}

	url := fmt.Sprintf("%s/api/v1/submissions/%s/capture", apiURL(), id)
	req, err := authedRequest("POST", url, bytes.NewReader(blob))
	if err != nil {
		logf("request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sboot-Shadow-Nonce", a.Nonce)
	resp, err := send(&http.Client{Timeout: 30 * time.Second}, req)
	if err != nil {
		logf("upload: %v", err)
		return
	}
	resp.Body.Close()
	logf("uploaded capture (%d bytes) → HTTP %d", len(blob), resp.StatusCode)
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

func fetchSubmission(id string) (*submissionResp, error) {
	req, err := authedRequest("GET", apiURL()+"/api/v1/submissions/"+id, nil)
	if err != nil {
		return nil, err
	}
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var s submissionResp
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
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
		// Tar entry names are "/"-separated BY SPEC, and the runner's extractor reads
		// them that way (runner/main.go, which uses filepath.ToSlash for the same
		// reason). A Windows walk hands us `kernel\main.rs`, which the extractor takes
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
