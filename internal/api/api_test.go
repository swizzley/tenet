package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/dbuf"
	"github.com/swizzley/tenet/internal/github"
	"github.com/swizzley/tenet/internal/narrate"
	"github.com/swizzley/tenet/internal/reconstruct"
	"github.com/swizzley/tenet/internal/store"
	"github.com/swizzley/tenet/internal/testgit"
	"github.com/swizzley/tenet/web"
)

const (
	ghToken  = "ghp_api_test_secret_never_shown"
	llmKey   = "sk-ant-api-test-secret-never-shown"
	period   = "since=2025-12-31&until=2026-01-04"
	contribs = "/api/v1/repos/o/r/insights/contributors?"
)

// fakeLLM is a stand-in Anthropic endpoint that answers with queued texts.
type fakeLLM struct {
	mu      sync.Mutex
	answers []string
	calls   int
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	io.Copy(io.Discard, r.Body)
	text := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	tb, _ := json.Marshal(text)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, tb)
}

type env struct {
	t    *testing.T
	repo *testgit.Repo
	gh   *testgit.Server
	llm  *fakeLLM
	url  string
	logs *bytes.Buffer
	sink *dbuf.Writer[collect.Record]
}

type syncBuf struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// newEnv runs the whole service in process: real store, double buffer, collector,
// GitHub client, reconstruction and narrator, against a git-backed fake GitHub and
// a fake Anthropic endpoint.
func newEnv(t *testing.T) *env {
	t.Helper()
	repo := testgit.New(t, 60)
	ghSrv := &testgit.Server{R: repo}
	ghHTTP := httptest.NewServer(ghSrv)
	t.Cleanup(ghHTTP.Close)
	llm := &fakeLLM{}
	llmHTTP := httptest.NewServer(llm)
	t.Cleanup(llmHTTP.Close)

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	sink := dbuf.New(db.UpsertCommits, 25)
	t.Cleanup(func() { sink.Close(); db.Close() })
	gh := github.New(github.Options{BaseURL: ghHTTP.URL, Token: ghToken, Logger: log})
	srv := &Server{
		Svc: &Service{Store: db, Collector: &collect.Collector{GH: gh, Sink: sink, MaxCommits: 10_000},
			Narrator: narrate.New(llmKey, "claude-opus-5-5", option.WithBaseURL(llmHTTP.URL), option.WithMaxRetries(0)), TTL: time.Minute},
		Recon: &reconstruct.Manager{Runner: &reconstruct.Runner{GH: gh, Cache: db, Chunk: gh.Core.Chunk, BufferCap: 25},
			MaxLive: 1, Keep: time.Minute, Timeout: time.Minute},
		GH: gh, Sink: sink, Static: web.FS(), Log: log,
		MaxPeriod: 366 * 24 * time.Hour, MaxReconstruct: 500,
		Now: func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) },
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &env{t, repo, ghSrv, llm, hs.URL, &logs.Buffer, sink}
}

func (e *env) get(path string, out any) *http.Response {
	e.t.Helper()
	r, err := http.Get(e.url + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), ghToken) || strings.Contains(string(b), llmKey) {
		e.t.Fatalf("a credential leaked into a response: %s", b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s: %v: %s", path, err, b)
		}
	}
	return r
}

// The insight is computed from what GitHub served and the totals agree with an
// independent count straight from git.
func TestContributorsAddUpAgainstGit(t *testing.T) {
	e := newEnv(t)
	var got ContributorsResponse
	if r := e.get(contribs+period+"&limit=100", &got); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	// The source of truth, independently: every commit on main in the period.
	log := e.repo.Git("log", "--format=@%aN", "--numstat", "--since=2025-12-31", "--until=2026-01-04", "main")
	commits, lines := 0, 0
	perAuthor := map[string]int{}
	author := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, "@") {
			commits++
			author = l[1:]
			perAuthor[author]++
		} else if f := strings.Fields(l); len(f) >= 2 {
			a, _ := strconv.Atoi(f[0])
			d, _ := strconv.Atoi(f[1])
			lines += a + d
		}
	}
	if got.TotalCommits != commits || got.TotalLines != lines {
		t.Fatalf("API says %d commits / %d lines; git says %d / %d", got.TotalCommits, got.TotalLines, commits, lines)
	}
	sum := 0
	for _, c := range got.Contributors {
		sum += c.Commits
		want := perAuthor[c.Name]
		if c.Commits != want {
			t.Errorf("%s: API %d commits, git %d", c.ID, c.Commits, want)
		}
		if (c.Name == "Cy") != strings.HasPrefix(c.ID, "~") {
			t.Errorf("%s: unmapped authors are ~name, mapped ones are their login", c.ID)
		}
	}
	if sum != commits || got.ContributorsTotal != len(perAuthor) || got.Source != "github" || got.Metric == "" {
		t.Fatalf("sum %d, contributors %d, source %q", sum, got.ContributorsTotal, got.Source)
	}
	if got.Collection == nil || got.Collection.Pages != (commits+24)/25 {
		t.Fatalf("collection %+v: want one page per 25-record buffer", got.Collection)
	}
}

// A repeat question is answered from SQLite: zero GraphQL calls, same numbers.
func TestRepeatQueryServedFromDatabase(t *testing.T) {
	e := newEnv(t)
	var first, second ContributorsResponse
	e.get(contribs+period, &first)
	calls := e.gh.GraphQLCalls.Load()
	e.get(contribs+period+"&by=lines", &second)
	if e.gh.GraphQLCalls.Load() != calls {
		t.Fatal("the repeat query went back to GitHub")
	}
	if second.Source != "database" || second.TotalCommits != first.TotalCommits || second.By != "lines" {
		t.Fatalf("second: source %q, %d commits, by %q", second.Source, second.TotalCommits, second.By)
	}
	// A narrower period inside the stored one is also served from disk.
	var inner ContributorsResponse
	e.get(contribs+"since=2026-01-01&until=2026-01-02", &inner)
	if inner.Source != "database" || e.gh.GraphQLCalls.Load() != calls || inner.TotalCommits == 0 || inner.TotalCommits >= first.TotalCommits {
		t.Fatalf("inner: source %q, %d commits", inner.Source, inner.TotalCommits)
	}
	var st struct {
		Writer dbuf.Stats `json:"writer"`
	}
	e.get("/api/v1/status", &st)
	if st.Writer.Records != int64(first.TotalCommits) || st.Writer.FailedBatches != 0 {
		t.Fatalf("writer %+v", st.Writer)
	}
}

// Concurrent identical requests share one upstream fetch.
func TestConcurrentRequestsShareOneFetch(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := http.Get(e.url + contribs + period)
			if err != nil || r.StatusCode != 200 {
				bad.Add(1)
			}
			if r != nil {
				r.Body.Close()
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d requests failed", bad.Load())
	}
	var cnt ContributorsResponse
	e.get(contribs+period, &cnt)
	pages := int64((cnt.TotalCommits + 24) / 25)
	if got := e.gh.GraphQLCalls.Load(); got != pages {
		t.Fatalf("%d GraphQL calls for 8 identical requests, want %d (one fetch)", got, pages)
	}
}

func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	cases := map[string]int{
		contribs + "since=2026-02-01&until=2026-01-01":              400, // backwards
		contribs + "since=2020-01-01&until=2026-01-01":              400, // longer than a year
		contribs + "since=yesterday":                                400,
		contribs + "since=2099-01-01&until=2099-02-01":              400, // future
		contribs + "since=1990-01-01&until=1990-02-01":              400, // before git
		contribs + period + "&by=reviews":                           400,
		contribs + period + "&limit=0":                              400,
		contribs + period + "&limit=1000":                           400,
		"/api/v1/repos/-bad/r/insights/contributors?" + period:      400,
		"/api/v1/repos/o/r%2F..%2F..%2Fx/insights/contributors":     400, // path traversal
		"/api/v1/repos/nobody/here/insights/contributors?" + period: 404,
	}
	for path, want := range cases {
		var body struct {
			Error apiError `json:"error"`
		}
		r := e.get(path, &body)
		if r.StatusCode != want || body.Error.Code == "" || body.Error.Message == "" {
			t.Errorf("%s: %d %+v, want %d with an error body", path, r.StatusCode, body.Error, want)
		}
	}
	r, _ := http.Post(e.url+contribs+period, "application/json", nil)
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST on a GET route: %d", r.StatusCode)
	}
}

const goodNarrative = `{"summary":"Ann carried the period.","hypothesis":"Ann is the main maintainer.","confidence":0.6,
"evidence":[{"claim":"total","metric":"total_commits","value":%d},{"claim":"top share","metric":"contributors[0].commit_share","value":%v}],"caveats":["main only"]}`

// The narrative endpoint returns the narrative with its evidence checked, plus the
// exact document the evidence paths point into.
func TestNarrativeEvidencePointsIntoData(t *testing.T) {
	e := newEnv(t)
	var c ContributorsResponse
	e.get(contribs+period, &c)
	e.llm.answers = []string{fmt.Sprintf(goodNarrative, c.TotalCommits, c.Contributors[0].CommitShare)}
	var got NarrativeResponse
	if r := e.get("/api/v1/repos/o/r/insights/narrative?"+period, &got); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if !got.Narrative.Validated || len(got.Narrative.Evidence) != 2 {
		t.Fatalf("%+v", got.Narrative)
	}
	for _, ev := range got.Narrative.Evidence {
		v, err := narrate.Resolve(got.Data, ev.Metric)
		if err != nil || v != ev.Value {
			t.Fatalf("evidence %s: data holds %v (%v), narrative says %v", ev.Metric, v, err, ev.Value)
		}
	}
	// Cached for the TTL: the second call costs nothing.
	calls := e.llm.calls
	e.get("/api/v1/repos/o/r/insights/narrative?"+period, &got)
	if !got.Cached || e.llm.calls != calls {
		t.Fatal("the repeat narrative called the LLM again")
	}
}

func TestNarrativeWithFabricatedNumbersIsWithheld(t *testing.T) {
	e := newEnv(t)
	e.llm.answers = []string{`{"summary":"x","hypothesis":"y","confidence":0.99,"evidence":[{"claim":"z","metric":"total_commits","value":123456},{"claim":"w","metric":"reviews","value":3}],"caveats":[]}`}
	var body struct {
		Error apiError `json:"error"`
	}
	r := e.get("/api/v1/repos/o/r/insights/narrative?"+period, &body)
	if r.StatusCode != 502 || body.Error.Code != "narrative_unsupported" || body.Error.Details == nil {
		t.Fatalf("%d %+v", r.StatusCode, body.Error)
	}
}

// The API query triggers the reconstruction; the WebSocket streams it to the end.
func TestReconstructionOverWebSocket(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.url+"/api/v1/reconstructions", "application/json", strings.NewReader(`{"repo":"o/r","max_commits":500}`))
	if err != nil {
		t.Fatal(err)
	}
	var start map[string]string
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if resp.StatusCode != 202 || resp.Header.Get("Location") != start["status"] {
		t.Fatalf("%d %v", resp.StatusCode, start)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.url, "http")+start["events"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var events []reconstruct.Event
	for {
		var ev reconstruct.Event
		if err := wsjson.Read(ctx, c, &ev); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("stream ended with %v after %d events", err, len(events))
			}
			break
		}
		events = append(events, ev)
	}
	last := events[len(events)-1]
	if last.T != "done" || last.Summary == nil || !last.Summary.RootReached || last.Summary.Mismatch != 0 {
		t.Fatalf("last event %+v", last)
	}
	s := last.Summary
	if s.HeadExact+s.HeadUnknown != s.HeadFiles || s.HeadMissing != 0 || s.HeadUnknown != 1 {
		t.Fatalf("summary %+v: every head file rebuilt, only the binary unproven", s)
	}
	var st reconstruct.Status
	e.get(start["status"], &st)
	if st.State != "done" || st.Events != len(events) {
		t.Fatalf("status %+v, streamed %d", st, len(events))
	}
	// Resume: ?from=N replays from event N.
	c2, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.url, "http")+start["events"]+"?from="+strconv.Itoa(len(events)-1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	var again reconstruct.Event
	if err := wsjson.Read(ctx, c2, &again); err != nil || again.T != "done" {
		t.Fatalf("resume read %+v %v", again, err)
	}
}

func TestReconstructionRequestValidation(t *testing.T) {
	e := newEnv(t)
	for body, want := range map[string]int{
		`{"repo":"o/r","max_commits":501}`:               400,
		`{"repo":"o/r","max_commits":-1}`:                400,
		`{"repo":"../../etc"}`:                           400,
		`{"repo":"o/r","extra":1}`:                       400,
		`not json`:                                       400,
		`{"repo":"` + strings.Repeat("a", 8000) + `/b"}`: 400,
	} {
		r, err := http.Post(e.url+"/api/v1/reconstructions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("%.40s: %d, want %d", body, r.StatusCode, want)
		}
	}
	if r := e.get("/api/v1/reconstructions/deadbeef", nil); r.StatusCode != 404 {
		t.Errorf("unknown id: %d", r.StatusCode)
	}
	// MaxLive is 1: a second run while the first is live is refused with 429.
	r1, _ := http.Post(e.url+"/api/v1/reconstructions", "application/json", strings.NewReader(`{"repo":"o/r"}`))
	r2, _ := http.Post(e.url+"/api/v1/reconstructions", "application/json", strings.NewReader(`{"repo":"o/r"}`))
	r1.Body.Close()
	r2.Body.Close()
	if r1.StatusCode != 202 || r2.StatusCode != 429 || r2.Header.Get("Retry-After") == "" {
		t.Errorf("busy: %d then %d", r1.StatusCode, r2.StatusCode)
	}
}

// Another site cannot open the event stream from a viewer's browser.
func TestWebSocketRefusesForeignOrigin(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Post(e.url+"/api/v1/reconstructions", "application/json", strings.NewReader(`{"repo":"o/r","max_commits":3}`))
	var start map[string]string
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, r, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.url, "http")+start["events"],
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || r == nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err=%v status=%v", err, r)
	}
}

func TestPageAndSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	r, err := http.Get(e.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), `id="query"`) || !strings.Contains(string(b), "app.js") {
		t.Fatalf("index: %d", r.StatusCode)
	}
	h := r.Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers %v", h)
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		if r := e.get(f, nil); r.StatusCode != 200 {
			t.Errorf("%s: %d", f, r.StatusCode)
		}
	}
	if r := e.get("/healthz", nil); r.StatusCode != 200 {
		t.Errorf("healthz %d", r.StatusCode)
	}
}

// Neither credential appears in any log line, across every kind of request.
func TestLogsCarryNoCredentials(t *testing.T) {
	e := newEnv(t)
	e.llm.answers = []string{"not json", "still not json"}
	e.get(contribs+period, nil)
	e.get("/api/v1/repos/o/r/insights/narrative?"+period, nil)
	e.get("/api/v1/repos/nobody/here/insights/contributors?"+period, nil)
	logs := e.logs.String()
	if logs == "" {
		t.Fatal("no logs written, so the check proved nothing")
	}
	if strings.Contains(logs, ghToken) || strings.Contains(logs, llmKey) {
		t.Fatal("a credential is in the logs")
	}
}
