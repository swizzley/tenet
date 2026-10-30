package api

import (
	"context"
	"errors"
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
	Narrator  Narrator // nil without an Anthropic key: the narrative runs in the browser only
	Browser   BrowserModel
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

// ErrNoServerNarrator: no Anthropic key is configured.
var ErrNoServerNarrator = errors.New("no anthropic_api_key is configured; the page writes the narrative in the browser")

// ErrDataChanged: the period's data changed between the prompt and the check.
var ErrDataChanged = errors.New("the period's data changed since the prompt was built")

func (s *Service) Narrative(ctx context.Context, p Period) (NarrativeResponse, error) {
	if s.Narrator == nil {
		return NarrativeResponse{}, ErrNoServerNarrator
	}
	s.narrMu.Lock()
	if c, ok := s.narrCache[p.key()]; ok && time.Since(c.at) < s.TTL {
		s.narrMu.Unlock()
		c.out.Cached = true
		return c.out, nil
	}
	s.narrMu.Unlock()
	v, err, _ := s.flight.Do("narrative|"+p.key(), func() (any, error) {
		payload, src, err := s.payload(ctx, p, false)
		if err != nil {
			return NarrativeResponse{}, err
		}
		n, err := s.Narrator.Narrate(ctx, payload)
		if err != nil {
			return NarrativeResponse{}, err
		}
		out := NarrativeResponse{Narrative: n, Data: payload, Source: src}
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

// BrowserModel is the model the page runs over WebGPU.
type BrowserModel struct {
	ID           string `json:"id"`
	Revision     string `json:"revision"`
	DType        string `json:"dtype"`
	MaxNewTokens int    `json:"max_new_tokens"`
}

// PromptResponse is what the page needs to write the narrative in the browser.
type PromptResponse struct {
	narrate.Prompt
	Model  BrowserModel   `json:"model"`
	Data   map[string]any `json:"data"`
	Source string         `json:"source"`
}

// payload is the document a narrative reads: the full one for Claude, the
// trimmed one for the small browser model.
func (s *Service) payload(ctx context.Context, p Period, browser bool) (map[string]any, string, error) {
	d, err := s.Load(ctx, p)
	if err != nil {
		return nil, "", err
	}
	in := insight.Compute(p.Repo(), d.Records, p.Since, p.Until, "commits", d.Truncated)
	if browser {
		return narrate.BrowserPayload(in, d.Records), d.Source, nil
	}
	return narrate.Payload(in, d.Records), d.Source, nil
}

// NarrativePrompt builds the browser model's prompt over the period's data.
func (s *Service) NarrativePrompt(ctx context.Context, p Period) (PromptResponse, error) {
	payload, src, err := s.payload(ctx, p, true)
	if err != nil {
		return PromptResponse{}, err
	}
	return PromptResponse{Prompt: narrate.BrowserPrompt(payload), Model: s.Browser, Data: payload, Source: src}, nil
}

// VerifyNarrative checks a browser model's raw output against the period's data,
// rebuilt here: the page sends only text, never the numbers it is checked against.
// It returns *narrate.UnsupportedError when the answer fails the check.
func (s *Service) VerifyNarrative(ctx context.Context, p Period, digest, output string, attempt int) (NarrativeResponse, error) {
	payload, src, err := s.payload(ctx, p, true)
	if err != nil {
		return NarrativeResponse{}, err
	}
	if narrate.Digest(payload) != digest {
		return NarrativeResponse{}, ErrDataChanged
	}
	n, err := narrate.Extract(output)
	if err != nil {
		return NarrativeResponse{}, &narrate.UnsupportedError{Problems: []string{err.Error()}, Rejected: n}
	}
	n.Model, n.Attempts = "browser:"+s.Browser.ID, attempt
	if probs := narrate.Validate(n, payload); len(probs) > 0 {
		return NarrativeResponse{}, &narrate.UnsupportedError{Problems: probs, Rejected: n}
	}
	n.Validated = true
	return NarrativeResponse{Narrative: n, Data: payload, Source: src}, nil
}
