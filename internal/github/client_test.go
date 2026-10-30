package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRepo(t *testing.T) {
	good := map[string][2]string{
		"golang/go":                        {"golang", "go"},
		" rsc/quote ":                      {"rsc", "quote"},
		"https://github.com/rsc/quote":     {"rsc", "quote"},
		"https://github.com/rsc/quote.git": {"rsc", "quote"},
		"a-b/c.d_e-f":                      {"a-b", "c.d_e-f"},
	}
	for in, want := range good {
		o, n, err := ParseRepo(in)
		if err != nil || o != want[0] || n != want[1] {
			t.Errorf("ParseRepo(%q) = %q %q %v", in, o, n, err)
		}
	}
	// Each of these would steer a request somewhere else, or is malformed.
	bad := []string{"", "golang", "golang/", "/go", "a/b/c", "a/..", "a/.", "../etc/passwd",
		"evil.com/x", "-a/b", "a b/c", "a/b?x=1", "a/b#f", "a/%2e%2e", "http://169.254.169.254/x",
		strings.Repeat("a", 40) + "/b", "a/" + strings.Repeat("b", 101)}
	for _, in := range bad {
		if o, n, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) accepted as %q/%q", in, o, n)
		}
	}
}

type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (f *fakeClock) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept = append(f.slept, d)
	f.t = f.t.Add(d)
	return nil
}

func hdr(limit, remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// The chunk fills one double-buffer half when the budget allows, shrinks to the
// budget when it is smaller, and probes with one request while the budget is unknown.
func TestLimiterChunkFollowsBudgetAndBufferCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter(5)
	l.now, l.sleep = clk.now, clk.sleep
	if got := l.Chunk(100); got != 1 {
		t.Fatalf("unknown budget: chunk = %d, want 1", got)
	}
	ctx := context.Background()
	l.Acquire(ctx)
	l.Release(hdr(5000, 4000, clk.t.Add(time.Hour)))
	if got := l.Chunk(100); got != 100 {
		t.Fatalf("big budget: chunk = %d, want the buffer cap 100", got)
	}
	l.Acquire(ctx)
	l.Release(hdr(5000, 50, clk.t.Add(time.Hour)))
	if got := l.Chunk(100); got != 45 {
		t.Fatalf("budget 50 less reserve 5: chunk = %d, want 45", got)
	}
	// Requests in flight count against the budget.
	for range 10 {
		l.Acquire(ctx)
	}
	if got := l.Chunk(100); got != 35 {
		t.Fatalf("10 in flight: chunk = %d, want 35", got)
	}
	for range 10 {
		l.Release(nil)
	}
	l.Acquire(ctx)
	l.Release(hdr(5000, 5, clk.t.Add(time.Hour)))
	if got := l.Chunk(100); got != 1 {
		t.Fatalf("spent budget: chunk = %d, want 1", got)
	}
}

// With the budget at the reserve, Acquire parks until the window resets.
func TestLimiterParksUntilReset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter(5)
	l.now, l.sleep = clk.now, clk.sleep
	reset := clk.t.Add(10 * time.Minute)
	l.Acquire(context.Background())
	l.Release(hdr(60, 5, reset))
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(clk.slept) != 1 || clk.slept[0] != 10*time.Minute+time.Second {
		t.Fatalf("slept %v, want one wait of 10m1s", clk.slept)
	}
	if clk.now().Before(reset) {
		t.Fatal("Acquire returned before the reset")
	}
}

func TestLimiterAcquireHonoursCancel(t *testing.T) {
	l := NewLimiter(5)
	l.Acquire(context.Background())
	l.Release(hdr(60, 0, time.Now().Add(time.Hour)))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want deadline exceeded", err)
	}
}

// Out-of-order responses: a stale higher count from the same window leaves the budget alone.
func TestLimiterKeepsLowestCountInWindow(t *testing.T) {
	l := NewLimiter(0)
	reset := time.Now().Add(time.Hour)
	l.Acquire(context.Background())
	l.Release(hdr(5000, 100, reset))
	l.Acquire(context.Background())
	l.Release(hdr(5000, 120, reset))
	if got := l.Snapshot().Remaining; got != 100 {
		t.Fatalf("remaining = %d, want 100", got)
	}
}

func newTestClient(t *testing.T, h http.Handler, token string, logs *bytes.Buffer) (*Client, *fakeClock) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var lg *slog.Logger
	if logs != nil {
		lg = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	c := New(Options{BaseURL: srv.URL, Token: token, Logger: lg})
	clk := &fakeClock{t: time.Now()}
	c.sleep = clk.sleep
	return c, clk
}

const secret = "ghp_THIS_MUST_NEVER_BE_LOGGED_0123456789"

// The token goes in the Authorization header and nowhere else: not in a URL, not in
// a log line, not in an error.
func TestTokenIsSentButNeverLogged(t *testing.T) {
	var calls atomic.Int32
	var logs bytes.Buffer
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if strings.Contains(r.URL.String(), secret) {
			t.Error("token in URL")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // forces a logged retry
			return
		}
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, `{"message":"short and stout"}`)
	}), secret, &logs)
	_, err := c.Repo(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("token leaked: err=%q logs=%q", err, logs.String())
	}
	if !strings.Contains(logs.String(), "github retry") {
		t.Fatal("the retry left no log line, so the log check proved nothing")
	}
}

// A repeat GET sends If-None-Match and reuses the cached body on 304, which GitHub
// does not charge against the rate limit.
func TestConditionalRequestReusesBody(t *testing.T) {
	var got304 atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			got304.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`)
	}), "", nil)
	for range 2 {
		r, err := c.Repo(context.Background(), "o", "r")
		if err != nil || r.DefaultBranch != "main" {
			t.Fatalf("Repo = %+v, %v", r, err)
		}
	}
	if got304.Load() != 1 {
		t.Fatalf("304s = %d, want 1", got304.Load())
	}
}

// A 304 omits the Link header; a repeat page walk must still see "next".
// Regression: found live on google/uuid, where a repeat reconstruction stopped early.
func TestConditionalRequestKeepsPageLinks(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"p1"` {
			w.WriteHeader(http.StatusNotModified) // no Link, as GitHub sends it
			return
		}
		w.Header().Set("ETag", `"p1"`)
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		fmt.Fprint(w, `[]`)
	}), "", nil)
	for i := range 2 {
		_, more, err := c.ListCommits(context.Background(), "o", "r", ListOptions{})
		if err != nil || !more {
			t.Fatalf("call %d: more=%v err=%v", i+1, more, err)
		}
	}
}

// A secondary rate limit (403 + Retry-After) is waited out and retried.
func TestSecondaryLimitRetryAfter(t *testing.T) {
	var calls atomic.Int32
	c, clk := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
			return
		}
		fmt.Fprint(w, `{"default_branch":"main"}`)
	}), "", nil)
	if _, err := c.Repo(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if len(clk.slept) != 1 || clk.slept[0] != 7*time.Second {
		t.Fatalf("slept %v, want [7s]", clk.slept)
	}
}

// A primary limit hit (403, remaining 0) parks in the limiter until the reset, then succeeds.
func TestPrimaryLimitParksUntilReset(t *testing.T) {
	var calls atomic.Int32
	reset := time.Now().Add(90 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			for k, v := range hdr(60, 0, reset) {
				w.Header()[k] = v
			}
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"default_branch":"main"}`)
	}))
	defer srv.Close()
	c := New(Options{BaseURL: srv.URL})
	clk := &fakeClock{t: time.Now()}
	c.sleep, c.Core.sleep, c.Core.now = clk.sleep, clk.sleep, clk.now
	if _, err := c.Repo(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	var parked time.Duration
	for _, d := range clk.slept {
		parked += d
	}
	if parked < 80*time.Second {
		t.Fatalf("waited %v in total, want about 91s (until the reset)", parked)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestStatusErrors(t *testing.T) {
	cases := []struct {
		code int
		body string
		want error
	}{
		{404, `{"message":"Not Found"}`, ErrNotFound},
		{409, `{"message":"Git Repository is empty."}`, ErrEmptyRepo},
		{401, `{"message":"Bad credentials"}`, ErrUnauthorized},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			fmt.Fprint(w, tc.body)
		}), "", nil)
		if _, err := c.Repo(context.Background(), "o", "r"); !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: err = %v, want %v", tc.code, err, tc.want)
		}
	}
}

func TestListCommitsQueryAndPagination(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/o/r/commits" || q.Get("per_page") != "100" || q.Get("since") != "2026-01-01T00:00:00Z" || q.Get("sha") != "main" {
			t.Errorf("bad request %s", r.URL)
		}
		if q.Get("page") == "" {
			w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		}
		fmt.Fprint(w, `[{"sha":"`+strings.Repeat("a", 40)+`","parents":[{"sha":"`+strings.Repeat("b", 40)+`"}],"author":{"login":"ann"},"commit":{"author":{"name":"Ann","date":"2026-02-01T00:00:00Z"}}}]`)
	}), "", nil)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs, more, err := c.ListCommits(context.Background(), "o", "r", ListOptions{SHA: "main", Since: since})
	if err != nil || !more || len(cs) != 1 || cs[0].Login() != "ann" || cs[0].ParentSHAs()[0] != strings.Repeat("b", 40) {
		t.Fatalf("page 1: %+v more=%v err=%v", cs, more, err)
	}
	_, more, err = c.ListCommits(context.Background(), "o", "r", ListOptions{SHA: "main", Since: since, Page: 2})
	if err != nil || more {
		t.Fatalf("page 2: more=%v err=%v", more, err)
	}
}

// A 650-file commit arrives as pages of 300, 300 and 50; Commit returns all of them.
func TestCommitReadsEveryFilePage(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		n := map[int]int{1: 300, 2: 300, 3: 50}[page]
		files := make([]File, n)
		for i := range files {
			files[i] = File{Filename: fmt.Sprintf("p%d/f%d", page, i)}
		}
		json.NewEncoder(w).Encode(map[string]any{"sha": strings.Repeat("a", 40), "files": files})
	}), "", nil)
	got, err := c.Commit(context.Background(), "o", "r", strings.Repeat("a", 40))
	if err != nil || len(got.Files) != 650 || got.Files[649].Filename != "p3/f49" {
		t.Fatalf("%d files, %v", len(got.Files), err)
	}
}

func TestBranchHead(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/o/r/branches/feat%2Fx" {
			t.Errorf("path %s", r.URL.EscapedPath())
		}
		fmt.Fprint(w, `{"commit":{"sha":"`+strings.Repeat("c", 40)+`"}}`)
	}), "", nil)
	sha, err := c.BranchHead(context.Background(), "o", "r", "feat/x")
	if err != nil || sha != strings.Repeat("c", 40) {
		t.Fatalf("%q %v", sha, err)
	}
}

func TestCommitRejectsBadSHA(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1"})
	if _, err := c.Commit(context.Background(), "o", "r", "../../x"); err == nil {
		t.Fatal("accepted a non-hex commit id")
	}
}

func TestHistoryGraphQL(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || req.Variables["owner"] != "o" || req.Variables["first"] != float64(100) {
			t.Errorf("bad request %s %s %v", r.Method, r.URL.Path, req.Variables)
		}
		if req.Variables["cursor"] == "c1" {
			fmt.Fprint(w, `{"data":{"repository":{"defaultBranchRef":{"target":{"history":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"repository":{"defaultBranchRef":{"target":{"history":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},
			"nodes":[{"oid":"x","additions":3,"deletions":1,"author":{"name":"Ann","user":{"login":"ann"}}}]}}}}}}`)
	}), "tok", nil)
	now := time.Now()
	nodes, next, err := c.History(context.Background(), "o", "r", now.Add(-time.Hour), now, 500, "")
	if err != nil || next != "c1" || len(nodes) != 1 || nodes[0].Additions != 3 || nodes[0].Author.User.Login != "ann" {
		t.Fatalf("page 1: %+v %q %v", nodes, next, err)
	}
	nodes, next, err = c.History(context.Background(), "o", "r", now.Add(-time.Hour), now, 100, "c1")
	if err != nil || next != "" || len(nodes) != 0 {
		t.Fatalf("page 2: %+v %q %v", nodes, next, err)
	}

	anon := New(Options{BaseURL: "http://127.0.0.1:1"})
	if _, _, err := anon.History(context.Background(), "o", "r", now, now, 1, ""); !errors.Is(err, ErrNeedsToken) {
		t.Fatalf("anonymous History = %v, want ErrNeedsToken", err)
	}
}

func TestHistoryNotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Repository missing"}]}`)
	}), "tok", nil)
	if _, _, err := c.History(context.Background(), "o", "r", time.Now(), time.Now(), 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
