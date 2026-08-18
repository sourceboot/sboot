// sboot login / logout / whoami — first-party device connect (ux-plan §11.f).
//
// The flow is the Stripe/gh pairing shape over OUR platform, not GitHub's device
// flow: `sboot login` asks the platform to mint a pairing code, prints the code
// and the approve URL, the already-signed-in browser approves the machine, and
// the CLI polls until a token arrives. The token lands in
// <state-dir>/credentials.json, mode 0600, next to state.json.
//
// The wire contract (platform: /api/v1/device — see its route.ts for the
// server half; the field names are /api/v1-append-only and live forever):
//
//	POST /api/v1/device {"host": "<hostname>"}
//	  → 200 {"code", "connect_url", "poll", "interval_secs", "expires_in_secs"}
//	    `poll` is a RELATIVE path carrying a per-pairing secret — the CLI polls
//	    exactly what it is handed and never constructs the URL itself
//	  → 404/405 from a platform released before this flow: fall back to the
//	    copied-token path on /account, by name.
//	GET <api><poll>
//	  → 202 {"pending": true}    approved by nobody yet — poll again
//	  → 200 {"token", "handle"}  approved — store and stop
//	  → 410                      expired, denied, or already collected — stop
//	  → 429                      polling too fast — just wait out the interval
//
// TOKEN RESOLUTION ORDER, used by every authed request (main.go authedRequest):
// SBOOT_TOKEN env → the credentials file, if its `site` matches the platform this
// run is talking to (mismatch warns and is NOT used — a token minted by one
// deployment is noise on another) → the local dev default. The env override is
// deliberate and permanent: CI and the copied-token fallback both live there.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const credentialsFile = "credentials.json"

// credentials is the stored login: the token, which platform issued it, and the
// handle it belonged to when stored (display nicety — `whoami` always re-asks).
type credentials struct {
	Token  string `json:"token"`
	Site   string `json:"site"`
	Handle string `json:"handle,omitempty"`
}

func credentialsPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialsFile), nil
}

func loadCredentials() *credentials {
	p, err := credentialsPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var c credentials
	if json.Unmarshal(b, &c) != nil || c.Token == "" {
		return nil
	}
	return &c
}

func saveCredentials(c *credentials) error {
	p, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// siteMismatchNoted keeps the warning to once per run (this binary has no
// goroutines; a bool is the whole synchronisation story).
var siteMismatchNoted bool

// resolveToken is THE token rule. It returns the token and where it came from
// ("env" | "login" | "dev"), so callers can name the source in messages.
func resolveToken() (token, source string) {
	if t := os.Getenv("SBOOT_TOKEN"); t != "" {
		return t, "env"
	}
	if c := loadCredentials(); c != nil {
		if c.Site == "" || strings.TrimRight(c.Site, "/") == apiURL() {
			return c.Token, "login"
		}
		if !siteMismatchNoted {
			siteMismatchNoted = true
			fmt.Fprintf(os.Stderr, "sboot: your stored login is for %s, not %s — not using it.\n",
				c.Site, apiURL())
			fmt.Fprintf(os.Stderr, "sboot: run `sboot login` against this platform, or set SBOOT_TOKEN.\n")
		}
	}
	return "sboot-dev-token", "dev"
}

// ── login ───────────────────────────────────────────────────────────────────────

// devicePostResp is the minted pairing code. Append-only, like every /api/v1
// shape. `Poll` is the server-issued poll path (it carries the pairing's
// secret); the CLI follows it verbatim rather than knowing the route.
type devicePostResp struct {
	Code       string `json:"code"`
	ConnectURL string `json:"connect_url"`
	Poll       string `json:"poll"`
	Interval   int    `json:"interval_secs"`
	ExpiresIn  int    `json:"expires_in_secs"`
	Err        string `json:"error"`
}

type devicePollResp struct {
	Token  string `json:"token"`
	Handle string `json:"handle"`
	Err    string `json:"error"`
}

func runLogin() int {
	out := os.Stderr // the flow is conversation, not data — stderr end to end
	if os.Getenv("SBOOT_TOKEN") != "" {
		fmt.Fprintln(out, "sboot: note — SBOOT_TOKEN is set and always overrides a stored login.")
	}
	if offline() {
		fmt.Fprintln(out, "sboot: SBOOT_OFFLINE is set — connecting needs the network. Unset it and retry.")
		return 2
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}
	body, _ := json.Marshal(map[string]string{"host": host})
	req, err := authedRequest("POST", apiURL()+"/api/v1/device", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(out, "sboot: %v\n", err)
		return 2
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(&http.Client{Timeout: 15 * time.Second}, req)
	if err != nil {
		fmt.Fprintf(out, "sboot: could not reach the platform (%v)\n", err)
		fmt.Fprintf(out, "sboot: connect once online, or paste a token from %s/account into SBOOT_TOKEN.\n", siteURL())
		return 2
	}
	var minted devicePostResp
	decodeErr := json.NewDecoder(resp.Body).Decode(&minted)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		// A platform released before device connect existed. The copied-token path
		// is the documented fallback (ux-plan §11.f), so hand it over by name.
		fmt.Fprintf(out, "sboot: this platform does not support `sboot login` yet.\n")
		fmt.Fprintf(out, "sboot: get a token at %s/account and set it:  export SBOOT_TOKEN=<the token>\n", siteURL())
		return 2
	}
	if decodeErr != nil || minted.Code == "" || minted.ConnectURL == "" || minted.Poll == "" {
		if minted.Err != "" {
			fmt.Fprintf(out, "sboot: the platform refused to mint a pairing code: %s (HTTP %d)\n", minted.Err, resp.StatusCode)
		} else {
			fmt.Fprintf(out, "sboot: unexpected response minting a pairing code (HTTP %d)\n", resp.StatusCode)
		}
		fmt.Fprintf(out, "sboot: fallback: get a token at %s/account and set SBOOT_TOKEN.\n", siteURL())
		return 2
	}

	fmt.Fprintf(out, "connect this machine to your %s account\n\n", brandName)
	fmt.Fprintf(out, "  code    %s\n\n", minted.Code)
	fmt.Fprintf(out, "  open    %s\n", minted.ConnectURL)
	fmt.Fprintf(out, "          # already signed in there? one click approves this machine\n\n")
	tryOpen(minted.ConnectURL)

	interval := time.Duration(minted.Interval) * time.Second
	if interval < time.Second {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(10 * time.Minute)
	if minted.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(minted.ExpiresIn) * time.Second)
	}
	pollURL := minted.Poll
	if strings.HasPrefix(pollURL, "/") {
		pollURL = apiURL() + pollURL
	}

	stop := startSpinner(out, "  waiting for approval ")
	granted, code := pollDevice(pollURL, interval, deadline)
	stop()
	if code != 0 {
		return code
	}

	handle := granted.Handle
	if err := saveCredentials(&credentials{Token: granted.Token, Site: apiURL(), Handle: handle}); err != nil {
		// The approval happened; losing the file must not eat the token.
		fmt.Fprintf(out, "sboot: connected, but the token could not be stored (%v).\n", err)
		fmt.Fprintf(out, "sboot: use it directly:  export SBOOT_TOKEN=%s\n", granted.Token)
		return 1
	}
	p, _ := credentialsPath()
	if handle != "" {
		fmt.Fprintf(out, "\n✓ connected as @%s\n", handle)
	} else {
		fmt.Fprintf(out, "\n✓ connected\n")
	}
	fmt.Fprintf(out, "  token stored in %s (0600) — SBOOT_TOKEN env still overrides.\n\n", p)
	fmt.Fprintf(out, "next:  sboot courses              # pick a course, then `sboot start <id>`\n")
	return 0
}

// pollDevice waits for the browser's approval. Transient trouble (network blips,
// 5xx) is retried; only the platform's explicit answers stop the loop.
func pollDevice(url string, interval time.Duration, deadline time.Time) (devicePollResp, int) {
	out := os.Stderr
	misses := 0
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(out, "\nsboot: the code expired before it was approved — run `sboot login` again.")
			return devicePollResp{}, 1
		}
		req, err := authedRequest("GET", url, nil)
		if err != nil {
			fmt.Fprintf(out, "\nsboot: %v\n", err)
			return devicePollResp{}, 2
		}
		resp, err := send(&http.Client{Timeout: 15 * time.Second}, req)
		if err != nil {
			time.Sleep(interval)
			continue
		}
		var r devicePollResp
		_ = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusGone:
			fmt.Fprintln(out, "\nsboot: the code expired or the request was denied — nothing was connected.")
			fmt.Fprintln(out, "sboot: run `sboot login` again for a fresh code.")
			return devicePollResp{}, 1
		case resp.StatusCode == http.StatusOK && r.Token != "":
			return r, 0
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
			misses = 0
		case resp.StatusCode == http.StatusNotFound:
			// Either "unknown code" or a route that vanished mid-poll. A few in a row
			// means nobody will ever approve this.
			if misses++; misses >= 5 {
				fmt.Fprintln(out, "\nsboot: the platform no longer recognises this pairing code — run `sboot login` again.")
				return devicePollResp{}, 1
			}
		}
		time.Sleep(interval)
	}
}

// ── logout / whoami ─────────────────────────────────────────────────────────────

func runLogout() int {
	p, err := credentialsPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	if _, statErr := os.Stat(p); statErr != nil {
		fmt.Fprintln(os.Stderr, "nothing to log out — no stored login on this machine.")
	} else if err := os.Remove(p); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: could not remove %s: %v\n", p, err)
		return 1
	} else {
		fmt.Fprintf(os.Stderr, "logged out — removed %s.\n", p)
	}
	if os.Getenv("SBOOT_TOKEN") != "" {
		fmt.Fprintln(os.Stderr, "note: SBOOT_TOKEN is still set in your environment and still works — unset it too if you meant to disconnect.")
	}
	return 0
}

// runWhoami answers with what the CURRENT credential resolves to — a cheap authed
// call, not a read of the file, because the file is not who you are to the
// platform (SBOOT_TOKEN may override it, the token may be revoked).
func runWhoami() int {
	_, source := resolveToken()
	if offline() {
		fmt.Fprintln(os.Stderr, "sboot: SBOOT_OFFLINE is set — whoami asks the platform, so it cannot answer offline.")
		return 1
	}
	req, err := authedRequest("GET", apiURL()+"/api/v1/completions?limit=1", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: could not reach the platform (%v)\n", err)
		return 1
	}
	defer resp.Body.Close()
	var r struct {
		User string `json:"user"`
		Err  string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Fprintf(os.Stderr, "sboot: not connected — the platform did not accept your token (%s).\n", tokenSourceNoun(source))
		fmt.Fprintf(os.Stderr, "sboot: reconnect with `sboot login`, or paste a fresh token from %s/account into SBOOT_TOKEN.\n", siteURL())
		return 1
	}
	if resp.StatusCode != http.StatusOK || r.User == "" {
		if r.Err == "" {
			r.Err = "unexpected response"
		}
		fmt.Fprintf(os.Stderr, "sboot: %s (HTTP %d)\n", r.Err, resp.StatusCode)
		return 1
	}
	fmt.Printf("@%s\n", r.User)
	fmt.Fprintf(os.Stderr, "  %s · token: %s\n", apiURL(), tokenSourceNoun(source))
	return 0
}

func tokenSourceNoun(source string) string {
	switch source {
	case "env":
		return "SBOOT_TOKEN"
	case "login":
		return "stored login (sboot login)"
	default:
		return "the dev default"
	}
}

// ── small shared machinery ──────────────────────────────────────────────────────

// startSpinner animates on a TTY and prints the line once anywhere else (CI, a
// pipe). The returned stop erases the spinner glyph so the next line starts
// clean. >2s feedback is §12.2 rule 5; the login poll is the one CLI-owned wait
// this release adds.
func startSpinner(f *os.File, label string) (stop func()) {
	if !isTerminal(f) {
		fmt.Fprintln(f, label+"…")
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		i := 0
		for {
			select {
			case <-done:
				fmt.Fprintf(f, "\r%s… done\n", label)
				close(finished)
				return
			default:
				fmt.Fprintf(f, "\r%s… %c", label, frames[i%len(frames)])
				i++
				time.Sleep(120 * time.Millisecond)
			}
		}
	}()
	return func() { close(done); <-finished }
}

// tryOpen hands a URL to the OS browser, best-effort and only when a human is
// plausibly present (a TTY): a CI job must never spawn a browser. Errors are
// silent — the URL was already printed, which is the part that must work.
func tryOpen(url string) {
	if !isTerminal(os.Stderr) && !isTerminal(os.Stdout) {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
}
