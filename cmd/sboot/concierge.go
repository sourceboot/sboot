// The repo concierge — `sboot repo`, and the same offer inside `sboot start`
// (ux-plan §11.j, ratified; the ux-v2 repo/start tiles are the copy of record).
//
// The posture this extends and NEVER reverses is code-hosting Option D: we hold
// zero credentials. Everything here runs on the learner's machine with gh's own
// credential — `git init`, the first commit, `gh repo create --private --source
// . --push`. The concierge's whole job is to CONVINCE while staying optional:
//
//	gh authed     → ONE confirm, then create + push, then the trust note
//	gh unauthed   → offer `gh auth login` first, then the same one confirm
//	no gh         → the two-minute path: a PREFILLED github.com/new link (every
//	                learner has a GitHub account — it is the only sign-in), the
//	                two git commands, and the gh install named as the smooth rail
//	remote exists → a no-op that says so; it never touches an existing remote
//
// Prompts appear ONLY on a TTY and every one is bypassable with --yes (§12.2
// rule 7). Declining costs nothing: `sboot repo` re-runs the offer any time.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// runRepo is the standalone verb: detect the machine's state, offer once.
func runRepo(yes bool) int {
	r, err := findRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sboot: no course workspace here — nothing to push.")
		fmt.Fprintln(os.Stderr, "sboot: `sboot courses` lists them; `sboot start <id>` begins one.")
		return 2
	}
	out := os.Stderr

	if url, ok := existingRemote(r.dir); ok {
		fmt.Fprintf(out, "remote already set → %s — nothing to create.\n", url)
		fmt.Fprintln(out, "push as usual with git; `sboot repo` only ever offers — it never touches an existing remote.")
		return 0
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(out, "sboot: git is not installed (or is not on your PATH).")
		fmt.Fprintf(out, "sboot:   %s\n", pkgInstall("git"))
		fmt.Fprintln(out, "sboot: install it, then `sboot repo` creates and pushes your repo.")
		return 2
	}

	if _, err := exec.LookPath("gh"); err != nil {
		printManualRail(out, r.dir, r.course)
		return 0
	}

	user, authed := ghAccount()
	if !authed {
		fmt.Fprintln(out, "gh detected — but it is not signed in yet.")
		if yes || !interactiveTTY() {
			fmt.Fprintln(out, "sign in first:  gh auth login   · then `sboot repo` finishes in one confirm.")
			return 0
		}
		if !confirm(out, "run `gh auth login` now? [Y/n] ") {
			fmt.Fprintln(out, "── skipped. any time: gh auth login · then sboot repo")
			return 0
		}
		login := exec.Command("gh", "auth", "login")
		login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := login.Run(); err != nil {
			fmt.Fprintln(out, "sboot: gh auth login did not finish — run it directly, then `sboot repo`.")
			return 1
		}
		user, authed = ghAccount()
		if !authed {
			fmt.Fprintln(out, "sboot: gh still reports no login — run `gh auth status`, then `sboot repo`.")
			return 1
		}
	}

	fmt.Fprintf(out, "gh detected — authed as %s · no remote on this workspace yet\n", atUser(user))
	if !yes && !interactiveTTY() {
		fmt.Fprintf(out, "re-run from a terminal (or with --yes) to confirm:\n")
		fmt.Fprintf(out, "  create github.com/%s/%s (private) and push\n", userOrYou(user), r.course)
		return 0
	}
	if !yes && !confirm(out, fmt.Sprintf("create github.com/%s/%s (private) and push? [Y/n] ", userOrYou(user), r.course)) {
		fmt.Fprintf(out, "── skipped. any time: sboot repo · or DIY: https://github.com/new?name=%s\n", r.course)
		return 0
	}

	url, err := createAndPush(out, r.dir, r.course)
	if err != nil {
		fmt.Fprintf(out, "sboot: %v\n", err)
		fmt.Fprintln(out, "sboot: fix the above and re-run `sboot repo` — nothing here blocks the course.")
		return 1
	}
	p := painter(os.Stdout)
	fmt.Println(p(ansiGreen, "✓ pushed → "+url))
	fmt.Println()
	fmt.Printf("next:  %s                 %s\n", p(ansiGreen, "sboot test"), p(ansiDim, "# back to the loop"))
	return 0
}

// startConcierge is the same offer inside `sboot start`, right after the
// workspace block. Skippable, never a blocker, and in a non-interactive run
// (no TTY, no --yes) it spawns NOTHING — it prints the offer and moves on, so
// CI and scripts see one extra line, not a subprocess.
func startConcierge(course, dir string, yes bool) {
	out := os.Stderr
	fmt.Fprintln(out)
	fmt.Fprintf(out, "── your repo %s\n", painter(out)(ansiDim, "(recommended, never required — worth it: your own commit history)"))
	if !yes && !interactiveTTY() {
		fmt.Fprintf(out, "   `sboot repo` creates github.com/<you>/%s (private) and pushes — one confirm, on your machine.\n", course)
		return
	}
	if _, err := exec.LookPath("gh"); err != nil {
		printManualRail(out, dir, course)
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintf(out, "   git is not installed — %s · then: sboot repo\n", pkgInstall("git"))
		return
	}
	user, authed := ghAccount()
	if !authed {
		fmt.Fprintln(out, "   gh detected — not signed in yet.")
		fmt.Fprintln(out, "   sign in:  gh auth login   · then `sboot repo` finishes in one confirm.")
		return
	}
	fmt.Fprintf(out, "   gh detected — authed as %s\n", atUser(user))
	if !yes && !confirm(out, fmt.Sprintf("   create github.com/%s/%s (private) and push? [Y/n] ", userOrYou(user), course)) {
		fmt.Fprintf(out, "   ── skipped. any time: sboot repo · or DIY: https://github.com/new?name=%s\n", course)
		return
	}
	url, err := createAndPush(out, dir, course)
	if err != nil {
		fmt.Fprintf(out, "   sboot: %v\n", err)
		fmt.Fprintln(out, "   ── nothing here blocks the course — `sboot repo` retries any time.")
		return
	}
	fmt.Fprintf(out, "   %s   %s\n", painter(out)(ansiGreen, "✓ pushed → "+url),
		painter(out)(ansiDim, "# gh's credential, on your machine — never ours"))
}

// createAndPush runs the machine's side of the one confirm: git init if needed,
// a first commit if there is none, then gh creates the private repo and pushes.
// The summary line names exactly the steps that will run (the tile's register).
func createAndPush(out *os.File, dir, course string) (string, error) {
	var steps []string
	needInit := !isDir(filepath.Join(dir, ".git"))
	if needInit {
		steps = append(steps, "git init")
	}
	needCommit := needInit || !gitHasHead(dir)
	if needCommit {
		steps = append(steps, fmt.Sprintf("commit %q", "start "+course))
	}
	steps = append(steps, "gh repo create --private --source . --push")
	fmt.Fprintf(out, "── %s\n", strings.Join(steps, " · "))

	if needInit {
		if b, err := gitRun(dir, "init", "-q"); err != nil {
			return "", fmt.Errorf("git init failed: %s", firstLine(b, err))
		}
	}
	if needCommit {
		if b, err := gitRun(dir, "add", "-A"); err != nil {
			return "", fmt.Errorf("git add failed: %s", firstLine(b, err))
		}
		if b, err := gitRun(dir, "commit", "-q", "-m", "start "+course); err != nil {
			msg := firstLine(b, err)
			if strings.Contains(string(b), "user.name") || strings.Contains(string(b), "user.email") {
				msg += ` — set your git identity first: git config --global user.name "You" && git config --global user.email you@example.com`
			}
			return "", fmt.Errorf("git commit failed: %s", msg)
		}
	}

	create := exec.Command("gh", "repo", "create", course, "--private", "--source", ".", "--push")
	create.Dir = dir
	var stdout bytes.Buffer
	create.Stdout = &stdout
	create.Stderr = out // gh's own progress and errors, live
	if err := create.Run(); err != nil {
		return "", errors.New("gh could not create the repo (see above)")
	}
	if url := firstURL(stdout.String()); url != "" {
		return url, nil
	}
	if user, ok := ghAccount(); ok {
		return "github.com/" + user + "/" + course, nil
	}
	return "github.com/<you>/" + course, nil
}

// printManualRail is the no-gh path: the prefilled new-repo link (opened when a
// human is present), the git commands, and the smoother rail by name.
func printManualRail(out *os.File, dir, course string) {
	newURL := "https://github.com/new?name=" + course
	fmt.Fprintln(out, "no gh here — the two-minute path:")
	fmt.Fprintf(out, "  open   %s        # prefilled; create it private\n", newURL)
	tryOpen(newURL)
	if !gitHasHead(dir) {
		fmt.Fprintf(out, "  then   git init && git add . && git commit -m %q\n", "start "+course)
		fmt.Fprintf(out, "         git remote add origin https://github.com/<you>/%s.git\n", course)
	} else {
		fmt.Fprintf(out, "  then   git remote add origin https://github.com/<you>/%s.git\n", course)
	}
	fmt.Fprintln(out, "         git push -u origin main")
	fmt.Fprintf(out, "smoother: install GitHub's CLI (%s) and `sboot repo` does it in one confirm.\n",
		pkgInstall("gh"))
}

// ── plumbing ────────────────────────────────────────────────────────────────────

func existingRemote(dir string) (string, bool) {
	b, err := gitRun(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(b))
	return url, url != ""
}

func gitRun(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
}

func gitHasHead(dir string) bool {
	_, err := gitRun(dir, "rev-parse", "-q", "--verify", "HEAD")
	return err == nil
}

// ghAccountRe reads the login out of `gh auth status`, which prints either
// "Logged in to github.com account puneet (keyring)" (gh ≥ 2.40) or
// "Logged in to github.com as puneet" (older). Local, offline, fast.
var ghAccountRe = regexp.MustCompile(`(?:account|as)\s+(\S+)`)

func ghAccount() (string, bool) {
	b, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		return "", false
	}
	if m := ghAccountRe.FindSubmatch(b); m != nil {
		return strings.Trim(string(m[1]), "()"), true
	}
	return "", true // authed; version prints something we do not parse
}

func atUser(user string) string {
	if user == "" {
		return "@you"
	}
	return "@" + user
}

func userOrYou(user string) string {
	if user == "" {
		return "<you>"
	}
	return user
}

// confirm asks one [Y/n] question on the prompt's stream and reads one line
// from stdin. Empty means yes (the tile's default); a closed stdin declines.
// Callers gate on TTY, so a pipe never blocks here.
func confirm(out *os.File, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func firstLine(b []byte, fallback error) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return fallback.Error()
	}
	return s
}

var urlRe = regexp.MustCompile(`(?:https://)?github\.com/\S+`)

func firstURL(s string) string {
	return strings.TrimPrefix(urlRe.FindString(s), "https://")
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
