package reconstruct

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrBusy means the maximum number of reconstructions is already running.
var ErrBusy = errors.New("reconstruct: too many reconstructions running")

// Session is one reconstruction. Its events are kept, so a viewer who connects
// late, or reconnects, replays from any point and then follows live.
type Session struct {
	ID      string    `json:"id"`
	Repo    string    `json:"repo"`
	Max     int       `json:"max_commits"`
	Created time.Time `json:"created"`

	mu      sync.Mutex
	events  []Event
	done    bool
	summary *Summary
	err     string
	changed chan struct{}
}

func (s *Session) emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) finish(sum *Summary, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	s.summary = sum
	if err != nil {
		s.err = err.Error()
		s.events = append(s.events, Event{T: "error", Msg: s.err})
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// Since returns the events from index from on, whether the run is over, and a
// channel that closes when more arrive.
func (s *Session) Since(from int) ([]Event, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = min(max(from, 0), len(s.events))
	return s.events[from:len(s.events):len(s.events)], s.done, s.changed
}

// Status is the session's state for the REST status endpoint.
type Status struct {
	ID      string    `json:"id"`
	Repo    string    `json:"repo"`
	Max     int       `json:"max_commits"`
	Created time.Time `json:"created"`
	State   string    `json:"state"` // running | done | failed
	Events  int       `json:"events"`
	Summary *Summary  `json:"summary,omitempty"`
	Error   string    `json:"error,omitempty"`
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{ID: s.ID, Repo: s.Repo, Max: s.Max, Created: s.Created, Events: len(s.events), Summary: s.summary, Error: s.err, State: "running"}
	if s.done {
		st.State = "done"
		if s.err != "" {
			st.State = "failed"
		}
	}
	return st
}

// Manager starts sessions and keeps them for a while after they end.
type Manager struct {
	Runner  *Runner
	MaxLive int
	Keep    time.Duration // how long a finished session stays readable
	Timeout time.Duration // a run's time limit
	Log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	live     int
}

// Start launches a reconstruction and returns at once.
func (m *Manager) Start(owner, name string, maxCommits int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
	}
	for id, s := range m.sessions {
		if st := s.Status(); st.State != "running" && time.Since(st.Created) > m.Keep {
			delete(m.sessions, id)
		}
	}
	if m.live >= m.MaxLive {
		return nil, ErrBusy
	}
	b := make([]byte, 16)
	rand.Read(b)
	s := &Session{ID: hex.EncodeToString(b), Repo: owner + "/" + name, Max: maxCommits, Created: time.Now().UTC(), changed: make(chan struct{})}
	m.sessions[s.ID] = s
	m.live++
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.Timeout)
		defer cancel()
		sum, err := m.Runner.Run(ctx, owner, name, maxCommits, s.emit)
		s.finish(sum, err)
		if m.Log != nil {
			m.Log.Info("reconstruction finished", "id", s.ID, "repo", s.Repo, "err", err)
		}
		m.mu.Lock()
		m.live--
		m.mu.Unlock()
	}()
	return s, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}
