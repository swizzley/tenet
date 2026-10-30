package insight

import (
	"testing"
	"time"

	"github.com/swizzley/tenet/internal/collect"
)

var t0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC) // a Monday

func rec(login, name string, day int, add, del int) collect.Record {
	return collect.Record{Login: login, Name: name, Date: t0.AddDate(0, 0, day), Additions: add, Deletions: del}
}

func fixture() []collect.Record {
	return []collect.Record{
		rec("ann", "Ann", 0, 10, 2),
		rec("ann", "Ann", 0, 5, 5),
		rec("ann", "Ann A.", 1, 1, 0),
		rec("bob", "Bob", 2, 3000, 100), // one bulk change
		rec("", "Cy", 8, 4, 4),          // unmapped author, second week
		rec("", "Cy", 9, 2, 0),
	}
}

// Hand-checked numbers: the totals add up and every share is right.
func TestComputeByCommits(t *testing.T) {
	in := Compute("o/r", fixture(), t0, t0.AddDate(0, 0, 14), "", false)
	if in.By != "commits" || in.TotalCommits != 6 || in.TotalLines != 12+10+1+3100+8+2 {
		t.Fatalf("totals: %+v", in)
	}
	got := []string{}
	for _, c := range in.Contributors {
		got = append(got, c.ID)
	}
	if want := []string{"ann", "~Cy", "bob"}; !eq(got, want) {
		t.Fatalf("ranking %v, want %v", got, want)
	}
	ann := in.Contributors[0]
	if ann.Commits != 3 || ann.Additions != 16 || ann.Deletions != 7 || ann.LinesChanged != 23 || ann.ActiveDays != 2 || ann.CommitShare != 0.5 {
		t.Fatalf("ann %+v", ann)
	}
	if in.Top1Share != 0.5 || in.BusFactor50 != 1 || in.LargeCommits != 1 {
		t.Fatalf("concentration %v %v %v", in.Top1Share, in.BusFactor50, in.LargeCommits)
	}
	if len(in.Weekly) != 2 || in.Weekly[0].Commits != 4 || in.Weekly[0].Top != "ann" || in.Weekly[1].Commits != 2 || in.Weekly[1].Top != "~Cy" {
		t.Fatalf("weekly %+v", in.Weekly)
	}
	if in.PeakWeek == nil || !in.PeakWeek.Start.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("peak %+v", in.PeakWeek)
	}
	// commit sizes sorted: 1 2 8 10 12 3100
	if in.MedianWeekly != 3 || in.MedianCommit != (8+10)/2.0 {
		t.Fatalf("medians %v %v", in.MedianWeekly, in.MedianCommit)
	}
	sumShare := 0.0
	sumCommits := 0
	for _, c := range in.Contributors {
		sumShare += c.CommitShare
		sumCommits += c.Commits
	}
	if sumCommits != in.TotalCommits || sumShare < 0.999 || sumShare > 1.001 {
		t.Fatalf("shares do not add up: %d commits, %.4f share", sumCommits, sumShare)
	}
}

func TestComputeByLinesFlagsBulk(t *testing.T) {
	in := Compute("o/r", fixture(), t0, t0.AddDate(0, 0, 14), "lines", false)
	if in.Contributors[0].ID != "bob" || in.Contributors[0].LargeCommits != 1 || in.Contributors[0].LinesShare != 0.9895 { // 3100 / 3133
		t.Fatalf("%+v", in.Contributors[0])
	}
	// Concentration stays about commits whatever the ranking.
	if in.Top1Share != 0.5 {
		t.Fatalf("top1 share %v", in.Top1Share)
	}
}

func TestComputeTiesAreDeterministic(t *testing.T) {
	recs := []collect.Record{rec("zed", "Z", 0, 1, 0), rec("amy", "A", 0, 1, 0)}
	for range 20 {
		in := Compute("o/r", recs, t0, t0.AddDate(0, 0, 1), "commits", false)
		if in.Contributors[0].ID != "amy" {
			t.Fatalf("tie order %v", in.Contributors)
		}
	}
}

func TestComputeEmpty(t *testing.T) {
	in := Compute("o/r", nil, t0, t0, "commits", false)
	if in.TotalCommits != 0 || in.Contributors == nil || in.Weekly == nil || in.PeakWeek != nil || len(in.Top(5)) != 0 {
		t.Fatalf("%+v", in)
	}
}

func TestWeekStartIsMondayUTC(t *testing.T) {
	sun := time.Date(2026, 3, 8, 23, 0, 0, 0, time.UTC)
	if ws := weekStart(sun); !ws.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week of Sunday = %v", ws)
	}
	// Monday 01:00 in UTC+3 is still Sunday in UTC.
	loc := time.FixedZone("+3", 3*3600)
	if ws := weekStart(time.Date(2026, 3, 9, 1, 0, 0, 0, loc)); !ws.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v", ws)
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
