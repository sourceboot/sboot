// Git and GitHub — the local repo `sboot start` makes, and the remote `sboot
// repo` offers.
//
// ── THE SPLIT (ratified 2026-09-02, P-21; the workspace-split design) ─────────
//
// Until now ONE command did both: `sboot start` unpacked the workspace and then,
// if `gh` happened to be installed, offered to create and push a GitHub repo in
// the same breath — and printed a manual rail when it was not. Three fresh-VM laps
// and one founder's own run found every failure in the GitHub half, bolted onto
// the one command a learner must run before anything works: the printed rail had
// no `cd`, so the PARENT folder became the repo; it pushed over HTTPS without
// saying GitHub rejects passwords; and the first commit carried whatever identity
// git guessed (`you@hostname.local`).
//
// So the boundary moved, not the posture:
//
//	sboot start   unpack · git init INSIDE the folder · identity · first commit
//	              · one line naming `sboot repo`.  It cannot fail on GitHub,
//	              because it never speaks to GitHub.
//	sboot repo    the remote, and only the remote. gh drives it; the manual path
//	              is a link to GitHub's own doc, not a tutorial of ours.
//
// THE POSTURE THIS NEVER REVERSES is code-hosting Option D: we hold zero
// credentials. Everything here runs on the learner's machine with gh's own
// credential — `git init`, the commits, `gh repo create --private --source .
// --push`. Nothing in the course depends on any of it: grading reads the local
// tree, so a learner who never runs `sboot repo` loses nothing but the portfolio.
//
// Prompts appear ONLY on a TTY and every one is bypassable with --yes (ux-plan
// §12.2 rule 7). Declining costs nothing: `sboot repo` re-runs the offer any time.
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
	"runtime"
	"strings"
)

// ghTokenDoc is the ONE link the manual path offers: GitHub's own page about the
// credential a `git push` over HTTPS actually wants.
//
// Deliberately theirs and deliberately singular (P-9). We used to print
// `git push -u origin main` with no word about auth, into an audience with no
// token and no SSH key — GitHub rejects the password they will type, with a
// message about "support for password authentication" they have no way to read.
// Writing our own token walkthrough would date the moment GitHub moves a button,
// and an SSH tutorial is a second credential system to teach; gh is the path we
// explain, and this is the escape hatch for someone who declines it.
//
// FETCHED BEFORE IT SHIPPED, and it needed to be: the first spelling of this
// constant — `keeping-your-account-secure` — 404s. GitHub renamed that path
// segment to `keeping-your-account-and-data-secure`, and a 404 handed to someone
// who has just been told "GitHub will ask for a token" is worse than saying
// nothing, because it reads as the tool being wrong about the whole thing.
// Verified 200 on 2026-09-03; re-check it whenever this line is edited.
const ghTokenDoc = "https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens"

// runRepo owns the remote: detect the machine's state, explain, offer once.
//
// `name` is the repo to create ("" = the course's own default, `<artifact>-sb`).
func runRepo(yes bool, name string) int {
	r, err := findRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sboot: no course workspace here — nothing to push.")
		fmt.Fprintln(os.Stderr, "sboot: `sboot courses` lists them; `sboot start <id>` begins one.")
		return 2
	}
	out := os.Stderr
	repo := name
	if repo == "" {
		repo = repoName(r.course, courseArtifact(r.course))
	}

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

	// gh missing: the two offers, (a) install it or (b) do it by hand. The install
	// itself is PRINTED, never run — every one of these needs sudo or an admin
	// shell, and a CLI that shells out to a password prompt it does not own is a
	// support case, not a convenience (P-8/P-15).
	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintf(out, "to put this on GitHub, %s uses GitHub's own CLI (`gh`) — it holds the login, we never do.\n", brandName)
		fmt.Fprintln(out, "it is not installed here. two ways forward:")
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  a) install it:  %s\n", ghInstallLine())
		fmt.Fprintln(out, "     run that, then `sboot repo` again — it does the rest in one confirm.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  b) do it by hand:")
		printManualRail(out, r.dir, repo)
		return 0
	}

	user, authed := ghAccount()
	if !authed {
		fmt.Fprintln(out, "gh is installed but not signed in — that is the one step only you can do.")
		fmt.Fprintln(out, "`gh auth login` opens your browser and stores the credential on this machine.")
		if !yes && !interactiveTTY() {
			fmt.Fprintln(out, "run it from a terminal:  gh auth login   · then `sboot repo` finishes in one confirm.")
			return 0
		}
		if !yes && !confirm(out, "run `gh auth login` now? [Y/n] ") {
			fmt.Fprintln(out, "── skipped. any time: gh auth login · then sboot repo")
			return 0
		}
		// EXEC'd as a child with THIS terminal attached (P-15, ratified over
		// printing it): gh is interactive, opens the browser itself and writes its
		// own credential store, and telling this audience to "open another terminal"
		// is where the flow was being lost.
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
	fmt.Fprintf(out, "this creates ONE private repo in your account and pushes what you have — your code, your repo, our credential never involved.\n")
	if !yes && !interactiveTTY() {
		fmt.Fprintf(out, "re-run from a terminal (or with --yes) to confirm:\n")
		fmt.Fprintf(out, "  create github.com/%s/%s (private) and push\n", userOrYou(user), repo)
		return 0
	}
	if !yes && !confirm(out, fmt.Sprintf("create github.com/%s/%s (private) and push? [Y/n] ", userOrYou(user), repo)) {
		fmt.Fprintf(out, "── skipped. any time: sboot repo · or DIY: https://github.com/new?name=%s\n", repo)
		return 0
	}

	url, err := createAndPush(out, r.dir, r.course, repo, yes)
	if err != nil {
		// The Command Line Tools remedy is already printed, and it is the whole
		// answer — "fix the above and re-run" would be a second instruction over
		// an install that has to finish first.
		if errors.Is(err, errDevTools) {
			return 2
		}
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

// ghInstallLine is the install command for THIS machine, and it is checked rather
// than assumed.
//
// The old `pkgInstall("gh")` printed `brew install gh` on every Mac whether or not
// brew existed (D-MACOS-6) and `sudo apt install gh` on Windows (D-WIN-2) — a line
// that cannot work is worse than no line, because the reader cannot tell our
// mistake from theirs. So: brew if it is actually here, else brew's own installer
// first; apt or dnf by which one exists; winget on Windows.
func ghInstallLine() string {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("brew"); err == nil {
			return "brew install gh"
		}
		// Homebrew first, gh second — two commands, in the order they must run.
		return `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"` +
			"   then:  brew install gh"
	case "windows":
		return "winget install --id GitHub.cli"
	default:
		if _, err := exec.LookPath("dnf"); err == nil {
			return "sudo dnf install gh"
		}
		if _, err := exec.LookPath("apt-get"); err == nil {
			return "sudo apt-get install gh"
		}
		if _, err := exec.LookPath("apt"); err == nil {
			return "sudo apt install gh"
		}
		// Neither package manager found: name the project's own page rather than
		// guessing a distro.
		return "see https://github.com/cli/cli#installation"
	}
}

// printManualRail is offer (b): the commands, from the right directory, with the
// one sentence that decides whether the last of them works.
func printManualRail(out *os.File, dir, repo string) {
	fmt.Fprintf(out, "     open   https://github.com/new?name=%s        # prefilled; create it private, no README\n", repo)
	// THE `cd` IS THE BUG THIS EXISTS TO NOT REPEAT (P-12): without it the learner
	// runs these in the PARENT folder, and the repo they push is their home
	// directory. `start` has already run `git init` in here, so the rail is two
	// commands, not five.
	fmt.Fprintf(out, "     then   cd %s\n", quoteIfSpaced(dir))
	fmt.Fprintf(out, "            git remote add origin https://github.com/<you>/%s.git\n", repo)
	fmt.Fprintln(out, "            git push -u origin main")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "     GitHub will ask for a token, not your password — make one here:")
	fmt.Fprintf(out, "     %s\n", ghTokenDoc)
}

// createAndPush runs the machine's side of the one confirm: make sure there is a
// local repo with a commit in it, then let gh create the private repo and push.
// The summary line names exactly the steps that will run.
func createAndPush(out *os.File, dir, course, repo string, yes bool) (string, error) {
	if err := ensureLocalRepo(out, dir, course, yes, "sboot repo"); err != nil {
		return "", err
	}
	// ensureLocalRepo leaves the commit UNDONE when git has no identity to make it
	// under (P-13), and says so. Stop here rather than let `gh repo create --push`
	// fail on a repo with no commits — that error is gh's, arrives after the
	// identity to-do above, and reads as a second problem when it is the same one.
	if !gitHasHead(dir) {
		return "", errors.New("nothing is committed yet — finish the steps above, then `sboot repo`")
	}
	fmt.Fprintf(out, "── gh repo create %s --private --source . --push\n", repo)

	create := exec.Command("gh", "repo", "create", repo, "--private", "--source", ".", "--push")
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
		return "github.com/" + user + "/" + repo, nil
	}
	return "github.com/<you>/" + repo, nil
}

// ── the LOCAL repo: what `sboot start` makes, before GitHub is a question ───────

// errDevTools is an ANSWER, not a failure to report: by the time it is returned
// the Command Line Tools remedy has already been printed, and the caller's whole
// job is to not talk over it. Compared with errors.Is, never rendered.
var errDevTools = errors.New("this Mac has no Command Line Tools, so git cannot run")

// localRepoStep is the whole `── your history starts here` step — the local repo,
// and then the ONE true thing about what happened. Both `start` paths call it, so
// the fresh run and the repair cannot drift.
//
// THE TWO SENTENCES ARE CONDITIONAL, and that is the 2026-09-03 macOS lap's
// finding (G161): "nothing here blocks the course" is FALSE on a Mac whose git is
// missing, because the missing git and the missing linker are the same package —
// the first build stops at the same place. And `sboot repo` is a nudge toward a
// command that would refuse for exactly the same reason. On Linux and Windows a
// missing git stays orthogonal to the toolchain and both lines are true, so both
// still print.
func localRepoStep(out *os.File, dir, course string, yes bool) {
	err := ensureLocalRepo(out, dir, course, yes, "sboot start "+course)
	if errors.Is(err, errDevTools) {
		return
	}
	if err != nil {
		fmt.Fprintf(out, "   %v\n", err)
		fmt.Fprintln(out, "   nothing here blocks the course — the files are on disk either way.")
	}
	if _, ok := existingRemote(dir); !ok {
		fmt.Fprintf(out, "   when you want it on GitHub: %s\n", painter(out)(ansiGreen, "sboot repo"))
	}
}

// printDevToolsTodo is what a Mac with no Command Line Tools gets instead of a
// note it cannot act on.
//
// WHAT SHIPPED BEFORE THIS (macos lap, sboot-v0.12.0): `/usr/bin/git` is Apple's
// install-on-demand SHIM, so the `exec.LookPath("git")` gate passed on a machine
// with no git at all, and `git init` then failed with
// `xcode-select: note: No developer tools were found, requesting install.` —
// which `sboot start` printed verbatim as "git init failed: <that>". A learner
// three minutes into their first course was handed the operating system's own
// half-sentence, with no command to run.
//
// `retry` is the command that brought them here, so the sentence names the thing
// they already typed rather than a second one to learn.
func printDevToolsTodo(out *os.File, retry string) {
	fmt.Fprintln(out, "   git here is Apple's, and this Mac has no Command Line Tools yet — so it cannot run:")
	// The same line, and the same measured size, as the missing-linker rung in
	// toolchain.go: it is one install, so it must read as one install.
	fmt.Fprintf(out, "     %s     # ~900 MB, once per machine; click through the dialog\n", pkgInstall("build-essential"))
	// Wrapped by hand at ~85 columns, like the rest of this command's narration.
	fmt.Fprintf(out, "   when it finishes, run `%s` again here — it keeps your\n", retry)
	fmt.Fprintln(out, "   files and only adds the repository.")
	fmt.Fprintln(out, "   your files are already on disk, but the course does need that install too: Rust")
	fmt.Fprintln(out, "   hands your code to the same tools to link it, so the first build stops here too.")
}

// gitFailed turns a git command that failed into the answer the reader can act
// on: on a Mac with no developer tools EVERY git command fails with the same
// note, and the remedy is the install — not the note.
func gitFailed(out *os.File, what string, b []byte, err error, retry string) error {
	if missingDevTools(hostOS, string(b)) {
		printDevToolsTodo(out, retry)
		return errDevTools
	}
	return fmt.Errorf("%s failed: %s", what, firstLine(b, err))
}

// ensureLocalRepo makes `dir` a git repository with at least one commit in it.
//
// Called by `sboot start` (where it is the whole GitHub-free half of P-21) and by
// `sboot repo` (where it is the precondition `gh repo create --source .` needs).
// Idempotent by construction: an existing .git is left alone, and an existing HEAD
// means there is nothing to commit.
//
// It returns an error only for a git failure the learner has to act on. A MISSING
// IDENTITY IS NOT ONE: it is answered by printing what to run and leaving the
// commit undone, because the alternative — committing under git's guess — is what
// stamped `you@<hostname>.local` on the founder's own first commit (P-13), in a
// repo built to be shown to people.
func ensureLocalRepo(out *os.File, dir, course string, yes bool, retry string) error {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintf(out, "   git is not installed — %s\n", pkgInstall("git"))
		fmt.Fprintln(out, "   install it, then `sboot repo` makes the repo and pushes it.")
		return nil
	}
	// The shim, caught before a doomed command runs: on macOS the check above
	// passes whether or not there is a git behind /usr/bin/git, and `xcode-select
	// -p` is the cheap question that actually answers it.
	if devToolsAbsent(hostOS) {
		printDevToolsTodo(out, retry)
		return errDevTools
	}
	if !isDir(filepath.Join(dir, ".git")) {
		if b, err := gitRun(dir, "init", "-q", "-b", "main"); err != nil {
			// `-b main` needs git ≥ 2.28; retry without it rather than refuse on an
			// older git, which is the whole reason this is not one call.
			if b2, err2 := gitRun(dir, "init", "-q"); err2 != nil {
				return gitFailed(out, "git init", append(b, b2...), err, retry)
			}
		}
		fmt.Fprintf(out, "   git repo created in %s\n", filepath.Base(dir))
	}
	if gitHasHead(dir) {
		return nil
	}
	name, email := gitIdentity(dir)
	if name == "" || email == "" {
		name, email = askIdentity(out, dir, name, email, yes)
	}
	if name == "" || email == "" {
		printIdentityTodo(out, dir, course)
		return nil
	}
	if b, err := gitRun(dir, "add", "-A"); err != nil {
		return gitFailed(out, "git add", b, err, retry)
	}
	if b, err := gitRun(dir, "commit", "-q", "-m", "start "+course); err != nil {
		return gitFailed(out, "git commit", b, err, retry)
	}
	fmt.Fprintf(out, "   first commit made as %s <%s>\n", name, email)
	return nil
}

// gitIdentity reads the identity git WOULD use here, the way git itself resolves
// it: the environment first (`GIT_AUTHOR_*` / `GIT_COMMITTER_*`, then `EMAIL`),
// and only then the local → global → system config, asked of git rather than
// reimplemented — so a learner who set it any of the ways git supports is never
// asked again.
//
// THE ENVIRONMENT HALF IS NOT OPTIONAL (skeptic, 2026-09-03): the first cut read
// only `git config`, and a machine with an identity exported but nothing in
// ~/.gitconfig — every CI runner, and anyone who keeps their identity in a shell
// profile — was told "git has no name and email yet" and left uncommitted. The
// tests in this package supply their identity exactly that way, and two of them
// failed on a HOME with no gitconfig, which is what GitHub's runners are.
func gitIdentity(dir string) (name, email string) {
	name = strings.TrimSpace(firstNonEmpty(os.Getenv("GIT_AUTHOR_NAME"),
		os.Getenv("GIT_COMMITTER_NAME"), gitConfigGet(dir, "user.name")))
	email = strings.TrimSpace(firstNonEmpty(os.Getenv("GIT_AUTHOR_EMAIL"),
		os.Getenv("GIT_COMMITTER_EMAIL"), os.Getenv("EMAIL"), gitConfigGet(dir, "user.email")))
	return name, email
}

func gitConfigGet(dir, key string) string {
	b, err := gitRun(dir, "config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// askIdentity asks for the two values git needs and writes them --global.
//
// GLOBAL, not local: this is the identity on every commit they will ever make on
// this machine, and asking again in the next course would read as a bug. The name
// defaults to the GitHub handle this machine is logged in as, because that is the
// name the repo will live under anyway.
//
// Silent and non-committal without a terminal — and with --yes too, which is the
// one place --yes cannot mean "yes": an email cannot be invented, and a wrong one
// is permanent in the history of a repo built to be shown to people.
func askIdentity(out *os.File, dir, name, email string, yes bool) (string, string) {
	if !interactiveTTY() || yes {
		return name, email
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "   git stamps a name and an email on every commit, and has neither yet.")
	fmt.Fprintln(out, "   set once, used for every commit you ever make on this machine:")
	if name == "" {
		name = ask(out, "   name  ", defaultGitName())
	}
	if email == "" {
		email = ask(out, "   email ", "")
	}
	if name == "" || email == "" {
		return "", ""
	}
	for _, kv := range [][2]string{{"user.name", name}, {"user.email", email}} {
		if b, err := gitRun(dir, "config", "--global", kv[0], kv[1]); err != nil {
			fmt.Fprintf(out, "   could not save %s: %s\n", kv[0], firstLine(b, err))
			return "", ""
		}
	}
	return name, email
}

// defaultGitName is the GitHub handle this machine is logged in as, or "".
func defaultGitName() string {
	if c := loadCredentials(); c != nil {
		return c.Handle
	}
	return ""
}

// printIdentityTodo is what an unanswerable identity question leaves behind: the
// exact commands, in order, including the commit that did not happen.
func printIdentityTodo(out *os.File, dir, course string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "   your work is saved, but not committed — git has no name and email yet:")
	fmt.Fprintln(out, `     git config --global user.name "Your Name"`)
	fmt.Fprintln(out, "     git config --global user.email you@example.com")
	fmt.Fprintf(out, "     cd %s && git add -A && git commit -m %q\n", quoteIfSpaced(dir), "start "+course)
}

// ── the nudge: your repo has no remote yet ──────────────────────────────────────

// nudgeKey is one course's repo nudge, in state.json's per-day map.
func nudgeKey(course string) string { return "repo/" + course }

// repoNudge is the one line that names `sboot repo` to a learner whose workspace
// has no remote. Cadence (ratified 2026-09-02, P-21): after a PASSING submit,
// every time — the moment there is something worth showing — and on `test` at most
// once a day, which is what keeps the practice loop free of it.
//
// It says nothing at all when git is missing, when there is no repo yet, or when a
// remote exists: a nudge toward a command that would refuse is noise.
//
// AND IT NEVER CLAIMS A COMMIT THAT DOES NOT EXIST (G162, the 2026-09-03 macOS
// lap). `sboot start` leaves the first commit UNDONE when git has no identity to
// make it under (P-13) — a real state, reached on that lap and on any machine
// whose owner has never configured git — and this line was telling that learner
// "your work is committed here" on every `sboot test`. It was wrong twice over:
// the commit is the thing that has not happened, and `sboot repo` would refuse
// with "nothing is committed yet" if they followed it. With a repo and no HEAD
// the truthful line is the one that finishes the commit.
func repoNudge(r repo, daily bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	if !isDir(filepath.Join(r.dir, ".git")) {
		return
	}
	if _, ok := existingRemote(r.dir); ok {
		return
	}
	if daily {
		st := loadState()
		if !st.nudgeDue(nudgeKey(r.course)) {
			return
		}
		st.markNudged(nudgeKey(r.course))
		saveQuietly(st)
	}
	p := painter(os.Stderr)
	if !gitHasHead(r.dir) {
		// Which of the two reasons it is decides which commands close it, and the
		// identity one is the common case: it is the branch `start` takes.
		if name, email := gitIdentity(r.dir); name == "" || email == "" {
			printIdentityTodo(os.Stderr, r.dir, r.course)
			return
		}
		fmt.Fprintf(os.Stderr, "nothing is committed here yet — %s, then %s\n",
			p(ansiGreen, "git add -A && git commit"), p(ansiGreen, "sboot repo"))
		return
	}
	fmt.Fprintf(os.Stderr, "your work is committed here but has no home on GitHub yet — %s\n",
		p(ansiGreen, "sboot repo"))
}

// ── plumbing ────────────────────────────────────────────────────────────────────

// existingRemote is the origin of the repo AT `dir` — and only of that one. Without
// the `.git` check, `git -C dir remote get-url` walks UP and answers for an
// enclosing repository (a home directory under version control, a `~/dev` that is
// itself a repo), so a workspace with no repo of its own would be reported as
// "remote already set" and never offered one — the parent-becomes-the-repo bug
// (P-12) in a second costume.
func existingRemote(dir string) (string, bool) {
	if !isDir(filepath.Join(dir, ".git")) {
		return "", false
	}
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

// quoteIfSpaced makes a path safe to paste into a shell. A learner's home
// directory really can be `/Users/Anna Smith/…`, and an unquoted `cd` there
// fails with an error about a directory half its name.
func quoteIfSpaced(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
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

// ask reads one free-text line, offering `def` as the bracketed default. Callers
// gate on TTY, exactly as confirm's do.
func ask(out *os.File, label, def string) string {
	if def != "" {
		fmt.Fprintf(out, "%s[%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	if v := strings.TrimSpace(line); v != "" {
		return v
	}
	return def
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
