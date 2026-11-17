package main

import (
	"fmt"
	"io"
)

// ── a course whose Windows path is WSL2 (D-WINDOWS-2, sprint 8 #112) ────────────
//
// The boot/kernel family runs inside WSL2 on Windows (the 2026-09-27 split), and
// the native Windows CLI used to accept such a course anyway: `sboot start
// kernel-in-rust` in PowerShell made a workspace, and the first `sboot test` then
// sent the learner after rustup-init.exe and the MSVC linker — a road the course
// does not support and that ends at QEMU and a GNU toolchain Windows does not have
// (kernel-in-rust W1 review, 2026-10-25). The ruling (the PM's pick Q2 (i),
// 2026-10-25; stop, not warn, sprint 8): the course's manifest says `windows:
// wsl2`, and on windows/* `start`, and every command that builds (`test`,
// `submit`, `hint`, `debug`, through prepare), stop with the WSL2 line before any
// workspace, download or toolchain work.
//
// THE DEFAULT REFUSES NOTHING. No key, an unknown value, or no answer at all reads
// as native — a refusal needs the platform's word (this run's, or one it cached).
// Off Windows the key changes nothing: inside WSL2 this binary is a Linux one.

// wsl2Line is the ONE sentence that says how such a course runs on Windows. Its
// words are the shared welcome's `install-wsl2` block (content/shared/welcome.md,
// sprint 8 #94), and G2284 holds reportWSL2's sentences to that block both ways
// round (scripts/lib/welcome-guards.mjs checkWSL2Words), so the lab 00 page and
// the refusal cannot say two things.
const wsl2Line = "On Windows this course runs inside WSL2: `wsl --install`, reboot, open Ubuntu, then follow the Linux steps; keep the workspace under `~`, never `/mnt/c`."

// windowsWSL2 is the only manifest value that stops a command (platform
// lib/content.ts WINDOWS_MODES).
const windowsWSL2 = "wsl2"

// courseWindowsMode is what this binary knows of `course`'s `windows` key, with no
// request of its own: this run's spec manifest when one was fetched (the
// platform's current word, "" included — it beats every cache), else the spec.json
// an earlier online run cached, else the catalog state.json cached. A cache
// written by sboot 0.22.0 or earlier never holds the field (that binary's structs
// drop it), so it reads as "" — native — until the next online run.
func courseWindowsMode(course string, st *guidanceState) string {
	if m, ok := fetchedManifest[course]; ok {
		return m.Windows
	}
	if m, ok := cachedManifest(course); ok && m.Windows != "" {
		return m.Windows
	}
	if st != nil && st.Catalog != nil {
		if w := catalogWindowsMode(st.Catalog.Courses, course); w != "" {
			return w
		}
	}
	return ""
}

func catalogWindowsMode(cs []catalogCourse, course string) string {
	for _, c := range cs {
		if c.ID == course {
			return c.Windows
		}
	}
	return ""
}

// wsl2Stops reports whether this command must stop here: a Windows host and a
// course that runs inside WSL2. hostOS, never runtime.GOOS, so the Windows answer
// is pinned from a Mac (wsl2_test.go).
func wsl2Stops(mode string) bool {
	return hostOS == "windows" && mode == windowsWSL2
}

// reportWSL2 prints the refusal. `nothing` says what was not done, so the learner
// knows their machine was left as it was.
func reportWSL2(w io.Writer, course, nothing string) {
	fmt.Fprintf(w, "sboot: %s runs inside WSL2 on Windows, and this is native Windows — %s.\n", course, nothing)
	fmt.Fprintf(w, "  %s\n", wsl2Line)
	fmt.Fprintln(w, "  Run `wsl --install` in PowerShell opened as an administrator.")
	fmt.Fprintln(w, "  `sboot login` prints its link inside Ubuntu: open it in your Windows browser.")
	fmt.Fprintf(w, "  the setup steps: %s/courses/%s/stages/00-welcome\n", siteURL(), course)
}
