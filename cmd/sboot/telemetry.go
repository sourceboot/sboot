// Hint-usage telemetry: one content-free line per hint shown (2026-09-13; ledger G272).
//
// WHAT IT IS FOR. The founder panel can see where a learner's runs stop (practice
// runs, `POST /api/v1/completions`) but not where they got STUCK and asked for help:
// the ladder is fully local, so five identical `sboot hint` runs on one compile error
// (a measured beginner stall, 2026-09-12) left no trace anywhere. This sends one event
// per hint shown, and /founder aggregates them per course · lab · check.
//
// WHAT IT CARRIES, EXHAUSTIVELY: the course id, the lab id, the check id (or "build"),
// how deep the hint was, and which kind of hint it was. NEVER code, NEVER compiler
// output, NEVER a file path — the struct below is the whole payload, and the platform
// route refuses any other key (platform/lib/hint-telemetry.ts).
//
// HOW IT BEHAVES. Sent AFTER the hint is printed, with a short timeout, and every
// failure is swallowed: a hint is the learner's help and must never wait on, or
// break for, our bookkeeping. SBOOT_OFFLINE skips it entirely, as it promises to skip
// every network call. A machine that is not signed in and talks to production skips
// it too — the dev token is refused there, so the request could only ever cost time.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// hintShown is what one printed hint reports. kind "" means "nothing to report"
// (no hint was shown), which is the zero value on purpose.
type hintShown struct {
	check string
	// 0 | 1 | 2 | "evidence" | "past" — see platform/lib/hint-telemetry.ts HINT_RUNGS.
	rung any
	kind string
}

// hintEvent is the WHOLE wire payload of POST /api/v1/telemetry. Adding a field is a
// privacy decision, not a refactor: the route rejects unknown keys.
type hintEvent struct {
	Event  string `json:"event"`
	Course string `json:"course"`
	Stage  string `json:"stage"`
	Check  string `json:"check"`
	Rung   any    `json:"rung"`
	Kind   string `json:"kind"`
}

// hintTelemetryTimeout is the most a hint can wait on the network, after it is
// already on the screen. A var so a test can prove the bound without waiting it out.
var hintTelemetryTimeout = 1500 * time.Millisecond

// reportHintShown sends one hint_shown event, best effort.
func reportHintShown(course, stage string, h hintShown) {
	if h.kind == "" || offline() {
		return
	}
	if _, source := resolveToken(); source == "dev" && apiURL() == defaultAPI {
		return
	}
	body, err := json.Marshal(hintEvent{
		Event:  "hint_shown",
		Course: course,
		Stage:  stage,
		Check:  h.check,
		Rung:   h.rung,
		Kind:   h.kind,
	})
	if err != nil {
		return
	}
	req, err := authedRequest("POST", apiURL()+"/api/v1/telemetry", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(&http.Client{Timeout: hintTelemetryTimeout}, req)
	if err != nil {
		debugf("hint telemetry not sent: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}
