package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of the local state file: Layer 2 arrives on the THIRD
// consecutive failure of the SAME check, and a pass wipes the slate.
func TestLadderEscalatesOnThirdConsecutiveFailureAndResetsOnPass(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)

	fail := func(ids ...string) string {
		st := loadState()
		spec := st.tierSpec("os-rust", "09-paging")
		var checks []localCheck
		for _, id := range ids {
			checks = append(checks, localCheck{id: id, pass: false})
		}
		st.record("os-rust", "09-paging", checks)
		if err := st.save(); err != nil {
			t.Fatalf("save: %v", err)
		}
		return spec
	}

	if got := fail("serial:paging"); got != "" {
		t.Errorf("1st failure must render tier 1 only, got tier spec %q", got)
	}
	if got := fail("serial:paging"); got != "" {
		t.Errorf("2nd failure is normal iteration, not stuck; got tier spec %q", got)
	}
	if got := fail("serial:paging"); got != "serial:paging=2" {
		t.Errorf("3rd consecutive failure must earn tier 2, got %q", got)
	}
	// Still stuck: it stays escalated rather than lapsing.
	if got := fail("serial:paging"); got != "serial:paging=2" {
		t.Errorf("4th failure should stay escalated, got %q", got)
	}

	// A pass clears the counter, so the ladder tracks being stuck NOW.
	st := loadState()
	st.record("os-rust", "09-paging", []localCheck{{id: "serial:paging", pass: true}})
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n := len(loadState().Fails); n != 0 {
		t.Errorf("a passing check must drop its row, %d left", n)
	}
	if got := fail("serial:paging"); got != "" {
		t.Errorf("after a pass the next failure is a first failure again, got %q", got)
	}
}

// Counted per check, not per stage: flailing across three different problems must
// not burn the escalation.
func TestLadderIsPerCheckNotPerStage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)
	st := loadState()
	for _, id := range []string{"serial:paging", "serial:huge_page", "static:x:y"} {
		st.record("os-rust", "09-paging", []localCheck{{id: id, pass: false}})
	}
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := loadState().tierSpec("os-rust", "09-paging"); got != "" {
		t.Errorf("three different checks failing once each is not stuck; got %q", got)
	}
}

// Keys carry course and stage, so the same check id in two stages (00-env-setup and
// 02-boot really do share "serial:longmode") counts separately.
func TestStateKeyedByCourseAndStage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)
	st := loadState()
	for i := 0; i < 3; i++ {
		st.record("os-rust", "02-boot", []localCheck{{id: "serial:longmode", pass: false}})
	}
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	st = loadState()
	if got := st.tierSpec("os-rust", "02-boot"); got != "serial:longmode=2" {
		t.Errorf("02-boot should be escalated, got %q", got)
	}
	if got := st.tierSpec("os-rust", "00-env-setup"); got != "" {
		t.Errorf("a different stage must not inherit the escalation, got %q", got)
	}
	if got := st.tierSpec("kv-store", "02-boot"); got != "" {
		t.Errorf("a different course must not inherit the escalation, got %q", got)
	}
}

// A check with no id (an older lab.toml) still grades — it just cannot escalate.
// It must never create a phantom row keyed on the empty string, which would
// escalate every id-less check at once.
func TestChecksWithoutIDsAreNotTracked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)
	st := loadState()
	for i := 0; i < 5; i++ {
		st.record("os-rust", "01-boot", []localCheck{{id: "", pass: false}})
	}
	if len(st.Fails) != 0 {
		t.Errorf("id-less checks must not be tracked, got %v", st.Fails)
	}
}

// Nothing to record means nothing to write. A learner whose stage passes first try
// — and every E2E run, whose stub grader emits no check ids — should not have a
// state file conjured into their config dir.
func TestNothingToRecordWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)
	st := loadState()
	st.record("os-rust", "01-boot", []localCheck{{id: "serial:pm_rust", pass: true}})
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("state file created with nothing to record (err=%v)", err)
	}
}

func TestStateSurvivesCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBOOT_STATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Guidance is a nicety; a corrupt file must not stop someone grading.
	st := loadState()
	if st.Fails == nil {
		t.Fatal("loadState must always return a usable state")
	}
	st.record("os-rust", "01-boot", []localCheck{{id: "serial:pm_rust", pass: false}})
	if err := st.save(); err != nil {
		t.Fatalf("save over a corrupt file: %v", err)
	}
	if loadState().Fails["os-rust/01-boot/serial:pm_rust"] != 1 {
		t.Error("state should have been rewritten cleanly")
	}
}

// The protocol is append-only: a record from a NEWER grader (more fields) and one
// from an OLDER grader (fewer) must both parse. Getting this wrong drops every
// check silently — the verdict lists nothing, and it does not look like an error.
func TestParseVerdictAcceptsOldAndNewRecords(t *testing.T) {
	out := strings.Join([]string{
		"LBX_CHECK\tPASS\t1\tkernel boots\t\tserial:reached_kernel_entry",
		"LBX_CHECK\tFAIL\t3\ta fresh 4 KiB mapping\tL0\x1fbooted\x1eL1\x1fnudge\tserial:paging",
		"LBX_CHECK\tFAIL\t2\tan older grader, four fields",
		"LBX_CHECK\tFAIL\t1\tfive fields\tL0\x1fbooted",
		"LBX_CHECK\tFAIL\t1\tseven fields, from the future\t\tserial:future\tsomething-new",
		"LBX_SCORE\t1\t8",
	}, "\n")
	checks, score, max := parseVerdict(out)
	if len(checks) != 5 {
		t.Fatalf("want 5 checks, got %d: %+v", len(checks), checks)
	}
	if score != 1 || max != 8 {
		t.Fatalf("want score 1/8, got %d/%d", score, max)
	}
	if !checks[0].pass || checks[0].id != "serial:reached_kernel_entry" {
		t.Errorf("first record parsed wrong: %+v", checks[0])
	}
	if checks[1].pass || checks[1].points != 3 || checks[1].id != "serial:paging" {
		t.Errorf("second record parsed wrong: %+v", checks[1])
	}
	if checks[2].id != "" || checks[3].id != "" {
		t.Errorf("records without an id field must yield an empty id: %+v %+v", checks[2], checks[3])
	}
	if checks[4].id != "serial:future" {
		t.Errorf("extra trailing fields must not shift the id: %+v", checks[4])
	}
}
