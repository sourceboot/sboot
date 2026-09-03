// The update nudge — this binary's half of the notice channel.
//
// ── WHY IT SHIPS IN v0.1.0 ──────────────────────────────────────────────────────
// You cannot roll back a binary on someone else's laptop. A bad release is never
// recalled, only announced — and the only announcement that reaches a binary that
// is already installed is a header on a request it was going to make anyway. A
// v0.1.0 without this code can never be told anything, for the rest of its life on
// that machine. That is why this is not a "later" feature, and why the throttling
// and kill switches are here from the first tag too: a nudge that annoys people is
// a nudge they will find a way to silence, and then the incident channel is gone.
//
// Spec: the CLI release policy §1. Server half: platform/lib/cli-version.ts, whose
// values come from the committed platform/config/cli.yaml (so what learners are
// told never requires a CLI release).
//
// FOUR RULES THAT ARE EASY TO GET WRONG, all asserted in update_test.go:
//
//  1. `!=`, never `<`. After a bad release `latest` points BACKWARDS at the last
//     good version, so "an update is available" becomes a downgrade prompt (§7).
//  2. stderr, and only after the verdict. `sboot test`'s stdout is parseable and
//     nothing may appear between "── building" and the result.
//  3. Its throttle lives in its OWN file. loadState() discards state whose Version
//     != stateVersion, so putting nudge bookkeeping in state.json would make a
//     routine stateVersion bump silently reset everyone's guidance counters.
//  4. Any network failure is silence. An offline learner is unnudgeable by
//     construction, and that is accepted — it is strictly better than a tool that
//     complains about the network while grading perfectly well without it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Header names, matching CLI_HEADERS in platform/lib/cli-version.ts.
const (
	hdrCLIVersion    = "X-Sboot-CLI-Version"
	hdrCLILatest     = "X-Sboot-CLI-Latest"
	hdrCLIMin        = "X-Sboot-CLI-Min"
	hdrCLIDeprecated = "X-Sboot-CLI-Deprecated-Below"
	hdrCLINotice     = "X-Sboot-CLI-Notice"
)

// nudgeInterval is how often an ordinary "there is a newer version" line may
// appear — and, since 2026-09-02, how often the same NOTICE text may repeat. A
// deprecation warning still ignores it, and so does a notice nobody has been shown
// yet (dueForNotice): the ration is on repetition, never on news.
const nudgeInterval = 24 * time.Hour

// noticeMax bounds what we will print from the server. The server already
// sanitises (platform/lib/cli-version.ts), but this binary cannot be patched, so
// it does not rely on that: the worst case a hostile or misconfigured header can
// produce here is one ugly line, never forged multi-line output that could be
// mistaken for sboot's own.
const noticeMax = 200

// channel is what the platform told us during this run. Package-level because it
// is written from every HTTP call site and read once, at exit.
type cliChannel struct {
	seen            bool
	latest          string
	min             string
	deprecatedBelow string
	notice          string
}

var channel cliChannel

func resetChannelForTests() { channel = cliChannel{} }

// noteCLIHeaders records the channel from any response. Called on every authed
// request; the LAST answer wins, which is right — it is the freshest.
func noteCLIHeaders(resp *http.Response) {
	if resp == nil {
		return
	}
	get := func(k string) string { return strings.TrimSpace(resp.Header.Get(k)) }
	latest, min := get(hdrCLILatest), get(hdrCLIMin)
	dep, notice := get(hdrCLIDeprecated), get(hdrCLINotice)
	if latest == "" && min == "" && dep == "" && notice == "" {
		// An older platform, a proxy that stripped them, or a non-API host. Not
		// "no news" — no channel at all, which must stay indistinguishable from
		// offline so both fail the same silent way.
		return
	}
	channel = cliChannel{seen: true, latest: latest, min: min, deprecatedBelow: dep, notice: notice}
}

// flushCLINotice prints whatever the platform had to say, at the very end of a
// command. Called from exitWith, so it lands after the verdict on every path —
// including the failure paths, which is where a learner is most likely to be
// reading and most likely to be running something broken.
func flushCLINotice() {
	renderNotice(os.Stderr, version, time.Now())
}

// renderNotice is flushCLINotice with the clock and the writer injected, which is
// what makes the throttle and the "dev is never nudged" rule testable.
func renderNotice(w io.Writer, running string, now time.Time) {
	// A source build fixes itself with `git pull && go build`. Nudging it would
	// tell someone hacking on this file to replace their own binary.
	if running == "dev" || running == "" {
		return
	}
	// A documented kill switch is a trust signal for an audience that distrusts
	// tools which phone home — and SBOOT_OFFLINE already promises no network
	// chatter, so it must cover what the network told us too.
	if os.Getenv("SBOOT_NO_UPDATE_CHECK") != "" || os.Getenv("SBOOT_OFFLINE") != "" {
		return
	}
	if !channel.seen {
		return
	}

	var lines []string
	// The notice comes first, and since 2026-09-02 it is THROTTLED — once per 24h
	// per distinct text.
	//
	// It used to print on every networked command, on the reasoning that a notice
	// is for incidents and an incident should not be rationed. Then the 2026-09-01
	// course renames used the channel for its other designed purpose (a standing
	// announcement, not an incident) and three fresh machines met the same 178-byte
	// line after all 32 of their networked commands — including a brand-new learner
	// for whom `rust-start` had never existed, on PowerShell, where a native stderr
	// line renders as a red error record (D-MACOS-9 / D-WIN-10).
	//
	// PER DISTINCT TEXT is what keeps the incident case whole, and it settles the
	// scope question cli-releases.md §1 left open: the throttle is the CHANNEL's,
	// but a notice nobody has been shown yet is never throttled, so a real incident
	// still reaches every binary on its very next run. Only a REPEAT is rationed.
	if n := safeLine(channel.notice); n != "" && dueForNotice(n, now) {
		lines = append(lines, "sboot: "+n)
		markNoticed(n, now)
	}

	// `!=`, never `<` (rule 1) — but on NORMALISED strings. release.yml stamps
	// "v0.1.0" while a human writes "0.1.0" in cli.yaml, and comparing those raw
	// would nudge every learner on every run, forever, from the very first release.
	if channel.latest != "" && !sameVersion(channel.latest, running) {
		deprecated := versionBelow(running, channel.deprecatedBelow)
		if deprecated || dueForNudge(now) {
			// The remedy is the installer, spelled out: `sboot upgrade` does not
			// exist (the CLI release policy §3 — "do not build yet"), and a nudge
			// naming a command that errors is worse than no nudge.
			remedy := updateCommandFor(hostOS)
			line := fmt.Sprintf(
				"sboot %s → %s available · update: %s · notes: github.com/sourceboot/sboot/releases",
				running, channel.latest, remedy)
			if deprecated {
				line = fmt.Sprintf(
					"sboot %s is deprecated (minimum supported is %s) · update to %s: %s",
					running, channel.min, channel.latest, remedy)
			}
			lines = append(lines, line)
			if !deprecated {
				markNudged(now)
			}
		}
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// updateCommandFor is HOW to update, per OS — the one line in the nudge a learner
// is expected to act on.
//
// It was the `curl … | sh` one-liner everywhere until 2026-09-02, and on Windows
// that is not merely the wrong spelling: pasted into PowerShell it produces a red
// error block about `sh` not being a cmdlet (D-WIN-2), i.e. the tool's own advice
// is the next thing that breaks. There is no install.ps1 and no `sboot upgrade`
// (cli-releases.md §3 — "do not build yet"), and a nudge must never name a command
// that does not exist, so Windows gets the instruction lab 00 step 1 already gives:
// download the new .exe and replace the one you have.
func updateCommandFor(goos string) string {
	if goos == "windows" {
		return "download sboot-windows-amd64.exe from github.com/sourceboot/sboot/releases and replace your sboot.exe"
	}
	return "curl -fsSL https://sourceboot.com/install.sh | sh"
}

// safeLine flattens and bounds a server-supplied string. See noticeMax.
func safeLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) > noticeMax {
		s = s[:noticeMax]
	}
	return s
}

// ── the throttle file ───────────────────────────────────────────────────────────

// updateStateVersion is deliberately independent of stateVersion in state.go.
// That independence IS the feature (rule 3 in the file header).
const updateStateVersion = 1

type updateState struct {
	Version int `json:"version"`
	// RFC3339. When we last printed an ordinary update nudge on this machine.
	LastNudge string `json:"last_nudge"`
	// RFC3339, and the TEXT it belongs to: the notice channel's own throttle
	// (2026-09-02). Both fields together, because "have they seen THIS one?" is
	// the question — a new notice must never inherit an old one's timestamp.
	// Append-only, like every other wire shape here: an update.json written by an
	// older binary simply lacks them, which reads as "never shown" and prints.
	LastNotice string `json:"last_notice,omitempty"`
	Notice     string `json:"notice,omitempty"`
}

// loadUpdateState reads the throttle file, or returns a zero state. Never an
// error: every caller's honest fallback is "we have shown nothing yet", which
// costs one extra line and never silences the channel.
func loadUpdateState() updateState {
	p, err := updatePath()
	if err != nil {
		return updateState{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return updateState{}
	}
	var s updateState
	if err := json.Unmarshal(b, &s); err != nil || s.Version != updateStateVersion {
		return updateState{}
	}
	return s
}

// saveUpdateState rewrites the throttle file atomically (temp + rename), the same
// way state.json is written and for the same reason: an interrupted run must not
// leave a truncated file that silently re-enables the nag.
//
// It takes the WHOLE state, so a nudge cannot clobber the notice's bookkeeping or
// the other way round — the failure that shape invites is one throttle quietly
// resetting the other every run.
func saveUpdateState(s updateState) {
	p, err := updatePath()
	if err != nil {
		return
	}
	s.Version = updateStateVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		debugf("could not create the state dir: %v", err)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		debugf("could not write the update throttle: %v", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		debugf("could not save the update throttle: %v", err)
	}
}

// dueForNotice answers "should this notice print now?" and fails OPEN, like
// dueForNudge: an unreadable file costs one extra line, which is the right way
// round for the only channel that can reach a binary we cannot patch.
//
// A notice whose TEXT differs from the last one shown is always due — that is the
// incident case, and it must not wait behind yesterday's announcement.
func dueForNotice(text string, now time.Time) bool {
	s := loadUpdateState()
	if s.Notice != text || s.LastNotice == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, s.LastNotice)
	if err != nil {
		return true
	}
	return now.Sub(last) >= nudgeInterval
}

// markNoticed records WHICH notice was shown and when.
func markNoticed(text string, now time.Time) {
	s := loadUpdateState()
	s.Notice = text
	s.LastNotice = now.UTC().Format(time.RFC3339)
	saveUpdateState(s)
}

func updatePath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update.json"), nil
}

// dueForNudge answers "has it been 24h?" and fails OPEN: an unreadable or corrupt
// file costs one extra line, which is the right way round for a mechanism whose
// whole job is to be able to reach someone.
func dueForNudge(now time.Time) bool {
	last, err := time.Parse(time.RFC3339, loadUpdateState().LastNudge)
	if err != nil {
		return true
	}
	return now.Sub(last) >= nudgeInterval
}

// markNudged records the nudge. Best-effort: a machine where we cannot write this
// gets nudged every run, which is noisy but never wrong. Atomic (temp + rename)
// for the same reason state.json is — an interrupted run must not leave a
// truncated file that silently re-enables the nag.
func markNudged(now time.Time) {
	s := loadUpdateState()
	s.LastNudge = now.UTC().Format(time.RFC3339)
	saveUpdateState(s)
}

// ── version ordering, only where ordering is actually the question ──────────────

// versionBelow reports whether `have` sorts before `floor`. Used ONLY for the
// deprecation band, which is an ordering question by nature. The update nudge
// itself compares with `!=` — see the file header, rule 1.
//
// Anything it cannot parse is treated as NOT below the floor: a pre-release
// string or a future version scheme must never produce a spurious "you are
// deprecated" on a binary we cannot fix.
func versionBelow(have, floor string) bool {
	if floor == "" || floor == "0.0.0" || have == "" {
		return false
	}
	a, okA := parseVersion(have)
	b, okB := parseVersion(floor)
	if !okA || !okB {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// sameVersion compares two version strings written by different hands.
//
// The tag is `sboot-v0.1.0`, the -ldflags stamp is `v0.1.0` (release.yml strips only
// the `sboot-`), and cli.yaml is edited by a human who will write `0.1.0`. All three
// name the same binary and must compare equal; anything else nudges a learner
// towards the version they already have, forever, on a binary we cannot patch.
// Same normalisation scripts/install.sh applies to SBOOT_VERSION.
func sameVersion(a, b string) bool {
	return normVersion(a) == normVersion(b)
}

func normVersion(s string) string {
	// Both prefixes, deliberately. The tag scheme changed from `cli-v*` to `sboot-v*`
	// on 2026-08-02 and no binary carrying the old one was ever installed by anyone —
	// but this is the function that cannot be patched in the field once it ships, so
	// it costs one TrimPrefix to be right about a string it may never see.
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "sboot-")
	s = strings.TrimPrefix(s, "cli-")
	return strings.TrimPrefix(s, "v")
}

func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = normVersion(s)
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
