// Local guidance state: how many times in a row each check has failed.
//
// The failure-guidance ladder (the failure-guidance spec) escalates to Layer 2 —
// the full likelihood-ordered debug ladder — on the THIRD CONSECUTIVE failure of
// the SAME check in `sboot test`. Two attempts is normal iteration; three is
// stuck. Per check, not per stage: flailing across three different problems must
// not burn the escalation. Reset on pass, so the ladder tracks being stuck now
// rather than having once been stuck.
//
// The CLI owns this, not the grader. The grading engine is handed a tier cap and
// stays historyless — the same separation that lets the judge run next to untrusted
// code on the server, and it keeps the threshold tunable here rather than in the
// engine both ends share.
//
// WHERE IT LIVES: ~/.config/sboot/state.json (%AppData%\sboot\ on Windows), never
// a dotfile in the workspace. Under the learner-owned-repo model (`gh repo create`,
// the workspace-split design) the workspace becomes a repo they push and show
// people, so it stays free of our bookkeeping.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// l2Threshold is the number of consecutive failures of one check that earns
// Layer 2. Local, so it is tunable without a deploy.
const l2Threshold = 3

// stateVersion stays 1 even though the file has grown fields since (hint_rungs,
// last_failed — the v2 pull ladder, 2026-08-13). The file is append-only like
// every wire shape here: an old file simply lacks the new maps, and a NEW field
// must never require a bump — loadState discards a file whose version it does
// not recognise, so bumping would wipe every learner's ladder counters on
// upgrade (and a downgraded binary would wipe the rungs on its next save).
// Bump only for a change an old reader would MISREAD, not merely miss.
const stateVersion = 1

type guidanceState struct {
	Version int `json:"version"`
	// "course/stage/check-id" -> consecutive failures. A row is deleted the moment
	// its check passes, so the file stays small and self-pruning.
	Fails map[string]int `json:"consecutive_failures"`
	// "course/stage/check-id" -> highest hint rung revealed by `sboot hint`
	// (the failure-guidance spec "Failure guidance v2"). MONOTONIC, deliberately
	// never reset on pass — the v2 spec ratified exactly this ("one monotonic
	// per-check marker … that never needs reset"): the rung records how much
	// authored help has been REVEALED, and un-revealing text someone has read is
	// not a thing; a passing check cannot be hinted anyway (runHint refuses), so
	// reset would only ever re-gate a learner who regressed back into the same
	// hole. Contrast Fails above, which measures being stuck NOW and so clears.
	Rungs map[string]int `json:"hint_rungs,omitempty"`
	// "course/stage" -> the check ids that FAILED in the most recent graded run,
	// in verdict order. What a bare `sboot hint <stage>` targets (the first one),
	// and how it can tell "everything passes" from "never graded". Only written
	// once a stage has ids to say something about, so the e2e stub grader (no
	// check ids) still creates no state file.
	LastFailed map[string][]string `json:"last_failed,omitempty"`
	// course id -> what the SERVER said about this course the last time we could
	// ask it (ux-plan §5/§7.5, the v0.4 orientation release). This is the offline
	// half of "server truth when online, cache offline": bare `sboot` and the
	// stage-defaulting commands read it when the platform is unreachable, and
	// every successful GET /api/v1/completions refreshes it. Append-only like
	// every other field here — stateVersion stays 1.
	Sync map[string]*courseSync `json:"course_sync,omitempty"`
	// "course/stage" -> the last LOCAL practice score ("3/5"), written by every
	// graded run. Purely presentational (the ▸ row's "last practice 3/5"), so
	// losing it costs one dash in the status screen and nothing else.
	LastScore map[string]string `json:"last_score,omitempty"`
	// The course catalog as of the last successful GET /api/v1/courses, so
	// `sboot courses` and the out-of-repo status degrade to cached orientation
	// rather than a wall (P7).
	Catalog *catalogCache `json:"catalog,omitempty"`
	// Where this was loaded from. Unexported, so never serialized.
	path string
	// Whether record() actually changed anything. Nothing to record means nothing
	// to write: a learner whose checks all pass (and every run of the E2E, which
	// drives a stub grader emitting no check ids) should not have a state file
	// created for it at all.
	dirty bool
}

// courseSync is one course's server-derived progress, cached for offline
// orientation ("as of last sync").
type courseSync struct {
	// stage id -> the official score ("4/4"; may be "" when the server did not
	// say). Presence in the map is what means "verified".
	Verified map[string]string `json:"verified"`
	// stage id -> the latest PRACTICE score the server has seen for a stage not
	// yet verified — what makes "last practice 3/5" true on a fresh machine,
	// where the local LastScore has never been written.
	Practice map[string]string `json:"practice,omitempty"`
	// The most recent stage a PRACTICE run touched, per the server's attempt
	// list — what lets the out-of-repo course rows say "lab 02 in progress".
	LastAttempt string `json:"last_attempt,omitempty"`
	// RFC3339. When this course was last synced from the server.
	SyncedAt string `json:"synced_at,omitempty"`
}

// catalogCache is the course catalog as of the last successful fetch.
type catalogCache struct {
	Courses  []catalogCourse `json:"courses"`
	SyncedAt string          `json:"synced_at,omitempty"`
}

// stateDir resolves the config directory. SBOOT_STATE_DIR overrides it (tests and
// the guidance verification script use that rather than touching a real home).
func stateDir() (string, error) {
	if d := os.Getenv("SBOOT_STATE_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir() // %AppData%
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "sboot"), nil
	}
	// XDG on every unix, including macOS: the failure-guidance spec names
	// ~/.config/sboot explicitly, and `sboot config` (M3) will share this file.
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "sboot"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "sboot"), nil
}

// loadState never fails the caller: guidance is a nicety, and a corrupt or
// unreadable state file must not stop someone grading their kernel. The cost of a
// reset is one lost escalation.
func loadState() *guidanceState {
	s := &guidanceState{
		Version:    stateVersion,
		Fails:      map[string]int{},
		Rungs:      map[string]int{},
		LastFailed: map[string][]string{},
		Sync:       map[string]*courseSync{},
		LastScore:  map[string]string{},
	}
	dir, err := stateDir()
	if err != nil {
		return s
	}
	s.path = filepath.Join(dir, "state.json")
	b, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var loaded guidanceState
	if err := json.Unmarshal(b, &loaded); err != nil || loaded.Version != stateVersion {
		return s
	}
	if loaded.Fails != nil {
		s.Fails = loaded.Fails
	}
	if loaded.Rungs != nil {
		s.Rungs = loaded.Rungs
	}
	if loaded.LastFailed != nil {
		s.LastFailed = loaded.LastFailed
	}
	if loaded.Sync != nil {
		s.Sync = loaded.Sync
	}
	if loaded.LastScore != nil {
		s.LastScore = loaded.LastScore
	}
	s.Catalog = loaded.Catalog
	return s
}

// save writes the state atomically (temp file + rename) so an interrupted run
// cannot leave a truncated file that resets everyone's counters.
func (s *guidanceState) save() error {
	if s.path == "" || !s.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	s.Version = stateVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func stateKey(course, stage, checkID string) string {
	return course + "/" + stage + "/" + checkID
}

// tierSpec is the `id=N,...` ladder spec (grade.go `tierCaps` reads it): the checks
// in this stage that consecutive failures have already escalated. Returns "" when
// nobody has earned anything, which is the common case.
//
// A check with p prior consecutive failures is on attempt p+1 now, so Layer 2 is
// earned once p+1 >= l2Threshold.
func (s *guidanceState) tierSpec(course, stage string) string {
	prefix := stateKey(course, stage, "")
	var specs []string
	for k, n := range s.Fails {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if n+1 >= l2Threshold {
			specs = append(specs, strings.TrimPrefix(k, prefix)+"=2")
		}
	}
	// Deterministic order: the argument shows up in SBOOT_DEBUG output and in the
	// guidance verification script, and an unstable one is miserable to assert on.
	sort.Strings(specs)
	return strings.Join(specs, ",")
}

// record folds this run's verdict into the counters: a failure increments, a pass
// clears. Checks without an id (an older lab.toml) are skipped — they still get
// Layers 0 and 1, they just cannot escalate, which is why every check in our
// content carries one.
//
// It also records the run's FAILING SET in order, for `sboot hint` — but only
// once the stage has id-carrying checks and something to say: a first run where
// everything passes writes nothing, preserving "an all-green learner (and the
// e2e stub grader) gets no state file at all".
func (s *guidanceState) record(course, stage string, checks []localCheck) {
	var failing []string
	haveIDs := false
	for _, c := range checks {
		if c.id == "" {
			continue
		}
		haveIDs = true
		k := stateKey(course, stage, c.id)
		if c.pass {
			if _, had := s.Fails[k]; had {
				delete(s.Fails, k)
				s.dirty = true
			}
		} else {
			failing = append(failing, c.id)
			s.Fails[k]++
			s.dirty = true
		}
	}
	if !haveIDs {
		return
	}
	lk := course + "/" + stage
	_, had := s.LastFailed[lk]
	if len(failing) == 0 && !had {
		return // nothing failed and nothing was tracked — leave no trace
	}
	if had && sameStrings(s.LastFailed[lk], failing) {
		return
	}
	if failing == nil {
		failing = []string{} // an EXPLICIT empty list means "graded, all passing"
	}
	s.LastFailed[lk] = failing
	s.dirty = true
}

// lastFailed answers `sboot hint`'s two questions at once: which checks failed
// last time (in verdict order), and whether this stage has ever been graded at
// all — nil-with-false meaning "run `sboot test` first".
func (s *guidanceState) lastFailed(course, stage string) ([]string, bool) {
	f, ok := s.LastFailed[course+"/"+stage]
	return f, ok
}

// recordScore keeps the last local practice score for the status screen's
// "last practice 3/5". Never recorded as 0/0 — a run with no verdict is a launch
// failure, not a score (the L2a rule).
func (s *guidanceState) recordScore(course, stage string, score, max int) {
	if max <= 0 {
		return
	}
	v := fmt.Sprintf("%d/%d", score, max)
	k := course + "/" + stage
	if s.LastScore[k] == v {
		return
	}
	s.LastScore[k] = v
	s.dirty = true
}

// setSync replaces one course's cached server progress after a successful fetch.
func (s *guidanceState) setSync(course string, sync *courseSync) {
	s.Sync[course] = sync
	s.dirty = true
}

// markVerified folds one freshly verified stage into the cache — called when a
// submit just passed, so the next offline status does not un-verify it.
func (s *guidanceState) markVerified(course, stage, score string) {
	cs := s.Sync[course]
	if cs == nil {
		cs = &courseSync{Verified: map[string]string{}}
		s.Sync[course] = cs
	}
	if cs.Verified == nil {
		cs.Verified = map[string]string{}
	}
	cs.Verified[stage] = score
	s.dirty = true
}

// setCatalog replaces the cached catalog after a successful fetch.
func (s *guidanceState) setCatalog(c *catalogCache) {
	s.Catalog = c
	s.dirty = true
}

// bumpRung climbs one rung of the hint ladder for a check and returns the rung
// just earned (1-based). Monotonic; see the Rungs field for why there is no
// reset.
func (s *guidanceState) bumpRung(course, stage, checkID string) int {
	k := stateKey(course, stage, checkID)
	s.Rungs[k]++
	s.dirty = true
	return s.Rungs[k]
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// debugf reports state trouble only under SBOOT_DEBUG. A learner does not need to
// hear about our bookkeeping mid-lab.
func debugf(format string, args ...any) {
	if os.Getenv("SBOOT_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "sboot: "+format+"\n", args...)
	}
}
