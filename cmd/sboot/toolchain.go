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
		return []string{
			"no developer tools were found",                                 // MEASURED (rustc's `= note:` line)
			"xcode-select: error: unable to get active developer directory", // MEASURED (xcode-select -p)
			"xcrun: error: invalid active developer path",
			"xcrun: error: unable to find utility",
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

// linkerHint is what to say and what to type, per OS. Two lines of explanation
// (why a Rust install is not enough) and then the command — the shape
// `pkgInstall("gh")` already has, and the sizes are the ones the laps measured on
// real machines, because "one more download" and "5.5 GB" are different decisions
// for someone on a phone tether.
func linkerHint(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"Rust compiles your code and then hands it to your Mac's own linker to finish —",
			"and macOS ships without one until you install Apple's Command Line Tools:",
			"  xcode-select --install     # ~900 MB, once per machine; click through the dialog",
		}
	case "windows":
		return []string{
			"Rust compiles your code and then hands it to Windows' own linker to finish, and",
			"that linker is Microsoft's, not Rust's. Two ways to get one — both fully graded:",
			"  1. install the Visual C++ Build Tools (~5.5 GB, once) — the option rustup offers:",
			"     https://visualstudio.microsoft.com/visual-cpp-build-tools/  (\"Desktop development with C++\")",
			"  2. or switch Rust to its own GNU toolchain, a much smaller download (~350 MB):",
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
			"  " + linuxInstallLine("build-essential") + "     # ~100 MB, once per machine",
		}
	}
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
	{"apt-get", linuxPkgManager{install: "sudo apt-get install -y"}},
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
	return "sudo apt-get install -y " + pkg
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
