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
	headFiles := len(strings.Fields(g.repo.Git("ls-tree", "-r", "--name-only", "main")))
	if sum.HeadFiles != headFiles || sum.HeadExact != headFiles-1 || sum.HeadUnknown != 1 {
		t.Fatalf("head: %d files, %d exact, %d unknown; want %d, %d, 1 (the binary)", sum.HeadFiles, sum.HeadExact, sum.HeadUnknown, headFiles, headFiles-1)
	}
	modeOnly := 0
	for _, e := range rec.of("file") {
		if e.Status == "unknown" && e.Path != "logo.bin" {
			t.Fatalf("unproven %s: %s", e.Path, e.Why)
		}
		if e.Path == "logo.bin" && e.Status != "unknown" {
			t.Fatalf("binary reported %s", e.Status)
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
	sum, err := g.runner.Run(context.Background(), "o", "r", 1000, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	if target == "" {
		t.Fatal("the tamper hook never fired, so this test proved nothing")
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
	if second.CacheHits != second.Commits {
		t.Fatalf("second run: %d of %d from cache", second.CacheHits, second.Commits)
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
	s, err := m.Start("o", "r", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("o", "r", 1000); err != ErrBusy {
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
		if _, err := m.Start("o", "r", 5); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never freed after the run ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
