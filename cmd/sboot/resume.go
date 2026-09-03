// `sboot resume <path|git-url>` — pick your course back up on a machine that has
// never seen it.
//
// ── WHY IT IS A COMMAND (ratified 2026-09-02, P-17; Puneet's override of the
// recommendation to defer it) ──────────────────────────────────────────────────
//
// The course already PROMISES this in lab 00 — "new machine, months from now?
// install sboot, clone your repo, sign in, you are back" — and until now that
// promise was four commands the learner had to know, one of which (`sboot fetch`)
// is plumbing. Worse, none of them ANSWERS the question the learner actually has,
// which is not "are the files here" but "does this machine still work". So resume
// ends by grading the first lab: the only honest evidence that a rebuilt laptop
// can do the course is a verdict from it.
//
// It is deliberately thin. Everything it does is a step some other command already
// owns — clone, read sboot.toml, ensureSpec, runTestCode, the `sboot repo` offer —
// and the value is entirely in the ORDER and in not having to know it. Nothing
// here re-implements a second copy of any of them.
//
// WHAT IT NEVER DOES: rebuild the workspace from a graded archive. Resume restores
// from the learner's OWN repo, which is the only place their in-progress work
// exists — our submission archives hold the last graded state, which is a
// different (older, lossier) thing and is not what "resume" should ever mean.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runResume takes a directory or a git URL and leaves the learner able to work.
func runResume(arg string) int {
	dir := arg
	if isGitURL(arg) {
		cloned, err := cloneForResume(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
			return 2
		}
		dir = cloned
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "sboot: %s is not a directory.\n", abs)
		fmt.Fprintln(os.Stderr, "sboot: `sboot resume <folder>` or `sboot resume <git url>` — the repo `sboot repo` made.")
		return 2
	}
	course := courseFromManifest(abs)
	if course == "" {
		// THE ONE REFUSAL, and it is specific on purpose: a directory with no
		// sboot.toml is not a workspace, and the two ways to be here are cloning
		// the wrong repo and pointing at the parent of the right one.
		fmt.Fprintf(os.Stderr, "sboot: %s has no %s — it is not a course workspace.\n", abs, manifestName)
		if inner := innerWorkspace(abs); inner != "" {
			fmt.Fprintf(os.Stderr, "sboot: did you mean:  sboot resume %s\n", inner)
			return 2
		}
		fmt.Fprintln(os.Stderr, "sboot: start a fresh one with `sboot start <course>` — nothing was changed here.")
		return 2
	}

	fmt.Printf("── %s: %s\n", course, abs)

	// The tests and the grading engine, which is everything a clone is missing:
	// they were never in the repo (the workspace split), so a fresh clone has the
	// learner's code and none of ours until now.
	m, mErr := fetchManifest(course)
	firstStage := ""
	if mErr == nil {
		for _, l := range m.Labs {
			if l.Live {
				firstStage = l.Stage
				break
			}
		}
	} else if e, ok := mErr.(*apiError); ok && e.status == 401 {
		fmt.Fprintf(os.Stderr, "sboot: %s\n", e.msg)
		// The retry is THIS command with what they typed — not `sboot start`,
		// which would offer to unpack a second copy beside the one just restored.
		reportSignedOut("sboot resume " + quoteIfSpaced(arg))
		return 2
	}
	fetchTests(course, firstStage)
	s, haveTests := cachedSpec(course)
	if haveTests {
		fmt.Printf("   tests + grader: %s\n", s.dir)
	}

	// Prove it. A resume that printed "you're all set" without running anything
	// would be making exactly the claim it cannot support.
	//
	// GATED ON THE CACHE, because the alternative is worse than skipping: the
	// graded path exits hard when the spec cannot be materialised, so an offline
	// or half-published course would end this command on a download error instead
	// of on the workspace it just restored. The files are back either way, and
	// that is the sentence a learner on a rebuilt machine needs to read.
	code := 0
	switch {
	case firstStage != "" && haveTests:
		r := repo{dir: abs, course: course, tree: treeFromManifest(abs)}
		fmt.Println()
		code = runTestCode(r, firstStage, gradedArgs{})
	case !haveTests:
		fmt.Fprintln(os.Stderr, "sboot: the tests are not on this machine yet — run `sboot test` once you can reach the platform.")
	default:
		fmt.Fprintln(os.Stderr, "sboot: no live lab to check against — `sboot test` when you are back online.")
	}

	fmt.Println()
	fmt.Printf("you are back. work in %s and run %s.\n", abs, painter(os.Stdout)(ansiGreen, "sboot test"))
	if _, ok := existingRemote(abs); !ok {
		fmt.Fprintf(os.Stderr, "no GitHub remote on this copy — %s adds one.\n", "sboot repo")
	}
	// THE EXIT CODE IS THE FIRST LAB'S, and that is deliberate: the first lab of a
	// course a learner has already worked through should pass, so a non-zero code
	// here is real news about this machine (a missing toolchain, a half-clone) and
	// must not be swallowed by a cheerful zero.
	return code
}

// isGitURL decides whether the argument names a remote rather than a folder.
//
// Three shapes, all unambiguous against a path: a scheme, scp-style `git@host:`,
// and a trailing `.git`. Anything else is treated as a directory, which is the
// safe default — the worst case is a clear "not a directory" from the caller.
func isGitURL(s string) bool {
	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "ssh://"), strings.HasPrefix(s, "git://"):
		return true
	case strings.HasPrefix(s, "git@") && strings.Contains(s, ":"):
		return true
	case strings.HasSuffix(s, ".git"):
		return true
	}
	return false
}

// cloneForResume runs a PLAIN `git clone` into a folder named after the repo, and
// refuses rather than merging into anything that is already there.
//
// Plain, not gh: the learner may be cloning a repo that is theirs, someone else's
// or a fork, over ssh or https, with whatever credential helper their machine has
// — git already knows all of that, and gh would only add a login requirement to a
// command whose whole point is getting a machine working again.
func cloneForResume(url string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not installed (or is not on your PATH) — %s", pkgInstall("git"))
	}
	dest := repoDirFromURL(url)
	if dest == "" {
		return "", fmt.Errorf("could not work out a folder name from %q — clone it yourself, then `sboot resume <folder>`", url)
	}
	if dirNonEmpty(dest) {
		return "", fmt.Errorf("./%s already exists and is not empty — `sboot resume %s` picks it up as it is", dest, dest)
	}
	fmt.Printf("── cloning %s\n", url)
	clone := exec.Command("git", "clone", url, dest)
	clone.Stdout, clone.Stderr = os.Stderr, os.Stderr // git's own progress, live
	if err := clone.Run(); err != nil {
		return "", fmt.Errorf("git clone failed (see above) — check the URL and that you have access")
	}
	return dest, nil
}

// repoDirFromURL is git's own rule for the folder a clone lands in: the last path
// segment, minus a `.git` suffix.
func repoDirFromURL(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return ""
	}
	return s
}

// innerWorkspace names a single workspace one level down, for the commonest miss:
// pointing at the folder a clone was made INTO rather than at the clone.
func innerWorkspace(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if courseFromManifest(p) == "" {
			continue
		}
		if found != "" {
			return "" // more than one: naming either would be a guess
		}
		found = p
	}
	return found
}
