// The submit archive's wire format: what `sboot submit` uploads has to be a tree the
// runner's extractor can put back together, on every OS release.yml ships a binary for.
//
// Why these live here and not only in the client matrix: L11's assertions in
// clientmatrix/matrix_test.go can only fail where the host separator is not "/", so they
// SKIP on Linux and macOS — which is every per-PR job we run. tarGzDirSep takes the
// separator as an argument precisely so the rule can be exercised anywhere, by simulating
// a Windows walk on a POSIX host: a file whose NAME contains a backslash is legal here,
// so `kernel\main.rs` is a faithful stand-in for what filepath.Rel returns on Windows.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tar entry names are "/"-separated by spec. runner/main.go's extractTarGz reads them
// that way, so `kernel\main.rs` is not a path to it — it is one file with a backslash in
// its name, and the learner's tree arrives flattened. This is L11, which a real
// windows-latest runner confirmed on 2026-07-27.
func TestTarEntryNamesAreAlwaysSlashSeparated(t *testing.T) {
	dir := t.TempDir()
	// The names a Windows WalkDir would produce, spelled out. On this host they are
	// literal filenames; on Windows the same strings are genuine nested paths — either
	// way the walk hands tarGzDirSep a rel with the separator in it.
	write(t, filepath.Join(dir, `kernel\main.rs`), "fn main() {}\n")
	write(t, filepath.Join(dir, `src\arch\x86.rs`), "// arch\n")
	write(t, filepath.Join(dir, "Cargo.toml"), "[package]\n")

	gzData, err := tarGzDirSep(dir, '\\')
	if err != nil {
		t.Fatal(err)
	}
	files, dirs := tarNames(t, gzData)

	for _, name := range append(append([]string{}, files...), dirs...) {
		if strings.Contains(name, `\`) {
			t.Errorf("tar entry %q carries the host separator; the runner unpacks that as ONE "+
				"flat file with a backslash in its name, not as a path", name)
		}
	}
	for _, want := range []string{"kernel/main.rs", "src/arch/x86.rs", "Cargo.toml"} {
		if !contains(files, want) {
			t.Errorf("no %q entry in the archive; got files %v, dirs %v", want, files, dirs)
		}
	}
}

// The shape the extractor is owed, from the real entry point: nested names, dirs marked
// with a trailing slash, build output and git history left out. Unlike the test above
// this one cannot fail on a "/" host for separator reasons — it pins everything else
// about the archive, so a future rewrite of the walk cannot quietly change the wire
// format while the separator rule still holds.
func TestTarGzDirPackagesSourceOnly(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "kernel", "main.rs"), "fn main() {}\n")
	write(t, filepath.Join(dir, "Cargo.toml"), "[package]\n")
	write(t, filepath.Join(dir, "target", "debug", "kernel.bin"), "\x7fELF\n")
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	gzData, err := tarGzDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, dirs := tarNames(t, gzData)

	if !contains(files, "kernel/main.rs") || !contains(files, "Cargo.toml") {
		t.Errorf("the learner's source is not in the archive: files %v", files)
	}
	if !contains(dirs, "kernel/") {
		t.Errorf("a directory entry must end in \"/\": dirs %v", dirs)
	}
	for _, name := range append(append([]string{}, files...), dirs...) {
		if strings.HasPrefix(name, "target/") || strings.HasPrefix(name, ".git/") ||
			name == "target/" || name == ".git/" {
			t.Errorf("entry %q: build output and git history must never be uploaded", name)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

// tarNames returns the regular-file and directory entry names of a .tar.gz, exactly as
// written — no cleaning, since the raw header name is the thing under test.
func tarNames(t *testing.T, gzData []byte) (files, dirs []string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, dirs
		}
		if err != nil {
			t.Fatalf("read the archive: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			dirs = append(dirs, hdr.Name)
		case tar.TypeReg:
			files = append(files, hdr.Name)
		}
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// ── helpers ─────────────────────────────────────────────────────────────────────
//
// These live here rather than in cache_test.go so this file is SELF-CONTAINED: it is
// one of the two tests published to the public CLI repo (scripts/publish-cli.sh), and
// a published test that cannot compile is worse than one that was never shipped.

func write(t *testing.T, path, body string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
