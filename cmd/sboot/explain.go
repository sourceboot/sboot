// sboot explain — the ladder's rung between the written hints and the answer:
// the evidence-fed AI tutor (the failure-guidance spec "The ladder").
//
// The spec gives this verb a shape that is half CLI and half web, and both
// halves are built here because each covers what the other cannot:
//
//	default   PRINT + OPEN the lab page's `#stuck` deep link, carrying the
//	          target check. The chat lives there: it keeps context across
//	          turns, renders code, and is where the metering UI already is.
//	--here    the one-shot fallback. One question, the last run's evidence, one
//	          streamed answer, in the terminal the learner is already in —
//	          because "open a browser" is not an answer on a headless box, over
//	          ssh, or to someone who simply does not want to leave the shell.
//
// EVIDENCE IS REUSED, NEVER RE-DERIVED. Everything attached to the question
// comes from the same place `sboot hint` reads — state.json's record of the last
// LOCAL run (state.go: "test writes, hint reads"): which checks failed, in
// verdict order, the score, the VERBATIM failure block the target check printed
// (state.go Evidence — the same bytes the hint ladder's selectors read), and —
// when a run never reached a check at all — why. There is deliberately no second
// opinion here: re-parsing serial output or re-running the grader would let
// `explain` describe a failure `test` never reported.
//
// R2-1 (round-2 dogfood 2026-08-25): the failure block itself was NOT attached —
// only the check ids and the score were — so 2/2 live `--here` sessions opened by
// asking the learner to paste the very output this comment claimed was carried.
// The evidence now travels in the context block, and the endpoint's system prompt
// (platform/ai/bundle.ts EXPLAIN_INSTRUCTIONS) tells the tutor it is there and
// that asking for it again is wrong.
//
// WHAT IS NOT SENT: no source, no capture, no rubric. The CLI has no rubric to
// leak (the private one never ships) and the chat already knows the lab from the
// course/stage pair, so the message carries the learner's own words and the
// learner's own verdict, and nothing else.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// explainPath is the AI route, and it is NOT under /api/v1: the chat endpoint is
// shared with the web page (platform/app/api/ai/explain/route.ts), which is what
// keeps one metered path rather than two.
const explainPath = "/api/ai/explain"

// runExplain is both halves of the verb. Exit 0 for an answer (or a printed
// link), 2 for anything that never got to one — the CLI's "never reached a
// verdict" code, which is what a refused, unauthenticated or unreachable chat is.
func runExplain(r repo, stage string, ga gradedArgs) int {
	st := loadState()
	ev := explainEvidence(st, r.course, stage, ga.check)

	// The framing header, on stderr: what this run is about and how to aim it
	// elsewhere — the same shape `sboot hint` uses (§11.c, defaults stay visible).
	p := painter(os.Stderr)
	head := "explain — " + stage
	if ev.check != "" {
		head += " · " + ev.check
	}
	line := p(ansiAmber, head)
	if ev.checkDefaulted {
		line += p(ansiDim, " # your failing check; sboot explain <check> targets another")
	}
	fmt.Fprintln(os.Stderr, line)
	fmt.Fprintln(os.Stderr)

	if !ga.here {
		return explainOpenChat(r.course, stage, ev)
	}
	if offline() {
		reportExplainOffline("SBOOT_OFFLINE is set")
		return 2
	}
	return explainOneShot(r.course, stage, ev, ga.message)
}

// ── the default: hand off to the chat, with the check in the link ───────────────

// explainOpenChat prints the deep link and opens it where a human is plausibly
// present (tryOpen is the same best-effort path `sboot login` uses). The URL is
// printed FIRST and always, because opening a browser is the part that is allowed
// to fail.
func explainOpenChat(course, stage string, ev explainEv) int {
	u := explainURL(course, stage, ev.check)
	fmt.Println("the explain chat is on this lab's page — it keeps context across turns")
	fmt.Println("and can see the lab you are on:")
	fmt.Println()
	fmt.Println("  " + u)
	fmt.Println()
	if ev.check != "" {
		fmt.Printf("the link carries your failing check (%s), so the chat starts on it.\n", ev.check)
	}
	fmt.Println("no browser here? `sboot explain --here` answers in this terminal instead.")
	tryOpen(u)
	return 0
}

// explainURL extends the `#stuck` deep link every failing run already prints
// (main.go stageStuckURL) with the one thing the page cannot know: which check
// the learner is stuck on. Query params, not a richer fragment, because the page
// is a server component and a fragment never reaches it.
//
// ADDITIVE BY CONSTRUCTION: the page ignores parameters it does not know, so a
// CLI ahead of the deployment degrades to the plain Stuck? section rather than a
// 404. `from=cli` is there for the same reason it is on every other hand-off —
// so "how did people get here?" is answerable without guessing.
func explainURL(course, stage, check string) string {
	base := fmt.Sprintf("%s/courses/%s/stages/%s", siteURL(), course, stage)
	q := url.Values{}
	if check != "" {
		q.Set("check", check)
	}
	q.Set("from", "cli")
	return base + "?" + q.Encode() + "#stuck"
}

// ── --here: the one-shot ────────────────────────────────────────────────────────

// explainOneShot posts one turn and streams the reply. The request body is the
// web chat's own shape (`{course, stage, messages:[{role, content}]}`) because it
// is the web chat's own endpoint — one metered path, one prompt, one place where
// the tutor's behaviour is defined.
func explainOneShot(course, stage string, ev explainEv, question string) int {
	msg := explainMessage(stage, ev, question)
	body, _ := json.Marshal(map[string]any{
		"course": course,
		"stage":  stage,
		"messages": []map[string]string{
			{"role": "user", "content": msg},
		},
	})
	req, err := authedRequest("POST", apiURL()+explainPath, bytes.NewReader(body))
	if err != nil {
		reportExplainOffline(err.Error())
		return 2
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// No short timeout: this is a generation, and the server already caps itself
	// (90s). The ceiling here only exists so a dead connection cannot hang a
	// learner's terminal forever.
	resp, err := send(&http.Client{Timeout: 3 * time.Minute}, req)
	if err != nil {
		reportExplainOffline(err.Error())
		return 2
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return reportExplainRefusal(resp)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		// A platform too old to have this route, or a proxy that swallowed the
		// stream. Say which and point at the surface that certainly works.
		fmt.Fprintf(os.Stderr, "sboot: the platform answered the chat with %q, not a stream — it may be older than this CLI.\n", ct)
		fmt.Fprintf(os.Stderr, "sboot: the chat on the lab's page is the one that always works: %s\n", explainURL(course, stage, ev.check))
		return 2
	}

	stalled, streamErr := streamExplain(resp.Body, os.Stdout)
	fmt.Println()
	if streamErr != "" {
		fmt.Fprintf(os.Stderr, "sboot: %s\n", streamErr)
		fmt.Fprintf(os.Stderr, "sboot: the free ladder is unaffected: `sboot hint %s`.\n", stage)
		return 2
	}
	if stalled {
		fmt.Fprintln(os.Stderr, "sboot: the chat closed before it answered — try again, or use the page:")
		fmt.Fprintln(os.Stderr, "sboot:   "+explainURL(course, stage, ev.check))
		return 2
	}
	fmt.Printf("\nthat was one turn. the chat on the lab's page keeps the conversation going:\n  %s\n",
		explainURL(course, stage, ev.check))
	return 0
}

// streamExplain consumes the SSE the web chat consumes (platform/ai/sse.ts:
// `event: <name>` + `data: <one-line JSON>`, with `:` comment lines for the open
// and the 15s heartbeat) and prints `text` deltas AS THEY ARRIVE — a Socratic
// answer takes seconds, and watching it appear is the difference between waiting
// and staring.
//
// Returns (sawNothing, errorMessage): an `error` event is the server telling us
// why, and a stream that ends with neither `done` nor text is a truncation the
// learner must not read as an answer.
func streamExplain(body io.Reader, out io.Writer) (bool, string) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	event, got, done := "", false, false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			event = ""
		case strings.HasPrefix(line, ":"):
			// `: connected` / `: ping` — flush and heartbeat, nothing to render.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var payload struct {
				Delta string `json:"delta"`
				Err   string `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				debugf("explain: undecodable %s payload: %v", event, err)
				continue
			}
			switch event {
			case "text":
				if payload.Delta != "" {
					fmt.Fprint(out, payload.Delta)
					got = true
				}
			case "error":
				return got, firstNonEmpty(payload.Err, "the mentor could not answer that")
			case "done":
				done = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return got, "the chat connection dropped mid-answer (" + err.Error() + ")"
	}
	return !(got || done), ""
}

// reportExplainRefusal renders the route's structured refusals. The meter's 429
// is the one that matters most: it is a normal, recurring state (a daily
// allowance), so it names the number, when it resets, and the free rung the
// learner still has.
func reportExplainRefusal(resp *http.Response) int {
	var r struct {
		Err   string `json:"error"`
		Meter *struct {
			Used   int    `json:"used"`
			Limit  int    `json:"limit"`
			Resets string `json:"resets"`
		} `json:"meter"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	msg := firstNonEmpty(r.Err, "refused")
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		fmt.Fprintf(os.Stderr, "sboot: the platform did not accept your token (401: %s).\n", msg)
		fmt.Fprintln(os.Stderr, "sboot: connect this machine with `sboot login`, or paste a fresh token from")
		fmt.Fprintf(os.Stderr, "sboot:   %s/account   into SBOOT_TOKEN.\n", siteURL())
	case http.StatusTooManyRequests:
		fmt.Fprintf(os.Stderr, "sboot: %s\n", msg)
		if r.Meter != nil {
			fmt.Fprintf(os.Stderr, "sboot: %d of %d used (resets %s).\n", r.Meter.Used, r.Meter.Limit, r.Meter.Resets)
		}
		noteFreeLadder(msg)
	default:
		fmt.Fprintf(os.Stderr, "sboot: the chat could not answer (%d: %s).\n", resp.StatusCode, msg)
		noteFreeLadder(msg)
	}
	return 2
}

// noteFreeLadder points a refused learner at the rung that is never metered —
// unless the platform's own message already did. The route's refusals are
// written to name it (platform/ai/meter.ts), and saying it twice in four lines
// reads like the CLI is not listening to the server it just quoted.
func noteFreeLadder(serverMsg string) {
	if strings.Contains(serverMsg, "sboot hint") {
		return
	}
	fmt.Fprintln(os.Stderr, "sboot: the written ladder is never metered — `sboot hint` is still there.")
}

// reportExplainOffline: `--here` is the online half of an online rung. Same rule
// as reveal's — never let this read as a broken install, because the local loop
// this learner is running is provably fine.
func reportExplainOffline(detail string) {
	fmt.Fprintln(os.Stderr, "sboot: `sboot explain --here` needs the network — the tutor runs on our side.")
	fmt.Fprintln(os.Stderr, "sboot: nothing is wrong with your setup: `sboot test` and `sboot hint` keep working offline,")
	fmt.Fprintln(os.Stderr, "sboot: and the whole written ladder is already on your machine.")
	debugf("explain: %s", detail)
}

// ── the evidence, read from the same state `sboot hint` reads ───────────────────

// explainEv is what the last local run knows about this stage.
type explainEv struct {
	check          string   // the check being explained ("" when nothing has failed)
	checkDefaulted bool     // it was chosen for the learner, not named by them
	failing        []string // every failing check, in verdict order
	graded         bool     // a run has produced a verdict for this stage
	score          string   // "3/6", when the last local run recorded one
	runError       string   // "build" / "toolchain:<tool>" — a run that graded nothing
	observed       string   // the target check's verbatim failure block from the last run
}

func explainEvidence(st *guidanceState, course, stage, check string) explainEv {
	ev := explainEv{check: check, runError: st.lastRunError(course, stage)}
	ev.failing, ev.graded = st.lastFailed(course, stage)
	ev.score = st.LastScore[course+"/"+stage]
	if ev.check == "" && len(ev.failing) > 0 {
		// Same target rule as `sboot hint`: the first SPECIFIC failing check, so
		// the umbrella suite does not swallow every question (dogfood F01-5).
		ev.check = defaultHintTarget(ev.failing)
		ev.checkDefaulted = true
	}
	if ev.check != "" {
		// The failure block itself, verbatim — what the check printed on the last
		// graded run (state.go Evidence, ≤2 KB by construction). This is the R2-1
		// fix: without these bytes the tutor's only correct first move was to ask
		// for them, which is exactly what both live round-2 sessions did.
		ev.observed = st.evidence(course, stage, ev.check)
	}
	return ev
}

// explainMessage composes the one turn. The learner's words come first (their
// question is the point); the evidence follows, labelled as what it is, so the
// tutor can tell "what the learner says" from "what their machine reported".
func explainMessage(stage string, ev explainEv, question string) string {
	if strings.TrimSpace(question) == "" {
		question = defaultExplainQuestion(ev)
	}
	out := []string{strings.TrimSpace(question), "", "--", "Context from my `sboot test` run (sent by the sboot CLI):", "- lab: " + stage}
	if ev.check != "" {
		out = append(out, "- the check I'm stuck on: "+ev.check)
	}
	switch {
	case ev.runError != "":
		out = append(out, "- my last run never reached a check ("+ev.runError+"), so nothing was scored")
	case !ev.graded:
		out = append(out, "- I have not run `sboot test` on this lab yet")
	default:
		if ev.score != "" {
			out = append(out, "- score: "+ev.score)
		}
		if len(ev.failing) > 0 {
			out = append(out, "- failing checks, in order: "+strings.Join(ev.failing, ", "))
		} else {
			out = append(out, "- every check passed on my last run")
		}
		if ev.observed != "" {
			// Fenced so multi-line assert output survives as one block. The tutor's
			// system prompt names this section verbatim ("what the failing check
			// printed"), so the wording here is part of the CLI↔platform contract —
			// change both together or the model stops being told it has evidence.
			out = append(out,
				"- what the failing check printed on my last run (verbatim):",
				"", "```", strings.TrimRight(ev.observed, "\n"), "```")
		}
	}
	return strings.Join(out, "\n")
}

// defaultExplainQuestion is what a bare `sboot explain --here` asks. It is
// phrased as a learner asking for direction rather than for code, because the
// tutor's whole job is to guide and not to fix — asking it for the answer would
// spend a metered turn on a refusal.
func defaultExplainQuestion(ev explainEv) string {
	if ev.runError != "" {
		return "My build isn't getting as far as the checks. What should I look at first?"
	}
	if ev.check != "" {
		return "I'm stuck on `" + ev.check + "`. What should I look at first, and what would tell me whether I'm right?"
	}
	return "I'm stuck on this lab. What should I look at first?"
}
