// Package github is a small, rate-limit-aware client for the parts of the GitHub
// REST and GraphQL APIs this service reads: a repository, its commit list, one
// commit with its per-file patches, a tree, and the GraphQL commit history.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors a caller can branch on. They never carry the token.
var (
	ErrNotFound     = errors.New("github: not found")
	ErrEmptyRepo    = errors.New("github: repository is empty")
	ErrUnauthorized = errors.New("github: bad or missing credentials")
	ErrNeedsToken   = errors.New("github: the GraphQL API needs a token")
)

// StatusError is any other non-2xx answer.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Code, e.Message) }

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	shaRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ParseRepo validates "owner/name". The pieces become URL path segments under a
// fixed host, so nothing a caller types can change where a request goes.
func ParseRepo(s string) (owner, name string, err error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "https://github.com/"), ".git"))
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !ownerRe.MatchString(owner) || !nameRe.MatchString(name) || name == "." || name == ".." {
		return "", "", fmt.Errorf("repo must be owner/name, got %q", s)
	}
	return owner, name, nil
}

// Options configures a Client.
type Options struct {
	BaseURL     string // default https://api.github.com
	GraphQLURL  string // default BaseURL + /graphql
	Token       string // optional; raises the core limit from 60 to 5,000 an hour
	HTTP        *http.Client
	Logger      *slog.Logger
	Reserve     int // requests kept back in each window, default 5
	Concurrency int // requests in flight at once, default 4 (GitHub's secondary limits)
}

// Client talks to GitHub. It is safe for concurrent use.
type Client struct {
	base, gql string
	token     string
	http      *http.Client
	log       *slog.Logger
	sem       chan struct{}

	Core    *Limiter
	GraphQL *Limiter

	etagMu sync.Mutex
	etags  map[string]cached
	order  []string

	sleep func(ctx context.Context, d time.Duration) error
}

// cached is a GET's body and the Link header it came with. GitHub's 304 answer
// omits Link, so without the cached one a repeat walk read "last page" and
// stopped early (VERIFIED live 2026-10-30: google/uuid, 70 of 109 commits).
type cached struct {
	etag string
	body []byte
	link string
}

const etagCap = 1024

// New builds a client.
func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.github.com"
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.GraphQLURL == "" {
		o.GraphQLURL = o.BaseURL + "/graphql"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Reserve == 0 {
		o.Reserve = 5
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	return &Client{
		base: o.BaseURL, gql: o.GraphQLURL, token: o.Token, http: o.HTTP, log: o.Logger,
		sem:  make(chan struct{}, o.Concurrency),
		Core: NewLimiter(o.Reserve), GraphQL: NewLimiter(o.Reserve),
		etags: map[string]cached{}, sleep: sleepCtx,
	}
}

// HasToken reports whether requests are authenticated.
func (c *Client) HasToken() bool { return c.token != "" }

const maxBody = 64 << 20

// do sends one request through the limiter, with conditional-request caching for
// GETs and bounded retries for rate limiting and server errors.
func (c *Client) do(ctx context.Context, lim *Limiter, method, rawURL string, body []byte) ([]byte, http.Header, error) {
	for attempt := 0; ; attempt++ {
		b, h, retryAfter, err := c.once(ctx, lim, method, rawURL, body)
		if retryAfter < 0 || attempt >= 4 {
			return b, h, err
		}
		c.log.Warn("github retry", "url", rawURL, "attempt", attempt+1, "wait", retryAfter, "err", err)
		if serr := c.sleep(ctx, retryAfter); serr != nil {
			return nil, nil, serr
		}
	}
}

// once returns retryAfter >= 0 when the request should be tried again after that wait.
func (c *Client) once(ctx context.Context, lim *Limiter, method, rawURL string, body []byte) ([]byte, http.Header, time.Duration, error) {
	if err := lim.Acquire(ctx); err != nil {
		return nil, nil, -1, err
	}
	var hdr http.Header
	defer func() { lim.Release(hdr) }()
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, -1, ctx.Err()
	}
	defer func() { <-c.sem }()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, -1, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "tenet-commit-insights")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	var prev cached
	var havePrev bool
	if method == http.MethodGet {
		c.etagMu.Lock()
		prev, havePrev = c.etags[rawURL]
		c.etagMu.Unlock()
		if havePrev {
			req.Header.Set("If-None-Match", prev.etag)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, -1, ctx.Err()
		}
		return nil, nil, 2 * time.Second, err
	}
	defer resp.Body.Close()
	hdr = resp.Header
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, hdr, 2 * time.Second, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && havePrev:
		out := hdr.Clone() // the 304's rate-limit headers, the cached page links
		out.Set("Link", prev.link)
		return prev.body, out, -1, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if et := resp.Header.Get("ETag"); et != "" && method == http.MethodGet {
			c.remember(rawURL, cached{et, data, resp.Header.Get("Link")})
		}
		return data, hdr, -1, nil
	}
	msg := apiMessage(data)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, hdr, -1, ErrNotFound
	case resp.StatusCode == http.StatusConflict && strings.Contains(strings.ToLower(msg), "empty"):
		return nil, hdr, -1, ErrEmptyRepo
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, hdr, -1, ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			return nil, hdr, time.Duration(ra) * time.Second, &StatusError{resp.StatusCode, msg}
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			// The limiter has seen remaining=0; the next Acquire parks until the reset.
			return nil, hdr, 0, &StatusError{resp.StatusCode, msg}
		}
		return nil, hdr, -1, &StatusError{resp.StatusCode, msg}
	case resp.StatusCode >= 500:
		return nil, hdr, 2 * time.Second, &StatusError{resp.StatusCode, msg}
	}
	return nil, hdr, -1, &StatusError{resp.StatusCode, msg}
}

func apiMessage(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return m.Message
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

func (c *Client) remember(u string, e cached) {
	c.etagMu.Lock()
	defer c.etagMu.Unlock()
	if _, ok := c.etags[u]; !ok {
		c.order = append(c.order, u)
		if len(c.order) > etagCap {
			delete(c.etags, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.etags[u] = e
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) (http.Header, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	b, h, err := c.do(ctx, c.Core, http.MethodGet, u, nil)
	if err != nil {
		return h, err
	}
	return h, json.Unmarshal(b, out)
}

func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// Repo is the subset of a repository this service reads.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

func (c *Client) Repo(ctx context.Context, owner, name string) (Repo, error) {
	var r Repo
	_, err := c.getJSON(ctx, repoPath(owner, name), nil, &r)
	return r, err
}

// Person is a commit author as GitHub reports it.
type Person struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

// Commit is one commit. Files and Stats are set only by Commit (the detail call).
type Commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author  Person `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Stats *struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
		Total     int `json:"total"`
	} `json:"stats,omitempty"`
	Files []File `json:"files,omitempty"`
}

// File is one changed file of a commit. SHA is the blob id AFTER the change.
type File struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	Status           string `json:"status"`
	SHA              string `json:"sha"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Changes          int    `json:"changes"`
	Patch            string `json:"patch,omitempty"`
}

// ParentSHAs lists the parent ids, first parent first.
func (c Commit) ParentSHAs() []string {
	out := make([]string, len(c.Parents))
	for i, p := range c.Parents {
		out[i] = p.SHA
	}
	return out
}

// Login is the GitHub login, or the empty string for an unmapped author.
func (c Commit) Login() string {
	if c.Author != nil {
		return c.Author.Login
	}
	return ""
}

// ListOptions selects one page of the commit list.
type ListOptions struct {
	SHA          string
	Since, Until time.Time
	Page         int
	PerPage      int
}

// ListCommits returns one page, newest first, and whether another page follows.
func (c *Client) ListCommits(ctx context.Context, owner, name string, o ListOptions) ([]Commit, bool, error) {
	q := url.Values{}
	if o.SHA != "" {
		q.Set("sha", o.SHA)
	}
	if !o.Since.IsZero() {
		q.Set("since", o.Since.UTC().Format(time.RFC3339))
	}
	if !o.Until.IsZero() {
		q.Set("until", o.Until.UTC().Format(time.RFC3339))
	}
	if o.PerPage <= 0 || o.PerPage > 100 {
		o.PerPage = 100
	}
	q.Set("per_page", strconv.Itoa(o.PerPage))
	if o.Page > 1 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	var out []Commit
	h, err := c.getJSON(ctx, repoPath(owner, name)+"/commits", q, &out)
	if err != nil {
		return nil, false, err
	}
	return out, strings.Contains(h.Get("Link"), `rel="next"`), nil
}

// filesPerPage is how many files GitHub returns on one page of a commit.
const filesPerPage = 300

// Commit returns one commit with its stats and per-file patches. A commit with more
// than 300 files comes in pages; Commit reads them all (GitHub stops at 3,000).
func (c *Client) Commit(ctx context.Context, owner, name, sha string) (Commit, error) {
	var out Commit
	if !shaRe.MatchString(sha) {
		return out, fmt.Errorf("github: bad commit id %q", sha)
	}
	path := repoPath(owner, name) + "/commits/" + sha
	if _, err := c.getJSON(ctx, path, nil, &out); err != nil {
		return out, err
	}
	for page := 2; len(out.Files) == (page-1)*filesPerPage && page <= 10; page++ {
		var more Commit
		if _, err := c.getJSON(ctx, path, url.Values{"page": {strconv.Itoa(page)}}, &more); err != nil {
			return out, err
		}
		out.Files = append(out.Files, more.Files...)
	}
	return out, nil
}

// BranchHead returns the commit id at the tip of a branch.
func (c *Client) BranchHead(ctx context.Context, owner, name, branch string) (string, error) {
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if branch == "" || strings.ContainsAny(branch, "?#") {
		return "", fmt.Errorf("github: bad branch %q", branch)
	}
	_, err := c.getJSON(ctx, repoPath(owner, name)+"/branches/"+url.PathEscape(branch), nil, &out)
	if err == nil && !shaRe.MatchString(out.Commit.SHA) {
		err = fmt.Errorf("github: branch %q returned an invalid commit id", branch)
	}
	return out.Commit.SHA, err
}

// Tree is a recursive listing: path to blob id, for blobs only.
type Tree struct {
	Blobs     map[string]string
	Truncated bool
}

func (c *Client) Tree(ctx context.Context, owner, name, sha string) (Tree, error) {
	var raw struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	t := Tree{Blobs: map[string]string{}}
	if !shaRe.MatchString(sha) {
		return t, fmt.Errorf("github: bad tree id %q", sha)
	}
	if _, err := c.getJSON(ctx, repoPath(owner, name)+"/git/trees/"+sha, url.Values{"recursive": {"1"}}, &raw); err != nil {
		return t, err
	}
	for _, e := range raw.Tree {
		if e.Type == "blob" {
			t.Blobs[e.Path] = e.SHA
		}
	}
	t.Truncated = raw.Truncated
	return t, nil
}

// HistoryNode is one commit from the GraphQL history, with its line counts.
type HistoryNode struct {
	OID             string    `json:"oid"`
	CommittedDate   time.Time `json:"committedDate"`
	Additions       int       `json:"additions"`
	Deletions       int       `json:"deletions"`
	MessageHeadline string    `json:"messageHeadline"`
	Author          struct {
		Name  string    `json:"name"`
		Email string    `json:"email"`
		Date  time.Time `json:"date"`
		User  *struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"author"`
}

const historyQuery = `query($owner:String!,$name:String!,$since:GitTimestamp!,$until:GitTimestamp!,$first:Int!,$cursor:String){
  repository(owner:$owner,name:$name){
    defaultBranchRef{ target{ ... on Commit{
      history(first:$first,since:$since,until:$until,after:$cursor){
        pageInfo{ hasNextPage endCursor }
        nodes{ oid committedDate additions deletions messageHeadline
          author{ name email date user{ login } } }
      } } } }
  }
}`

// History returns one page (up to first ≤ 100 commits) of the default branch's
// history from since to until, with additions and deletions: 100 commits for one
// rate-limit point, where REST needs one request per commit.
func (c *Client) History(ctx context.Context, owner, name string, since, until time.Time, first int, cursor string) ([]HistoryNode, string, error) {
	if c.token == "" {
		return nil, "", ErrNeedsToken
	}
	first = max(1, min(first, 100))
	vars := map[string]any{
		"owner": owner, "name": name, "first": first,
		"since": since.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339),
	}
	if cursor != "" {
		vars["cursor"] = cursor
	}
	body, _ := json.Marshal(map[string]any{"query": historyQuery, "variables": vars})
	b, _, err := c.do(ctx, c.GraphQL, http.MethodPost, c.gql, body)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Data struct {
			Repository *struct {
				DefaultBranchRef *struct {
					Target struct {
						History struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []HistoryNode `json:"nodes"`
						} `json:"history"`
					} `json:"target"`
				} `json:"defaultBranchRef"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, "", err
	}
	if len(resp.Errors) > 0 {
		if resp.Errors[0].Type == "NOT_FOUND" {
			return nil, "", ErrNotFound
		}
		return nil, "", &StatusError{200, resp.Errors[0].Message}
	}
	r := resp.Data.Repository
	if r == nil {
		return nil, "", ErrNotFound
	}
	if r.DefaultBranchRef == nil {
		return nil, "", ErrEmptyRepo
	}
	h := r.DefaultBranchRef.Target.History
	next := ""
	if h.PageInfo.HasNextPage {
		next = h.PageInfo.EndCursor
	}
	return h.Nodes, next, nil
}
