// Package api is the HTTP surface: the REST endpoints, the reconstruction event
// stream (a WebSocket), and the page.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/dbuf"
	"github.com/swizzley/tenet/internal/github"
	"github.com/swizzley/tenet/internal/narrate"
	"github.com/swizzley/tenet/internal/reconstruct"
)

// Server wires the handlers.
type Server struct {
	Svc            *Service
	Recon          *reconstruct.Manager
	GH             *github.Client
	Sink           *dbuf.Writer[collect.Record]
	Static         fs.FS
	Log            *slog.Logger
	MaxPeriod      time.Duration
	MaxReconstruct int
	Now            func() time.Time
}

// Handler returns the routed, wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/insights/contributors", s.contributors)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/insights/narrative", s.narrative)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/insights/narrative/prompt", s.narrativePrompt)
	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/insights/narrative/verify", s.verifyNarrative)
	mux.HandleFunc("POST /api/v1/reconstructions", s.startReconstruction)
	mux.HandleFunc("GET /api/v1/reconstructions/{id}", s.reconstructionStatus)
	mux.HandleFunc("GET /api/v1/reconstructions/{id}/events", s.reconstructionEvents)
	mux.HandleFunc("GET /api/v1/reconstructions/{id}/file", s.reconstructionFile)
	mux.HandleFunc("GET /api/v1/reconstructions/{id}/diff", s.reconstructionDiff)
	mux.Handle("GET /", http.FileServerFS(s.Static))
	return s.wrap(mux)
}

// apiError is every error body: a stable code and a message for people.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// writeJSON encodes before it writes the status, so a value that fails to encode
// (a NaN) becomes a 500 with a body, never a status with an empty body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = 500
		b, _ = json.Marshal(map[string]apiError{"error": {Code: "internal", Message: "the response failed to encode"}})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func fail(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, map[string]apiError{"error": {code, msg, details}})
}

// upstream maps a service error to an HTTP answer. Messages never carry secrets:
// they are built from status codes and our own text.
func (s *Server) upstream(w http.ResponseWriter, r *http.Request, err error) {
	var se *github.StatusError
	var ue *narrate.UnsupportedError
	switch {
	case errors.Is(err, ErrNoServerNarrator):
		fail(w, 501, "narrative_in_browser", "no anthropic_api_key is configured, so the server writes no narrative. "+
			"The page writes it in the browser: GET .../insights/narrative/prompt gives the model's input, "+
			"POST .../insights/narrative/verify checks its answer against the data", nil)
	case errors.Is(err, github.ErrNotFound):
		fail(w, 404, "repo_not_found", "unknown repository: GitHub answered 404 (private repositories are out of scope)", nil)
	case errors.Is(err, github.ErrEmptyRepo):
		fail(w, 422, "repo_empty", "the repository is empty", nil)
	case errors.Is(err, github.ErrUnauthorized):
		fail(w, 502, "upstream_auth", "GitHub rejected the service's token", nil)
	case errors.As(err, &se) && (se.Code == 403 || se.Code == 429):
		w.Header().Set("Retry-After", "60")
		fail(w, 503, "upstream_rate_limited", "GitHub is rate limiting this service; try again later", nil)
	case errors.As(err, &ue):
		fail(w, 502, "narrative_unsupported", "the model's narrative twice cited numbers absent from the data, so it is withheld", map[string]any{"problems": ue.Problems, "rejected": ue.Rejected})
	case errors.Is(err, narrate.ErrRefused):
		fail(w, 422, "narrative_refused", "the model declined this request", nil)
	case errors.Is(err, narrate.ErrLLMBusy):
		w.Header().Set("Retry-After", "30")
		fail(w, 503, "llm_rate_limited", "the LLM provider is rate limiting; try again later", nil)
	case errors.Is(err, narrate.ErrLLMAuth):
		fail(w, 502, "llm_auth", "the LLM provider rejected the service's API key", nil)
	case errors.Is(err, context.DeadlineExceeded):
		fail(w, 504, "timeout", "the upstream work took too long", nil)
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads this.
	default:
		s.Log.Error("request failed", "path", r.URL.Path, "err", err)
		fail(w, 502, "upstream_error", "an upstream call failed", nil)
	}
}

// period reads owner/repo and the since/until window from the request.
func (s *Server) period(r *http.Request) (Period, error) {
	owner, name, err := github.ParseRepo(r.PathValue("owner") + "/" + r.PathValue("repo"))
	if err != nil {
		return Period{}, err
	}
	now := s.Now().UTC()
	q := r.URL.Query()
	// An open window ends at the next full hour, so repeat requests within the
	// hour share one stored fetch (and its cache TTL).
	until := now.Truncate(time.Hour).Add(time.Hour)
	if v := q.Get("until"); v != "" {
		if until, err = parseTime(v, true); err != nil {
			return Period{}, fmt.Errorf("until: %w", err)
		}
	}
	since := until.Add(-30 * 24 * time.Hour)
	if v := q.Get("since"); v != "" {
		if since, err = parseTime(v, false); err != nil {
			return Period{}, fmt.Errorf("since: %w", err)
		}
	}
	switch {
	case !since.Before(until):
		return Period{}, errors.New("since must be before until")
	case until.Sub(since) > s.MaxPeriod:
		return Period{}, fmt.Errorf("the period is longer than the %d-day limit", int(s.MaxPeriod.Hours()/24))
	case since.After(now):
		return Period{}, errors.New("since is in the future")
	case since.Year() < 2005:
		return Period{}, errors.New("since is before git existed")
	}
	return Period{Owner: owner, Name: name, Since: since, Until: until}, nil
}

// parseTime takes RFC 3339 or a date. A date for until means the end of that day.
func parseTime(v string, end bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return t, errors.New("want RFC 3339 (2026-10-01T00:00:00Z) or a date (2026-10-01)")
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

func (s *Server) contributors(w http.ResponseWriter, r *http.Request) {
	p, err := s.period(r)
	if err != nil {
		fail(w, 400, "bad_request", err.Error(), nil)
		return
	}
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "commits"
	}
	if by != "commits" && by != "lines" {
		fail(w, 400, "bad_request", `by must be "commits" or "lines"`, nil)
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			fail(w, 400, "bad_request", "limit must be 1 to 100", nil)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := s.Svc.Contributors(ctx, p, by, limit)
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) narrative(w http.ResponseWriter, r *http.Request) {
	p, err := s.period(r)
	if err != nil {
		fail(w, 400, "bad_request", err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.Svc.Narrative(ctx, p)
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) narrativePrompt(w http.ResponseWriter, r *http.Request) {
	p, err := s.period(r)
	if err != nil {
		fail(w, 400, "bad_request", err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := s.Svc.NarrativePrompt(ctx, p)
	if err != nil {
		s.upstream(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

// verifyNarrative checks a browser model's raw output. A failed check is the
// client's answer at fault, so it is a 422, with the problems and the message
// that asks the model to try again.
func (s *Server) verifyNarrative(w http.ResponseWriter, r *http.Request) {
	p, err := s.period(r)
	if err != nil {
		fail(w, 400, "bad_request", err.Error(), nil)
		return
	}
	var req struct {
		Digest  string `json:"digest"`
		Output  string `json:"output"`
		Attempt int    `json:"attempt"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Digest) != 64 || req.Output == "" || req.Attempt < 1 || req.Attempt > 10 {
		fail(w, 400, "bad_request", `body must be JSON like {"digest":"<from the prompt>","output":"<the model's text>","attempt":1}, at most 512 KB`, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := s.Svc.VerifyNarrative(ctx, p, req.Digest, req.Output, req.Attempt)
	var ue *narrate.UnsupportedError
	switch {
	case errors.As(err, &ue):
		fail(w, 422, "narrative_unsupported", "the answer failed the check against the data", map[string]any{
			"problems": ue.Problems, "rejected": ue.Rejected, "retry_prompt": narrate.RetryMessage(ue.Problems)})
	case errors.Is(err, ErrDataChanged):
		fail(w, 409, "data_changed", "the period's data changed since the prompt was built; fetch the prompt again", nil)
	case err != nil:
		s.upstream(w, r, err)
	default:
		writeJSON(w, 200, out)
	}
}

func (s *Server) startReconstruction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo       string `json:"repo"`
		MaxCommits int    `json:"max_commits"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, 400, "bad_request", `body must be JSON like {"repo":"owner/name","max_commits":200}`, nil)
		return
	}
	owner, name, err := github.ParseRepo(req.Repo)
	if err != nil {
		fail(w, 400, "bad_request", err.Error(), nil)
		return
	}
	if req.MaxCommits == 0 {
		req.MaxCommits = s.MaxReconstruct // to the root, up to the configured cap
	}
	if req.MaxCommits < 1 || req.MaxCommits > s.MaxReconstruct {
		fail(w, 400, "bad_request", fmt.Sprintf("max_commits must be 1 to %d", s.MaxReconstruct), nil)
		return
	}
	sess, err := s.Recon.Start(owner, name, req.MaxCommits)
	if errors.Is(err, reconstruct.ErrBusy) {
		w.Header().Set("Retry-After", "10")
		fail(w, 429, "busy", err.Error(), nil)
		return
	}
	loc := "/api/v1/reconstructions/" + sess.ID
	w.Header().Set("Location", loc)
	writeJSON(w, 202, map[string]string{"id": sess.ID, "status": loc, "events": loc + "/events"})
}

func (s *Server) reconstructionStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Recon.Get(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "unknown reconstruction id", nil)
		return
	}
	writeJSON(w, 200, sess.Status())
}

// reconstructionEvents streams a session over a WebSocket: everything so far, then
// each event as it happens. ?from=N resumes after a reconnect.
func (s *Server) reconstructionEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Recon.Get(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "unknown reconstruction id", nil)
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	// Accept checks that the Origin is this host, so other sites cannot open it.
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // the viewer sends nothing; this notices it leave
	for {
		evs, done, more := sess.Since(from)
		for _, e := range evs {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(wctx, c, e)
			cancel()
			if err != nil {
				return
			}
		}
		from += len(evs)
		if done {
			c.Close(websocket.StatusNormalClosure, "done")
			return
		}
		select {
		case <-more:
		case <-ctx.Done():
			return
		}
	}
}

// maxFileView is the most of one rebuilt file the file endpoint returns.
const maxFileView = 1 << 20

// reconstructionFile returns one file as the run rebuilt it, once the run is done.
func (s *Server) reconstructionFile(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Recon.Get(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "unknown reconstruction id", nil)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" || len(path) > 1024 {
		fail(w, 400, "bad_request", "path must name a file, at most 1024 characters", nil)
		return
	}
	f, done, ok := sess.File(path)
	switch {
	case !done:
		fail(w, 409, "running", "the files open when the reconstruction is done", nil)
		return
	case !ok:
		fail(w, 404, "not_found", "the reconstruction built no file at that path", nil)
		return
	}
	out := map[string]any{"path": path, "known": f.Known, "bytes": len(f.Content)}
	if !f.Known {
		out["why"] = f.Why
	}
	if f.Known {
		c := f.Content[:min(len(f.Content), maxFileView)]
		out["truncated"] = len(c) < len(f.Content)
		for i := 0; i < 3 && !utf8.Valid(c); i++ { // a cut may split the last character
			c = c[:len(c)-1]
		}
		if utf8.Valid(c) {
			out["content"] = string(c)
		} else {
			out["binary"] = true
		}
	}
	writeJSON(w, 200, out)
}

var commitIDRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reconstructionDiff returns one file's change in one commit of a run: the
// layer a viewer clicked. It is read from the commit details the run stored, so
// it costs no GitHub call.
func (s *Server) reconstructionDiff(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Recon.Get(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "unknown reconstruction id", nil)
		return
	}
	sha, path := r.URL.Query().Get("sha"), r.URL.Query().Get("path")
	if !commitIDRe.MatchString(sha) || path == "" || len(path) > 1024 {
		fail(w, 400, "bad_request", "sha must be a 40-character commit id, and path must name a file", nil)
		return
	}
	b, ok, err := s.Svc.Store.Detail(r.Context(), sess.Repo, sha)
	var c github.Commit
	if err != nil || !ok || json.Unmarshal(b, &c) != nil {
		fail(w, 404, "not_found", "this run stored no such commit", nil)
		return
	}
	for _, f := range c.Files {
		if f.Filename == path {
			headline, _, _ := strings.Cut(c.Commit.Message, "\n")
			writeJSON(w, 200, map[string]any{"sha": sha, "path": path, "status": f.Status, "from": f.PreviousFilename,
				"additions": f.Additions, "deletions": f.Deletions, "patch": f.Patch, "headline": headline})
			return
		}
	}
	fail(w, 404, "not_found", "that commit does not change that file", nil)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"github_core":    s.GH.Core.Snapshot(),
		"github_graphql": s.GH.GraphQL.Snapshot(),
		"writer":         s.Sink.Stats(),
		"narrative":      map[string]any{"server": s.Svc.Narrator != nil, "browser": s.Svc.Browser},
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int)           { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// The browser model: transformers.js and ONNX Runtime's wasm come from jsdelivr
// ('wasm-unsafe-eval' compiles the wasm), the weights from Hugging Face, whose
// downloads redirect to its CDN under hf.co.
const csp = "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self' https://cdn.jsdelivr.net https://huggingface.co https://*.hf.co; " +
	"worker-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// wrap adds security headers, panic recovery and one access-log line a request.
// The log has the path and status, never headers or bodies.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", csp)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic", "path", r.URL.Path, "panic", v)
				if sw.code == 200 {
					fail(sw, 500, "internal", "internal error", nil)
				}
			}
			if !strings.HasPrefix(r.URL.Path, "/healthz") {
				s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
