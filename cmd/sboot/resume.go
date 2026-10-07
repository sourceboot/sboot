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
	"io"
	"net/url"
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
	_, haveTests := cachedSpec(course) // where they live is not the learner's to read (G635)

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
	origin, hasOrigin := existingRemote(abs)
	rememberWorkspace(course, abs, origin)
	if !hasOrigin {
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

// ── WHERE A COURSE'S WORK LIVES ON THIS MACHINE ─────────────────────────────────
//
// Where a course's work lives on this machine — the returning learner (dogfood D3,
// 2026-10-06).
//
// A learner who comes back to a course after deleting or moving the workspace, or
// who simply runs `sboot start` again, used to get a fresh starter and no word
// about the folder or the GitHub repo that holds their work. The CLI now records
// both, per course, in state.json (`workspaces`), whenever a command that knows
// them runs — start, repo, resume, test — and names them back:
//
//   - `sboot start` on a course it has a record for, before it unpacks a fresh copy;
//   - `sboot test` in a workspace that is still exactly the starter, on an account
//     that has already verified labs in the course (a fresh scaffold graded against
//     a later lab fails, and the honest reason is "this is not your code").
//
// WHAT IT NEVER DOES: restore anything. The way back it names is the learner's own
// repo (`sboot resume <url>`) or their own folder (`cd`), never a graded upload —
// the rule resume.go already follows.
//
// LOCAL ONLY. Nothing here is sent to the server; a new machine has no record, and
// says nothing (G598). A remote is stored and printed with any userinfo stripped,
// because an https remote can carry a token (G596).
//
// COMPATIBILITY (cli-releases.md §0): the key is additive and omitempty, the state
// version stays 1, so an older binary loads a file this one wrote. An older binary
// that SAVES the file drops the key — which costs only the hint.
//
// (Kept in this file, not its own: the public CLI mirror publishes an allowlist of
// harness files, scripts/publish-cli.sh, G286.)

// workspaceRecord is where one course's work was last seen on this machine.
type workspaceRecord struct {
	// Path is the absolute workspace folder a command last ran in.
	Path string `json:"path,omitempty"`
	// Previous is the folder Path replaced, so a fresh scaffold that becomes Path
	// does not erase the one the learner's work was in.
	Previous string `json:"previous_path,omitempty"`
	// Remote is the last GitHub (or other) origin seen for this course, credentials
	// stripped. Never cleared by a workspace that has none.
	Remote string `json:"remote,omitempty"`
}

// recordWorkspace notes that this course's workspace is `path` (absolute) and,
// when known, that its origin is `remote`. Marks the state dirty only on change.
func (s *guidanceState) recordWorkspace(course, path, remote string) {
	if course == "" {
		return
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]*workspaceRecord{}
	}
	rec := s.Workspaces[course]
	if rec == nil {
		rec = &workspaceRecord{}
		s.Workspaces[course] = rec
		s.dirty = true
	}
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if rec.Path != path {
			// ONE earlier slot, so a fresh scaffold must not take it from the folder
			// with the work: a second fresh `sboot start` would otherwise leave the
			// record naming only two starters (G600). The test is the note's own.
			// A current folder that is gone has no work in it either, so it does not
			// take the slot — nor does one that is now another course's workspace
			// (fix round F2): earlierWork shows nothing for it, so the slot would
			// say nothing at all. A folder with no sboot.toml still may hold the
			// code (G602), and takes it. The manifest is read only for a record that
			// has a current folder AND an earlier slot to keep (fix round 2, R2-4: a
			// new record read ./sboot.toml from the cwd and discarded it).
			if rec.Path != "" && rec.Previous == "" {
				rec.Previous = rec.Path
			} else if rec.Path != "" && isDir(rec.Path) && !looksFresh(rec.Path, course) {
				if now := courseFromManifest(rec.Path); now == "" || now == course {
					rec.Previous = rec.Path
				}
			}
			rec.Path = path
			s.dirty = true
		}
	}
	if r := safeRemote(remote); r != "" && r != rec.Remote {
		rec.Remote = r
		s.dirty = true
	}
}

// safeRemote is a remote URL with any credential removed, or "" when it cannot
// tell that it has removed one. https://user:token@host/x → https://host/x.
// scp-style `git@host:path` carries no secret and is kept as written.
func safeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		if u.Scheme == "ssh" && u.User != nil {
			u.User = url.User(u.User.Username()) // ssh://git@host: the user is not a secret
		} else {
			u.User = nil
		}
		return u.String()
	}
	if at := strings.Index(raw, "@"); at >= 0 && strings.Contains(raw[:at], ":") {
		return "" // user:secret@host:path — not a shape git writes; refuse to keep it
	}
	return raw
}

// earlierWork writes what this machine knows about where `course`'s work is,
// leaving out `here` (the workspace the command is running in, if any). It
// reports whether it had anything to say.
func earlierWork(w io.Writer, rec *workspaceRecord, course, here string) bool {
	if rec == nil {
		return false
	}
	if here != "" {
		if abs, err := filepath.Abs(here); err == nil {
			here = abs
		}
	}
	said := false
	var keep string
	for _, p := range []string{rec.Path, rec.Previous} {
		if p == "" || p == keep {
			continue
		}
		switch courseFromManifest(p) {
		case course:
			// Skip only the workspace the command is in. A recorded path that is
			// `here` but holds no workspace is the commonest restart — deleted, then
			// `sboot start` again from the same folder — and falls to "no longer there".
			if p == here {
				continue
			}
			fmt.Fprintf(w, "   your earlier workspace: %s\n", p)
			if keep == "" {
				keep = p
			}
		case "":
			// Gone, or there without its sboot.toml — where the code may still be
			// (G602). Neither is offered as a `cd`: it is not a workspace now.
			if isDir(p) {
				fmt.Fprintf(w, "   your earlier workspace was %s — the folder is still there, but it has no %s now.\n", p, manifestName)
			} else {
				fmt.Fprintf(w, "   your earlier workspace was %s, and it is no longer there.\n", p)
			}
		default:
			continue // now some other course's folder: not this course's work
		}
		said = true
	}
	if rec.Remote != "" {
		fmt.Fprintf(w, "   your repo: %s\n", rec.Remote)
		said = true
	}
	if !said {
		return false
	}
	fmt.Fprintln(w, "   to carry on from your own code rather than a fresh starter:")
	if keep != "" {
		fmt.Fprintf(w, "     cd %s\n", quoteIfSpaced(keep))
	}
	if rec.Remote != "" {
		fmt.Fprintf(w, "     sboot resume %s\n", rec.Remote)
	}
	if keep == "" && rec.Remote == "" {
		fmt.Fprintln(w, "     sboot resume <your repo's url>   # if you pushed it with `sboot repo`")
	}
	return true
}

// looksFresh reports whether `dir` is still exactly the starter `sboot start`
// committed: one commit, made by start, and nothing changed since. Anything it
// cannot read (no git, no commit) is "not fresh" — the note is a hint and must
// never fire on a workspace that might hold the learner's work.
//
// The commit count is asked first and alone: it fails with no commit, and is not
// 1 once the learner has committed, so the usual workspace costs one git process
// on every `sboot test` (G603).
func looksFresh(dir, course string) bool {
	if !isDir(filepath.Join(dir, ".git")) {
		return false
	}
	n, err := gitRun(dir, "rev-list", "--count", "HEAD")
	if err != nil || strings.TrimSpace(string(n)) != "1" {
		return false
	}
	subj, err := gitRun(dir, "log", "-1", "--format=%s")
	if err != nil || strings.TrimSpace(string(subj)) != "start "+course {
		return false
	}
	st, err := gitRun(dir, "status", "--porcelain")
	return err == nil && strings.TrimSpace(string(st)) == ""
}

// freshScaffoldNote is printed by `sboot test` before it grades: a workspace that
// is still the starter, on an account that has already verified labs here, will
// fail a later lab for a reason that is not the learner's code.
//
// The verified labs must be the SIGNED-IN account's: a cache another account
// synced on this machine says nothing about this one (G601). A cache or a login
// with no handle cannot say whose progress it is, and stays quiet — mergeSync's rule.
func freshScaffoldNote(w io.Writer, st *guidanceState, r repo) {
	cs := st.Sync[r.course]
	if cs == nil || len(cs.Verified) == 0 || cs.Handle == "" {
		return
	}
	if c := loadCredentials(); c == nil || c.Handle != cs.Handle {
		return
	}
	if !looksFresh(r.dir, r.course) {
		return
	}
	fmt.Fprintf(w, "note: this workspace looks fresh — nothing has changed since the starter, and this account\n")
	fmt.Fprintf(w, "      has already verified labs in %s. Each lab builds on your code from the labs before it.\n", r.course)
	if !earlierWork(w, st.Workspaces[r.course], r.course, r.dir) {
		fmt.Fprintln(w, "   to carry on from your own code rather than a fresh starter:")
		fmt.Fprintln(w, "     sboot resume <your repo's url>   # if you pushed it with `sboot repo`")
	}
	fmt.Fprintln(w)
}

// rememberWorkspace records a workspace (and its origin, if any) in its own
// load-and-save, for the commands that do not otherwise touch the state file.
func rememberWorkspace(course, dir, remote string) {
	st := loadState()
	st.recordWorkspace(course, dir, remote)
	saveQuietly(st)
}

// startNotice is what `sboot start` says, before it unpacks a fresh starter, when
// this machine knows where the course's work was.
func startNotice(course, dest string) {
	rec := loadState().Workspaces[course]
	if rec == nil {
		return
	}
	var b strings.Builder
	if !earlierWork(&b, rec, course, dest) {
		return
	}
	fmt.Fprintln(os.Stderr, "── you have worked on this course on this machine before")
	fmt.Fprint(os.Stderr, b.String())
	fmt.Fprintln(os.Stderr, "   unpacking a fresh starter anyway — your earlier work is not touched.")
	fmt.Fprintln(os.Stderr)
}
