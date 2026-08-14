// The learner's REPO — the half of the old single workspace that is theirs.
//
// ── WHY THERE ARE TWO DIRECTORIES NOW (the workspace-split design, 2026-07-26) ─
//
// Until 2026-07-26 one directory held everything: the learner's `os/` tree AND our
// `labs/*/lab.toml` + `xtask/`. Under the learner-owned-repo model that directory
// becomes a git repo they push and show people, so publishing it republished our
// tests and grading engine too — possibly without realising.
//
// THE PRIMARY ARGUMENT IS PORTFOLIO QUALITY, NOT ANTI-LEAK. Nothing here is secret
// (D1 published every check) and `xtask` is destined to be open-sourced regardless.
// The reason to split is that a repo which is mostly our harness reads as "I did a
// tutorial", and a repo that is `os/` plus the learner's own commits reads as "I
// wrote a kernel." Minimising our footprint makes their portfolio genuinely better —
// which is the entire point of the learner-owned-repo model, so learner and business
// interests point the same way here. That is why this is worth doing at all.
//
// It is deliberately NOT done with `.gitignore`. That file is advisory: `git add -f`,
// zipping the folder, a different git client and any commit-everything editor all
// bypass it, and the concern is specifically *unknowing* publication. A separate path
// on disk is the only real guarantee. (We still write a `.gitignore`, for build
// artifacts — that is a different job.)
//
// So their repo is:
//
//	my-os-kernel/
//	├── os/{boot,kernel,shared}/   skeleton + their code
//	├── .cargo/config.toml         build settings
//	├── rust-toolchain.toml        pinned nightly, so plain `cargo build` works
//	├── sboot.toml                 which course this is — the ONLY marker we need
//	├── README.md                  ours; also a discovery surface
//	├── LICENSE                    MIT — this is theirs to publish
//	└── .gitignore
//
// and ours lives in the sboot data dir (see cache.go).
//
// ── WHY sboot.toml AND NOT ZERO FOOTPRINT ───────────────────────────────────────
// The tempting alternative was a mapping in ~/.config/sboot keyed by repo path, so
// their repo carried literally nothing of ours. It is more aligned with "the less of
// ours the better", and it was rejected because it breaks on move, rename, clone and
// second machine, and would need an `sboot link` to re-attach. One tiny explicit file
// beats fragile inference.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// manifestName is the marker file. Workspace detection used to look for `labs/` +
// `xtask/`; both of those moved out of the repo, so this is what identifies it now.
const manifestName = "sboot.toml"

// repo is a located learner repository.
type repo struct {
	dir    string // absolute path to the repo root
	course string // course id, from sboot.toml (or an env override)
}

// osDir is the tree the learner actually writes, and the only thing `sboot submit`
// uploads.
func (r repo) osDir() string { return filepath.Join(r.dir, "os") }

// findRepo locates the learner's repo by walking up from the cwd looking for
// sboot.toml, and resolves which course it is.
//
// Course precedence: SBOOT_COURSE (explicit override, used by tests and by anyone
// running two courses out of one tree) → sboot.toml → the default course.
func findRepo() (repo, error) {
	dir := os.Getenv("SBOOT_COURSE_DIR")
	if dir == "" {
		start, err := os.Getwd()
		if err != nil {
			return repo{}, err
		}
		found, err := walkUpForManifest(start)
		if err != nil {
			return repo{}, err
		}
		dir = found
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return repo{}, err
	}

	course := os.Getenv("SBOOT_COURSE")
	if course == "" {
		course = courseFromManifest(abs)
	}
	if course == "" {
		course = defaultCourse
	}
	return repo{dir: abs, course: course}, nil
}

func walkUpForManifest(start string) (string, error) {
	dir := start
	for {
		if st, err := os.Stat(filepath.Join(dir, manifestName)); err == nil && !st.IsDir() {
			return dir, nil
		}
		// A pre-split workspace has no sboot.toml but is recognisable, and telling
		// someone "not a workspace" while they are standing in one they used
		// yesterday is the worst version of this error. There are no released
		// `sboot-v*` binaries yet so nobody can really be in this state, but the
		// message costs four lines and removes a whole class of confused report.
		if isPreSplitWorkspace(dir) {
			return "", fmt.Errorf("%s looks like a pre-split workspace (it has labs/ + xtask/ in it).\n"+
				"  Those now live outside your repo — see `sboot where`.\n"+
				"  To adopt this directory: delete labs/ and xtask/, then create %s containing\n"+
				"    course = %q",
				dir, manifestName, defaultCourse)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found above the current directory.\n"+
				"  Run this inside your course repo, or create one with `sboot start`.\n"+
				"  (Override with SBOOT_COURSE_DIR.)", manifestName)
		}
		dir = parent
	}
}

func isPreSplitWorkspace(dir string) bool {
	for _, sub := range []string{"labs", "xtask"} {
		if st, err := os.Stat(filepath.Join(dir, sub)); err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// courseRe reads `course = "os-rust"` out of sboot.toml. Hand-rolled rather than a
// TOML dependency: the file is two lines that we write ourselves, and the harness
// has no third-party dependencies at all — that is worth more than generality here.
var courseRe = regexp.MustCompile(`(?m)^\s*course\s*=\s*"([^"]+)"`)

func courseFromManifest(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return ""
	}
	if m := courseRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// ── the files `sboot start` writes into their repo ──────────────────────────────
//
// These are generated here rather than shipped inside the scaffold tarball because
// each needs something only the client knows: which course this repo is, the course's
// catalog title, its first lab, and which site to link back to. Keeping them out of
// the tarball also means one scaffold serves every course.

// `title` and `firstStage` come from the course's spec manifest, so the README names
// the course the way the catalog does and points at a lab that actually exists.
func writeRepoFiles(dir, course, title, firstStage string) error {
	for _, f := range []struct {
		name string
		body string
	}{
		{manifestName, sbootToml(course)},
		{"LICENSE", mitLicense()},
		{".gitignore", gitignore()},
		{"README.md", readme(course, title, firstStage, buildHint(course))},
	} {
		p := filepath.Join(dir, f.name)
		// Never clobber: a learner re-running `sboot start` in a repo they have
		// edited (or one whose README they rewrote, which is exactly what a good
		// portfolio repo looks like) must not lose it.
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sbootToml(course string) string {
	return fmt.Sprintf(`# This file tells the `+"`sboot`"+` CLI which %s course this repo is for.
# Everything else — the tests and the grading engine — lives outside this repo:
# run `+"`sboot where`"+` to see exactly where.
course = %q
`, brandName, course)
}

// MIT, per the workspace-split design "Licensing, per artifact" (2026-07-26).
//
// Three artifacts take three licences, and that split is what dissolves the apparent
// conflict between "all rights reserved" and "publish your portfolio repo": what the
// learner publishes is not what we reserve. The scaffold in this repo is MIT — chosen
// over Apache-2.0 because it puts no NOTICE-file obligation on a learner who forks or
// publishes. The tests and grading engine, which live in the sboot data dir and never
// in this repo, stay all rights reserved.
func mitLicense() string {
	return `MIT License

Copyright (c) 2026 Puneet Kaushik

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`
}

// The .gitignore covers BUILD ARTIFACTS only. It is emphatically not the mechanism
// that keeps our tests and grader out of this repo — those are not on this path at
// all, which is a guarantee rather than a request (see the file header).
func gitignore() string {
	return `# Build artifacts. Cargo target dirs are large and machine-specific.
target/
build/

# Editor / OS noise
.DS_Store
*.swp
`
}

// The README is ours, and it is also a DISCOVERY SURFACE: this repo is meant to be
// public, so anyone who lands on it should be able to find the course (plan.md's
// public-template-repo item). It leads with what the learner built, because that is
// what a reader — or a hiring manager — came for.
// buildHint is the README's "Build it" section, DERIVED FROM THE COURSE'S OWN
// BUILD COMMAND — the resolved `grader.build` on the cached spec manifest, the
// same value `sboot test` executes.
//
// It used to sniff the scaffold's files: any repo with a rust-toolchain.toml got
// "cd os/kernel && cargo build --release … you need a nightly Rust toolchain".
// For os2-rust every word of that is false — stable Rust, built by `cargo xtask`
// as one workspace — and the README is the most visible file the learner has:
// the whole point of the learner-owned repo is that it reads like their work,
// not like a template nobody looked at. The build command cannot lie the way a
// file sniff can, because it is literally what grades the repo.
//
// The two known course shapes keep their existing text VERBATIM, keyed on the
// course id AND its exact resolved command (os-rust's default and os-c's
// `make -C os`). The command alone is not enough: os2-rust's course-level
// fallback is ALSO `cargo xtask build`, and its repo is stable Rust with the
// workspace at `os/`, so the os-rust text ("nightly", `cd os/kernel && cargo
// build --release`) would be false in every particular. Anything not matching
// a known pair gets an honest section built around its actual command. A blank
// command cannot happen (the default fills it), but an unrecognisable one
// still names itself rather than guessing at a toolchain lecture.
func buildHint(course string) string {
	build := strings.TrimSpace(courseGrader(course).Build)
	if build == "" {
		build = defaultBuildCmd
	}
	switch {
	case course == "os-rust" && build == defaultBuildCmd: // text unchanged
		return `## Build it

The toolchain is pinned by ` + "`rust-toolchain.toml`" + `, so a plain build works on a
fresh clone:

    cd os/kernel && cargo build --release

You need a nightly Rust toolchain (rustup installs the pinned one from that file),
plus ` + "`nasm`" + ` and ` + "`qemu-system-x86_64`" + `.
`
	case course == "os-c" && build == "make -C os": // text unchanged
		return `## Build it

A plain build works on a fresh clone, with no course tooling installed:

    make -C os

You need a C compiler (` + "`gcc`" + ` or ` + "`clang`" + `), ` + "`binutils`" + `, ` + "`nasm`" + ` and
` + "`qemu-system-x86_64`" + `. ` + "`make -C os run`" + ` boots it; ` + "`make -C os verify`" + ` boots it
and checks the markers.
`
	}
	toolchain := "You need the course's toolchain installed"
	if strings.HasPrefix(build, "cargo ") {
		toolchain = "You need Rust (rustup installs it; if the repo carries a " +
			"`rust-toolchain.toml`, that pins the exact toolchain)"
	}
	return `## Build it

The course builds with one command — the same one ` + "`sboot test`" + ` runs for you:

    ` + build + `

` + toolchain + `, plus ` + "`qemu-system-x86_64`" + ` to run what it builds.
`
}

func readme(course, title, firstStage, build string) string {
	if title == "" {
		title = course
	}
	if firstStage == "" {
		firstStage = "01-boot"
	}
	return fmt.Sprintf(`# %[1]s

My work on **%[1]s**, a %[2]s path — you build the real system instead of watching
someone build it.

Everything under `+"`os/`"+` is mine: the boot code, the kernel, and the code that
makes them run.

%[6]s
## Grade it

The course's tests are deliberately **not** in this repo — they live in the %[2]s
data directory, and the grader is the `+"`sboot`"+` command itself, so this repo stays
my code and nothing else:

    sboot where           # prints where both live
    sboot test %[3]s    # practice run, as often as you like
    sboot submit %[3]s  # the official grade

## The course

%[4]s/courses/%[5]s

---

The `+"`os/`"+` scaffold this started from is MIT-licensed (see LICENSE); the course
prose, tests and grader are %[2]s's and are not redistributable.
`, title, brandName, firstStage, siteURL(), course, build)
}
