// OUR half of the old workspace: the course's tests (`labs/<stage>/lab.toml`) and its
// build tooling, cached in the sboot data directory and never in the learner's git
// repo (the workspace-split design "Resolved (2026-07-26)").
//
//	~/.local/share/sboot/courses/<course>/
//	├── current                  ← one line: the active spec version
//	├── <spec-version>/          ← a whole spec, immutable once written
//	│   ├── spec.json              the manifest this version was built from
//	│   ├── LICENSE                all rights reserved — NOT the repo's MIT.
//	│   │                          Always present; see engineMarker.
//	│   ├── xtask/                 the course's BUILD tooling, if it has any
//	│   ├── .cargo/ + rust-toolchain.toml   likewise (Rust courses only)
//	│   └── labs/<stage>/lab.toml  the tests, one lab at a time
//	└── work/<repo-key>/         ← scratch build root, one per repo (see stageRun)
//
// THE GRADER IS IN HERE, as `bin/sboot-judge` — a compiled binary fetched per
// platform, digest-checked and exec'd, and NOT part of this program (it is not
// published with the MIT CLI; see the grading-engine distribution decision). What else is cached is
// a course's own build tooling and its tests — so a C course caches a LICENCE, some
// TOML and the engine, and needs no Rust anywhere.
//
// It has been three things in a month, which is why every comment about it is dated:
// a Rust crate in the learner's repo (`xtask/`), then code compiled into this binary
// (Phase 4 of the Rust→Go port, 2026-08-02), then this. The middle one lasted hours —
// it made publishing the CLI mean publishing the engine under an irreversible licence.
//
// macOS uses ~/Library/Application Support/sboot, Windows %LOCALAPPDATA%\sboot.
// SBOOT_CACHE_DIR overrides all of it (tests use that rather than a real home).
//
// ── WHY THE DIRECTORY IS VERSIONED ──────────────────────────────────────────────
// Because that is what makes a refresh safe: fetch alongside, flip a pointer, keep
// the old copy. A stale spec is a *different directory*, never an overwritten one, so
// a learner who is offline keeps a working grader, and downgrading a binary never
// forces a re-download. The version is a content hash of the engine plus every
// lab.toml, computed by scripts/build-workspace-template.sh — equality is all the
// staleness rule needs (the CLI release policy §6).
//
// ── WHY FETCHING IS PER LAB ──────────────────────────────────────────────────────
// A lab's tests arrive when the learner reaches that lab, so tests for labs they have
// not reached never touch their disk. content-protection.md ranks unpublished future
// stages above hidden rubrics by leak damage, and no licence achieves that — only the
// structure does. Two affordances keep that from ever blocking work at a lab
// boundary: the next lab is prefetched when one is completed, and `sboot fetch <course>`
// pulls everything for someone deliberately going offline.
//
// ── OFFLINE IS A HARD REQUIREMENT ───────────────────────────────────────────────
// architecture.md: practice "works offline; recorded as unverified telemetry, never
// completes a stage." So every network call in this file FAILS OPEN. If the platform
// cannot be reached and a usable spec is already cached, we grade with it and say
// nothing louder than a debug line. `SBOOT_OFFLINE=1` skips the network entirely.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// specManifest is GET /api/v1/courses/:id/spec. Fields are append-only, like every
// other /api/v1 shape: an older binary must keep working against a newer manifest.
type specManifest struct {
	Course  string `json:"course"`
	Title   string `json:"title"`
	Version string `json:"version"`
	// The minimum `sboot` this spec needs, or "". ENFORCED since 2026-08-02 — see
	// minCLIRefusal, and the CLI release policy §6 ("old binary + new spec refuses
	// THAT LAB by name"). It was carried-but-inert for a month before that, in the
	// wire shape from day one, so turning it on needed no manifest change and no
	// server deploy — which is the only reason it could be switched on in the same
	// commit as the change that made it load-bearing.
	MinCLI string `json:"min_cli"`
	// The learner's source directory name, or "" for the historical default "os"
	// (repo.go, defaultTree). Added 2026-08-18 so a course whose subject is not an
	// operating system stops shipping a top-level `os/`. Absent from every manifest
	// written before then, and unknown to every binary released before then — which
	// is why the empty value has to mean "os" rather than being an error, and why
	// `sboot start` only writes the sboot.toml line when it differs from the default.
	Tree string `json:"tree"`
	// The course's BUILD TOOLING bundle (`xtask/` and cargo config for a Rust
	// course; nothing but the LICENCE for a Makefile one). Named `engine` for the
	// oldest of reasons — it used to hold the Rust grader — and kept because
	// /api/v1 shapes are append-only.
	Engine struct {
		Digest string `json:"digest"`
	} `json:"engine"`
	// The GRADING ENGINE, which is a compiled binary and therefore per platform.
	// Added 2026-08-02; absent from an older platform's manifest, which is exactly
	// what judgeRelease reports as "this course publishes no engine for you".
	Judge specJudge `json:"judge"`
	// How this course is BUILT and where its artifact lands — course.yaml's
	// `grader:` block, served by the platform's getGrader. The CLI runs the build
	// itself and passes the artifact path to the engine; it once hardcoded
	// `cargo xtask grade`, which is exactly why a C or Zig course could not have used
	// the same CLI (docs/multi-language-options.md).
	//
	// Deliberately NOT part of the spec version's content hash: it is configuration
	// rather than content, and renaming a build command must not invalidate every
	// learner's cached tests. It rides on the manifest response next to `title` for
	// the same reason.
	Grader specGrader `json:"grader"`
	Labs   []specLab  `json:"labs"`
	Err    string     `json:"error"`
}

// specGrader mirrors the platform's `Grader` interface (platform/lib/content.ts).
// Only Build and Artifact are read here; the two judge commands are server-side
// overrides and are carried so a cached spec.json round-trips whole.
type specGrader struct {
	Build        string `json:"build"`
	Judge        string `json:"judge"`
	JudgeCapture string `json:"judgeCapture"`
	Artifact     string `json:"artifact"`
	// The capture shape ("boot" | "cmd", 2026-08-13). Carried for round-trip
	// fidelity like the judge overrides: the ENGINE decides how to capture from
	// the rubric's own `[[run]]` block, so the CLI reads nothing from this — a
	// cmd-capture lab simply never boots, and Artifact goes unused.
	Capture string `json:"capture"`
}

// specJudge is the grading engine's release, keyed by "<goos>-<goarch>".
//
// A MAP RATHER THAN A LIST, and the key is the same tuple the User-Agent already
// carries, so the CLI asks for a name it computes from its own build constants and
// never has to interpret a list it may not understand.
type specJudge struct {
	Platforms map[string]specJudgeBinary `json:"platforms"`
}

// specJudgeBinary is what a learner is about to EXECUTE, so it carries the digest
// that has to match before it may (fetchJudge, judgeBinary).
type specJudgeBinary struct {
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

type specLab struct {
	Stage  string `json:"stage"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
	Live   bool   `json:"live"`
	// The human title (ux-plan §7.3, additive): what the status rows and the
	// `grading <stage> — "<title>"` header render. Absent from an older
	// platform's manifest, and every consumer falls back to the stage id.
	Title string `json:"title"`
}

func (m *specManifest) lab(stage string) *specLab {
	for i := range m.Labs {
		if m.Labs[i].Stage == stage {
			return &m.Labs[i]
		}
	}
	return nil
}

// spec is a materialised spec version on disk: where it is and what it is.
type spec struct {
	course  string
	version string
	dir     string // <cache>/courses/<course>/<version>
}

// engineMarker is the one file EVERY build-tooling bundle carries, whatever the
// course: scripts/build-workspace-template.sh always writes it. See hasTooling.
const engineMarker = "LICENSE"

// judgeDir / judgeName: where the compiled grading engine lands, and what it is
// called. `bin/` rather than the spec root so that stageRun can skip it by name and
// not copy 3 MB into every repo's staging dir (the engine is exec'd where it lies).
const judgeDir = "bin"
const judgeName = "sboot-judge"

// hasTooling reports whether the course's BUILD TOOLING bundle has been extracted
// here. Keys on the LICENCE, which is the one file every such bundle carries
// whatever the course: scripts/build-workspace-template.sh always writes it.
//
// It used to key on `xtask/`, which stopped being universal on 2026-08-02 — os-c
// builds with a Makefile in the learner's own `os/`, so its tooling bundle is the
// LICENCE and nothing else. Keying on xtask/ would have made that course
// permanently "not cached": every command would refetch, every fetch would succeed,
// and cachedSpec would still say no.
func (s spec) hasTooling() bool {
	st, err := os.Stat(filepath.Join(s.dir, engineMarker))
	return err == nil && !st.IsDir()
}

// judgePath is the grading engine for THIS platform inside this spec version.
func (s spec) judgePath() string {
	return filepath.Join(s.dir, judgeDir, judgeName+exeSuffix())
}

// hasJudge reports whether the engine binary is here.
func (s spec) hasJudge() bool {
	st, err := os.Stat(s.judgePath())
	return err == nil && !st.IsDir()
}

// materialised reports whether this spec version can actually grade: the course's
// build tooling AND the engine that scores it.
//
// BOTH, since 2026-08-02, and the "both" is the point. The engine stopped being
// compiled into `sboot` and became a fetched binary, so a spec dir holding the
// tooling and no engine is a dir that looks cached and cannot produce a verdict.
// That is precisely the shape the grading-engine distribution decision §4 calls the one
// unacceptable outcome — a silent wrong number — so it is folded into the question
// every caller already asks.
func (s spec) materialised() bool { return s.hasTooling() && s.hasJudge() }

// exeSuffix is Windows' `.exe`, and nothing anywhere else.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// platformKey names the engine build this binary needs: the same `<goos>-<goarch>`
// tuple the User-Agent already reports, computed from THIS build's constants rather
// than read off anything, so it cannot be talked into fetching a foreign binary.
func platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// buildToolDir is the course's own build tooling, when it has any — `cargo xtask
// build` for the Rust courses, nothing at all for a Makefile course. NOT the grader:
// that is judgePath, a separate binary.
func (s spec) buildToolDir() string       { return filepath.Join(s.dir, "xtask") }
func (s spec) labsDir() string            { return filepath.Join(s.dir, "labs") }
func (s spec) labDir(stage string) string { return filepath.Join(s.labsDir(), stage) }

func (s spec) hasLab(stage string) bool {
	st, err := os.Stat(filepath.Join(s.labDir(stage), "lab.toml"))
	return err == nil && !st.IsDir()
}

// cachedManifest reads the manifest this spec version was materialised from
// (written beside it by `materialise`). Offline it is the only record of which labs
// the course HAS, as opposed to which ones have been downloaded — which is what
// separates a mistyped lab name from one that simply has not been fetched yet.
//
// Best effort: nil for a cache written before spec.json existed, and possibly a
// version or two behind the course, which is why its callers say so.
func (s spec) cachedManifest() *specManifest {
	b, err := os.ReadFile(filepath.Join(s.dir, "spec.json"))
	if err != nil {
		return nil
	}
	var m specManifest
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

// cachedLabs lists the labs this spec version has on disk, sorted.
func (s spec) cachedLabs() []string {
	entries, err := os.ReadDir(s.labsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && s.hasLab(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ── where things live ───────────────────────────────────────────────────────────

// cacheDir is the sboot DATA directory — our tests and grader, as opposed to
// ~/.config/sboot/state.json which holds the learner's own state (the failure-ladder
// counters). The pairing is deliberate: XDG config for state, XDG data for data.
func cacheDir() (string, error) {
	return dataDirFor(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// dataDirFor is cacheDir with the OS, the environment and the home directory
// injected.
//
// WHY THE INDIRECTION. `scripts/install.sh` has to write its install marker into
// THIS directory — `sboot upgrade` reads it back to decide whether it may replace
// the binary (the CLI release policy §4) — so a shell script and this Go function
// must agree about where it is, on each OS, forever. A test can only prove that on
// the machine it runs on unless the OS is a parameter, and the machine doing the
// fixing is a Linux one. Same idiom, same reason, as `tarGzDirSep` in archive.go:
// a rule nobody can watch go green is a rule nobody has verified.
// (harness/cache_test.go compares both against the script's own resolver.)
func dataDirFor(goos string, getenv func(string) string, userHome func() (string, error)) (string, error) {
	if d := getenv("SBOOT_CACHE_DIR"); d != "" {
		return d, nil
	}
	switch goos {
	case "windows":
		// %LOCALAPPDATA% — machine-local, not roamed. A cargo target dir has no
		// business being synced to a domain profile.
		if d := getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "sboot"), nil
		}
	case "darwin":
		home, err := userHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "sboot"), nil
	}
	if d := getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "sboot"), nil
	}
	home, err := userHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "sboot"), nil
}

// installMarkerName is the file `scripts/install.sh` drops beside the cache to
// record HOW this binary was installed. `sboot upgrade` (§4) reads it so it never
// fights Homebrew, a distro package or a `go install` — "refuse rather than
// fight", and every refusal names the command that does work.
//
// It has to be written by the FIRST release or the first cohort is permanently
// undetectable: nothing can retroactively add a marker to an install that already
// happened, and path heuristics alone cannot tell a curl install from a manual
// download.
const installMarkerName = "install.json"

func courseCacheDir(course string) (string, error) {
	base, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "courses", course), nil
}

// currentVersion reads the pointer file. Empty means nothing is cached yet.
func currentVersion(course string) string {
	dir, err := courseCacheDir(course)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// setCurrentVersion flips the pointer. Written to a temp file and renamed, so a
// crash mid-write can never leave a pointer to a half-fetched spec.
func setCurrentVersion(course, version string) error {
	dir, err := courseCacheDir(course)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "current.tmp")
	if err := os.WriteFile(tmp, []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "current"))
}

// cachedSpec returns the spec the pointer names, if it is materialised.
func cachedSpec(course string) (spec, bool) {
	v := currentVersion(course)
	if v == "" {
		return spec{}, false
	}
	dir, err := courseCacheDir(course)
	if err != nil {
		return spec{}, false
	}
	s := spec{course: course, version: v, dir: filepath.Join(dir, v)}
	if !s.materialised() {
		return spec{}, false
	}
	return s, true
}

// courseGrader is the course's build manifest as of the last time we spoke to the
// platform, read back out of the spec.json `materialise` cached alongside the tests.
//
// It has to come from the cache rather than from a live call, for the same reason
// the tests do: `sboot test` must grade with no network at all. A course cached
// before the platform started sending this (or one that never overrode it) yields
// the zero value, and grade.go falls back to the Rust/QEMU defaults — so an older
// cache keeps building exactly as it did.
func courseGrader(course string) specGrader {
	s, ok := cachedSpec(course)
	if !ok {
		return specGrader{}
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "spec.json"))
	if err != nil {
		return specGrader{}
	}
	var m specManifest
	if err := json.Unmarshal(b, &m); err != nil {
		debugf("cached spec.json for %s is unreadable (%v); using the default grader manifest", course, err)
		return specGrader{}
	}
	return m.Grader
}

// ── fetching ────────────────────────────────────────────────────────────────────

func offline() bool { return os.Getenv("SBOOT_OFFLINE") != "" }

func fetchManifest(course string) (*specManifest, error) {
	if offline() {
		return nil, errors.New("SBOOT_OFFLINE is set")
	}
	req, err := authedRequest("GET", apiURL()+"/api/v1/courses/"+course+"/spec", nil)
	if err != nil {
		return nil, err
	}
	resp, err := send(&http.Client{Timeout: 15 * time.Second}, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m specManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || m.Version == "" {
		msg := m.Err
		if msg == "" {
			msg = "unexpected response"
		}
		return nil, &apiError{status: resp.StatusCode, msg: msg}
	}
	return &m, nil
}

// fetchBundle downloads a gzipped tar and extracts it over dest.
func fetchBundle(url, dest string) error {
	if offline() {
		return errors.New("SBOOT_OFFLINE is set")
	}
	req, err := authedRequest("GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := send(&http.Client{Timeout: 120 * time.Second}, req)
	if err != nil {
		return err
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
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return extractTarGz(resp.Body, dest)
}

func fetchEngine(course, dest string) error {
	return fetchBundle(apiURL()+"/api/v1/courses/"+course+"/spec/engine", dest)
}

// fetchJudge downloads the grading engine for THIS platform and proves it is the
// binary the manifest describes before leaving it somewhere executable.
//
// ── WHY THE DIGEST CHECK IS NOT OPTIONAL ───────────────────────────────────────
// Every other bundle this file fetches is data. This one is a program we are about
// to run, so it is held to the bar scripts/install.sh already holds the CLI itself
// to: compare against a digest that arrived by a different message, and fail CLOSED
// naming the mismatch. A half-finished download is the everyday case and would
// otherwise surface as "exec format error" mid-grade.
//
// The digest comes from the spec manifest, which is fetched over the same TLS
// connection, so this is an INTEGRITY check and not a signature: it proves the bytes
// on disk are the bytes the platform meant to send, not that the platform is
// trustworthy. Saying so plainly matters more than the check does — an integrity
// check described as a security boundary is how a security boundary stops getting
// built.
func fetchJudge(course string, m *specManifest, dest string) error {
	key := platformKey()
	want, ok := m.Judge.Platforms[key]
	if !ok || want.Digest == "" {
		return fmt.Errorf("%s publishes no grading engine for %s.\n"+
			"  Supported: %s", course, key, strings.Join(sortedKeys(m.Judge.Platforms), " "))
	}
	if err := fetchBundle(apiURL()+"/api/v1/courses/"+course+"/spec/judge?platform="+key, dest); err != nil {
		return err
	}

	bin := filepath.Join(dest, judgeDir, judgeName+exeSuffix())
	b, err := os.ReadFile(bin)
	if err != nil {
		return fmt.Errorf("the grading engine bundle for %s carried no %s/%s: %w",
			key, judgeDir, judgeName+exeSuffix(), err)
	}
	if got := sha256hex(b); got != want.Digest {
		// Removed, not merely refused. Leaving it means the next run finds a
		// materialised spec, skips the fetch, and execs the bytes this one rejected.
		_ = os.Remove(bin)
		return fmt.Errorf("the grading engine for %s does not match its published digest\n"+
			"  expected %s\n  got      %s\n"+
			"  Nothing was run. Try again; if it persists the download is being altered",
			key, want.Digest, got)
	}
	// The tar carries a mode, but bundles are built by allowlist and a mode is one
	// more thing that can be wrong in a way that only shows up on someone else's
	// machine. Set it here, where it is checked.
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("could not make the grading engine executable: %w", err)
	}
	return nil
}

func sortedKeys(m map[string]specJudgeBinary) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// judgeBinary is how harness/grade.go gets the engine: the path, or an error saying
// why there is no grading today.
//
// It re-verifies the digest against the CACHED manifest, so the check survives
// offline and covers the window between the download and this run — a truncated
// file, a half-finished disk. It is deliberately not described as tamper protection:
// anything that can rewrite the binary can rewrite the spec.json beside it.
func judgeBinary(course string) (string, error) {
	s, ok := cachedSpec(course)
	if !ok {
		return "", fmt.Errorf("no grading engine cached for %s — run `sboot fetch %s`", course, course)
	}
	path := s.judgePath()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the grading engine is missing from %s (%w) — run `sboot fetch %s`",
			s.dir, err, course)
	}
	m := s.cachedManifest()
	if m == nil {
		return "", fmt.Errorf("the cached spec for %s has no manifest to verify the grading engine "+
			"against — run `sboot fetch %s`", course, course)
	}
	want, ok := m.Judge.Platforms[platformKey()]
	if !ok || want.Digest == "" {
		return "", fmt.Errorf("the cached spec for %s publishes no grading engine for %s — "+
			"run `sboot fetch %s`", course, platformKey(), course)
	}
	if got := sha256hex(b); got != want.Digest {
		return "", fmt.Errorf("the cached grading engine does not match its published digest\n"+
			"  expected %s\n  got      %s\n"+
			"  Nothing was run. `sboot fetch %s` re-downloads it", want.Digest, got, course)
	}
	return path, nil
}

func fetchLab(course, stage, dest string) error {
	return fetchBundle(apiURL()+"/api/v1/courses/"+course+"/spec/labs/"+stage, dest)
}

// ensureSpec is the one entry point `sboot test`/`submit`/`start` use: make sure a
// spec version is on disk, and that it holds this lab's tests.
//
// The order of the branches is the offline guarantee. A cached spec that already has
// the lab is usable, so the network call is an OPTIMISATION and every failure of it
// falls through to that cache. Only a learner with nothing cached and no network gets
// an error — and that error names the real problem instead of blaming the toolchain.
//
// `stage` may be "" to mean "just make sure a spec exists" (used by `sboot start`).
func ensureSpec(course, stage string) (spec, error) {
	have, haveOK := cachedSpec(course)
	haveLab := haveOK && (stage == "" || have.hasLab(stage))

	m, err := fetchManifest(course)
	if err != nil {
		// FAIL OPEN. Losing the freshness check costs a warning; refusing to grade
		// because a laptop is on a plane costs the product's offline promise.
		if haveLab {
			debugf("spec freshness check skipped (%v); using cached spec %s", err, have.version)
			return have, nil
		}
		var ae *apiError
		if errors.As(err, &ae) {
			return spec{}, fmt.Errorf("cannot fetch the tests for %s: %s", course, ae.msg)
		}
		// TWO DIFFERENT PROBLEMS, and until 2026-08-02 both were reported as the
		// second one. "no tests cached for os-rust" sends a learner whose course is
		// cached off to re-download a course they already have — when the real answer
		// is either a mistyped lab name or one lab they have never opened.
		if haveOK {
			return spec{}, offlineLabError(have, stage, err)
		}
		return spec{}, fmt.Errorf("no tests cached for %s and the platform is unreachable (%v).\n"+
			"  Connect once to download them, then `sboot test` works offline", course, err)
	}

	// A lab the course has not published has no bundle to fetch; say so plainly
	// rather than after a failed download.
	if stage != "" && m.lab(stage) == nil {
		return spec{}, fmt.Errorf("course %s has no lab %q", course, stage)
	}

	// THE ONE PLACE THIS FILE DOES NOT FAIL OPEN. Everything above falls back to the
	// cache when the network disappoints, because losing freshness must not cost the
	// offline promise. This is the opposite case: the platform answered, and the
	// answer was that this binary is too old to be trusted with these tests.
	if err := minCLIRefusal(course, stage, m); err != nil {
		return spec{}, err
	}

	target, err := materialise(course, m)
	if err != nil {
		// Materialising failed (disk, network mid-download). Same rule: a working
		// cached spec beats a hard stop.
		if haveLab {
			debugf("could not materialise spec %s (%v); using cached spec %s", m.Version, err, have.version)
			return have, nil
		}
		return spec{}, err
	}

	if stage != "" && !target.hasLab(stage) {
		if err := fetchLab(course, stage, target.dir); err != nil {
			if haveLab {
				debugf("could not fetch lab %s (%v); using cached spec %s", stage, err, have.version)
				return have, nil
			}
			return spec{}, fmt.Errorf("cannot fetch the tests for %s: %w", stage, err)
		}
	}

	// Flip the pointer only once the new version is genuinely usable, and announce a
	// change. Silence here would recreate the "it passed yesterday" confusion that
	// publishing every check exists to remove — data may auto-update, but it must
	// say so (the roadmap housekeeping: "data auto-updates, code does not").
	if target.version != have.version {
		if err := setCurrentVersion(course, target.version); err != nil {
			return spec{}, err
		}
		if haveOK {
			fmt.Printf("── the tests for %s were updated (spec %s → %s)\n",
				course, have.version, target.version)
		}
		pruneOldSpecs(course, target.version)
	}
	return target, nil
}

// minCLIRefusal is the data-side compatibility floor: an old binary meeting a spec
// that declares it needs a newer one (the CLI release policy §6).
//
// ── WHY A REFUSAL AND NOT A WARNING ────────────────────────────────────────────
// The interface between this binary and a course spec is the `lab.toml` SCHEMA, and
// the TOML decoder's degradation is to IGNORE what it does not understand. So a
// check whose meaning depends on a key this binary has never heard of does not fail
// — it silently scores as though that key were absent. That is a wrong number
// presented as a grade, on a machine we cannot patch, and it is the exact failure
// this whole mechanism was built for. Grading nothing is recoverable; grading wrong
// is not.
//
// It refuses BY NAME on three axes, because a refusal a learner cannot act on is
// just a crash with better grammar: which lab, which version they need, which
// version they have.
//
// A `dev` build is never refused. parseVersion fails on it, versionBelow answers
// false, and that is deliberate rather than accidental — a source build fixes itself
// with `git pull`, and refusing the machine that is developing the fix is absurd.
//
// OFFLINE THIS RULE DOES NOT FIRE, and cannot: it needs a manifest, and offline the
// binary keeps grading against whatever spec it already cached. That is the honest
// limit of a data-side floor, and it is safe for the reason above — an old binary
// with an old spec is a matched pair, and only the NEW spec is the one it cannot
// read correctly.
func minCLIRefusal(course, stage string, m *specManifest) error {
	if !versionBelow(version, m.MinCLI) {
		return nil
	}
	what := "the tests for " + course
	if stage != "" {
		what = stage + "'s tests"
	}
	return fmt.Errorf("%s need sboot %s or newer — this is sboot %s.\n"+
		"  Update: %s · notes: github.com/sourceboot/sboot/releases\n"+
		"  Nothing is wrong with your code. This binary would have to guess at part of\n"+
		"  these tests, and a guessed grade is worse than none",
		what, m.MinCLI, version, installCommand())
}

// installCommand is the update instruction that actually works on this OS. The
// curl-pipe-sh line is the documented install path everywhere else; Windows has no
// shell to pipe into, so it gets the releases page instead of an incantation that
// cannot run.
func installCommand() string {
	if runtime.GOOS == "windows" {
		return "download the new sboot.exe from github.com/sourceboot/sboot/releases"
	}
	return "curl -fsSL " + siteURL() + "/install.sh | sh"
}

// offlineLabError explains the cache miss that is NOT "you have nothing cached":
// the course is on disk and only this lab is missing from it.
//
// Naming what IS cached is what turns the message from "go and re-download the
// course" into something the learner can act on, because offline the two real
// causes look identical from the outside: a mistyped lab name, or a lab they have
// never opened (labs arrive one at a time, and the next one is prefetched when the
// previous is completed — so a lab they genuinely reached is normally already here).
// The cached manifest tells those apart without a network call.
func offlineLabError(have spec, stage string, err error) error {
	cached := "none yet"
	if labs := have.cachedLabs(); len(labs) > 0 {
		cached = strings.Join(labs, " ")
	}
	if m := have.cachedManifest(); m != nil && m.lab(stage) == nil {
		return fmt.Errorf("%s has no lab %q — check the name.\n"+
			"  Cached labs: %s (spec %s, the last manifest we saw; the platform is unreachable: %v)",
			have.course, stage, cached, have.version, err)
	}
	return fmt.Errorf("the tests for %q are not cached, and the platform is unreachable (%v).\n"+
		"  %s itself is cached: %s\n"+
		"  Connect once to download %s, then it grades offline too",
		stage, err, have.course, cached, stage)
}

// materialise makes sure <cache>/courses/<course>/<version>/ exists with the engine
// in it, WITHOUT touching the `current` pointer — that is what "fetch alongside" is.
//
// Labs already downloaded under the previous version are carried across when their
// digest is unchanged, so a spec bump caused by one edited lab does not silently
// shrink what a learner can grade offline.
func materialise(course string, m *specManifest) (spec, error) {
	base, err := courseCacheDir(course)
	if err != nil {
		return spec{}, err
	}
	target := spec{course: course, version: m.Version, dir: filepath.Join(base, m.Version)}

	// TWO BUNDLES, asked for separately, because they change on different clocks and
	// for different reasons: the course's build tooling is per course and rarely
	// moves, the grading engine is per PLATFORM and shared by every course. Fetching
	// them independently means a course whose tooling is already here does not
	// re-download 3 MB of engine, and vice versa.
	if !target.hasTooling() {
		if err := fetchEngine(course, target.dir); err != nil {
			return spec{}, fmt.Errorf("cannot fetch the course tooling for %s: %w", course, err)
		}
	}
	if !target.hasJudge() {
		if err := fetchJudge(course, m, target.dir); err != nil {
			return spec{}, fmt.Errorf("cannot fetch the grading engine for %s: %w", course, err)
		}
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(target.dir, "spec.json"), b, 0o644)
	}

	if prev, ok := cachedSpec(course); ok && prev.version != target.version {
		for _, stage := range prev.cachedLabs() {
			l := m.lab(stage)
			if l == nil || target.hasLab(stage) {
				continue
			}
			src := filepath.Join(prev.labDir(stage), "lab.toml")
			if b, err := os.ReadFile(src); err == nil && sha256hex(b) == l.Digest {
				if err := os.MkdirAll(target.labDir(stage), 0o755); err == nil {
					_ = os.WriteFile(filepath.Join(target.labDir(stage), "lab.toml"), b, 0o644)
				}
			}
		}
	}
	return target, nil
}

// pruneOldSpecs keeps the cache from growing without bound while still leaving
// history for offline use. Spec dirs are small (source + TOML, no build output —
// that lives in work/), so the bound is generous on purpose.
const keepSpecVersions = 5

func pruneOldSpecs(course, keep string) {
	base, err := courseCacheDir(course)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	type row struct {
		name string
		mod  time.Time
	}
	var versions []row
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "work" || e.Name() == keep {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		versions = append(versions, row{e.Name(), info.ModTime()})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].mod.After(versions[j].mod) })
	for i, v := range versions {
		if i >= keepSpecVersions-1 {
			_ = os.RemoveAll(filepath.Join(base, v.name))
		}
	}
}

// prefetchLab pulls one lab's tests into the current spec, quietly.
//
// Called when a lab is COMPLETED, so the next one is already on disk before the
// learner opens it — the boundary where a per-lab fetch would otherwise be the one
// moment offline work gets blocked. Entirely best-effort: a locked or unpublished
// next lab returns 403/404 and that is a normal outcome, not an error to report.
func prefetchLab(course, stage string) {
	s, ok := cachedSpec(course)
	if !ok || stage == "" || s.hasLab(stage) || offline() {
		return
	}
	if err := fetchLab(course, stage, s.dir); err != nil {
		debugf("prefetch of %s skipped: %v", stage, err)
		return
	}
	debugf("prefetched tests for %s", stage)
}

// ── the staging root: how the grader sees one repo plus one spec ─────────────────
//
// The course's build runs with this root as its working directory: it builds `os/`
// into the artifact course.yaml names, and the CLI reads <root>/labs/<NN>/lab.toml from
// the same place. So the build tooling, the tests and the learner's `os/` have to appear
// under one root — which is exactly the problem the server-side grading worker used
// to solve by staging OUR xtask + .cargo + rust-toolchain.toml into a scratch dir,
// dropping the learner's os/ on top, and judging with --os <dir>. That worker is
// gone (nothing of ours builds anything now), but the model it established is what
// the client still uses:
//
//	<cache>/courses/<course>/work/<repo-key>/
//	├── xtask/ labs/ .cargo/ rust-toolchain.toml   ← synced from the spec, if present
//	├── os -> <repo>/os                            ← symlink to their live tree
//	└── build/                                     ← disk.img and friends
//
// Each staged item is optional, and for os-c all of them but `labs/` are absent: its
// build is `make -C os`, whose artifact lands in `os/build/` inside the learner's own
// tree. Same root, same symlink, no Rust.
//
// `os` is a SYMLINK rather than a copy for two reasons: the learner's edits are
// picked up with no copy step (so `sboot test` cannot ever grade a stale snapshot),
// and cargo's target dirs land inside their repo where `.gitignore` already covers
// them, which keeps rebuilds incremental. Where symlinks are unavailable (Windows
// without developer mode) it degrades to a copy: correct, just slower.
//
// It is keyed by repo path so two repos on one machine — a second attempt, a
// colleague's clone — never share a build directory.

// dirEntries lists a directory's immediate children by name, sorted, or nothing at
// all if it cannot be read. Sorted because staging order should not depend on the
// filesystem: an error is easier to reproduce when the sequence is the same twice.
func dirEntries(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func repoKey(repoDir string) string {
	sum := sha256.Sum256([]byte(repoDir))
	return hex.EncodeToString(sum[:])[:12]
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// workDir is where a given repo's builds happen for a given course.
func workDir(course, repoDir string) (string, error) {
	base, err := courseCacheDir(course)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "work", repoKey(repoDir)), nil
}

// stageRun prepares the staging root and returns it.
func stageRun(s spec, r repo) (string, error) {
	run, err := workDir(s.course, r.dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		return "", err
	}

	// Sync our side. Content-compared rather than blindly copied: an unchanged
	// xtask/src must keep its mtime, or every practice run would recompile it. That
	// is also why the spec's target/ is NOT here — build output belongs to the run,
	// not to the spec.
	//
	// ── STAGED BY DEFAULT, SKIPPED BY NAME (inverted 2026-08-02) ────────────────
	// This used to be a fixed list of four names — `xtask`, `labs`, `.cargo`,
	// `rust-toolchain.toml` — which meant anything a spec bundle grew afterwards was
	// SILENTLY not staged. Silently is the whole problem: the build then runs in a
	// root missing a file it was promised, and what a learner sees is a wrong grade
	// rather than an error. A released binary cannot be taught a fifth name, so the
	// list could only ever have been right about the past.
	//
	// So the default is now "stage it", and the exceptions are named and few:
	//
	//	spec.json  our own bookkeeping about the version — not build input
	//	LICENSE    ditto, and it belongs beside the tests rather than in a build root
	//	bin/       the grading engine, 3 MB, exec'd where it lies (judgePath); copying
	//	           it per repo would buy nothing and cost a copy on every run
	for _, e := range dirEntries(s.dir) {
		switch e {
		case "spec.json", engineMarker, judgeDir:
			continue
		}
		if err := syncTree(filepath.Join(s.dir, e), filepath.Join(run, e)); err != nil {
			return "", fmt.Errorf("stage %s: %w", e, err)
		}
	}

	if err := linkOSTree(r.osDir(), filepath.Join(run, "os")); err != nil {
		return "", err
	}
	return run, nil
}

// linkOSTree points <run>/os at the learner's real source tree.
//
// The STAGED name stays `os` whatever the course calls the learner's directory, which
// is what keeps every course's `grader.build` (`--manifest-path os/Cargo.toml`) valid
// after the per-course tree name landed 2026-08-18.
func linkOSTree(src, dst string) error {
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return fmt.Errorf("no %s/ tree at %s — is this the right directory?", filepath.Base(src), src)
	}
	if cur, err := os.Readlink(dst); err == nil {
		if cur == src {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	} else if _, err := os.Lstat(dst); err == nil {
		// A previous run degraded to a copy (or a symlink became a directory);
		// replace it wholesale rather than merging two trees.
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	// Fallback for filesystems and OSes without usable symlinks. Correct but
	// slower: cargo's target dir then lives in the staging copy, so rebuilds are
	// incremental per staging dir rather than shared with the repo.
	debugf("symlinks unavailable; copying os/ into the staging dir instead")
	return syncTree(src, dst)
}

// generated names syncTree must never delete: they are build output, not content, and
// they are what keeps a rebuild incremental instead of a full recompile of the grader.
var generatedDirs = map[string]bool{"target": true, "build": true, ".git": true}

// syncTree makes dst LOOK LIKE src: it writes only files whose content differs (so an
// unchanged xtask/src keeps its mtime and cargo does not recompile the grader on every
// practice run) and removes anything src no longer has.
//
// The removal half matters more than it looks. A spec bump that DROPS a lab would
// otherwise leave that lab's stale lab.toml in the staging dir forever, where
// `sboot test` would happily still find and grade it — a learner passing checks
// the course has retired. Same reasoning for the copy fallback in linkOSTree: a file
// the learner deleted must disappear from the tree we build.
//
// Regular files and directories only. The spec bundles contain nothing else, so a
// symlink appearing in one would be a bug rather than a case to handle.
func syncTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return syncFile(src, dst, st.Mode().Perm())
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case e.IsDir():
			want[e.Name()] = true
			if err := syncTree(s, d); err != nil {
				return err
			}
		case e.Type().IsRegular():
			want[e.Name()] = true
			info, err := e.Info()
			if err != nil {
				return err
			}
			if err := syncFile(s, d, info.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	staged, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range staged {
		if want[e.Name()] || generatedDirs[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func syncFile(src, dst string, mode os.FileMode) error {
	want, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if have, err := os.ReadFile(dst); err == nil && string(have) == string(want) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, want, mode)
}

// ── extraction (shared by the scaffold and every spec bundle) ────────────────────

// extractTarGz unpacks a bundle defensively: regular files and dirs only, clean
// relative paths. Every artifact we serve is built by allowlist, so anything else
// arriving here means something is wrong and stopping is the right answer.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("illegal path in bundle: %q", hdr.Name)
		}
		target := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		default:
			// symlinks/devices are never legitimate in one of our bundles
			return fmt.Errorf("unsupported entry type in bundle: %q", hdr.Name)
		}
	}
}
