// Bare `sboot` — the orientation screen — plus `sboot courses` and the
// current-lab resolution every stage-optional command leans on (ux-plan §5,
// ratified §11.b/§11.c; the ux-v2 CLI tiles are the copy of record).
//
// The three questions every session starts with — where am I, what's next, how —
// are answered here at zero cost, exit 0, in every state (P1). The data model:
//
//	which labs exist, in order   the spec manifest (cached first, fetch once)
//	which are DONE               the server's completions (GET /api/v1/completions)
//	                             when online — server truth — cached in state.json
//	                             for offline orientation ("as of last sync", P7)
//	what the catalog holds       GET /api/v1/courses, cached the same way
//
// The CURRENT lab is the first live lab the server has not verified. That single
// rule is what lets `sboot test`/`submit`/`hint`/`debug` drop their stage
// argument — and the defaulted stage is ALWAYS printed, so the choice is visible
// and `sboot test <stage>` visibly overrides it.
//
// Both catalog routes are being introduced alongside this CLI. A platform that
// does not serve them yet answers 404, and every consumer here degrades to the
// cache, then to a pointer at the website — never a wall (P7).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ── the catalog (GET /api/v1/courses) ───────────────────────────────────────────

// catalogCourse is one catalog row (ux-plan §7.1; the field names are the
// /api/v1/courses route's, append-only). `path`/`path_order` are null for the
// no-path stubs, which Go reads as their zero values.
type catalogCourse struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Domain     string `json:"domain"`
	Live       int    `json:"live"`
	Total      int    `json:"total"`
	FreeStages int    `json:"free_stages"`
	Path       string `json:"path"`
	PathOrder  int    `json:"path_order"`
}

// fetchCatalog pulls the catalog. It accepts both a `{"courses": [...]}` wrapper
// and a bare array, because the route ships alongside this binary and a released
// CLI cannot be patched to match a shape decided later.
func fetchCatalog() ([]catalogCourse, error) {
	if offline() {
		return nil, errors.New("SBOOT_OFFLINE is set")
	}
	req, err := authedRequest("GET", apiURL()+"/api/v1/courses", nil)
	if err != nil {
		return nil, err
	}
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Err string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Err == "" {
			e.Err = "no catalog on this platform"
		}
		return nil, &apiError{status: resp.StatusCode, msg: e.Err}
	}
	var body []byte
	buf := make([]byte, 1024)
	for {
		n, readErr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if readErr != nil {
			break
		}
		if len(body) > 1<<20 {
			return nil, errors.New("catalog response too large")
		}
	}
	var wrapped struct {
		Courses []catalogCourse `json:"courses"`
	}
	if json.Unmarshal(body, &wrapped) == nil && wrapped.Courses != nil {
		return wrapped.Courses, nil
	}
	var bare []catalogCourse
	if json.Unmarshal(body, &bare) == nil && bare != nil {
		return bare, nil
	}
	return nil, errors.New("unreadable catalog response")
}

// catalog returns the freshest catalog it can: the platform's, cached on the way
// through, else the cache, else nothing. `src` is "server" | "cache" | "none".
func catalog(st *guidanceState) (courses []catalogCourse, src string) {
	if cs, err := fetchCatalog(); err == nil {
		st.setCatalog(&catalogCache{Courses: cs, SyncedAt: time.Now().UTC().Format(time.RFC3339)})
		return cs, "server"
	} else {
		debugf("catalog fetch skipped (%v)", err)
	}
	if st.Catalog != nil && len(st.Catalog.Courses) > 0 {
		return st.Catalog.Courses, "cache"
	}
	return nil, "none"
}

// sortCatalog: live courses first, otherwise the server's own order preserved
// (the route already answers path bands in step order — a stable partition is
// all the CLI adds).
func sortCatalog(cs []catalogCourse) []catalogCourse {
	out := append([]catalogCourse(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Live > 0 && out[j].Live == 0
	})
	return out
}

// ── completions (GET /api/v1/completions) ───────────────────────────────────────

// flexBool reads the platform's booleans whichever way they arrive: SQLite hands
// the driver 0/1, Postgres hands it true/false, and this binary must keep parsing
// both forever.
type flexBool bool

func (b *flexBool) UnmarshalJSON(d []byte) error {
	switch strings.TrimSpace(string(d)) {
	case "true":
		*b = true
	case "false", "null", "0":
		*b = false
	default:
		var n float64
		if err := json.Unmarshal(d, &n); err == nil {
			*b = n != 0
			return nil
		}
		*b = false
	}
	return nil
}

type attemptRow struct {
	CourseID string   `json:"course_id"`
	Course   string   `json:"course"` // tolerated alternate spelling
	StageID  string   `json:"stage_id"`
	Stage    string   `json:"stage"`
	Score    int      `json:"score"`
	MaxScore int      `json:"max_score"`
	Passed   flexBool `json:"passed"`
	Verified flexBool `json:"verified"`
	Created  string   `json:"created_at"`
}

func (a attemptRow) course() string {
	if a.CourseID != "" {
		return a.CourseID
	}
	return a.Course
}

func (a attemptRow) stage() string {
	if a.StageID != "" {
		return a.StageID
	}
	return a.Stage
}

type completionsData struct {
	handle   string
	attempts []attemptRow // newest first, as the server orders them
}

// fetchCompletions asks the platform for this account's attempts. `course` may
// be "" for all courses. The ?course=&limit= parameters are understood by the
// platform this ships beside and harmlessly ignored by an older one — the
// response shape is the same either way, and this filters again client-side.
func fetchCompletions(course string) (*completionsData, error) {
	if offline() {
		return nil, errors.New("SBOOT_OFFLINE is set")
	}
	u := apiURL() + "/api/v1/completions?limit=200"
	if course != "" {
		u += "&course=" + course
	}
	req, err := authedRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := send(&http.Client{Timeout: 10 * time.Second}, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		User     string       `json:"user"`
		Attempts []attemptRow `json:"attempts"`
		Err      string       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if r.Err == "" {
			r.Err = "unexpected response"
		}
		return nil, &apiError{status: resp.StatusCode, msg: r.Err}
	}
	d := &completionsData{handle: r.User}
	for _, a := range r.Attempts {
		if course != "" && a.course() != course {
			continue
		}
		if a.course() == "" || a.stage() == "" {
			continue
		}
		d.attempts = append(d.attempts, a)
	}
	return d, nil
}

// syncFromAttempts folds an attempt list into one course's cacheable progress.
func syncFromAttempts(d *completionsData, course string) *courseSync {
	cs := &courseSync{
		Verified: map[string]string{},
		Practice: map[string]string{},
		SyncedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, a := range d.attempts {
		if a.course() != course {
			continue
		}
		if bool(a.Verified) && bool(a.Passed) {
			if _, have := cs.Verified[a.stage()]; !have {
				score := ""
				if a.MaxScore > 0 {
					score = fmt.Sprintf("%d/%d", a.Score, a.MaxScore)
				}
				cs.Verified[a.stage()] = score
			}
		}
	}
	for _, a := range d.attempts { // newest-first: keep each stage's latest practice
		if a.course() != course || bool(a.Verified) {
			continue
		}
		if _, done := cs.Verified[a.stage()]; done {
			continue
		}
		if _, have := cs.Practice[a.stage()]; !have && a.MaxScore > 0 {
			cs.Practice[a.stage()] = fmt.Sprintf("%d/%d", a.Score, a.MaxScore)
		}
		if cs.LastAttempt == "" {
			cs.LastAttempt = a.stage()
		}
	}
	return cs
}

// startedCourses lists course ids by most-recent activity, newest first.
func startedCourses(d *completionsData) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range d.attempts {
		if c := a.course(); !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// courseProgress is the "server truth when online, cache offline" rule for one
// course. src is "server" | "cache" | "none"; handle is known only from a live
// answer.
func courseProgress(st *guidanceState, course string) (cs *courseSync, src, handle string) {
	if d, err := fetchCompletions(course); err == nil {
		cs = syncFromAttempts(d, course)
		st.setSync(course, cs)
		return cs, "server", d.handle
	} else {
		debugf("completions fetch skipped (%v)", err)
	}
	if cached := st.Sync[course]; cached != nil {
		return cached, "cache", ""
	}
	return nil, "none", ""
}

// ── the lab list (the spec manifest) ────────────────────────────────────────────

type labInfo struct {
	Stage string
	Title string
	Live  bool
}

func labsFromManifest(m *specManifest) []labInfo {
	var out []labInfo
	for _, l := range m.Labs {
		out = append(out, labInfo{Stage: l.Stage, Title: l.Title, Live: l.Live})
	}
	return out
}

// manifestMemo keeps a lab list this run FETCHED (as opposed to read off disk),
// so one command never fetches the manifest twice, and a title stays available
// to the grading header even when the spec has not been materialised into the
// cache yet. Fetch results only — disk reads are cheap and staying un-memoized
// keeps them honest when the cache dir changes under a test. Per process, never
// persisted; no goroutines, so a plain map suffices.
var manifestMemo = map[string]struct {
	labs  []labInfo
	title string
}{}

// cachedLabsInfo reads the lab list without touching the network — what the
// explicit-stage paths use for titles, so naming a stage never costs a fetch.
func cachedLabsInfo(course string) ([]labInfo, string) {
	if m, ok := manifestMemo[course]; ok {
		return m.labs, m.title
	}
	s, ok := cachedSpec(course)
	if !ok {
		return nil, ""
	}
	m := s.cachedManifest()
	if m == nil {
		return nil, ""
	}
	return labsFromManifest(m), m.Title
}

func memoManifest(course string, labs []labInfo, title string) {
	manifestMemo[course] = struct {
		labs  []labInfo
		title string
	}{labs, title}
}

// manifestLabs is cachedLabsInfo with a network fallback, for the paths that
// need an answer (status, the stage default) on a machine with nothing cached.
func manifestLabs(course string) ([]labInfo, string, error) {
	if labs, title := cachedLabsInfo(course); labs != nil {
		return labs, title, nil
	}
	m, err := fetchManifest(course)
	if err != nil {
		return nil, "", err
	}
	labs := labsFromManifest(m)
	memoManifest(course, labs, m.Title)
	return labs, m.Title, nil
}

// isWelcomeLab: a `00-*` stage is the course's welcome/setup lab (ux-plan §14)
// — always free, outside the marketed lab count and outside every "N of M"
// numerator. It still occupies its place in the lab ORDER (it is the fresh
// learner's current lab, and completing it is what opens lab 01), so only the
// COUNTS skip it, never the row lists. The platform's catalog live/total are
// marketed (numbered-only) for the same reason, and the mock's status tiles
// show "1 of 2 live labs verified" with `00 welcome ✓ verified` dim above.
func isWelcomeLab(stage string) bool {
	return strings.HasPrefix(stage, "00-")
}

// labNumber is the leading digits of a stage id ("02-glass-cockpit" → "02").
func labNumber(stage string) string {
	for i, r := range stage {
		if r < '0' || r > '9' {
			return stage[:i]
		}
	}
	return stage
}

// labSlug renders a stage id the way the status rows do: number column + slug
// ("02-glass-cockpit" → "glass-cockpit").
func labSlug(stage string) string {
	n := labNumber(stage)
	return strings.TrimPrefix(strings.TrimPrefix(stage, n), "-")
}

// labTitle is the human title for a stage, "" when the manifest does not carry
// one (the fallback is the stage id itself, applied by the renderers).
func labTitle(course, stage string) string {
	labs, _ := cachedLabsInfo(course)
	for _, l := range labs {
		if l.Stage == stage {
			return l.Title
		}
	}
	return ""
}

// resolveStageArg maps an explicit stage argument onto the manifest's id, by the
// same rule the engine uses (exact id, else the lab number). Purely cosmetic —
// the engine still owns real resolution — so an unknown arg passes through.
func resolveStageArg(course, arg string) (string, string) {
	labs, _ := cachedLabsInfo(course)
	for _, l := range labs {
		if l.Stage == arg {
			return l.Stage, l.Title
		}
	}
	if n := labNumber(arg); n != "" {
		for _, l := range labs {
			if strings.TrimLeft(labNumber(l.Stage), "0") == strings.TrimLeft(n, "0") &&
				(arg == n || strings.HasPrefix(l.Stage, n)) {
				return l.Stage, l.Title
			}
		}
	}
	return arg, ""
}

// ── the current lab ─────────────────────────────────────────────────────────────

// frontierInfo describes "every live lab is verified": what comes next (possibly
// nothing listed) and the live/listed counts for the message.
type frontierInfo struct {
	next   *labInfo // the first unverified lab of any liveness, nil if none listed
	live   int
	listed int
}

// currentLab picks the first LIVE lab the server has not verified.
// Returns exactly one of lab (ok) or fr (frontier). err covers "cannot know":
// no lab list reachable, or no progress source at all.
func currentLab(st *guidanceState, course string) (lab labInfo, fr *frontierInfo, src string, err error) {
	labs, _, err := manifestLabs(course)
	if err != nil {
		return labInfo{}, nil, "", fmt.Errorf("cannot fetch the lab list for %s (%v)", course, err)
	}
	cs, src, _ := courseProgress(st, course)
	if cs == nil {
		return labInfo{}, nil, src, fmt.Errorf("cannot tell which lab is current: no progress is cached and the platform is unreachable")
	}
	live := 0
	for _, l := range labs {
		if l.Live {
			live++
		}
	}
	for _, l := range labs {
		if _, done := cs.Verified[l.Stage]; done {
			continue
		}
		if l.Live {
			return l, nil, src, nil
		}
	}
	f := &frontierInfo{live: live, listed: len(labs)}
	for i, l := range labs {
		if _, done := cs.Verified[l.Stage]; !done {
			f.next = &labs[i]
			break
		}
	}
	return labInfo{}, f, src, nil
}

// previousVerifiedLab names the LIVE lab immediately before `stage` in the
// course's lab order, when the learner holds a VERIFIED completion on it — or
// "" otherwise. The other half of the R1-7 signal (state.go hasLocalWork): a
// defaulted submit on an untouched stage whose predecessor just completed is
// the resubmit-after-Complete footgun, and this is the lab the learner
// probably meant.
//
// Reads only what this process already has — the manifest memo/spec cache and
// the state.json sync the defaulting path itself refreshed — so it costs no
// network call and degrades to "" (no special copy) when either is absent.
func previousVerifiedLab(st *guidanceState, course, stage string) string {
	labs, _ := cachedLabsInfo(course)
	cs := st.Sync[course]
	if cs == nil || len(labs) == 0 {
		return ""
	}
	prev := ""
	for _, l := range labs {
		if l.Stage == stage {
			if prev == "" {
				return ""
			}
			if _, done := cs.Verified[prev]; done {
				return prev
			}
			return ""
		}
		if l.Live {
			prev = l.Stage
		}
	}
	return ""
}

// ── bare `sboot` ────────────────────────────────────────────────────────────────

func runStatus(jsonOut bool) int {
	st := loadState()
	r, err := findRepo()
	if err != nil {
		code := outOfRepoStatus(st, jsonOut)
		saveQuietly(st)
		return code
	}
	code := repoStatus(st, r, jsonOut)
	saveQuietly(st)
	return code
}

func saveQuietly(st *guidanceState) {
	if err := st.save(); err != nil {
		debugf("could not save state: %v", err)
	}
}

// repoStatus is the in-repo orientation screen (the S3/S5 tiles).
func repoStatus(st *guidanceState, r repo, jsonOut bool) int {
	labs, title, err := manifestLabs(r.course)
	if err != nil {
		// P7: a workspace with nothing cached and no network still orients.
		if jsonOut {
			return statusJSON(map[string]any{
				"command": "status", "in_repo": true, "course": r.course,
				"error": "no lab list cached and the platform is unreachable",
			})
		}
		fmt.Printf("%s — no lab list cached and the platform is unreachable.\n\n", r.course)
		fmt.Printf("next:  sboot fetch %s          # once online — cached labs still grade offline\n", r.course)
		fmt.Printf("read:  %s/courses/%s\n", siteURL(), r.course)
		return 0
	}
	cs, src, _ := courseProgress(st, r.course)
	if cs == nil {
		cs = &courseSync{Verified: map[string]string{}}
	}

	// Marketed counters — welcome labs are outside every numerator (§14). The
	// row list below still shows them; only the counting skips them.
	live, verifiedLive := 0, 0
	for _, l := range labs {
		if !l.Live || isWelcomeLab(l.Stage) {
			continue
		}
		live++
		if _, done := cs.Verified[l.Stage]; done {
			verifiedLive++
		}
	}
	var current *labInfo
	for i, l := range labs {
		if _, done := cs.Verified[l.Stage]; !done && l.Live {
			current = &labs[i]
			break
		}
	}
	var frontierNext *labInfo
	if current == nil {
		for i, l := range labs {
			if _, done := cs.Verified[l.Stage]; !done {
				frontierNext = &labs[i]
				break
			}
		}
	}

	// The arc line ("⋮ (24-lab arc …)") needs the catalog's marketed total —
	// the spec manifest lists only PUBLISHED labs, so a fresh machine that has
	// never run `sboot courses` would honestly but uselessly print "(3-lab
	// arc)". Fetch once when nothing is cached; the state cache serves every
	// later run, offline included, and a failed fetch just keeps the fallback.
	if st.Catalog == nil {
		catalog(st)
	}
	// MARKETED, like every other number on this screen (§14): the spec manifest
	// lists `00-welcome` and the catalog's total does not, so counting the manifest
	// rows printed "0 of 11 live labs verified" two lines above "(12-lab arc)" for
	// the first course with every lab published (dogfood F00-7). 11 is the count
	// the course is sold with, and the welcome lab keeps its row without joining it.
	listed := 0
	for _, l := range labs {
		if !isWelcomeLab(l.Stage) {
			listed++
		}
	}
	arcTotal := listed
	if st.Catalog != nil {
		for _, c := range st.Catalog.Courses {
			if c.ID == r.course && c.Total > arcTotal {
				arcTotal = c.Total
			}
		}
	}

	if jsonOut {
		rows := make([]map[string]any, 0, len(labs))
		for i := range labs {
			l := labs[i]
			row := map[string]any{"stage": l.Stage, "title": l.Title, "live": l.Live}
			if score, done := cs.Verified[l.Stage]; done {
				row["state"] = "verified"
				if score != "" {
					row["score"] = score
				}
			} else if current != nil && l.Stage == current.Stage {
				row["state"] = "current"
				lp := st.LastScore[r.course+"/"+l.Stage]
				if lp == "" && cs.Practice != nil {
					lp = cs.Practice[l.Stage]
				}
				if lp != "" {
					row["last_practice"] = lp
				}
				if failing, _ := st.lastFailed(r.course, l.Stage); len(failing) > 0 {
					row["failing"] = failing
				}
			} else {
				row["state"] = "locked"
			}
			rows = append(rows, row)
		}
		out := map[string]any{
			"command": "status", "in_repo": true, "course": r.course, "title": title,
			"live": live, "verified": verifiedLive, "labs": rows, "progress_source": src,
		}
		if current != nil {
			out["current"] = current.Stage
			out["next"] = "sboot test"
		}
		return statusJSON(out)
	}

	p := painter(os.Stdout)

	head := r.course
	if title != "" {
		head = title + " — " + r.course
	}
	suffix := ""
	switch src {
	case "cache":
		suffix = " · as of last sync"
	case "none":
		suffix = " · progress unknown (offline)"
	}
	counts := fmt.Sprintf("%d of %d live labs verified", verifiedLive, live)
	if verifiedLive == live && live > 0 {
		counts = p(ansiGreen, counts)
	}
	fmt.Printf("%s · %s%s\n\n", head, counts, p(ansiDim, suffix))

	width := 4
	for _, l := range labs {
		if n := len(labNumber(l.Stage)) + 1 + len(labSlug(l.Stage)); n > width {
			width = n
		}
	}
	width += 6

	shownAfterCurrent := 0
	hidden := 0
	for i := range labs {
		l := labs[i]
		label := labNumber(l.Stage) + " " + labSlug(l.Stage)
		_, done := cs.Verified[l.Stage]
		isCurrent := current != nil && l.Stage == current.Stage
		isFrontierNext := frontierNext != nil && l.Stage == frontierNext.Stage
		afterCurrent := current != nil && !done && !isCurrent

		switch {
		case done && isWelcomeLab(l.Stage):
			// The welcome lab's ✓ row is quiet (the mock's dim register): done,
			// permanent, outside the numerators — never the headline. Quiet, not
			// scoreless (R2-4, round-2 dogfood; same family as rust-for-systems R1-9):
			// this row alone dropped its n/n while every row under it showed one,
			// which reads as a difference in KIND that does not exist — the server
			// records the welcome lab's official score like any other's.
			v := "✓ verified"
			if score := cs.Verified[l.Stage]; score != "" {
				v += " " + score
			}
			fmt.Printf("  %s\n", p(ansiDim, padTo(label, width)+v))
		case done:
			score := cs.Verified[l.Stage]
			v := "✓ verified"
			if score != "" {
				v += " " + score
			}
			fmt.Printf("  %s%s\n", padTo(label, width), p(ansiGreen, v))
		case isCurrent:
			status := "up next"
			lp := st.LastScore[r.course+"/"+l.Stage]
			if lp == "" && cs.Practice != nil {
				lp = cs.Practice[l.Stage] // fresh machine: the server remembers
			}
			if lp != "" {
				status = "last practice " + lp
				if failing, _ := st.lastFailed(r.course, l.Stage); len(failing) > 0 {
					status += " — " + p(ansiRed, failing[0]) + " failing"
				}
			}
			fmt.Printf("%s %s%s\n", p(ansiAmber, "▸"), p(ansiAmber, padTo(label, width)), status)
		case isFrontierNext:
			note := fmt.Sprintf("next — not published yet (%d of %d live today)", live, arcTotal)
			if l.Live {
				note = "next"
			}
			fmt.Printf("%s %s%s\n", p(ansiAmber, "▸"), p(ansiAmber, padTo(label, width)), p(ansiDim, note))
		case afterCurrent && shownAfterCurrent == 0:
			shownAfterCurrent++
			note := "locked until " + labNumber(current.Stage) + " completes"
			if !l.Live {
				note += " · publishing soon"
			}
			fmt.Printf("  %s\n", p(ansiDim, padTo(label, width)+note))
		case afterCurrent:
			hidden++
		default: // unverified, not live, before any current — publishing soon
			fmt.Printf("  %s\n", p(ansiDim, padTo(label, width)+"not published yet"))
		}
	}
	if hidden > 0 || arcTotal > listed {
		fmt.Printf("  %s\n", p(ansiDim, fmt.Sprintf("⋮  (%d-lab arc — labs open in order)", arcTotal)))
	}
	fmt.Println()

	switch {
	case current != nil:
		fmt.Printf("next:  %s               %s\n", p(ansiGreen, "sboot test"), p(ansiDim, "# grades "+current.Stage))
		fmt.Printf("stuck: sboot hint · read: %s/courses/%s/stages/%s\n", siteURL(), r.course, current.Stage)
	case frontierNext != nil:
		fmt.Printf("all live labs verified. lab %s is being built — `sboot courses` will show it the day it lands.\n",
			labNumber(frontierNext.Stage))
		fmt.Printf("meanwhile: %s            %s\n", p(ansiGreen, "sboot courses"), p(ansiDim, "# the rest of your path"))
	default:
		fmt.Println("all listed labs verified — course complete.")
		fmt.Printf("next: %s            %s\n", p(ansiGreen, "sboot courses"), p(ansiDim, "# the rest of your path"))
	}
	return 0
}

// outOfRepoStatus is the S2/B3 view: who you are, your courses, how to start.
func outOfRepoStatus(st *guidanceState, jsonOut bool) int {
	var handle string
	var started []string
	src := "none"
	if d, err := fetchCompletions(""); err == nil {
		src = "server"
		handle = d.handle
		started = startedCourses(d)
		for _, c := range started {
			st.setSync(c, syncFromAttempts(d, c))
		}
	} else {
		debugf("completions fetch skipped (%v)", err)
		if len(st.Sync) > 0 {
			src = "cache"
			for c := range st.Sync {
				started = append(started, c)
			}
			sort.Slice(started, func(i, j int) bool {
				si, sj := st.Sync[started[i]], st.Sync[started[j]]
				if si.SyncedAt != sj.SyncedAt {
					return si.SyncedAt > sj.SyncedAt
				}
				return started[i] < started[j]
			})
		}
	}
	cat, catSrc := catalog(st)

	if jsonOut {
		return statusJSON(map[string]any{
			"command": "status", "in_repo": false, "handle": handle,
			"started": started, "catalog": cat, "progress_source": src, "catalog_source": catSrc,
		})
	}

	p := painter(os.Stdout)
	header := "sboot " + version
	switch {
	case handle != "":
		header += " · connected as " + p(ansiAmber, "@"+handle)
	case src == "cache":
		header += p(ansiDim, " · offline — as of last sync")
	}
	fmt.Println(header)
	fmt.Println()

	idW, titleW := courseColumns(cat, started)
	if len(started) > 0 {
		fmt.Println("no course workspace here — your courses:")
		fmt.Println()
		for _, line := range yourCourseRows(p, st, cat, started, idW, titleW) {
			fmt.Println(line)
		}
		fmt.Println()
		fmt.Printf("next:  cd into a course workspace — bare %s orients you there\n", p(ansiGreen, "sboot"))
		fmt.Printf("more:  sboot courses · read: %s/courses\n", siteURL())
		return 0
	}

	fmt.Println("no course workspace here yet.")
	fmt.Println()
	if len(cat) == 0 {
		fmt.Printf("the catalog could not be reached — browse it at %s/courses\n\n", siteURL())
		fmt.Printf("next:  %s        %s\n", p(ansiGreen, "sboot start <id>"), p(ansiDim, "# unpacks a course into ./<id>"))
		fmt.Printf("read:  %s/courses\n", siteURL())
		return 0
	}
	rows, sawDim := catalogRows(p, cat, nil, idW, titleW)
	for _, line := range rows {
		fmt.Println(line)
	}
	if sawDim {
		fmt.Printf("  %s\n", p(ansiDim, "(more in development — `sboot courses` lists everything)"))
	}
	fmt.Println()
	first := firstLive(cat)
	fmt.Printf("next:  %s        %s\n", p(ansiGreen, "sboot start "+first), p(ansiDim, "# unpacks the course into ./"+first))
	fmt.Printf("read:  %s/courses\n", siteURL())
	return 0
}

func firstLive(cat []catalogCourse) string {
	for _, c := range sortCatalog(cat) {
		if c.Live > 0 {
			return c.ID
		}
	}
	return "<id>"
}

// courseColumns computes the shared id/title column widths across every row a
// screen will print — the started rows AND the catalog rows — so the two
// sections line up the way the tiles do.
func courseColumns(cat []catalogCourse, started []string) (idW, titleW int) {
	byID := map[string]catalogCourse{}
	for _, c := range cat {
		byID[c.ID] = c
	}
	consider := func(id, title string) {
		if title == "" {
			title = id
		}
		if len(id) > idW {
			idW = len(id)
		}
		if n := len([]rune(title)); n > titleW {
			titleW = n
		}
	}
	for _, c := range cat {
		consider(c.ID, c.Title)
	}
	for _, id := range started {
		consider(id, byID[id].Title)
	}
	return idW + 3, titleW + 4
}

// catalogRows renders catalog lines (minus `skip`), live ones bright, the rest
// dim with their status — and reports whether any dim rows were left out of the
// bright list's world (for the "(more in development …)" note). Widths come from
// courseColumns so parallel sections align.
func catalogRows(p func(string, string) string, cat []catalogCourse, skip map[string]bool, idW, titleW int) (lines []string, sawDim bool) {
	sorted := sortCatalog(cat)
	for _, c := range sorted {
		if skip[c.ID] {
			continue
		}
		title := c.Title
		if title == "" {
			title = c.ID
		}
		if c.Live > 0 {
			meta := fmt.Sprintf("%d live", c.Live)
			if c.Total > c.Live {
				meta = fmt.Sprintf("%d of %d live", c.Live, c.Total)
			}
			if c.FreeStages > 0 {
				meta += fmt.Sprintf(" · first %d free", c.FreeStages)
			}
			lines = append(lines, "  "+p(ansiAmber, padTo(c.ID, idW))+padTo(title, titleW)+meta)
		} else {
			sawDim = true
			status := strings.ReplaceAll(c.Status, "-", " ")
			if status == "" {
				status = "coming soon"
			}
			lines = append(lines, "  "+p(ansiDim, padTo(c.ID, idW)+padTo(title, titleW)+status))
		}
	}
	return lines, sawDim
}

// yourCourseRows renders the started-course rows (the B3 shape, shared by the
// out-of-repo status and `sboot courses`). Widths come from courseColumns so
// the section lines up with the catalog below it.
func yourCourseRows(p func(string, string) string, st *guidanceState, cat []catalogCourse, started []string, idW, titleW int) []string {
	byID := map[string]catalogCourse{}
	for _, c := range cat {
		byID[c.ID] = c
	}
	var out []string
	for _, id := range started {
		c := byID[id]
		title := c.Title
		if title == "" {
			title = id
		}
		cs := st.Sync[id]
		verified := 0
		lastAttempt := ""
		if cs != nil {
			// Count against the catalog's marketed `live` (numbered labs only),
			// so a verified welcome lab must not enter this numerator either.
			for stage := range cs.Verified {
				if !isWelcomeLab(stage) {
					verified++
				}
			}
			lastAttempt = cs.LastAttempt
		}
		var meta string
		switch {
		case c.Live > 0 && verified >= c.Live:
			meta = p(ansiGreen, fmt.Sprintf("%d of %d live verified ✓", verified, c.Live)) + " · frontier"
		case c.Live > 0:
			meta = p(ansiGreen, fmt.Sprintf("%d of %d live verified", verified, c.Live))
			if lastAttempt != "" {
				meta += " · lab " + labNumber(lastAttempt) + " in progress"
			}
		default:
			meta = fmt.Sprintf("%d verified", verified)
			if lastAttempt != "" {
				meta += " · lab " + labNumber(lastAttempt) + " in progress"
			}
		}
		out = append(out, p(ansiAmber, "▸ "+padTo(id, idW))+padTo(title, titleW)+meta)
	}
	return out
}

// ── sboot courses ───────────────────────────────────────────────────────────────

func runCourses() int {
	st := loadState()
	defer saveQuietly(st)

	var started []string
	progressSrc := "none"
	if d, err := fetchCompletions(""); err == nil {
		progressSrc = "server"
		started = startedCourses(d)
		for _, c := range started {
			st.setSync(c, syncFromAttempts(d, c))
		}
	} else {
		debugf("completions fetch skipped (%v)", err)
		if len(st.Sync) > 0 {
			progressSrc = "cache"
			for c := range st.Sync {
				started = append(started, c)
			}
			sort.Strings(started)
		}
	}
	cat, catSrc := catalog(st)
	p := painter(os.Stdout)

	if len(cat) == 0 && len(started) == 0 {
		fmt.Fprintf(os.Stderr, "sboot: the catalog is unreachable and nothing is cached yet.\n")
		fmt.Fprintf(os.Stderr, "sboot: browse it at %s/courses — `sboot courses` works offline after one sync.\n", siteURL())
		return 1
	}

	note := "· cached for offline"
	if catSrc == "cache" || progressSrc == "cache" {
		note = "· offline — as of last sync"
	}

	idW, titleW := courseColumns(cat, started)
	if len(started) > 0 {
		fmt.Printf("your courses %s\n\n", p(ansiDim, note))
		for _, line := range yourCourseRows(p, st, cat, started, idW, titleW) {
			fmt.Println(line)
		}
		fmt.Println()
		if len(cat) > 0 {
			skip := map[string]bool{}
			for _, id := range started {
				skip[id] = true
			}
			rows, _ := catalogRows(p, cat, skip, idW, titleW)
			if len(rows) > 0 {
				fmt.Println("catalog")
				for _, line := range rows {
					fmt.Println(line)
				}
				fmt.Println()
			}
		}
		fmt.Printf("start another:  sboot start <id> %s\n", p(ansiDim, "· details: "+siteURL()+"/courses"))
		return 0
	}

	fmt.Printf("catalog — %s/courses %s\n\n", siteURL(), p(ansiDim, note))
	rows, _ := catalogRows(p, cat, nil, idW, titleW)
	for _, line := range rows {
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Printf("start one:  %s        %s\n", p(ansiGreen, "sboot start <id>"),
		p(ansiDim, "# e.g. sboot start "+firstLive(cat)))
	return 0
}

func statusJSON(v map[string]any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	fmt.Println(string(b))
	return 0
}
