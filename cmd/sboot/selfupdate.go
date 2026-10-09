package main

// `sboot upgrade` (DF-4, Puneet's rust-for-systems dogfood, 2026-10-07; named
// `upgrade` by his ruling the same night — the name every binary since 0.14.1
// already prints on a 426, and the one the CLI release policy §3 specs): run the
// same installer the manual and the nudge give, over THIS binary's folder, to the
// version the server calls latest. It is not §3's download + verify + rename in
// Go: it reuses the installer, which already verifies the checksum, runs the new
// binary once, and handles the PATH block. The target arrives the way every other
// response's does — the X-Sboot-CLI-Latest header — so it needs no new /api/v1
// field. `sboot update` is accepted as an alias (one dispatcher label).

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateExe is the binary being upgraded (a seam: tests point it at a fake).
var updateExe = os.Executable

// upgradeGOOS is the OS `sboot upgrade` decides for (a seam: the Windows branch
// is pinned from a unix test run, because both of its first tests skipped there).
var upgradeGOOS = runtime.GOOS

// installShURL is the unix installer `updateCommandFor` names.
const installShURL = "https://sourceboot.com/install.sh"

// packageManagerFor names the package manager that owns `exe`, with what to run
// instead, or "" for an install of ours. SBOOT_INSTALL_METHOD (§4's escape hatch)
// overrides the path: "curl"/"ps1" are ours, anything else is not.
func packageManagerFor(exe string) (owner, instead string) {
	if m := strings.ToLower(os.Getenv("SBOOT_INSTALL_METHOD")); m != "" {
		if m == "curl" || m == "ps1" {
			return "", ""
		}
		return m, "update it the way you installed it (" + m + ")"
	}
	p := filepath.ToSlash(exe)
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/homebrew/") || strings.Contains(p, "/linuxbrew/"):
		return "Homebrew", "brew upgrade sboot"
	case strings.Contains(p, "/nix/store/"):
		return "Nix", "update it through your Nix configuration"
	case strings.HasPrefix(p, "/snap/"):
		return "snap", "sudo snap refresh sboot"
	case strings.Contains(strings.ToLower(p), "/scoop/"):
		return "Scoop", "scoop update sboot"
	}
	return "", ""
}

// dirWritable probes by creating and removing a file — the only answer that
// counts (permissions, a read-only mount, an ACL all say no the same way).
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".sboot-update-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func dirOnPath(dir string) bool {
	want, _ := filepath.EvalSymlinks(dir)
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" {
			continue
		}
		if got, err := filepath.EvalSymlinks(d); err == nil && got == want {
			return true
		}
	}
	return false
}

// latestFromServer asks the platform once (the public catalog route) and reads
// the X-Sboot-CLI-Latest header off the answer.
func latestFromServer() (string, error) {
	req, err := http.NewRequest("GET", apiURL()+"/api/v1/courses", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(hdrCLIVersion, version)
	resp, err := send(&http.Client{Timeout: 15 * time.Second}, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	return strings.TrimSpace(resp.Header.Get(hdrCLILatest)), nil
}

// versionOrder is -1, 0 or 1 as a is below, equal to or above b; ok is false
// when either does not parse as X.Y.Z.
func versionOrder(a, b string) (order int, ok bool) {
	x, okA := parseVersion(a)
	y, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// runUpgrade exits 0 (upgraded, already latest, or newer than latest) or 2 (no
// upgrade happened, for any reason). Never 1: exit codes are append-only
// (cli-releases.md §0), and 1 means a graded run that came up short.
func runUpgrade(force bool) int {
	out := os.Stderr
	install := updateCommandFor(upgradeGOOS)
	if version == "dev" || version == "" {
		fmt.Fprintln(out, "sboot: this is a source build — update it with `git pull` and `go build`.")
		return 2
	}
	if os.Getenv("SBOOT_OFFLINE") != "" {
		fmt.Fprintln(out, "sboot: SBOOT_OFFLINE is set, so this cannot ask which version is latest. unset it, or reinstall by hand:")
		fmt.Fprintf(out, "  %s\n", install)
		return 2
	}
	latest, err := latestFromServer()
	if err != nil {
		fmt.Fprintf(out, "sboot: could not reach the platform (%v) — nothing changed.\n", err)
		return 2
	}
	if latest == "" {
		fmt.Fprintln(out, "sboot: the platform did not say which version is latest — nothing changed. to reinstall by hand:")
		fmt.Fprintf(out, "  %s\n", install)
		return 2
	}
	if sameVersion(version, latest) {
		fmt.Fprintf(out, "sboot %s is already the latest — nothing to do.\n", normVersion(version))
		return 0
	}
	// "And if I am newer?" (TL review, 2026-10-07). A binary ahead of `latest` is
	// the normal state of a soak ring (a tag cut before `latest` moves) and of a
	// founder's machine, and `latest` points BACKWARDS on purpose after a bad
	// release (cli-releases.md §7). Going down is a choice the learner makes with
	// --force, never a side effect of "upgrade"; versions that cannot be ordered
	// are not guessed at either.
	if !force {
		switch order, ok := versionOrder(latest, version); {
		case !ok:
			fmt.Fprintf(out, "sboot: cannot tell whether %s is newer than %s — nothing changed.\n", normVersion(latest), normVersion(version))
			fmt.Fprintf(out, "sboot: to install %s anyway:  sboot upgrade --force\n", normVersion(latest))
			return 2
		case order < 0:
			fmt.Fprintf(out, "sboot %s is newer than the latest release (%s) — nothing changed.\n", normVersion(version), normVersion(latest))
			fmt.Fprintf(out, "sboot: to go back to %s on purpose (after a bad release, say):  sboot upgrade --force\n", normVersion(latest))
			answeredTheNudge()
			return 0
		}
	}

	exe, err := updateExe()
	if err == nil {
		if real, e := filepath.EvalSymlinks(exe); e == nil {
			exe = real
		}
	}
	if err != nil {
		fmt.Fprintf(out, "sboot: cannot tell where this sboot is installed (%v). to update by hand:\n  %s\n", err, install)
		return 2
	}
	if owner, instead := packageManagerFor(exe); owner != "" {
		fmt.Fprintf(out, "sboot: this sboot (%s) was installed by %s, and `sboot upgrade` does not fight it.\n", exe, owner)
		fmt.Fprintf(out, "sboot: instead:  %s\n", instead)
		return 2
	}
	if upgradeGOOS == "windows" {
		// A running sboot.exe cannot be replaced by the process that is running
		// it, and starting PowerShell as a child is the wrapper Defender flagged
		// (G387) — so on Windows this is the one line to paste, not a child run.
		fmt.Fprintf(out, "sboot %s → %s: on Windows a running sboot.exe cannot replace itself. paste this into PowerShell:\n", normVersion(version), normVersion(latest))
		fmt.Fprintf(out, "  %s\n", install)
		return 2
	}
	dir := filepath.Dir(exe)
	if !dirWritable(dir) {
		fmt.Fprintf(out, "sboot: %s is not writable by you, so `sboot upgrade` cannot replace %s.\n", dir, filepath.Base(exe))
		fmt.Fprintf(out, "sboot: instead, install a copy you own (it goes in ~/.local/bin):  %s\n", install)
		return 2
	}

	env := append(os.Environ(), "SBOOT_INSTALL_DIR="+dir, "SBOOT_VERSION="+normVersion(latest))
	if dirOnPath(dir) {
		env = append(env, "SBOOT_NO_MODIFY_PATH=1")
	}
	fmt.Fprintf(out, "upgrading sboot %s → %s in %s\n", normVersion(version), normVersion(latest), dir)

	// Download, THEN run — never `curl … | sh` here. A pipe's status is its last
	// command's, so a curl that fails hands sh an empty script, sh exits 0, and
	// only the version check after it is left to notice (TL review, 2026-10-07).
	// Two steps make a failed download its own, named failure.
	script, err := os.CreateTemp("", "sboot-install-*.sh")
	if err != nil {
		fmt.Fprintf(out, "sboot: cannot make a temporary file (%v) — sboot %s is unchanged. to update by hand:\n  %s\n", err, normVersion(version), install)
		return 2
	}
	script.Close()
	defer os.Remove(script.Name())
	fmt.Fprintf(out, "── curl -fsSL %s\n", installShURL)
	dl := exec.Command("curl", "-fsSL", installShURL, "-o", script.Name())
	dl.Stdout, dl.Stderr = out, out
	if err := dl.Run(); err != nil {
		fmt.Fprintf(out, "sboot: could not download the installer (%v) — sboot %s is unchanged.\n", err, normVersion(version))
		return 2
	}
	if st, err := os.Stat(script.Name()); err != nil || st.Size() == 0 {
		fmt.Fprintf(out, "sboot: the installer downloaded empty — sboot %s is unchanged.\n", normVersion(version))
		return 2
	}
	fmt.Fprintln(out, "── sh install.sh")
	cmd := exec.Command("sh", script.Name())
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "sboot: the installer did not finish (see above) — sboot %s is unchanged.\n", normVersion(version))
		return 2
	}
	// The installer exiting 0 is not the proof: the binary in this folder is.
	after := "unknown"
	if b, err := exec.Command(exe, "version").Output(); err == nil {
		after = normVersion(strings.TrimPrefix(strings.TrimSpace(string(b)), "sboot "))
	}
	fmt.Fprintf(out, "sboot %s → %s\n", normVersion(version), after)
	if !sameVersion(after, latest) {
		fmt.Fprintf(out, "sboot: expected %s — run `sboot version`, and if it is still old: %s\n", normVersion(latest), install)
		return 2
	}
	answeredTheNudge()
	return 0
}

// answeredTheNudge drops the version half of what this run's own request heard
// (X-Sboot-CLI-Latest and its deprecation) on the two exit-0 paths that have just
// answered it. After an upgrade `version` is still the OLD build's string, so
// exitWith's notice printed "sboot OLD is deprecated · update to NEW: curl …"
// right under "sboot OLD → NEW" (and burned the 24h nudge slot); a binary newer
// than latest has just been told so in its own words. The free-text notice is
// kept — an incident still reaches this run.
func answeredTheNudge() {
	channel.latest, channel.min, channel.deprecatedBelow = "", "", ""
}
