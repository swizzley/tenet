package collect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/swizzley/tenet/internal/dbuf"
	"github.com/swizzley/tenet/internal/github"
)

// fakeHistory serves total commits in pages of the requested size.
type fakeHistory struct {
	total int
	asked []int
	err   error
}

func (f *fakeHistory) History(_ context.Context, owner, name string, since, until time.Time, first int, cursor string) ([]github.HistoryNode, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	f.asked = append(f.asked, first)
	off := 0
	if cursor != "" {
		fmt.Sscan(cursor, &off)
	}
	var out []github.HistoryNode
	for i := off; i < min(off+first, f.total); i++ {
		n := github.HistoryNode{OID: fmt.Sprintf("%040d", i), Additions: i, Deletions: 1, MessageHeadline: "fix\x1b[31m bug"}
		n.Author.Name = "dev\n" + fmt.Sprint(i%3)
		if i%2 == 0 {
			n.Author.User = &struct {
				Login string `json:"login"`
			}{"user" + fmt.Sprint(i%3)}
		}
		out = append(out, n)
	}
	next := ""
	if off+first < f.total {
		next = fmt.Sprint(off + first)
	}
	return out, next, nil
}

type batches struct {
	mu   sync.Mutex
	got  [][]Record
	fail error
}

func (b *batches) write(r []Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.got = append(b.got, append([]Record(nil), r...))
	return nil
}

// Every commit arrives once, each page is one buffer half (one write batch), and
// Collect returns only after every batch is written.
func TestCollectPagesFillBufferHalves(t *testing.T) {
	var b batches
	sink := dbuf.New(b.write, 100)
	defer sink.Close()
	f := &fakeHistory{total: 250}
	c := &Collector{GH: f, Sink: sink, MaxCommits: 10_000}
	recs, m, err := c.Collect(context.Background(), "o", "r", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 250 || m.Pages != 3 || m.Truncated {
		t.Fatalf("got %d records, meta %+v", len(recs), m)
	}
	if fmt.Sprint(m.PageSizes) != "[100 100 50]" || fmt.Sprint(f.asked) != "[100 100 100]" {
		t.Fatalf("page sizes %v, asked %v", m.PageSizes, f.asked)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.got) != 3 || len(b.got[0]) != 100 || len(b.got[2]) != 50 {
		t.Fatalf("batches at return: %d", len(b.got))
	}
	seen := map[string]bool{}
	for _, batch := range b.got {
		for _, r := range batch {
			if seen[r.SHA] {
				t.Fatalf("duplicate %s", r.SHA)
			}
			seen[r.SHA] = true
		}
	}
	if len(seen) != 250 {
		t.Fatalf("sink saw %d distinct commits", len(seen))
	}
}

func TestCollectStopsAtCapAndSaysSo(t *testing.T) {
	var b batches
	sink := dbuf.New(b.write, 100)
	defer sink.Close()
	f := &fakeHistory{total: 1000}
	c := &Collector{GH: f, Sink: sink, MaxCommits: 150}
	recs, m, err := c.Collect(context.Background(), "o", "r", time.Time{}, time.Now())
	if err != nil || len(recs) != 150 || !m.Truncated {
		t.Fatalf("%d records, %+v, %v", len(recs), m, err)
	}
	if fmt.Sprint(f.asked) != "[100 50]" {
		t.Fatalf("asked %v: the last page should ask only for what fits under the cap", f.asked)
	}
}

func TestCollectCleansUntrustedText(t *testing.T) {
	var b batches
	sink := dbuf.New(b.write, 100)
	defer sink.Close()
	c := &Collector{GH: &fakeHistory{total: 2}, Sink: sink, MaxCommits: 10}
	recs, _, _ := c.Collect(context.Background(), "o", "r", time.Time{}, time.Now())
	if recs[0].Name != "dev 0" || recs[0].Headline != "fix [31m bug" || recs[0].Login != "user0" || recs[1].Login != "" {
		t.Fatalf("%+v", recs)
	}
}

func TestCollectPassesErrors(t *testing.T) {
	var b batches
	sink := dbuf.New(b.write, 100)
	defer sink.Close()
	c := &Collector{GH: &fakeHistory{err: github.ErrNotFound}, Sink: sink, MaxCommits: 10}
	if _, _, err := c.Collect(context.Background(), "o", "r", time.Time{}, time.Now()); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	// A failed database write fails the collection: nothing is reported as stored.
	disk := errors.New("disk full")
	b.fail = disk
	c.GH = &fakeHistory{total: 5}
	if _, _, err := c.Collect(context.Background(), "o", "r", time.Time{}, time.Now()); !errors.Is(err, disk) {
		t.Fatalf("err = %v, want the write error", err)
	}
}
