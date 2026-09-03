//go:build windows

// The Windows console speaks whatever code page it was started with, and this
// binary speaks UTF-8 (rust-for-beginners review round 2026-09-02, D-WIN-3; ledger
// G144).
//
// Measured on Windows 11 with raw-byte capture, so the mangling is not inferred:
//
//	U+00B7  ·   utf-8 C2 B7      rendered by cp437 as  ┬╖
//	U+25B8  ▸   utf-8 E2 96 B8   rendered by cp437 as  Γû╕
//
// `sboot courses`, the bare dashboard and the login screen are the first three
// screens a Windows learner ever sees, and all three carry those two runes. The
// grading output (`[PASS]`, `score: 6/6`) and the game itself are pure ASCII, so
// nothing a learner is graded on was affected — this is the chrome on the welcome
// mat, which is exactly the worst place to look broken.
//
// SetConsoleOutputCP(CP_UTF8) is the one-call fix. No cgo (the CLI cross-compiles
// CGO-free to five targets, harness/go.mod has no requires at all), no new
// dependency — `syscall.NewLazyDLL` resolves kernel32 lazily, so a Windows build
// that never reaches a console pays nothing.
//
// It is deliberately NOT restored on exit. The code page is per console and
// modern Windows Terminal/PowerShell handle 65001 fine; a restore would have to
// run on every exit path including a panic, and getting THAT wrong leaves someone
// else's console mis-set. The failure mode of not restoring is a console that
// keeps rendering UTF-8 correctly.
package main

import "syscall"

// CP_UTF8, from the Windows SDK.
const cpUTF8 = 65001

func enableUTF8Console() {
	// Best-effort by construction: every failure path here (no kernel32, no
	// console, a redirected stdout, an old Windows) leaves the console exactly as
	// it was, which is the behaviour that shipped before this existed. A CLI must
	// never fail to grade because a code page could not be set.
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setCP := kernel32.NewProc("SetConsoleOutputCP")
	if err := setCP.Find(); err != nil {
		return
	}
	_, _, _ = setCP.Call(uintptr(cpUTF8))
}
