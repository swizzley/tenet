package api

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/insight"
	"github.com/swizzley/tenet/internal/narrate"
	"github.com/swizzley/tenet/internal/store"
)

// Narrator writes the narrative (narrate.Narrator in production).
type Narrator interface {
	Narrate(ctx context.Context, payload map[string]any) (narrate.Narrative, error)
}

// Collector fetches a period from GitHub into the database.
type Collector interface {
	Collect(ctx context.Context, owner, name string, since, until time.Time) ([]collect.Record, collect.Meta, error)
}

// Service answers the insight questions: from the database when a stored fetch
// covers the period, from GitHub (through the double-buffered writer) otherwise.
type Service struct {
	Store     *store.Store
	Collector Collector
	Narrator  Narrator
	TTL       time.Duration

	flight singleflight.Group

	narrMu    sync.Mutex
	narrCache map[string]cachedNarrative
}

type cachedNarrative struct {
	at  time.Time
	out NarrativeResponse
}

// Period is one insight question.
type Period struct {
	Owner, Name  string
	Since, Until time.Time
}

func (p Period) Repo() string { return p.Owner + "/" + p.Name }
func (p Period) key() string {
	return p.Repo() + "|" + p.Since.UTC().Format(time.RFC3339) + "|" + p.Until.UTC().Format(time.RFC3339)
}

// Data is a period's commits and where they came from.
type Data struct {
	Records    []collect.Record
	Source     string // "github" or "database"
	Truncated  bool
	Collection *collect.Meta
}

// Load returns the period's commits. Concurrent requests for one period share one fetch.
func (s *Service) Load(ctx context.Context, p Period) (Data, error) {
	v, err, _ := s.flight.Do(p.key(), func() (any, error) {
		ok, truncated, err := s.Store.Covered(ctx, p.Repo(), p.Since, p.Until, s.TTL)
		if err != nil {
			return Data{}, err
		}
		d := Data{Source: "database", Truncated: truncated}
		if !ok {
			_, meta, err := s.Collector.Collect(ctx, p.Owner, p.Name, p.Since, p.Until)
			if err != nil {
				return Data{}, err
			}
			if err := s.Store.AddCoverage(ctx, p.Repo(), p.Since, p.Until, meta.Truncated); err != nil {
				return Data{}, err
			}
			d = Data{Source: "github", Truncated: meta.Truncated, Collection: &meta}
		}
		// The database is the source of truth for the answer, fetched now or before.
		d.Records, err = s.Store.Commits(ctx, p.Repo(), p.Since, p.Until)
		return d, err
	})
	if err != nil {
		return Data{}, err
	}
	return v.(Data), nil
}

// ContributorsResponse is the insight endpoint's body.
type ContributorsResponse struct {
	insight.Insight
	ContributorsTotal int           `json:"contributors_total"`
	Metric            string        `json:"metric"`
	Source            string        `json:"source"` // "github" or "database"
	Collection        *collect.Meta `json:"collection,omitempty"`
}

const metricDoc = "Who carried the period, two ways at once: commits (how often someone landed work on the default branch) and lines changed (additions + deletions, how much). Each contributor also carries large_commits (over 1,000 lines), which shows when a lines ranking rests on a few bulk or generated changes."

func (s *Service) Contributors(ctx context.Context, p Period, by string, limit int) (ContributorsResponse, error) {
	d, err := s.Load(ctx, p)
	if err != nil {
		return ContributorsResponse{}, err
	}
	in := insight.Compute(p.Repo(), d.Records, p.Since, p.Until, by, d.Truncated)
	total := len(in.Contributors)
	in.Contributors = in.Top(limit)
	return ContributorsResponse{Insight: in, ContributorsTotal: total, Metric: metricDoc, Source: d.Source, Collection: d.Collection}, nil
}

// NarrativeResponse is the narrative endpoint's body: the narrative and the exact
// document its evidence paths point into.
type NarrativeResponse struct {
	Narrative narrate.Narrative `json:"narrative"`
	Data      map[string]any    `json:"data"`
	Source    string            `json:"source"`
	Cached    bool              `json:"cached"`
}

func (s *Service) Narrative(ctx context.Context, p Period) (NarrativeResponse, error) {
	s.narrMu.Lock()
	if c, ok := s.narrCache[p.key()]; ok && time.Since(c.at) < s.TTL {
		s.narrMu.Unlock()
		c.out.Cached = true
		return c.out, nil
	}
	s.narrMu.Unlock()
	v, err, _ := s.flight.Do("narrative|"+p.key(), func() (any, error) {
		d, err := s.Load(ctx, p)
		if err != nil {
			return NarrativeResponse{}, err
		}
		in := insight.Compute(p.Repo(), d.Records, p.Since, p.Until, "commits", d.Truncated)
		payload := narrate.Payload(in, d.Records)
		n, err := s.Narrator.Narrate(ctx, payload)
		if err != nil {
			return NarrativeResponse{}, err
		}
		out := NarrativeResponse{Narrative: n, Data: payload, Source: d.Source}
		s.narrMu.Lock()
		if s.narrCache == nil {
			s.narrCache = map[string]cachedNarrative{}
		}
		for k, c := range s.narrCache {
			if time.Since(c.at) >= s.TTL {
				delete(s.narrCache, k)
			}
		}
		s.narrCache[p.key()] = cachedNarrative{time.Now(), out}
		s.narrMu.Unlock()
		return out, nil
	})
	if err != nil {
		return NarrativeResponse{}, err
	}
	return v.(NarrativeResponse), nil
}
