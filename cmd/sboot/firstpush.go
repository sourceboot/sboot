package main

// The half-done manual path (DF-3, Puneet's rust-for-systems dogfood, 2026-10-07).
//
// A learner followed `sboot repo`'s by-hand rail: `git remote add origin
// https://github.com/<them>/…`, then `git push`, which GitHub refused ("Password
// authentication is not supported"). They installed gh and ran `sboot repo`
// again, and it answered "remote already set — nothing to create": true, and no
// help, because nothing had ever been pushed and plain `git push` still had no
// credential. So: a remote that is set but has NEVER BEEN PUSHED TO is still ours
// to offer for — create the repo it names if it does not exist, and push with
// gh's credential — once, after saying what will run. A branch with an upstream,
// or a GitHub repo with any commit in it, is still never touched.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// githubHTTPSRe reads owner and name out of an https GitHub remote, with or
// without credentials in it. HTTPS only, deliberately: gh's credential helper is
// what makes the push work, and it plays no part in an ssh push.
var githubHTTPSRe = regexp.MustCompile(`^https://(?:[^@/]+@)?github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)

func githubSlug(url string) (owner, name string, ok bool) {
	m := githubHTTPSRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// hasUpstream: the current branch tracks a remote branch — it was pushed with -u
// (or set up to track). The remote is in use, and it is not ours.
func hasUpstream(dir string) bool {
	_, err := gitRun(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return err == nil
}

func currentBranch(dir string) string {
	b, err := gitRun(dir, "symbolic-ref", "--short", "HEAD")
	if s := strings.TrimSpace(string(b)); err == nil && s != "" {
		return s
	}
	return "main"
}

type remoteRepoState int

const (
	remoteUnknown remoteRepoState = iota // gh could not say — treated as in use
	remoteMissing                        // GitHub has no such repo
	remoteEmpty                          // the repo exists with no commits
	remotePushed                         // the repo has history
)

// ghRepoState asks GitHub (through gh, so a private repo answers) whether
// owner/name exists and whether anything was ever pushed to it. `isEmpty` is
// GitHub's own answer to "has this repo any commit", which is the question `git
// ls-remote` would answer — without the credential prompt an anonymous
// ls-remote of a private repo stops at. Every answer it cannot read is
// remoteUnknown, and the caller leaves the remote alone.
func ghRepoState(slug string) remoteRepoState {
	b, err := exec.Command("gh", "repo", "view", slug, "--json", "isEmpty", "--jq", ".isEmpty").CombinedOutput()
	out := strings.TrimSpace(string(b))
	switch {
	case err != nil && strings.Contains(out, "Could not resolve to a Repository"):
		return remoteMissing
	case err != nil:
		return remoteUnknown
	case out == "true":
		return remoteEmpty
	case out == "false":
		return remotePushed
	}
	return remoteUnknown
}

// ghCredGit is `git` with gh's credential helper for github.com for this one
// command — the same lines `gh auth setup-git` writes — and no terminal prompt,
// so this push can never stop at "Username for 'https://github.com':" again.
func ghCredGit(dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir,
		"-c", "credential.https://github.com.helper=",
		"-c", "credential.https://github.com.helper=!gh auth git-credential"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// offerFirstPush handles a workspace whose origin is set but has never been
// pushed to. handled=false means "not this case": the caller prints its
// remote-already-set answer, unchanged.
func offerFirstPush(out *os.File, dir, url, shown string, yes bool) (code int, handled bool) {
	// No commit: nothing to push, and on a Mac without the Command Line Tools git
	// is the install shim — ask nothing more of it.
	if !gitHasHead(dir) || hasUpstream(dir) {
		return 0, false
	}
	owner, name, ok := githubSlug(url)
	if !ok {
		return 0, false
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return 0, false
	}
	user, authed := ghAccount()
	if !authed {
		return 0, false
	}
	slug := owner + "/" + name
	state := ghRepoState(slug)
	if state == remotePushed || state == remoteUnknown {
		return 0, false
	}
	if state == remoteMissing && user != "" && !strings.EqualFold(owner, user) {
		// The remote names another account (or a placeholder typed literally).
		// Creating it there is not ours to do, and changing origin is the
		// learner's call: say the one command.
		fmt.Fprintf(out, "remote set → %s — but GitHub has no repo there, and gh is signed in as %s.\n", shown, atUser(user))
		fmt.Fprintf(out, "point it at your account:  git remote set-url origin https://github.com/%s/%s.git\n", user, name)
		fmt.Fprintln(out, "then `sboot repo` creates it and pushes in one confirm. (`sboot repo` never changes an existing remote itself.)")
		return 0, true
	}
	if state == remoteMissing && user == "" {
		// gh is signed in but printed a login ghAccount cannot read, so whose
		// account the remote names is unknown — and a placeholder pasted literally
		// looks just like a real owner. Fail closed: create nothing, say the set-url
		// line, and the commands that finish it by hand (G720). `gh repo create`
		// with no owner creates in the account gh is signed in as.
		fmt.Fprintf(out, "remote set → %s — but GitHub has no repo there, and sboot cannot read which account gh is signed in as, so it creates nothing.\n", shown)
		fmt.Fprintf(out, "if that is not your account, point it at yours:  git remote set-url origin https://github.com/%s/%s.git\n", githubNamePlaceholder, name)
		fmt.Fprintf(out, "%s is your GitHub user name: put yours in before you run that line.\n", githubNamePlaceholder)
		fmt.Fprintf(out, "then:  gh repo create %s --private && gh auth setup-git && git push -u origin %s\n", name, currentBranch(dir))
		return 0, true
	}

	branch := currentBranch(dir)
	fmt.Fprintf(out, "remote set → %s — but nothing has been pushed there yet. gh is signed in as %s, so it can finish this:\n", shown, atUser(user))
	if state == remoteMissing {
		fmt.Fprintf(out, "  gh repo create %s --private     # the repo your remote names does not exist on GitHub yet\n", slug)
	}
	fmt.Fprintln(out, "  gh auth setup-git                # plain `git push` uses gh's login from now on")
	fmt.Fprintf(out, "  git push -u origin %s\n", branch)
	if gitDirty(dir) {
		fmt.Fprintf(out, "note: you have changes that are not committed, and a push sends commits only — to include them, first:  %s\n", commitLine(""))
	}
	what := "push to github.com/" + slug
	if state == remoteMissing {
		what = "create github.com/" + slug + " (private) and push"
	}
	if !yes && !interactiveTTY() {
		fmt.Fprintf(out, "re-run from a terminal (or with --yes) to confirm:\n  %s\n", what)
		return 0, true
	}
	if !yes && !confirm(out, what+"? [Y/n] ") {
		fmt.Fprintln(out, "── skipped. any time: sboot repo")
		return 0, true
	}

	if state == remoteMissing {
		fmt.Fprintf(out, "── gh repo create %s --private\n", slug)
		create := exec.Command("gh", "repo", "create", slug, "--private")
		create.Dir, create.Stdout, create.Stderr = dir, out, out
		if err := create.Run(); err != nil {
			fmt.Fprintln(out, "sboot: gh could not create the repo (see above) — your remote and your commits are untouched.")
			return 1, true
		}
	}
	// Best effort: the push below carries gh's helper itself, so a failure here
	// only means a later plain `git push` may ask again — said, not fatal.
	fmt.Fprintln(out, "── gh auth setup-git")
	setup := exec.Command("gh", "auth", "setup-git")
	setup.Stdout, setup.Stderr = out, out
	if err := setup.Run(); err != nil {
		fmt.Fprintln(out, "note: gh auth setup-git did not finish; a later `git push` may ask for a login — run it yourself then.")
	}
	fmt.Fprintf(out, "── git push -u origin %s\n", branch)
	push := ghCredGit(dir, "push", "-u", "origin", branch)
	push.Stdout, push.Stderr = out, out
	if err := push.Run(); err != nil {
		fmt.Fprintln(out, "sboot: the push did not go through (see above) — fix that, then `sboot repo` again.")
		return 1, true
	}
	p := painter(os.Stdout)
	fmt.Println(p(ansiGreen, "✓ pushed → https://github.com/"+slug))
	fmt.Println()
	fmt.Printf("next:  %s                 %s\n", p(ansiGreen, "sboot test"), p(ansiDim, "# back to the loop"))
	return 0, true
}

// manualRemoteLine is the rail's `git remote add` line. It used to print
// `https://github.com/<you>/…` as a thing to paste, and a learner pasted it
// literally (zsh: "no such file or directory: you" — DF-3). With a login we know
// (the platform signs in with GitHub, and its handle is the GitHub login) it is
// filled in. Without one the placeholder is YOUR-GITHUB-NAME, never `<you>`:
// `<` and `>` are redirections to every shell, and zsh does not treat `#` as a
// comment at an interactive prompt by default, so even an instruction in a
// trailing comment re-created the same error when the line was pasted whole (TL
// review, 2026-10-07). The instruction is its own line, `note`, and a
// placeholder pasted literally reaches `sboot repo`'s set-url answer (G661).
func manualRemoteLine(login, repo string) (line, note string) {
	if login != "" {
		return fmt.Sprintf("git remote add origin https://github.com/%s/%s.git", login, repo), ""
	}
	return fmt.Sprintf("git remote add origin https://github.com/%s/%s.git", githubNamePlaceholder, repo),
		githubNamePlaceholder + " is your GitHub user name: put yours in before you run that line."
}

const githubNamePlaceholder = "YOUR-GITHUB-NAME"

// knownGitHubLogin is the learner's GitHub login when this machine knows it
// without a network call: the handle stored at `sboot login`. (gh is not
// installed on the path that prints the rail, so `gh api user` cannot answer.)
func knownGitHubLogin() string {
	if c := loadCredentials(); c != nil {
		return c.Handle
	}
	return ""
}
