// sboot reveal — the escalation ladder's LAST rung: the course's own solution,
// with deliberate friction and total honesty (the failure-guidance spec, "The
// ladder": hint → explain → reveal).
//
// The 2026-08-17 resolution retired the terminal verbs in favour of a web-only
// AI tier; Puneet REVERSED that on 2026-08-23 and they are built here. What did
// not change is the shape the spec always gave this rung:
//
//	gated      the server decides whether this lab may be revealed at all, and
//	           in what order — SKELETON first, the full REFERENCE only on a
//	           later run. The CLI never picks the step; it reads `step_taken`
//	           and narrates what comes next.
//	penalised  a reveal marks the lab solution-assisted on the learner's own
//	           progress record. D5-compatible: it annotates their record, it
//	           never touches a `verified` claim and it never blocks completing.
//	honest     the confirm says exactly those two things BEFORE anything is
//	           fetched, because a learner who is one keystroke from the answer
//	           deserves to know the price in the same breath as the offer.
//
// ONLINE BY NATURE, and the copy says so. There is no cached solution and there
// must not be one — shipping every lab's reference into the spec bundle would
// hand it to everyone who ever ran `sboot fetch`. So when the network is gone
// this verb refuses, and its refusal must never read like a broken install:
// `sboot test` and `sboot hint` are still working perfectly, offline, right now.
//
// WHERE THE TEXT LANDS. Printed to stdout, and written under the run directory
// (`sboot where`'s "scratch") — NEVER into the learner's tree. Overwriting their
// code with ours would destroy the thing they are being graded on, and a diff
// they choose to open is strictly better than a merge they did not ask for.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// revealStep names the two rungs the server serves, in order.
const (
	stepNone      = "none"
	stepSkeleton  = "skeleton"
	stepReference = "reference"
)

// revealState is GET /api/v1/reveal?course=&stage=.
type revealState struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason"`
	StepTaken string   `json:"step_taken"`
	Modules   []string `json:"modules"`
	Err       string   `json:"error"`
}

// revealFile is one file of the served step.
type revealFile struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// revealResp is POST /api/v1/reveal.
type revealResp struct {
	Step   string       `json:"step"`
	Files  []revealFile `json:"files"`
	Marked string       `json:"marked"`
	Err    string       `json:"error"`
}

// runReveal is the whole verb: state → confirm → fetch → render → save.
//
// Exit codes follow the CLI's rule (content/manual/cli.md): 1 is "asked, and the
// answer was no" (this lab has no reveal), 2 is "never got to an answer at all"
// (offline, unauthenticated, refused, a platform problem), 0 is a reveal — or a
// learner who read the price and declined, which is the verb working.
func runReveal(r repo, stage string, yes bool) int {
	if offline() {
		reportRevealOffline("SBOOT_OFFLINE is set")
		return 2
	}

	state, err := fetchRevealState(r.course, stage)
	if err != nil {
		return reportRevealError(err)
	}
	if !state.Available {
		// The server's own words for why, because it is the only side that knows
		// (a lab with no authored solution, an unmet gate, a spent allowance).
		reason := strings.TrimSpace(state.Reason)
		if reason == "" {
			reason = "this lab has no reveal"
		}
		fmt.Fprintf(os.Stderr, "sboot: no reveal for %s — %s\n", stage, reason)
		fmt.Fprintf(os.Stderr, "sboot: the written ladder is still there: `sboot hint %s`, then `sboot explain %s`.\n", stage, stage)
		return 1
	}

	step := nextRevealStep(state.StepTaken)
	fmt.Fprintln(os.Stderr, revealOffer(stage, step, state))

	if !yes {
		if !interactiveTTY() {
			fmt.Fprintln(os.Stderr, "sboot: reveal changes your record, so it asks first — and nothing here is a terminal.")
			fmt.Fprintf(os.Stderr, "sboot: re-run as `sboot reveal %s --yes` if you meant it.\n", stage)
			return 2
		}
		if !confirm(os.Stderr, fmt.Sprintf("show the %s for %s? [Y/n] ", step, stage)) {
			fmt.Fprintln(os.Stderr, "nothing was revealed — your record is untouched.")
			return 0
		}
	}

	resp, err := postReveal(r.course, stage, step)
	if err != nil {
		return reportRevealError(err)
	}

	served := resp.Step
	if served == "" {
		served = step
	}
	fmt.Println(renderRevealFiles(stage, served, resp.Files))
	if served == stepSkeleton && revealFilesContain(resp.Files, "todo!(") {
		// F04-1 (rust-start dogfood 2026-08-24): `todo!()` is a construct no
		// course teaches before a learner meets it HERE, in their first skeleton
		// — and this course's audience will wonder whether they were supposed to
		// know it. One line, printed only when the served files actually use it,
		// so a course whose skeletons are shaped differently never sees it.
		fmt.Println("(`todo!()` marks a body you haven't written yet — the file compiles, and panics if that function is ever called.)")
	}

	if dir, err := saveReveal(r, stage, served, resp.Files); err == nil {
		fmt.Printf("saved to %s\n", dir)
		fmt.Printf("your own %s/ is untouched — this is a copy to read beside your code, not a patch.\n", r.treeName())
	} else {
		// Printing already happened, so a failed write costs nothing but the copy.
		debugf("could not save the revealed files: %v", err)
	}

	if m := strings.TrimSpace(resp.Marked); m != "" {
		fmt.Printf("\n→ %s is now marked %s on your record. It never blocks completing the lab.\n", stage, m)
	}
	if served == stepSkeleton {
		fmt.Printf("still stuck after this? `sboot reveal %s` again shows the full reference.\n", stage)
	}
	return 0
}

// nextRevealStep is the CLI's reading of the server's order. The SERVER enforces
// it — this only decides what to ask for and what to narrate, so a server that
// disagrees still wins (the response's own `step` is what gets rendered).
func nextRevealStep(taken string) string {
	switch taken {
	case stepSkeleton:
		return stepReference
	case stepReference:
		// Nothing deeper exists. Asking again re-prints what has already been
		// revealed and already been recorded — a re-read, not a new penalty.
		return stepReference
	default: // "", "none", or anything a newer server grows
		return stepSkeleton
	}
}

// revealOffer is the honest confirm tile: what will be shown, what it costs, and
// what comes after. Printed BEFORE the prompt and also on `--yes`, so a
// non-interactive run still records the price in its own output.
func revealOffer(stage, step string, st revealState) string {
	p := painter(os.Stderr)
	head := "reveal — " + stage + " · "
	switch {
	case step == stepSkeleton:
		head += "step 1 of 2: the skeleton"
	case st.StepTaken == stepReference:
		head += "the reference, again"
	default:
		head += "step 2 of 2: the full reference"
	}
	out := []string{p(ansiAmber, head), ""}

	what := "the course's own solution"
	if step == stepSkeleton {
		what = "the course's own skeleton"
	}
	subject := "this lab"
	if len(st.Modules) > 0 {
		subject = strings.Join(st.Modules, ", ")
	}
	out = append(out,
		"  · shows "+what+" for "+subject,
		"  · marks this lab solution-assisted on your record — it never blocks completing it",
		"  · prints it here and saves a copy beside your run; your own files are never written")
	switch {
	case step == stepSkeleton:
		out = append(out, "  · next `sboot reveal` shows the full reference")
	case st.StepTaken == stepReference:
		out = append(out, "  · you have already revealed this — re-reading costs nothing more")
	default:
		out = append(out, "  · this is the last step; there is nothing deeper")
	}
	return strings.Join(out, "\n")
}

// renderRevealFiles prints the served files with a header per file, so a learner
// scrolling back can always tell whose file they are looking at.
func renderRevealFiles(stage, step string, files []revealFile) string {
	if len(files) == 0 {
		return fmt.Sprintf("the %s for %s came back empty — nothing to show. Report it if it persists.", step, stage)
	}
	out := []string{fmt.Sprintf("── %s · %s (the course's own %s) ──", stage, step, step)}
	for _, f := range files {
		out = append(out, "", "── "+f.Path+" ──"+strings.Repeat("─", max(0, 56-len(f.Path))),
			strings.TrimRight(f.Text, "\n"))
	}
	return strings.Join(out, "\n")
}

// revealFilesContain reports whether any served file's text carries the needle
// — what gates the `todo!()` gloss above on the skeleton actually using it.
func revealFilesContain(files []revealFile, needle string) bool {
	for _, f := range files {
		if strings.Contains(f.Text, needle) {
			return true
		}
	}
	return false
}

// saveReveal writes the served files under the run directory — the same scratch
// root `sboot where` prints — in `reveal/<stage>/<step>/`. Deliberately NOT the
// learner's tree: their code is the thing being graded, and the one thing this
// verb must never do is quietly become the author of it.
func saveReveal(r repo, stage, step string, files []revealFile) (string, error) {
	run, err := workDir(r.course, r.dir)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(run, "reveal", stage, step)
	// Absolute, because the path is PRINTED and a learner has to be able to open
	// it from wherever they are — a relative SBOOT_CACHE_DIR (the test rigs use
	// one) would otherwise print a path that only resolves from one directory.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, f := range files {
		rel, ok := safeRelPath(f.Path)
		if !ok {
			// A path we cannot place safely is skipped, not sanitised into some
			// other file: the text was already printed, which is the part that
			// matters, and writing outside the run dir is never worth guessing at.
			debugf("reveal: skipping unsafe path %q", f.Path)
			continue
		}
		dest := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dest, []byte(f.Text), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// safeRelPath keeps a server-supplied path inside the directory we chose for it.
// The server is ours, but a path that escapes is the one bug in this verb that
// could destroy a learner's work, so it is checked here rather than trusted.
func safeRelPath(p string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", false
	}
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", false
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return "", false
	}
	return filepath.Join(parts...), true
}

// ── the two calls ───────────────────────────────────────────────────────────────

func fetchRevealState(course, stage string) (revealState, error) {
	var st revealState
	url := fmt.Sprintf("%s/api/v1/reveal?course=%s&stage=%s", apiURL(), course, stage)
	req, err := authedRequest("GET", url, nil)
	if err != nil {
		return st, err
	}
	resp, err := send(&http.Client{Timeout: 15 * time.Second}, req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if resp.StatusCode != http.StatusOK {
		return st, &apiError{status: resp.StatusCode, msg: firstNonEmpty(st.Err, st.Reason, "refused")}
	}
	return st, nil
}

func postReveal(course, stage, step string) (revealResp, error) {
	var out revealResp
	body, _ := json.Marshal(map[string]string{"course": course, "stage": stage, "step": step})
	req, err := authedRequest("POST", apiURL()+"/api/v1/reveal", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(&http.Client{Timeout: 30 * time.Second}, req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return out, &apiError{status: resp.StatusCode, msg: firstNonEmpty(out.Err, "refused")}
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── refusals, in the CLI's voice ────────────────────────────────────────────────

// reportRevealError separates the three answers a learner can act on: sign in,
// wait for the network, or read what the platform said.
func reportRevealError(err error) int {
	ae, ok := err.(*apiError)
	if !ok {
		reportRevealOffline(err.Error())
		return 2
	}
	switch ae.status {
	case http.StatusUnauthorized:
		fmt.Fprintf(os.Stderr, "sboot: the platform did not accept your token (401: %s).\n", ae.msg)
		fmt.Fprintln(os.Stderr, "sboot: connect this machine with `sboot login`, or paste a fresh token from")
		fmt.Fprintf(os.Stderr, "sboot:   %s/account   into SBOOT_TOKEN.\n", siteURL())
		fmt.Fprintln(os.Stderr, "sboot: nothing was revealed and your record is untouched.")
	default:
		fmt.Fprintf(os.Stderr, "sboot: the platform refused the reveal (%d: %s).\n", ae.status, ae.msg)
		fmt.Fprintln(os.Stderr, "sboot: nothing was revealed and your record is untouched.")
	}
	return 2
}

// reportRevealOffline is the ONE message this file exists to get right: reveal is
// inherently online, and a learner reading this must not go hunting for a broken
// install. Nothing is wrong with their machine — the practice loop is still
// running on it, offline, right now.
func reportRevealOffline(detail string) {
	fmt.Fprintln(os.Stderr, "sboot: `sboot reveal` needs the network — the solution is fetched when you ask for it,")
	fmt.Fprintln(os.Stderr, "sboot: never shipped with the tests, so there is nothing cached to show.")
	fmt.Fprintln(os.Stderr, "sboot: nothing is wrong with your setup: `sboot test` and `sboot hint` keep working offline.")
	debugf("reveal: %s", detail)
}
