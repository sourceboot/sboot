# sboot

The command-line tool for a learning Rust and Python.

`sboot` is how you work: it sets up a lab, runs the tests **on your machine**, and
submits a run for an official grade.

## Install

```sh
curl -fsSL https://sourceboot.com/install.sh | sh
```

Or with Go:

```sh
go install github.com/sourceboot/sboot/cmd/sboot@latest
```

Binaries for linux, macOS and Windows (amd64 + arm64) are on the
[releases page](https://github.com/sourceboot/sboot/releases), with `SHA256SUMS`.

## Use

```sh
sboot start os-rust        # create your workspace and fetch the first lab's tests
sboot test 01-boot         # build, boot it in QEMU, grade it — locally, offline
sboot submit 01-boot       # the official run; records a verified completion
sboot where                # where your repo, tests and grader live
sboot debug 04-debugging   # boot frozen, waiting for a debugger on :1234
sboot version
```

`sboot test` needs the lab's own toolchain (for the OS course: rustup, nasm, qemu).
`sboot --help` lists every flag and environment variable.

## How it works

**Grading runs on your machine.** `sboot test` builds your code, boots it, and checks
what it actually did — no upload, no network, no account needed once a lab's tests are
cached. That is why the loop is fast and works on a plane.

**The grader itself is a separate program.** `sboot` downloads `sboot-judge` for your
platform alongside a course's tests, checks it against a published SHA-256 before
making it executable, and runs it locally. It is not in this repository and is not
MIT — `sboot where` prints where it landed, and it carries its own licence. Everything
that decides *what leaves your machine* is here, in this repo, and readable.

**`sboot submit` is the same checks, recorded.** It grades locally first and refuses to
submit a failing run, then uploads that run for the server to judge independently. The
difference between the two commands is not what is checked — it is who computes the
verdict that goes on your record.

**Two directories, on purpose.** Your code lives in a git repo you own and can publish;
our tests and grader live in a separate cache directory (`sboot where` prints both). A
portfolio repo that is mostly someone else's harness reads as "I did a tutorial"; your
kernel plus your commits reads as "I wrote a kernel."

## About this repository

This is the CLI only, mirrored from a private monorepo that also holds the course
content and grading rubrics. Issues and pull requests are welcome here; note that
changes are merged upstream first and mirrored back, so a PR may land as an equivalent
commit rather than a merge.

MIT licensed — see [LICENSE](LICENSE). What is NOT here: the grading engine
(`sboot-judge`, fetched per platform and separately licensed) and the course content —
the labs, the prose and the rubrics that say what each check looks for.
