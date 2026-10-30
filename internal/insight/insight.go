// Package insight turns a period's commits into the contributor metrics the API
// serves and the narrative cites.
//
// The metric: who carried the period, measured two ways at once. Commits count
// how often someone landed work; lines changed (additions + deletions) count how
// much. Either alone misleads: commits reward many small changes, and lines reward
// one generated or vendored dump. So every contributor carries both, plus a count
// of large commits (over LargeCommitLines) that shows when a lines ranking rests on
// a few bulk changes. Concentration (the top contributor's share, and how many
// people make up half the commits) and a weekly series give the narrative something
// to explain.
package insight

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/swizzley/tenet/internal/collect"
)

// LargeCommitLines marks a commit as a bulk change.
const LargeCommitLines = 1000

type Contributor struct {
	ID           string    `json:"id"` // the login, or "~name" for an unmapped author
	Login        string    `json:"login,omitempty"`
	Name         string    `json:"name"`
	Commits      int       `json:"commits"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	LinesChanged int       `json:"lines_changed"`
	CommitShare  float64   `json:"commit_share"` // 0..1, rounded to 4 places
	LinesShare   float64   `json:"lines_share"`
	LargeCommits int       `json:"large_commits"`
	ActiveDays   int       `json:"active_days"`
	First        time.Time `json:"first"`
	Last         time.Time `json:"last"`
}

type Week struct {
	Start        time.Time `json:"start"` // Monday 00:00 UTC
	Commits      int       `json:"commits"`
	LinesChanged int       `json:"lines_changed"`
	Top          string    `json:"top"` // contributor id with the most commits that week
}

type Insight struct {
	Repo         string        `json:"repo"`
	Since        time.Time     `json:"since"`
	Until        time.Time     `json:"until"`
	By           string        `json:"by"` // "commits" or "lines"
	TotalCommits int           `json:"total_commits"`
	TotalLines   int           `json:"total_lines_changed"`
	Contributors []Contributor `json:"contributors"` // ranked by By, all of them
	Top1Share    float64       `json:"top1_commit_share"`
	BusFactor50  int           `json:"bus_factor_50"` // fewest contributors covering half the commits
	MedianCommit float64       `json:"median_lines_per_commit"`
	LargeCommits int           `json:"large_commits"`
	Weekly       []Week        `json:"weekly"`
	PeakWeek     *Week         `json:"peak_week,omitempty"`
	MedianWeekly float64       `json:"median_weekly_commits"`
	Truncated    bool          `json:"truncated"`
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

func idOf(r collect.Record) string {
	if r.Login != "" {
		return r.Login
	}
	return "~" + r.Name
}

func weekStart(t time.Time) time.Time {
	t = t.UTC()
	d := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, time.UTC)
}

// Compute builds the insight. by is "commits" (default) or "lines".
func Compute(repo string, recs []collect.Record, since, until time.Time, by string, truncated bool) Insight {
	if by != "lines" {
		by = "commits"
	}
	in := Insight{Repo: repo, Since: since.UTC(), Until: until.UTC(), By: by, Truncated: truncated}
	people := map[string]*Contributor{}
	days := map[string]map[string]bool{}
	weeks := map[time.Time]*Week{}
	weekBy := map[time.Time]map[string]int{}
	sizes := make([]float64, 0, len(recs))
	for _, r := range recs {
		id := idOf(r)
		c := people[id]
		if c == nil {
			c = &Contributor{ID: id, Login: r.Login, Name: r.Name, First: r.Date, Last: r.Date}
			people[id] = c
			days[id] = map[string]bool{}
		}
		lines := r.Additions + r.Deletions
		c.Commits++
		c.Additions += r.Additions
		c.Deletions += r.Deletions
		c.LinesChanged += lines
		if lines > LargeCommitLines {
			c.LargeCommits++
			in.LargeCommits++
		}
		if r.Date.Before(c.First) {
			c.First = r.Date
		}
		if r.Date.After(c.Last) {
			c.Last = r.Date
		}
		days[id][r.Date.UTC().Format(time.DateOnly)] = true
		in.TotalCommits++
		in.TotalLines += lines
		sizes = append(sizes, float64(lines))

		ws := weekStart(r.Date)
		w := weeks[ws]
		if w == nil {
			w = &Week{Start: ws}
			weeks[ws] = w
			weekBy[ws] = map[string]int{}
		}
		w.Commits++
		w.LinesChanged += lines
		weekBy[ws][id]++
	}
	for id, c := range people {
		c.ActiveDays = len(days[id])
		if in.TotalCommits > 0 {
			c.CommitShare = round4(float64(c.Commits) / float64(in.TotalCommits))
		}
		if in.TotalLines > 0 {
			c.LinesShare = round4(float64(c.LinesChanged) / float64(in.TotalLines))
		}
		in.Contributors = append(in.Contributors, *c)
	}
	key := func(c Contributor) int { return c.Commits }
	alt := func(c Contributor) int { return c.LinesChanged }
	if by == "lines" {
		key, alt = alt, key
	}
	slices.SortFunc(in.Contributors, func(a, b Contributor) int {
		return cmp.Or(cmp.Compare(key(b), key(a)), cmp.Compare(alt(b), alt(a)), cmp.Compare(a.ID, b.ID))
	})

	// Concentration is about commits whatever the ranking.
	byCommits := slices.Clone(in.Contributors)
	slices.SortFunc(byCommits, func(a, b Contributor) int {
		return cmp.Or(cmp.Compare(b.Commits, a.Commits), cmp.Compare(a.ID, b.ID))
	})
	if len(byCommits) > 0 {
		in.Top1Share = byCommits[0].CommitShare
		acc := 0
		for i, c := range byCommits {
			acc += c.Commits
			if 2*acc >= in.TotalCommits {
				in.BusFactor50 = i + 1
				break
			}
		}
	}
	in.MedianCommit = median(sizes)

	for ws, w := range weeks {
		best, bestN := "", -1
		for id, n := range weekBy[ws] {
			if n > bestN || (n == bestN && id < best) {
				best, bestN = id, n
			}
		}
		w.Top = best
		in.Weekly = append(in.Weekly, *w)
	}
	slices.SortFunc(in.Weekly, func(a, b Week) int { return a.Start.Compare(b.Start) })
	wc := make([]float64, len(in.Weekly))
	for i, w := range in.Weekly {
		wc[i] = float64(w.Commits)
		if in.PeakWeek == nil || w.Commits > in.PeakWeek.Commits {
			pw := w
			in.PeakWeek = &pw
		}
	}
	in.MedianWeekly = median(wc)
	if in.Contributors == nil {
		in.Contributors = []Contributor{}
	}
	if in.Weekly == nil {
		in.Weekly = []Week{}
	}
	return in
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// Top returns the first n contributors of the ranking.
func (in Insight) Top(n int) []Contributor {
	return in.Contributors[:min(n, len(in.Contributors))]
}
