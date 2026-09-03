//go:build !windows

// Every other platform's terminal already reads UTF-8 (see console_windows.go for
// the one that does not, and why). A no-op with the same name keeps the call site
// in main() unconditional — a `if runtime.GOOS == "windows"` there would compile
// the Windows syscall into every binary and read as a runtime choice, when it is a
// platform fact.
package main

func enableUTF8Console() {}
