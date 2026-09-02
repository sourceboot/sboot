// The display-side answer to the staged tree name (rust-for-beginners dogfood F00-1,
// 2026-08-24; ledger G54).
//
// Inside the grader's staging root the learner's tree is ALWAYS spelled `os`
// (cache.go linkOSTree, repo.go stagedTree) — that is what lets one authored
// build command and one rubric `[[run]]` line serve every course. The learner's
// own repo spells it whatever the course says (`game`, `db`), and every line of
// prose follows the course. So any staging-spelled line that reaches the
// TERMINAL — the engine's "── running `cargo build --manifest-path
// os/Cargo.toml`" banner, cargo's "Compiling lantern v0.1.0 (<run>/os/lantern)",
// libtest's "Running unittests … (os/target/…)" — names a directory the
// learner's workspace does not contain, on exactly the audience least equipped
// to tell the tool's inconsistency from their own mistake.
//
// This file rewrites that spelling at the DISPLAY boundary and nowhere else:
//
//   - the verdict protocol, the capture blob, the uploaded practice detail and
//     state.json's evidence all keep the raw bytes — graders, rubric matchers
//     and authored evidence selectors are written against what actually ran;
//   - a course whose tree IS `os` never even constructs the filter
//     (retreeStreams returns the raw streams), so every OS course stays
//     byte-identical by construction, the same guarantee repoBuildCmd gives;
//   - the rewrite is boundary-guarded, never a substring replace: a word that
//     merely contains "os" ("macos/", "osmium") is left alone, and `/os/` in
//     the middle of a path the staging rules did not claim is left alone too.
package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// retreeText rewrites one chunk of display text from the staging root's
// spelling into the learner's. Three rules, applied in order:
//
//  1. `<runDir>/os/…` → `<tree>/…`, and a bare `<runDir>/os` → `<tree>` — the
//     absolute staging spelling of their tree (cargo's "Compiling … (…)" line).
//  2. any other `<runDir>/…` → `…` — staging scratch the learner has never
//     seen (`labs/<stage>/checks/`, `build/`), the same shortening the
//     engine's relativizePaths applies to quoted failure blocks.
//  3. a relative `os/` (or `os\` on Windows) opening a path token →
//     `<tree>/` — the engine's echoed commands and libtest's
//     "(os/target/debug/…)".
//
// Purely cosmetic by design: it only ever renames or shortens, so a shape it
// does not recognise passes through untouched.
func retreeText(text, runDir, tree string) string {
	tree = treeOr(tree)
	if tree == stagedTree || runDir == "" {
		return text
	}
	sep := string(filepath.Separator)
	prefixes := []string{runDir}
	// macOS hands out symlinked temp roots (/var → /private/var); cargo may
	// print either spelling, so both prefixes are claimed when they differ.
	if resolved, err := filepath.EvalSymlinks(runDir); err == nil && resolved != runDir {
		prefixes = append(prefixes, resolved)
	}
	for _, p := range prefixes {
		staged := p + sep + stagedTree
		text = strings.ReplaceAll(text, staged+sep, tree+sep)
		text = replaceBounded(text, staged, tree)
		text = strings.ReplaceAll(text, p+sep, "")
	}
	return retreeRelTokens(text, tree)
}

// replaceBounded replaces occurrences of old that are NOT followed by a word
// byte, so `<run>/os` is claimed while `<run>/os-old` (a sibling that merely
// starts the same way) is not.
func replaceBounded(text, old, new string) string {
	if !strings.Contains(text, old) {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, old)
		if i < 0 {
			break
		}
		rest := text[i+len(old):]
		if rest != "" && isPathWordByte(rest[0]) {
			b.WriteString(text[:i+len(old)])
		} else {
			b.WriteString(text[:i])
			b.WriteString(new)
		}
		text = rest
	}
	b.WriteString(text)
	return b.String()
}

// retreeRelTokens rewrites `os/` (or `os\`) where it OPENS a path token — start
// of text, or after a byte that cannot be part of a path word. `macos/` keeps
// its word, and `/some/os/` mid-path keeps its segment: the absolute rules
// above already claimed every staging path, so a surviving mid-path `/os/`
// belongs to something that is not the staging root.
func retreeRelTokens(text, tree string) string {
	if !strings.Contains(text, stagedTree+"/") && !strings.Contains(text, stagedTree+`\`) {
		return text
	}
	var b strings.Builder
	prev := byte(0)
	for i := 0; i < len(text); {
		if (strings.HasPrefix(text[i:], stagedTree+"/") || strings.HasPrefix(text[i:], stagedTree+`\`)) &&
			(i == 0 || !isPathWordByte(prev) && prev != '/' && prev != '\\') {
			b.WriteString(tree)
			sep := text[i+len(stagedTree)]
			b.WriteByte(sep)
			prev = sep
			i += len(stagedTree) + 1
			continue
		}
		b.WriteByte(text[i])
		prev = text[i]
		i++
	}
	return b.String()
}

func isPathWordByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' ||
		('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// maxHeldPartialLine bounds how much of an unterminated line a retreeWriter
// holds before passing it through unfiltered — the stream must stay LIVE, and a
// producer that writes kilobytes with no newline is not printing a path banner.
const maxHeldPartialLine = 8 << 10

// retreeWriter is a line-oriented pass-through that applies retreeText to each
// complete line as it arrives. Line-oriented because a path could otherwise be
// torn across two Write calls and escape the rewrite; live because every
// complete line is forwarded inside the Write that completed it, and Flush
// (called when the producer exits) forwards whatever trailing partial remains.
type retreeWriter struct {
	w      io.Writer
	runDir string
	tree   string
	buf    []byte
}

func newRetreeWriter(w io.Writer, runDir, tree string) *retreeWriter {
	return &retreeWriter{w: w, runDir: runDir, tree: tree}
}

func (rw *retreeWriter) Write(p []byte) (int, error) {
	rw.buf = append(rw.buf, p...)
	for {
		i := bytes.IndexByte(rw.buf, '\n')
		if i < 0 {
			break
		}
		if err := rw.emit(rw.buf[:i+1]); err != nil {
			return len(p), err
		}
		rw.buf = rw.buf[i+1:]
	}
	if len(rw.buf) > maxHeldPartialLine {
		if err := rw.emit(rw.buf); err != nil {
			return len(p), err
		}
		rw.buf = rw.buf[:0]
	}
	return len(p), nil
}

func (rw *retreeWriter) Flush() {
	if len(rw.buf) > 0 {
		_ = rw.emit(rw.buf)
		rw.buf = nil
	}
}

func (rw *retreeWriter) emit(b []byte) error {
	_, err := io.WriteString(rw.w, retreeText(string(b), rw.runDir, rw.tree))
	return err
}

// retreeStreams is what runGrader wires the judge subprocess's stdout/stderr
// through. For a course whose tree is `os` — every OS course — it returns the
// raw streams and a no-op flush: not a transparent filter but NO filter, which
// is what makes "os-tree courses are byte-identical" true by construction.
// The judge's streams are already pipes (its stdout is teed into the practice
// record), so wrapping them costs nothing a course could notice: no child of
// the judge ever saw a TTY on them anyway.
func retreeStreams(runDir, tree string) (stdout, stderr io.Writer, flush func()) {
	if treeOr(tree) == stagedTree {
		return os.Stdout, os.Stderr, func() {}
	}
	o := newRetreeWriter(os.Stdout, runDir, tree)
	e := newRetreeWriter(os.Stderr, runDir, tree)
	return o, e, func() { o.Flush(); e.Flush() }
}
