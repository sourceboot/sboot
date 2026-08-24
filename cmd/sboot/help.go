// Help — short by default, complete on request (ux-plan §5 + §12.2 rules 1/9).
//
// Bare `sboot` is NOT help any more: it is the orientation screen (status.go),
// exit 0. Help is a command (`sboot help`) and the GNU-floor flags (`--help`,
// `-h`), and the short form deliberately lists ONLY the learner verbs — the
// seven things a learner does — with everything else behind `sboot help --all`.
// `sboot where` is the canonical demotion: still real, still documented, out of
// the short list (ux-plan §5, "demoted to plumbing").
package main

import (
	"fmt"
	"io"
	"os"
)

const shortHelpText = `sboot — learn by building

usage: sboot [command] [args]      bare ` + "`sboot`" + ` shows where you are and what's next

  start [course]        create a course workspace in ./<course>/ (no-arg lists the catalog)
  test [stage]          practice: grade the current lab locally (fast loop, offline-friendly)
  hint [stage] [check]  a hint for a failing check — one rung deeper per run
  explain [check]       the AI tutor on that check, fed your last run (--here: in this terminal)
  submit [stage]        official: check locally first, then upload for the server grade
  courses               the catalog + your progress
  login                 connect this machine to your account
  repo                  create your GitHub repo for this course and push (via gh)

` + "`sboot help --all`" + ` lists every command, flag and environment variable.
`

func fullHelpText() string {
	return fmt.Sprintf(`sboot — learn by building

usage: sboot [command] [args]      bare `+"`sboot`"+` shows where you are and what's next

the learner verbs:
  start [course]        create a course workspace in ./<course>/ (no-arg lists the catalog)
  test [stage]          practice: grade the current lab locally (fast loop, offline-friendly)
  hint [stage] [check]  a hint for a failing check — one rung deeper per run
  explain [check]       the AI tutor on that check, fed your last run (--here: in this terminal)
  submit [stage]        official: check locally first, then upload for the server grade
  courses               the catalog + your progress
  login                 connect this machine to your account (device connect)
  repo                  create your GitHub repo for this course and push (via gh)

everything else:
  reveal [stage]        the course's own solution for a lab — marks it solution-assisted
  logout                remove the stored login from this machine
  whoami                who the current credential resolves to
  debug [stage]         boot this stage under QEMU frozen for a debugger on :1234
  fetch [course]        download the current lab's tests on purpose (refresh/pre-cache)
  where                 print where your repo, tests and grader live
  version               print the version (also --version)
  help [--all]          this text (also --help, -h)

flags:
  --force, -f           submit even if the local check fails (submit only)
  --here                answer in this terminal instead of opening the chat (explain)
  --message, -m TEXT    your question, instead of the one explain composes (explain)
  --json                machine-readable output on `+"`sboot`"+`, test and submit
  --yes, -y             answer yes to every prompt (start, repo, reveal)
  --no-color            no ANSI color (NO_COLOR is honored too)
  --                    end of options; everything after is positional

environment:
  SBOOT_API_URL      platform URL           (default %s)
  SBOOT_TOKEN        your API token         (overrides `+"`sboot login`"+`'s stored one)
  SBOOT_COURSE       course id              (default: read from sboot.toml)
  SBOOT_COURSE_DIR   your course repo       (default: walk up from cwd)
  SBOOT_CACHE_DIR    tests + grader cache   (default: the OS data dir)
  SBOOT_STATE_DIR    login + local state    (default: the OS config dir)
  SBOOT_OFFLINE      set to skip every network call (cached tests still grade)
`, defaultAPI)
}

// printHelp writes the requested help to w. Help is the DATA of a help command,
// so the callers who answered a help request use stdout; usage errors reuse the
// short form on stderr.
func printHelp(w io.Writer, all bool) {
	if all {
		fmt.Fprint(w, fullHelpText())
		return
	}
	fmt.Fprint(w, shortHelpText)
}

// usageError reports a broken invocation: the error, then the imperative fix
// (§12.2 rule 2 — the LAST line is the next move). Exit 2, the usage code every
// suite already asserts on.
func usageError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sboot: "+format+"\n", args...)
	fmt.Fprintln(os.Stderr, "sboot: `sboot help` lists the commands; `sboot help --all` lists everything.")
	exitWith(2)
}
