package reconstruct

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swizzley/tenet/internal/github"
	"github.com/swizzley/tenet/internal/store"
	"github.com/swizzley/tenet/internal/testgit"
)

type rig struct {
	repo   *testgit.Repo
	srv    *testgit.Server
	runner *Runner
}

func newRig(t *testing.T, commits int, chunk int) *rig {
	t.Helper()
	repo := testgit.New(t, commits)
	srv := &testgit.Server{R: repo}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	gh := github.New(github.Options{BaseURL: hs.URL, Concurrency: 16})
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &rig{repo, srv, &Runner{GH: gh, Cache: db, BufferCap: 100, Chunk: func(c int) int { return min(c, chunk) }}}
}

type recorder struct {
	mu sync.Mutex
	ev []Event
}

func (r *recorder) emit(e Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }

func (r *recorder) of(kind string) []Event {
	var out []Event
	for _, e := range r.ev {
		if e.T == kind {
			out = append(out, e)
		}
	}
	return out
}

// Walking the whole first-parent history rebuilds every file of the head tree
// byte-exact, from diffs alone. The binary file is the one thing it may not
// prove, and it says so.
func TestFullHistoryRebuildsHeadExactly(t *testing.T) {
	g := newRig(t, 40, 8)
	var rec recorder
	sum, err := g.runner.Run(context.Background(), "o", "r", 1000, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	firstParents := len(strings.Fields(g.repo.Git("rev-list", "--first-parent", "main")))
	if !sum.RootReached || sum.Commits != firstParents {
		t.Fatalf("walked %d commits (root %v), git has %d on the first-parent chain", sum.Commits, sum.RootReached, firstParents)
	}
	if sum.Mismatch != 0 || sum.HeadMissing != 0 || sum.Extra != 0 {
		t.Fatalf("summary %+v", sum)
	}
	// Every head file is proven, the binary too: GitHub sends no patch for it,
	// so its blob is fetched by id and proven by its hash.
	headFiles := len(strings.Fields(g.repo.Git("ls-tree", "-r", "--name-only", "main")))
	if sum.HeadFiles != headFiles || sum.HeadExact != headFiles || sum.HeadUnknown != 0 || sum.Fetched < 1 {
		t.Fatalf("head: %d files, %d exact, %d unknown, %d fetched; want %d exact, 0 unknown, the binary fetched",
			sum.HeadFiles, sum.HeadExact, sum.HeadUnknown, sum.Fetched, headFiles)
	}
	// The code the page paints is the rebuilt file itself: each line equals git's
	// content of that file at that commit, at that line number.
	sha, painted, withAdds := "", 0, 0
	for _, e := range rec.ev {
		if e.T == "commit" {
			sha = e.SHA
		}
		if e.T != "file" || len(e.Code) == 0 {
			continue
		}
		want := strings.Split(strings.ReplaceAll(g.repo.Git("show", sha+":"+e.Path), "\t", "  "), "\n")
		for i, l := range e.Code {
			if n := e.At - 1 + i; n >= len(want) || l != want[n] {
				t.Fatalf("%s@%s line %d painted %q, git has %q", e.Path, sha[:8], e.At+i, l, want[min(n, len(want)-1)])
			}
		}
		for _, a := range e.Added {
			if a < 0 || a >= len(e.Code) {
				t.Fatalf("%s: added index %d outside %d lines", e.Path, a, len(e.Code))
			}
		}
		painted++
		if len(e.Added) > 0 {
			withAdds++
		}
	}
	// Each inverted commit that changed text shows the start of a patch. (The
	// generated history has commits that change no file; those show none.)
	withDiff := 0
	for _, e := range rec.of("invert") {
		if len(e.Diff) > 0 {
			withDiff++
			changed := false
			for _, l := range e.Diff {
				changed = changed || l[0] == '+' || l[0] == '-'
			}
			if !changed || !strings.Contains("+- ", e.Diff[0][:1]) || e.Path == "" {
				t.Fatalf("invert %d: diff %q for %q shows no change", e.I, e.Diff, e.Path)
			}
		}
	}
	if painted == 0 || withAdds == 0 || withDiff < len(rec.of("invert"))/2 {
		t.Fatalf("painted %d files (%d with added lines); %d of %d inverted commits show a diff", painted, withAdds, withDiff, len(rec.of("invert")))
	}
	modeOnly := 0
	for _, e := range rec.of("file") {
		if e.Status != "exact" && e.Status != "deleted" {
			t.Fatalf("%s %s: %s", e.Status, e.Path, e.Why)
		}
		if e.Path == "logo.bin" && !e.Fetched {
			t.Fatalf("the binary was proven without its blob: %+v", e)
		}
		if e.Why == "mode change only; content unchanged" {
			modeOnly++
		}
	}
	// GitHub sends a mode-only change with no patch and no blob id; it stays exact.
	if modeOnly != 1 {
		t.Fatalf("%d mode-only changes seen, want 1 (the chmod commit)", modeOnly)
	}
	t.Logf("%d commits, %d file versions: %d exact, %d unknown; head %d/%d exact",
		sum.Commits, sum.FileVersions, sum.Exact, sum.Unknown, sum.HeadExact, sum.HeadFiles)
}

// A history longer than the commit list's pages (100 a page) walks to the root
// across three of them, past the old 200-commit default; a smaller max stops
// at exactly that many commits.
func TestWalkPagesPastTwoHundredCommits(t *testing.T) {
	g := newRig(t, 230, 16)
	firstParents := len(strings.Fields(g.repo.Git("rev-list", "--first-parent", "main")))
	if firstParents <= 200 {
		t.Fatalf("the test history has %d first-parent commits; want more than 200", firstParents)
	}
	sum, err := g.runner.Run(context.Background(), "o", "r", 1000, func(Event) {})
	if err != nil || !sum.RootReached || sum.Commits != firstParents || sum.HeadMissing != 0 || sum.Mismatch != 0 {
		t.Fatalf("%+v %v; want all %d commits to the root", sum, err, firstParents)
	}
	sum, err = g.runner.Run(context.Background(), "o", "r", 210, func(Event) {})
	if err != nil || sum.RootReached || sum.Commits != 210 || sum.Edge != EdgeMaxCommits {
		t.Fatalf("%+v %v; want exactly 210 commits, root not reached, edge %q", sum, err, EdgeMaxCommits)
	}
}

// A period bounds the walk: the commit list is asked only back to Since, so a
// busy repository's long history costs nothing. The rig stamps commit n at hour n.
func TestWalkStopsAtThePeriod(t *testing.T) {
	g := newRig(t, 230, 16)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(200 * time.Hour)
	want := len(strings.Fields(g.repo.Git("rev-list", "--first-parent", "--since="+since.Format(time.RFC3339), "main")))
	var rec recorder
	sum, _, err := g.runner.RunFiles(context.Background(), "o", "r", Window{Max: 1000, Since: since}, rec.emit)
	if err != nil || sum.RootReached || sum.Commits != want || sum.Edge != EdgePeriod {
		t.Fatalf("%+v %v; want the %d commits since %s, edge %q", sum, err, want, since, EdgePeriod)
	}
	if want == 0 || want > 40 {
		t.Fatalf("the window holds %d first-parent commits; the test wants a few", want)
	}
	walks := 0
	for _, e := range rec.ev {
		if e.T == "walk" {
			walks++
		}
		if e.T == "turnstile" && e.Edge != EdgePeriod {
			t.Fatalf("turnstile edge %q", e.Edge)
		}
	}
	if walks != 1 {
		t.Fatalf("%d list pages for a 30-commit window; want 1", walks)
	}

	// A quiet repository: no commit since the period's start. The walk takes the
	// same span back from the last commit instead of nothing.
	rec = recorder{}
	quiet := time.Now().Add(-48 * time.Hour)
	sum, _, err = g.runner.RunFiles(context.Background(), "o", "r", Window{Max: 1000, Since: quiet}, rec.emit)
	if err != nil || sum.Commits == 0 || sum.Commits > 60 || sum.Edge != EdgePeriod {
		t.Fatalf("%+v %v; want the commits of the 2 days before the last commit", sum, err)
	}
	shifts := 0
	for _, e := range rec.ev {
		if e.T == "shift" {
			shifts++
		}
	}
	if shifts != 1 {
		t.Fatalf("%d shift events; want 1", shifts)
	}

	// A call budget stops a run that would spend more.
	g.runner.MaxCalls = 20
	sum, err = g.runner.Run(context.Background(), "o", "r", 1000, func(Event) {})
	if err != nil || sum.Edge != EdgeBudget || sum.Commits == 0 || sum.Commits >= 230 {
		t.Fatalf("%+v %v; want a short run stopped at the call budget", sum, err)
	}
}

// The events follow the pincer: meta, the inverted walk newest first, the
// turnstile, then the forward replay in exactly the reverse order, then done.
func TestEventOrderIsAPincer(t *testing.T) {
	g := newRig(t, 25, 5)
	var rec recorder
	if _, err := g.runner.Run(context.Background(), "o", "r", 1000, rec.emit); err != nil {
		t.Fatal(err)
	}
	if rec.ev[0].T != "meta" || rec.ev[len(rec.ev)-1].T != "done" {
		t.Fatalf("first %q, last %q", rec.ev[0].T, rec.ev[len(rec.ev)-1].T)
	}
	phase := 0 // 0 invert, 1 after turnstile
	for _, e := range rec.ev[1 : len(rec.ev)-1] {
		switch e.T {
		case "invert":
			if phase != 0 {
				t.Fatal("invert after the turnstile")
			}
		case "turnstile":
			phase = 1
		case "commit", "file":
			if phase != 1 {
				t.Fatalf("%s before the turnstile", e.T)
			}
		}
	}
	inv, fwd := rec.of("invert"), rec.of("commit")
	if len(inv) != len(fwd) {
		t.Fatalf("%d inverted, %d replayed", len(inv), len(fwd))
	}
	head := strings.TrimSpace(g.repo.Git("rev-parse", "main"))
	if inv[0].SHA != head || fwd[len(fwd)-1].SHA != head {
		t.Fatal("the walk starts at the head and the replay ends there")
	}
	for i := range inv {
		if inv[i].SHA != fwd[len(fwd)-1-i].SHA {
			t.Fatalf("replay position %d is not the walk reversed", i)
		}
	}
}

// A window shorter than the history leaves the files older than the window
// unproven. It never claims them exact.
func TestPartialWindowNeverClaimsWhatItCannotProve(t *testing.T) {
	g := newRig(t, 40, 8)
	var rec recorder
	sum, err := g.runner.Run(context.Background(), "o", "r", 6, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RootReached || sum.Commits != 6 || sum.Mismatch != 0 {
		t.Fatalf("summary %+v", sum)
	}
	if sum.Unknown == 0 || sum.HeadUnknown+sum.HeadMissing == 0 {
		t.Fatalf("a 6-commit window over 40 commits proved everything? %+v", sum)
	}
	// A file that existed before the window and is modified inside it has a past
	// the walk never saw: it must be unknown, unless the window itself added it.
	oldest := rec.of("commit")[0].SHA
	before := map[string]bool{}
	for _, p := range strings.Fields(g.repo.Git("ls-tree", "-r", "--name-only", oldest+"^")) {
		before[p] = true
	}
	added, seen := map[string]bool{}, map[string]bool{}
	checked := 0
	for _, e := range rec.of("file") {
		if e.Op == "added" {
			added[e.Path] = true
		}
		if e.Op == "modified" && before[e.Path] && !added[e.Path] {
			checked++
			// The reason matters: the strict patch and the blob check would also stop a
			// wrong claim, but this file is unproven because its past was never
			// streamed (first touch), and stays so because that version is unproven.
			want := "an earlier version of it is unproven"
			if !seen[e.Path] {
				want = "its content predates the walked history"
			}
			seen[e.Path] = true
			if e.Status != "unknown" || e.Why != want {
				t.Fatalf("%s predates the window: reported %s (%s), want unknown (%s)", e.Path, e.Status, e.Why, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the window modified no older file, so this test proved nothing")
	}
}

// A tampered patch is caught: the file is reported as not proven, never exact.
func TestTamperedPatchIsCaught(t *testing.T) {
	g := newRig(t, 30, 8)
	target := ""
	g.srv.Tamper = func(sha string, files []map[string]any) {
		for _, f := range files {
			p, _ := f["patch"].(string)
			if target == "" && f["status"] == "modified" && strings.Contains(p, "\n+") {
				target = sha
				f["patch"] = strings.Replace(p, "\n+", "\n+TAMPERED ", 1)
				return
			}
		}
	}
	var rec recorder
	sum, files, err := g.runner.RunFiles(context.Background(), "o", "r", Window{Max: 1000}, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if target == "" {
		t.Fatal("the tamper hook never fired, so this test proved nothing")
	}
	// A file left unproven says why, for the page's file view.
	whys := 0
	for _, f := range files {
		if !f.Known && f.Why != "" {
			whys++
		}
	}
	if whys == 0 {
		t.Fatal("no unproven file carries its reason")
	}
	if sum.Mismatch == 0 {
		t.Fatalf("a tampered patch went unnoticed: %+v", sum)
	}
	if sum.HeadExact == sum.HeadFiles {
		t.Fatal("the head tree check passed with a tampered history")
	}
	// After the mismatch, the file's later versions say why they are unproven.
	tainted := map[string]bool{}
	for _, e := range rec.of("file") {
		if e.Status == "mismatch" {
			tainted[e.Path] = true
		} else if tainted[e.Path] && e.Op == "modified" && e.Status == "unknown" && e.Why != "an earlier version of it is unproven" {
			t.Fatalf("%s: unproven after a mismatch, but the reason given is %q", e.Path, e.Why)
		}
	}
}

// GitHub sends no patch for a very large diff. The version is proven by its
// blob, fetched by id, and the file's later versions build on it: one withheld
// patch left every later version unproven (live, tensorwire/mongoose).
func TestWithheldPatchIsFetchedAndHistoryContinues(t *testing.T) {
	g := newRig(t, 40, 8)
	// An early change to README.md, one that later changes build on (git lists
	// newest first; the last entry is the file's creation).
	changes := strings.Fields(g.repo.Git("log", "--first-parent", "--format=%H", "main", "--", "README.md"))
	if len(changes) < 4 {
		t.Fatalf("README.md changes %d times; the test needs later ones", len(changes))
	}
	target, stripped := changes[len(changes)-2], ""
	g.srv.Tamper = func(sha string, files []map[string]any) {
		for _, f := range files {
			if sha == target && f["filename"] == "README.md" && f["status"] == "modified" {
				stripped = "README.md"
				delete(f, "patch")
			}
		}
	}
	var rec recorder
	sum, err := g.runner.Run(context.Background(), "o", "r", 1000, rec.emit)
	if err != nil || stripped == "" {
		t.Fatalf("%v; stripped %q", err, stripped)
	}
	after, fetched := 0, false
	for _, e := range rec.of("file") {
		if e.Status != "exact" && e.Status != "deleted" {
			t.Fatalf("%s %s: %s", e.Status, e.Path, e.Why)
		}
		if e.Path == stripped && e.Fetched {
			fetched = true
		} else if e.Path == stripped && fetched {
			after++
		}
	}
	if !fetched || after == 0 || sum.HeadExact != sum.HeadFiles || g.srv.BlobCalls.Load() == 0 {
		t.Fatalf("fetched %v, head %d of %d exact, %d blob calls", fetched, sum.HeadExact, sum.HeadFiles, g.srv.BlobCalls.Load())
	}
	t.Logf("%s: patch withheld, blob fetched, %d later versions proven on top of it", stripped, after)
}

// Detail requests run in chunks no bigger than the rate-limit chunk, and a second
// run is served from the database without touching the commit-detail endpoint.
func TestChunkingAndCache(t *testing.T) {
	g := newRig(t, 30, 3)
	var rec recorder
	first, err := g.runner.Run(context.Background(), "o", "r", 1000, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.srv.MaxSeen.Load(); got > 3 {
		t.Fatalf("%d requests in flight at once, chunk is 3", got)
	}
	if first.CacheHits != 0 {
		t.Fatalf("first run cache hits = %d", first.CacheHits)
	}
	calls := g.srv.Calls.Load()
	second, err := g.runner.Run(context.Background(), "o", "r", 1000, (&recorder{}).emit)
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHits != second.Commits+second.Fetched {
		t.Fatalf("second run: %d from cache, want all %d commits and %d blobs", second.CacheHits, second.Commits, second.Fetched)
	}
	if extra := g.srv.Calls.Load() - calls; extra > 4 { // repo, branch, list, tree
		t.Fatalf("second run made %d upstream calls", extra)
	}
	if second.HeadExact != first.HeadExact {
		t.Fatal("cached run proved a different tree")
	}
}

func TestUnknownRepoFails(t *testing.T) {
	g := newRig(t, 3, 3)
	if _, err := g.runner.Run(context.Background(), "nobody", "here", 10, (&recorder{}).emit); err == nil {
		t.Fatal("want an error for an unknown repo")
	}
}

// Sessions keep their events: a viewer who joins after the run replays it all,
// and the manager refuses runs past its limit.
func TestSessionsReplayAndLimit(t *testing.T) {
	g := newRig(t, 15, 5)
	m := &Manager{Runner: g.runner, MaxLive: 1, Keep: time.Minute, Timeout: time.Minute}
	s, err := m.Start("o", "r", Window{Max: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("o", "r", Window{Max: 1000}); err != ErrBusy {
		t.Fatalf("second start = %v, want ErrBusy", err)
	}
	var live []Event
	from := 0
	for {
		evs, done, wait := s.Since(from)
		live = append(live, evs...)
		from += len(evs)
		if done {
			break
		}
		select {
		case <-wait:
		case <-time.After(30 * time.Second):
			t.Fatal("session stalled")
		}
	}
	late, done, _ := s.Since(0)
	if !done || len(late) != len(live) || late[len(late)-1].T != "done" {
		t.Fatalf("late joiner saw %d events, live saw %d", len(late), len(live))
	}
	if st := s.Status(); st.State != "done" || st.Summary == nil {
		t.Fatalf("status %+v", st)
	}
	if got, ok := m.Get(s.ID); !ok || got != s {
		t.Fatal("Get lost the session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := m.Start("o", "r", Window{Max: 5}); err == nil {
			// Cancel stops a run: it ends failed, and its slot frees.
			c.Cancel()
			for c.Status().State == "running" {
				if time.Now().After(deadline) {
					t.Fatal("a cancelled run kept running")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if st := c.Status(); st.State != "failed" || !strings.Contains(st.Error, "canceled") {
				t.Fatalf("cancelled run: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never freed after the run ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
