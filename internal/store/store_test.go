package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/dbuf"
)

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "tenet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func recs(repo string, n int) []collect.Record {
	out := make([]collect.Record, n)
	for i := range out {
		out[i] = collect.Record{Repo: repo, SHA: fmt.Sprintf("%040x", i), Login: "u", Name: "U",
			Date: t0.Add(time.Duration(i) * time.Hour), Additions: i, Deletions: 1, Headline: "h"}
	}
	return out
}

func TestUpsertAndPeriodQuery(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.UpsertCommits(recs("o/r", 48)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCommits(recs("o/other", 5)); err != nil {
		t.Fatal(err)
	}
	// Re-upserting the same shas changes values, never the row count.
	again := recs("o/r", 2)
	again[0].Additions = 999
	if err := s.UpsertCommits(again); err != nil {
		t.Fatal(err)
	}
	got, err := s.Commits(ctx, "o/r", t0, t0.Add(24*time.Hour))
	if err != nil || len(got) != 24 {
		t.Fatalf("%d rows, %v; want 24 (since inclusive, until exclusive)", len(got), err)
	}
	all, _ := s.Commits(ctx, "o/r", t0.Add(-time.Hour), t0.Add(100*time.Hour))
	if len(all) != 48 {
		t.Fatalf("%d rows, want 48", len(all))
	}
	last := all[len(all)-1]
	if last.SHA != fmt.Sprintf("%040x", 0) || last.Additions != 999 || !last.Date.Equal(t0) {
		t.Fatalf("oldest row %+v", last)
	}
}

func TestCoverageRules(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := t0.Add(30 * 24 * time.Hour)
	s.now = func() time.Time { return now }
	ttl := 10 * time.Minute

	past := [2]time.Time{t0, t0.Add(7 * 24 * time.Hour)} // ended long before the fetch
	live := [2]time.Time{t0, now.Add(-10 * time.Minute)} // reaches into the last hour
	s.AddCoverage(ctx, "o/r", past[0], past[1], false)
	s.AddCoverage(ctx, "o/r", live[0], live[1], false)

	check := func(label string, since, until time.Time, want bool) {
		t.Helper()
		ok, _, err := s.Covered(ctx, "o/r", since, until, ttl)
		if err != nil || ok != want {
			t.Errorf("%s: covered = %v, %v; want %v", label, ok, err, want)
		}
	}
	check("exact past period", past[0], past[1], true)
	check("inside a past period", past[0].Add(time.Hour), past[1].Add(-time.Hour), true)
	check("starts before every fetch", past[0].Add(-time.Hour), past[1], false)
	check("live period within its TTL", live[0], live[1], true)

	now = now.Add(time.Hour) // an hour later
	check("settled past period, later", past[0], past[1], true)
	check("live period past its TTL", live[0], live[1], false)

	if ok, _, _ := s.Covered(ctx, "o/zzz", past[0], past[1], ttl); ok {
		t.Error("coverage leaked across repos")
	}
}

func TestCoverageReportsTruncation(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.AddCoverage(ctx, "o/r", t0, t0.Add(time.Hour), true)
	ok, tr, _ := s.Covered(ctx, "o/r", t0, t0.Add(time.Hour), time.Hour)
	if !ok || !tr {
		t.Fatalf("ok=%v truncated=%v", ok, tr)
	}
	s.AddCoverage(ctx, "o/r", t0, t0.Add(time.Hour), false)
	if _, tr, _ = s.Covered(ctx, "o/r", t0, t0.Add(time.Hour), time.Hour); tr {
		t.Fatal("a complete fetch should win over a truncated one")
	}
}

func TestDetailCache(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, ok, err := s.Detail(ctx, "o/r", "a"); ok || err != nil {
		t.Fatalf("empty cache: %v %v", ok, err)
	}
	s.PutDetail(ctx, "o/r", "a", []byte(`{"v":1}`))
	s.PutDetail(ctx, "o/r", "a", []byte(`{"v":2}`)) // immutable: the first write stays
	b, ok, err := s.Detail(ctx, "o/r", "a")
	if !ok || err != nil || string(b) != `{"v":1}` {
		t.Fatalf("%s %v %v", b, ok, err)
	}
}

// The double buffer drives SQLite as its single writer while readers query in
// parallel: every record lands, and readers never see an error.
func TestDoubleBufferIntoSQLiteWithConcurrentReaders(t *testing.T) {
	s := open(t)
	w := dbuf.New(s.UpsertCommits, 100)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := s.Commits(ctx, "o/r", t0, t0.Add(1e6*time.Hour)); err != nil && ctx.Err() == nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for _, r := range recs("o/r", 5000) {
		if err := w.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	cancel()
	wg.Wait()
	w.Close()
	got, _ := s.Commits(context.Background(), "o/r", t0, t0.Add(1e6*time.Hour))
	st := w.Stats()
	if len(got) != 5000 || st.Batches != 50 || st.FailedBatches != 0 {
		t.Fatalf("%d rows, stats %+v", len(got), st)
	}
	t.Logf("5000 rows in %d transactions, producer waits %d, write time %v", st.Batches, st.ProducerWaits, time.Duration(st.WriteNs))
}
