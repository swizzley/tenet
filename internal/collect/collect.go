// Package collect pulls a repository's commits for a period from GitHub and writes
// each one through the double-buffered writer into the database.
//
// The source is the GraphQL commit history: one page carries up to 100 commits
// with their line counts for one rate-limit point. The page size is the buffer
// capacity, so one page fills exactly one buffer half: the flusher writes that
// half while the next page is in flight, and producers wait only if a page lands
// before the previous write is done. The GraphQL limiter parks a request when the
// point budget reaches its reserve.
package collect

import (
	"context"
	"strings"
	"time"

	"github.com/swizzley/tenet/internal/dbuf"
	"github.com/swizzley/tenet/internal/github"
)

// Record is one commit as stored.
type Record struct {
	Repo      string    `json:"repo"`
	SHA       string    `json:"sha"`
	Login     string    `json:"login,omitempty"`
	Name      string    `json:"name"`
	Date      time.Time `json:"date"`
	Additions int       `json:"additions"`
	Deletions int       `json:"deletions"`
	Headline  string    `json:"headline"`
}

// Meta says how the collection went.
type Meta struct {
	Pages     int   `json:"pages"`
	PageSizes []int `json:"page_sizes"`
	Truncated bool  `json:"truncated"` // stopped at the commit cap
	Millis    int64 `json:"millis"`
}

// History is the GitHub call the collector needs (a seam for tests).
type History interface {
	History(ctx context.Context, owner, name string, since, until time.Time, first int, cursor string) ([]github.HistoryNode, string, error)
}

type Collector struct {
	GH         History
	Sink       *dbuf.Writer[Record]
	MaxCommits int
}

// Collect reads every commit on the default branch from since to until, up to MaxCommits.
func (c *Collector) Collect(ctx context.Context, owner, name string, since, until time.Time) ([]Record, Meta, error) {
	start := time.Now()
	var out []Record
	var m Meta
	repo := owner + "/" + name
	cursor := ""
	for {
		page := min(c.Sink.Cap(), c.MaxCommits-len(out))
		nodes, next, err := c.GH.History(ctx, owner, name, since, until, page, cursor)
		if err != nil {
			return nil, m, err
		}
		m.Pages++
		m.PageSizes = append(m.PageSizes, len(nodes))
		for _, n := range nodes {
			r := Record{
				Repo: repo, SHA: n.OID, Name: clean(n.Author.Name, 100), Date: n.CommittedDate,
				Additions: n.Additions, Deletions: n.Deletions, Headline: clean(n.MessageHeadline, 200),
			}
			if n.Author.User != nil {
				r.Login = n.Author.User.Login
			}
			if err := c.Sink.Put(r); err != nil {
				return nil, m, err
			}
			out = append(out, r)
		}
		if next == "" {
			break
		}
		if len(out) >= c.MaxCommits {
			m.Truncated = true
			break
		}
		cursor = next
	}
	// The period counts as stored only once its rows are committed.
	if err := c.Sink.Sync(); err != nil {
		return nil, m, err
	}
	m.Millis = time.Since(start).Milliseconds()
	return out, m, nil
}

// clean drops control characters and caps the length of untrusted text.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return strings.TrimSpace(s)
}
