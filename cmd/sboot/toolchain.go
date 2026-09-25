// The build that never started — reading a MISSING LINKER out of a build that ran
// and failed (rust-for-beginners review round, 2026-09-02; ledger G141).
//
// ── THE FAILURE THIS ANSWERS ───────────────────────────────────────────────────
// Three fresh machines, three operating systems, one lab: the very first
// `sboot test` of lab 00 died before a check was scored, because rustup installs
// Rust and Rust does not ship a linker. What each learner saw:
//
//	linux    error: linker `cc` not found
//	windows  error: linker `link.exe` not found
//	         note: the msvc targets depend on the msvc linker but `link.exe` was not found
//	macos    warning: failed running `"xcrun" … ` to find MacOSX.sdk
//	         = note: xcode-select: note: No developer tools were found, requesting install.
//	         error: linking with `cc` failed: exit status: 1
//
// The CLI already knew how to answer a toolchain problem — `missingTool` classifies
// a build command that could not be LAUNCHED, and renderBlockedHint's `toolchain`
// branch says "this is your machine, not your code" and prints the install line.
// None of it fired, because cargo launched perfectly well; the missing linker was
// inside the build's own output, where nothing looked. So this file is the missing
// CLASSIFIER, not a second printer.
//
// ── WHY THE SIGNATURES ARE KEYED ON runtime.GOOS ───────────────────────────────
// Two of the three are unambiguous on any OS, but macOS's is not: `linking with
// \`cc\` failed` is also what a genuine link error in the learner's own code looks
// like. On darwin the deciding evidence is xcode-select/xcrun saying the developer
// tools are absent, so the darwin rules ask for that and never for the generic
// line. Keying on GOOS is what lets each platform's rules be as tight as its
// evidence allows.
package main

import (
	"os/exec"
	"runtime"
	"strings"
)

// hostOS is the operating system every OS-keyed remedy in this binary reads —
// the linker classifier, pkgInstall, the rustup line, the update nudge, the
// new-terminal note. ONE variable rather than `runtime.GOOS` at each site, for a
// testing reason with teeth: a Mac cannot run the Windows branch, so a pin that
// only called the Windows helper directly would stay green after the CALL SITE
// was reverted to a Unix string. Tests set this and drive the shipped paths
// (onboarding_test.go withHostOS); nothing else ever writes it. The pre-existing
// runtime.GOOS reads that decide FACTS about this machine (state.go's config
// dir, the User-Agent tuple) deliberately do not go through it.
var hostOS = runtime.GOOS

// linkerSignatures are the substrings (matched case-insensitively) that mean "this
// machine has no linker", per OS.
//
// Provenance, stated exactly because a signature nobody has seen is one nobody can
// trust to fire: the entries marked MEASURED are lines an actual fresh machine
// printed in the 2026-09-02 laps (the linux/windows/macos.md reports and their
// evidence/ transcripts). The rest are the SAME toolchain's other spellings of the
// same state — rustc's message for a differently-named default linker, xcrun's two
// errors when the developer directory is set but broken — kept because none of
// them can be produced by a learner's own code, so a false match is impossible and
// the cost of missing one is the generic build-failure answer.
func linkerSignatures(goos string) []string {
	switch goos {
	case "darwin":
		// `xcrun: error: unable to find utility "<tool>"` is NOT here (dropped
		// 2026-09-23, skeptic): it is also what a machine WITH the Command Line
		// Tools prints for a full-Xcode-only tool, where `xcode-select --install`
		// does nothing — the one darwin spelling that could fire on a machine
		// the remedy cannot fix. A Mac with no tools at all prints the two
		// MEASURED lines above first.
		return []string{
			"no developer tools were found",                                 // MEASURED (rustc's `= note:` line)
			"xcode-select: error: unable to get active developer directory", // MEASURED (xcode-select -p)
			"xcrun: error: invalid active developer path",
		}
	case "windows":
		return []string{
			"linker `link.exe` not found",                         // MEASURED
			"link.exe` was not found",                             // MEASURED (rustc's follow-up note)
			"the msvc targets depend on the msvc linker",          // MEASURED (same note)
			"installing msvc toolchain without its prerequisites", // MEASURED (rustup-init's own warning)
		}
	default:
		return []string{
			"linker `cc` not found", // MEASURED
			"linker `gcc` not found",
			"linker `ld` not found",
		}
	}
}

// missingLinker reports whether a build's own output says the machine has no C
// linker. Conservative by construction: no match means the generic build-failure
// answer, which is the behaviour that shipped before this existed.
func missingLinker(goos, buildOut string) bool {
	if buildOut == "" {
		return false
	}
	hay := strings.ToLower(buildOut)
	for _, sig := range linkerSignatures(goos) {
		if strings.Contains(hay, strings.ToLower(sig)) {
			return true
		}
	}
	return false
}

// missingDevTools reports whether a command's OWN output is macOS saying the
// Command Line Tools are not installed.
//
// SAME SIGNATURES AS THE BUILD CASE, deliberately (2026-09-03 macOS lap): on a
// Mac with no developer tools, `git`, `cc`, `clang` and `make` are all the same
// install-on-demand shim, so the note that means "no linker" inside a cargo
// build is the note that means "no git" inside `sboot start` — one package, one
// remedy, one signature list to keep current.
//
// Non-darwin is always false, and that is the rule rather than an omission: a
// missing git on Linux or Windows is orthogonal to the compiler, so nothing
// there may be answered with a toolchain install.
func missingDevTools(goos, out string) bool {
	return goos == "darwin" && missingLinker(goos, out)
}

// devToolsAbsent answers the same question BEFORE a doomed command is run, which
// on macOS is the only way to know: `exec.LookPath("git")` succeeds against the
// shim, so the PATH proves nothing. `xcode-select -p` exits non-zero with
// "Unable to get active developer directory" when the Tools are absent
// (MEASURED — the 2026-09-02 lap's baseline, exit 2) and prints the developer
// directory when they are there.
//
// FAILS OPEN in the one case it cannot read: /usr/bin/xcode-select is part of
// macOS, so its absence from PATH is not evidence about the Tools — we say
// nothing and let missingDevTools classify what the command itself prints.
func devToolsAbsent(goos string) bool {
	if goos != "darwin" {
		return false
	}
	if _, err := exec.LookPath("xcode-select"); err != nil {
		return false
	}
	return exec.Command("xcode-select", "-p").Run() != nil
}

// linkerHint is what to say and what to type, per OS. Two lines of explanation
// (why a Rust install is not enough) and then the command — the shape
// `pkgInstall("gh")` already has, and the sizes are the ones the laps measured on
// real machines, because "one more download" and "7 GB" are different decisions
// for someone on a phone tether.
//
// THE NUMBERS ARE THE PAGE'S (G333, sboot-v0.15.0): every `N MB|GB` here must appear
// in the shared welcome's linker-<os> block (content/shared/welcome.md, re-measured
// 2026-09-16 — wave 3 decision 2), because a learner who reads lab 00 and then hits
// this hint is otherwise told two sizes for one download. verify-content §15
// (scripts/lib/welcome-guards.mjs checkCliSizes) reads these three arms and fails the
// build on a token the page does not carry, so a re-measurement lands on the page
// first and here second, never here alone.
func linkerHint(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"Rust compiles your code and then hands it to your Mac's own linker to finish —",
			"and macOS ships without one until you install Apple's Command Line Tools:",
			"  " + cltInstallLine,
		}
	case "windows":
		return []string{
			"Rust compiles your code and then hands it to Windows' own linker to finish, and",
			"that linker is Microsoft's, not Rust's. Two ways to get one — both fully graded:",
			"  1. rustup's option 1, the Visual Studio Community installer, which brings the C++",
			"     build tools (roughly 7 GB and 15–20 minutes, once — plan on about 10 GB free):",
			"     https://visualstudio.microsoft.com/visual-cpp-build-tools/  (\"Desktop development with C++\")",
			"  2. or switch Rust to its own GNU toolchain (about 350 MB to download, 1.5 GB on disk):",
			// The form the Windows lap MEASURED to work once rustup is already
			// installed (windows.md D-WIN-1): `rustup-init.exe --default-host …` is a
			// no-op by then, and `rustup default <toolchain>` loses to the
			// workspace's rust-toolchain.toml, which pins only the channel and lets
			// rustup fill the host half in from `default-host` — so that setting is
			// the lever, and it is sticky. The fully-qualified toolchain name makes
			// the install correct in either order.
			"     rustup toolchain install stable-x86_64-pc-windows-gnu",
			"     rustup set default-host x86_64-pc-windows-gnu",
		}
	default:
		return []string{
			"Rust compiles your code and then hands it to your system's linker to finish —",
			"and your distribution does not install one by default:",
			"  " + linuxInstallLine("build-essential") + "     # about 70 MB to download, 230 MB on disk, once",
		}
	}
}

// cltInstallLine is the one Command Line Tools remedy, printed by the linker hint
// after a failed build and by `sboot start` before a doomed `git init`
// (concierge.go localRepoStep) — ONE string so the two cannot quote two sizes.
const cltInstallLine = "xcode-select --install     # about 1 GB on disk, once per machine; click through the dialog"

// tokenSetLine is how to put a pasted token into the environment on THIS shell.
// PowerShell has no `export` (D-WIN-4, 2026-09-13): a Windows learner who typed the
// Unix line got "The term 'export' is not recognized" on the signed-out screen,
// the first screen `sboot start` shows before a login. `token` is the literal to
// print — a placeholder like `<the token>`, or the real one when the flow already
// holds it and only failed to store it.
func tokenSetLine(token string) string {
	if hostOS == "windows" {
		return "$env:SBOOT_TOKEN = \"" + token + "\""
	}
	return "export SBOOT_TOKEN=" + token
}

// tokenAdvice is the one-line "how to set it" that prose sites append ("or set
// SBOOT_TOKEN" was a verb PowerShell reads as Set-Variable).
func tokenAdvice() string {
	return "`" + tokenSetLine("<the token>") + "`"
}

// tokenUnsetLine is the other half: how to take a pasted token back out of THIS
// shell, so `sboot login` can pair. "unset" is prose on PowerShell.
func tokenUnsetLine() string {
	if hostOS == "windows" {
		return "Remove-Item Env:SBOOT_TOKEN"
	}
	return "unset SBOOT_TOKEN"
}

// ── which package manager, on Linux ────────────────────────────────────────────
//
// pkgInstall used to answer `sudo apt install <pkg>` for every non-macOS machine,
// on the honest reasoning that a Fedora learner still reads the package's NAME.
// That reasoning breaks twice: the name itself differs (Debian's `build-essential`
// is Fedora's `gcc` and Arch's `base-devel`), and on Windows nothing in the line
// exists at all. So the manager is detected — by the tool that is actually on the
// PATH, never by parsing /etc/os-release, because what matters is what the learner
// can run, and a container or a WSL distro can disagree with its own os-release.
type linuxPkgManager struct {
	install string   // the command prefix, e.g. "sudo apt install"
	names   []string // per-package overrides, "generic=native" pairs
}

var linuxManagers = []struct {
	probe string
	mgr   linuxPkgManager
}{
	// `apt-get update` FIRST (D-LINUX-2, 2026-09-13): a fresh Ubuntu cloud image
	// has an empty package list, and `apt-get install` on it exits 100 with
	// "Unable to locate package" — the learner's very first command, failing on a
	// machine that has nothing wrong with it. The update is cheap and idempotent.
	{"apt-get", linuxPkgManager{install: "sudo apt-get update && sudo apt-get install -y"}},
	{"dnf", linuxPkgManager{install: "sudo dnf install", names: []string{
		"build-essential=gcc", "qemu-system-x86=qemu-system-x86", "gh=gh"}}},
	{"pacman", linuxPkgManager{install: "sudo pacman -S", names: []string{
		"build-essential=base-devel", "qemu-system-x86=qemu-system-x86", "gh=github-cli"}}},
	{"zypper", linuxPkgManager{install: "sudo zypper install", names: []string{
		"build-essential=gcc", "qemu-system-x86=qemu"}}},
	{"apk", linuxPkgManager{install: "sudo apk add", names: []string{
		"build-essential=build-base", "gh=github-cli"}}},
}

// lookPath is exec.LookPath, indirected so a test can pretend to be on Fedora
// without one being present.
var lookPath = exec.LookPath

// linuxInstallLine renders the install command for one package on THIS Linux box.
// Falls back to apt when nothing is detected: it is the overwhelmingly common case
// and the line still names the package.
func linuxInstallLine(pkg string) string {
	for _, m := range linuxManagers {
		if _, err := lookPath(m.probe); err != nil {
			continue
		}
		return m.mgr.install + " " + m.mgr.name(pkg)
	}
	return "sudo apt-get update && sudo apt-get install -y " + pkg
}

func (m linuxPkgManager) name(pkg string) string {
	for _, pair := range m.names {
		if g, native, ok := strings.Cut(pair, "="); ok && g == pkg {
			return native
		}
	}
	return pkg
}

// windowsInstallLine renders the winget line for one package. winget ships with
// Windows 11 and with current Windows 10, which is the whole installed base this
// course can reach — and `--id … -e` is the exact-id form, so a learner never gets
// the interactive "multiple packages matched" prompt.
func windowsInstallLine(pkg string) string {
	ids := map[string]string{
		"git":  "Git.Git",
		"gh":   "GitHub.cli",
		"nasm": "NASM.NASM",
	}
	if id, ok := ids[pkg]; ok {
		return "winget install --id " + id + " -e"
	}
	return "winget install " + pkg
}

// newTerminalNote is the sentence Windows needs more than any other platform and
// is true everywhere: a shell that was already open when a tool was installed does
// not have it on its PATH. On Windows it is the COMMON cause of "cargo is not
// installed" — rustup edits the user PATH, and every terminal older than the
// install keeps the old one. The unix half names `source` because that is the line
// rustup itself prints there.
func newTerminalNote() string {
	if hostOS == "windows" {
		return "already installed it? open a NEW terminal — this one's PATH is older than the install."
	}
	return "already installed it? open a new terminal (or `source \"$HOME/.cargo/env\"`) — " +
		"this shell's PATH is older than the install."
}
