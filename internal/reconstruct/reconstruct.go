// Package reconstruct rebuilds a repository's files from nothing but its diff
// history, in two passes, like a temporal pincer:
//
//  1. Inverted. The walker goes back in time from the branch head along first
//     parents, toward the root commit, and publishes each commit's diff on a
//     stream, newest first, the way a websocket would deliver real-time diffs.
//  2. Forward. At the turnstile (the end of the stream, the oldest commit), the
//     replayer plays the diffs forward from an empty tree and applies every file
//     patch strictly (package patch).
//
// Every file version it builds is checked against the blob id GitHub reports for
// that file at that commit, and the final tree is checked against the head tree.
// A file it cannot prove (a binary, a patch GitHub truncated, a file older than
// the walked window) is reported as unknown, never guessed.
package reconstruct

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swizzley/tenet/internal/github"
	"github.com/swizzley/tenet/internal/patch"
)

// Event is one message to the viewer. T is its kind:
//
//	meta      the repo, branch and head
//	invert    one commit received on the stream, walking back (newest first)
//	turnstile the walk is over; the replay starts
//	commit    one commit replayed forward (oldest first)
//	file      one file of that commit, with its proof status
//	done      the summary
//	error     the run failed
type Event struct {
	T        string `json:"t"`
	I        int    `json:"i,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Author   string `json:"author,omitempty"`
	Date     string `json:"date,omitempty"`
	Headline string `json:"headline,omitempty"`
	Files    int    `json:"files,omitempty"`
	Path     string `json:"path,omitempty"`
	From     string `json:"from,omitempty"`
	Op       string `json:"op,omitempty"`
	Status   string `json:"status,omitempty"` // exact | unknown | mismatch | deleted
	Why      string `json:"why,omitempty"`
	Lines    int    `json:"lines,omitempty"`
	Adds     int    `json:"adds,omitempty"`
	Dels     int    `json:"dels,omitempty"`
	// For the page to paint the code. All of it is untrusted text from the
	// repository; the page draws it on canvases, never as HTML.
	Diff    []string `json:"diff,omitempty"`    // invert: the start of one file's patch, "+"/"-"/" " lines
	Code    []string `json:"code,omitempty"`    // file: up to 16 lines of the rebuilt file, around the change
	At      int      `json:"at,omitempty"`      // file: the line number of Code[0]
	Added   []int    `json:"added,omitempty"`   // file: indices into Code of the lines this patch added
	Fetched bool     `json:"fetched,omitempty"` // file: GitHub sent no patch; the blob was fetched by id
	Commits int      `json:"commits,omitempty"`
	Root    bool     `json:"root,omitempty"`
	Edge    string   `json:"edge,omitempty"` // turnstile: why the walk stopped short of the root
	Summary *Summary `json:"summary,omitempty"`
	Msg     string   `json:"msg,omitempty"`
}

// Summary is the proof at the end of a run.
type Summary struct {
	Commits      int    `json:"commits"`
	RootReached  bool   `json:"root_reached"`
	Edge         string `json:"edge,omitempty"` // why the walk stopped short of the root
	FileVersions int    `json:"file_versions"`  // file states built, one per file per commit
	Exact        int    `json:"exact"`          // ...whose blob id matched GitHub's
	Unknown      int    `json:"unknown"`
	Mismatch     int    `json:"mismatch"`
	Fetched      int    `json:"fetched"`        // exact versions whose blob was fetched, GitHub having sent no patch
	HeadFiles    int    `json:"head_files"`     // files in the head tree
	HeadExact    int    `json:"head_exact"`     // ...rebuilt byte-exact
	HeadUnknown  int    `json:"head_unknown"`   // ...present but unproven
	HeadMissing  int    `json:"head_missing"`   // ...never produced
	Extra        int    `json:"extra"`          // files rebuilt that the head tree lacks
	TreeTrunc    bool   `json:"tree_truncated"` // GitHub truncated the head tree listing
	Millis       int64  `json:"millis"`
	APICalls     int    `json:"api_calls"`
	CacheHits    int    `json:"cache_hits"`
}

// GitHub is the part of the client the walker uses.
type GitHub interface {
	Repo(ctx context.Context, owner, name string) (github.Repo, error)
	BranchHead(ctx context.Context, owner, name, branch string) (string, error)
	ListCommits(ctx context.Context, owner, name string, o github.ListOptions) ([]github.Commit, bool, error)
	Commit(ctx context.Context, owner, name, sha string) (github.Commit, error)
	Tree(ctx context.Context, owner, name, sha string) (github.Tree, error)
	Blob(ctx context.Context, owner, name, sha string) ([]byte, error)
}

// Cache keeps commit details, which never change for a given id.
type Cache interface {
	Detail(ctx context.Context, repo, sha string) ([]byte, bool, error)
	PutDetail(ctx context.Context, repo, sha string, body []byte) error
}

// Runner holds what one reconstruction needs.
type Runner struct {
	GH        GitHub
	Cache     Cache
	Chunk     func(bufferCap int) int // rate-limit-aware chunk size
	BufferCap int
	MaxBytes  int // memory cap for rebuilt content, default 256 MiB
	MaxCalls  int // GitHub calls one run may spend, default 1,500
}

// Window bounds a run: at most Max commits, and none older than Since (zero:
// back to the root).
type Window struct {
	Max   int
	Since time.Time
}

// The reasons a walk stops short of the root (Summary.Edge).
const (
	EdgePeriod     = "period"
	EdgeMaxCommits = "max commits"
	EdgeBudget     = "call budget"
)

// Run reconstructs owner/name, walking at most max commits, and emits events.
func (r *Runner) Run(ctx context.Context, owner, name string, max int, emit func(Event)) (*Summary, error) {
	sum, _, err := r.RunFiles(ctx, owner, name, Window{Max: max}, emit)
	return sum, err
}

// File is one rebuilt file at the end of a run: its content when proven, or
// unknown (Known false, no content).
type File struct {
	Content []byte
	Known   bool
	Why     string // when unknown: the reason
}

// RunFiles is Run, and also returns the rebuilt files, so a viewer can open one.
func (r *Runner) RunFiles(ctx context.Context, owner, name string, w Window, emit func(Event)) (*Summary, map[string]File, error) {
	start := time.Now()
	stream := make(chan github.Commit, 64)
	sum := &Summary{}
	var walkErr error
	var head string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stream)
		head, walkErr = r.walk(ctx, owner, name, w, stream, sum, emit)
	}()
	// Blob fetches are counted apart: the walker goroutine writes sum's counters.
	var blobCalls, blobHits int
	fetch := func(sha string) ([]byte, error) {
		b, hit, err := r.blob(ctx, owner, name, sha)
		if hit {
			blobHits++
		} else {
			blobCalls++
		}
		return b, err
	}
	files, err := r.replay(ctx, stream, sum, emit, fetch)
	wg.Wait()
	sum.APICalls += blobCalls
	sum.CacheHits += blobHits
	if walkErr != nil {
		return nil, nil, walkErr
	}
	if err != nil {
		return nil, nil, err
	}
	if err := r.checkTree(ctx, owner, name, head, files, sum); err != nil {
		return nil, nil, err
	}
	sum.Millis = time.Since(start).Milliseconds()
	out := make(map[string]File, len(files))
	for p, f := range files {
		out[p] = File{Content: f.content, Known: f.known, Why: f.why}
	}
	emit(Event{T: "done", Summary: sum})
	return sum, out, nil
}

// walk finds the first-parent chain from the head, fetches the commits' diffs in
// rate-limit-sized chunks (from the cache when it can), and publishes them on the
// stream newest first.
func (r *Runner) walk(ctx context.Context, owner, name string, w Window, stream chan<- github.Commit, sum *Summary, emit func(Event)) (string, error) {
	budget := r.MaxCalls
	if budget <= 0 {
		budget = 1500
	}
	repo := owner + "/" + name
	info, err := r.GH.Repo(ctx, owner, name)
	sum.APICalls++
	if err != nil {
		return "", err
	}
	head, err := r.GH.BranchHead(ctx, owner, name, info.DefaultBranch)
	sum.APICalls++
	if err != nil {
		return "", err
	}
	emit(Event{T: "meta", Repo: repo, Branch: info.DefaultBranch, SHA: head, Commits: w.Max})

	// The commit list gives 100 commits and their parents per request, so the
	// first-parent chain costs one request per 100 commits, not one per commit.
	// The list holds every ancestor, merged branches too, so Since bounds it: a
	// busy repository's full list is thousands of pages.
	parents := map[string][]string{}
	page, more, shifted := 0, true, false
	var chain []string
	cur := head
	for len(chain) < w.Max {
		ps, ok := parents[cur]
		if !ok {
			if !more {
				if !w.Since.IsZero() {
					sum.Edge = EdgePeriod
				}
				break
			}
			if sum.APICalls >= budget {
				sum.Edge = EdgeBudget
				break
			}
			page++
			cs, next, err := r.GH.ListCommits(ctx, owner, name, github.ListOptions{SHA: head, Since: w.Since, Page: page, PerPage: 100})
			sum.APICalls++
			if err != nil {
				return head, err
			}
			if len(cs) == 0 && page == 1 && !w.Since.IsZero() && !shifted {
				// A quiet repository: its head is older than the period. Walk the
				// same span back from the head instead of nothing.
				hc, hit, err := r.detail(ctx, owner, name, repo, head)
				if !hit {
					sum.APICalls++
				}
				if err != nil {
					return head, err
				}
				last := hc.Commit.Author.Date.UTC()
				span := time.Since(w.Since)
				days := int(span.Hours()/24 + 0.5)
				w.Since = last.Add(-span)
				shifted, page = true, 0
				emit(Event{T: "shift", Date: last.Format(time.RFC3339),
					Msg: fmt.Sprintf("0 commits in the last %d days; walking the %d days before the last commit, %s", days, days, last.Format(time.DateOnly))})
				continue
			}
			for _, c := range cs {
				parents[c.SHA] = c.ParentSHAs()
			}
			more = next
			emit(Event{T: "walk", I: page, Commits: len(parents)})
			continue
		}
		chain = append(chain, cur)
		if len(ps) == 0 {
			sum.RootReached = true
			break
		}
		cur = ps[0]
	}
	if len(chain) == w.Max && !sum.RootReached {
		sum.Edge = EdgeMaxCommits
	}

	// Fetch the diffs in chunks: as many requests as fill one buffer, capped by the
	// rate-limit budget and the run's call budget. Requests within a chunk run in
	// parallel; a cached commit costs no call.
	for i := 0; i < len(chain); {
		left := budget - sum.APICalls
		if left <= 0 {
			sum.Edge = EdgeBudget
			break
		}
		n := min(r.Chunk(r.BufferCap), len(chain)-i, left)
		got := make([]github.Commit, n)
		hit := make([]bool, n)
		errs := make([]error, n)
		var cw sync.WaitGroup
		for k := range n {
			cw.Add(1)
			go func() {
				defer cw.Done()
				got[k], hit[k], errs[k] = r.detail(ctx, owner, name, repo, chain[i+k])
			}()
		}
		cw.Wait()
		for k := range n {
			if hit[k] {
				sum.CacheHits++
			} else {
				sum.APICalls++
			}
			if errs[k] != nil {
				return head, errs[k]
			}
			select {
			case stream <- got[k]:
			case <-ctx.Done():
				return head, ctx.Err()
			}
		}
		i += n
	}
	return head, nil
}

// detail returns one commit's diff, from the cache when it holds it (hit = true).
func (r *Runner) detail(ctx context.Context, owner, name, repo, sha string) (c github.Commit, hit bool, err error) {
	if b, ok, err := r.Cache.Detail(ctx, repo, sha); err == nil && ok && json.Unmarshal(b, &c) == nil {
		return c, true, nil
	}
	if c, err = r.GH.Commit(ctx, owner, name, sha); err != nil {
		return c, false, err
	}
	if b, err := json.Marshal(c); err == nil {
		r.Cache.PutDetail(ctx, repo, sha, b)
	}
	return c, false, nil
}

// maxBlob is the largest blob a run fetches when GitHub sends no patch.
const maxBlob = 20 << 20

// blob returns a file's content by its blob id, from the cache when it holds it.
// A blob id names its bytes, so a stored blob never goes stale.
func (r *Runner) blob(ctx context.Context, owner, name, sha string) (b []byte, hit bool, err error) {
	repo, key := owner+"/"+name, "blob:"+sha
	if b, ok, err := r.Cache.Detail(ctx, repo, key); err == nil && ok {
		return b, true, nil
	}
	if b, err = r.GH.Blob(ctx, owner, name, sha); err != nil {
		return nil, false, err
	}
	if len(b) > maxBlob {
		return nil, false, fmt.Errorf("the blob is %d MiB, over the %d MiB a run fetches", len(b)>>20, maxBlob>>20)
	}
	r.Cache.PutDetail(ctx, repo, key, b)
	return b, false, nil
}

// file is one rebuilt file: its content when proven, or unknown.
type file struct {
	content []byte
	known   bool
	why     string // when unknown: the reason
}

// replay receives the inverted stream, then plays it forward.
func (r *Runner) replay(ctx context.Context, stream <-chan github.Commit, sum *Summary, emit func(Event), fetch func(string) ([]byte, error)) (map[string]*file, error) {
	var inverted []github.Commit
	for c := range stream {
		inverted = append(inverted, c)
		ev := Event{T: "invert", I: len(inverted), SHA: c.SHA, Author: author(c), Date: c.Commit.Author.Date.UTC().Format(time.RFC3339),
			Headline: headline(c.Commit.Message), Files: len(c.Files)}
		for _, f := range c.Files {
			if d := diffHead(f.Patch, 10); len(d) > 0 {
				ev.Path, ev.Diff = f.Filename, d
				break
			}
		}
		emit(ev)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sum.Commits = len(inverted)
	emit(Event{T: "turnstile", Commits: len(inverted), Root: sum.RootReached, Edge: sum.Edge})

	maxBytes := r.MaxBytes
	if maxBytes == 0 {
		maxBytes = 256 << 20
	}
	files := map[string]*file{}
	total := 0
	for i := len(inverted) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := inverted[i]
		emit(Event{T: "commit", I: len(inverted) - i, SHA: c.SHA, Author: author(c), Date: c.Commit.Author.Date.UTC().Format(time.RFC3339),
			Headline: headline(c.Commit.Message), Files: len(c.Files)})
		for _, f := range c.Files {
			ev, delta := apply(files, f, sum.RootReached, fetch)
			total += delta
			if total > maxBytes {
				return nil, fmt.Errorf("reconstruct: rebuilt content passed %d MiB; walk fewer commits", maxBytes>>20)
			}
			switch ev.Status {
			case "exact":
				sum.Exact++
				if ev.Fetched {
					sum.Fetched++
				}
			case "unknown":
				sum.Unknown++
			case "mismatch":
				sum.Mismatch++
			}
			if ev.Status != "deleted" {
				sum.FileVersions++
			}
			emit(ev)
		}
	}
	return files, nil
}

// apply plays one file change and proves the result. It returns the event and the
// change in rebuilt bytes.
func apply(files map[string]*file, f github.File, fromRoot bool, fetch func(sha string) ([]byte, error)) (Event, int) {
	ev := Event{T: "file", Path: f.Filename, Op: f.Status, Adds: f.Additions, Dels: f.Deletions}
	before := 0
	if old := files[f.Filename]; old != nil {
		before = len(old.content)
	}
	var base *file
	switch f.Status {
	case "removed":
		delete(files, f.Filename)
		ev.Status = "deleted"
		return ev, -before
	case "renamed", "copied":
		ev.From = f.PreviousFilename
		base = files[f.PreviousFilename]
		if f.Status == "renamed" {
			if base != nil {
				before += len(base.content)
			}
			delete(files, f.PreviousFilename)
		}
	case "added":
		base = &file{known: true}
	default: // "modified", "changed"
		base = files[f.Filename]
	}
	why := "an earlier version of it is unproven"
	if base == nil {
		// A file older than the walked window: its earlier content was never streamed.
		base = &file{known: fromRoot}
		why = "its content predates the walked history"
	}
	out := &file{}
	switch {
	case f.Patch == "" && f.SHA == "":
		// A mode-only change: GitHub sends no patch and no blob id (VERIFIED live
		// 2026-10-30, google/uuid 386477fd90). The bytes are the previous
		// version's; the head-tree check confirms them at the end.
		if !base.known {
			ev.Status, ev.Why = "unknown", why
			break
		}
		out = &file{content: base.content, known: true}
		ev.Status, ev.Why = "exact", "mode change only; content unchanged"
		ev.Lines = strings.Count(string(base.content), "\n")
	case f.Patch == "" && base.known && f.SHA == patch.BlobSHA(base.content):
		// No patch because the bytes are the same (a pure rename, an empty file).
		out = &file{content: base.content, known: true}
		ev.Status = "exact"
		ev.Lines = strings.Count(string(base.content), "\n")
	case f.Patch == "":
		// GitHub sends no patch for a binary file or a very large diff (live:
		// tensorwire/mongoose, a +2,929 −15 change, after which every later
		// version of that file read as unproven; and 10 binaries that read as
		// mismatches). The blob is fetched by the id GitHub reports and proven by
		// its hash, so the file's history continues from there.
		content, err := fetch(f.SHA)
		switch {
		case err != nil:
			ev.Status, ev.Why = "unknown", "GitHub sent no patch, and fetching the blob failed: "+err.Error()
		case patch.BlobSHA(content) != f.SHA:
			ev.Status, ev.Why = "mismatch", "the fetched blob hashes to "+patch.BlobSHA(content)[:12]+", not "+short(f.SHA)
		default:
			out = &file{content: content, known: true}
			ev.Status, ev.Fetched = "exact", true
			ev.Why = "GitHub sent no patch (a binary or a large diff); the blob, fetched by id, matches"
			ev.Lines = strings.Count(string(content), "\n")
		}
	case !base.known:
		ev.Status, ev.Why = "unknown", why
	default:
		content, err := patch.Apply(base.content, f.Patch)
		if err != nil {
			ev.Status, ev.Why = "unknown", err.Error()
			break
		}
		if got := patch.BlobSHA(content); got != f.SHA {
			ev.Status, ev.Why = "mismatch", "blob "+got[:12]+" but GitHub says "+short(f.SHA)
			break
		}
		out = &file{content: content, known: true}
		ev.Status = "exact"
		ev.Lines = strings.Count(string(content), "\n")
		ev.Code, ev.At, ev.Added = excerpt(content, f.Patch)
	}
	if !out.known {
		out.why = ev.Why
	}
	files[f.Filename] = out
	return ev, len(out.content) - before
}

// checkTree compares the rebuilt files with the head commit's tree.
func (r *Runner) checkTree(ctx context.Context, owner, name, head string, files map[string]*file, sum *Summary) error {
	tree, err := r.GH.Tree(ctx, owner, name, head)
	sum.APICalls++
	if err != nil {
		return err
	}
	sum.TreeTrunc = tree.Truncated
	sum.HeadFiles = len(tree.Blobs)
	for path, sha := range tree.Blobs {
		f, ok := files[path]
		switch {
		case !ok:
			sum.HeadMissing++
		case f.known && patch.BlobSHA(f.content) == sha:
			sum.HeadExact++
		default:
			sum.HeadUnknown++
		}
	}
	for path := range files {
		if _, ok := tree.Blobs[path]; !ok {
			sum.Extra++
		}
	}
	return nil
}

func author(c github.Commit) string {
	if l := c.Login(); l != "" {
		return l
	}
	return clip(c.Commit.Author.Name, 60)
}

func headline(msg string) string {
	h, _, _ := strings.Cut(msg, "\n")
	return clip(h, 120)
}

// codeLines is how much of a file the page paints at once.
const codeLines = 16

// codeLine readies one line of code for the page: tabs as two spaces, cut to 96
// characters, control characters dropped (clip).
func codeLine(s string) string {
	return clip(strings.ReplaceAll(strings.TrimRight(s, "\r"), "\t", "  "), 96)
}

// diffHead is the part of a patch the inverted phase shows: up to n of its
// "+", "-" and " " lines, from two lines above its first change (a patch opens
// with context lines, so its first n lines are often all context), hunk
// headers skipped.
func diffHead(p string, n int) []string {
	var all []string
	first := -1
	for _, l := range strings.Split(p, "\n") {
		if l == "" || strings.HasPrefix(l, "@@") || strings.HasPrefix(l, `\`) {
			continue
		}
		if first < 0 && l[0] != ' ' {
			first = len(all)
		}
		all = append(all, l[:1]+codeLine(l[1:]))
		if first >= 0 && len(all) >= first+n {
			break
		}
	}
	if first < 0 {
		return nil // context only: nothing changed to show
	}
	start := max(0, first-2)
	return all[start:min(len(all), start+n)]
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// excerpt is the window of the rebuilt file the page paints: codeLines lines
// starting a little above the first line the patch added (or its first hunk),
// the line number of the first, and which of them the patch added.
func excerpt(content []byte, p string) (code []string, at int, added []int) {
	var adds []int // 1-based line numbers, in the new file, of added lines
	first, n := 0, 0
	for _, l := range strings.Split(p, "\n") {
		if m := hunkRe.FindStringSubmatch(l); m != nil {
			n, _ = strconv.Atoi(m[1])
			if first == 0 {
				first = max(n, 1)
			}
			continue
		}
		switch {
		case n == 0 || l == "" || strings.HasPrefix(l, `\`):
		case l[0] == '+':
			adds = append(adds, n)
			n++
		case l[0] == ' ':
			n++
		}
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, 0, nil
	}
	if len(adds) > 0 {
		first = adds[0]
	}
	at = max(1, min(first-3, len(lines)-codeLines+1))
	for i := at; i <= len(lines) && i < at+codeLines; i++ {
		code = append(code, codeLine(lines[i-1]))
	}
	for _, a := range adds {
		if a >= at && a < at+len(code) {
			added = append(added, a-at)
		}
	}
	return code, at, added
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
