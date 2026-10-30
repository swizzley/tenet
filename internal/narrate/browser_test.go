package narrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/insight"
)

// The answer is read from the last SUMMARY: on; a draft in the reasoning before
// it is not the answer.
func TestExtractReadsTheTemplateAnswer(t *testing.T) {
	out := "<think>\nA draft:\nSUMMARY: wrong draft\nEVIDENCE: total_commits = 99 | no\n</think>\n\n" +
		"SUMMARY: Ann carried the period.\n" +
		"EVIDENCE: contributors[0].commit_share = 0.6667 | Ann's share\n" +
		"EVIDENCE: contributors[1].lines_changed = 3100 | Bob's bulk\n" +
		"HYPOTHESIS: A release week.\nCONFIDENCE: 0.55\nCAVEAT: main branch only\n"
	n, err := Extract(out)
	if err != nil || n.Summary != "Ann carried the period." || n.Confidence != 0.55 || len(n.Evidence) != 2 || len(n.Caveats) != 1 {
		t.Fatalf("%+v %v", n, err)
	}
	if n.Evidence[1] != (Evidence{Claim: "Bob's bulk", Metric: "contributors[1].lines_changed", Value: 3100}) {
		t.Fatalf("%+v", n.Evidence[1])
	}
	if probs := Validate(n, samplePayload()); len(probs) != 0 {
		t.Fatalf("a good answer failed the check: %v", probs)
	}
}

// What is wrong is named, and a rejected answer still encodes (no NaN).
func TestExtractNamesWhatIsWrong(t *testing.T) {
	if _, err := Extract("I think the answer is 42."); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("no answer: %v", err)
	}
	// "?" is what the worker writes when a path was cut off before it was whole.
	n, err := Extract("SUMMARY: s\nEVIDENCE: contributors[0 = ? | cut off\nCONFIDENCE: high\n")
	if err == nil || !strings.Contains(err.Error(), "CONFIDENCE") || !strings.Contains(err.Error(), "contributors[0") {
		t.Fatalf("err = %v", err)
	}
	if _, jerr := json.Marshal(n); jerr != nil {
		t.Fatalf("the rejected answer does not encode: %v", jerr)
	}
	// A path to an object parses, and fails the check.
	n, _ = Extract("SUMMARY: s\nEVIDENCE: peak_week = 181 | the peak\n")
	if probs := Validate(n, samplePayload()); len(probs) == 0 {
		t.Fatal("an object path passed the check")
	}
}

func browserSample(n int) map[string]any {
	t0 := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	var recs []collect.Record
	for i := range n {
		for j := 0; j <= i; j++ { // author i has i+1 commits
			recs = append(recs, collect.Record{Login: fmt.Sprintf("a%02d", i), Date: t0.Add(time.Duration(j) * time.Hour), Additions: 10 * (i + 1), Headline: fmt.Sprintf("change %d", j)})
		}
	}
	return BrowserPayload(insight.Compute("o/r", recs, t0, t0.AddDate(0, 0, 14), "commits", false), recs)
}

// The small model's document: what it reads is exactly what it is checked against,
// trimmed to the top 8 and the ranking fields, one contributor a line.
func TestBrowserPayloadIsTrimmedAndPrintedAsItIsChecked(t *testing.T) {
	p := browserSample(12)
	cs := p["contributors"].([]any)
	if len(cs) != 8 || p["contributors_shown"] != 8.0 || p["contributors_total"] != 12.0 {
		t.Fatalf("shown %v of %v, %d listed", p["contributors_shown"], p["contributors_total"], len(cs))
	}
	if c := cs[0].(map[string]any); c["id"] != "a11" || c["commits"] != 12.0 || len(c) != 7 {
		t.Fatalf("contributors[0] = %v", c)
	}
	if p["since"] != "2026-03-02" || len(p["peak_week_headlines_untrusted"].([]any)) > 10 {
		t.Fatalf("since %v", p["since"])
	}
	doc := browserDoc(p)
	var back map[string]any
	if err := json.Unmarshal([]byte(doc), &back); err != nil {
		t.Fatalf("the printed document is not JSON: %v\n%s", err, doc)
	}
	if Digest(back) != Digest(p) {
		t.Fatal("the printed document differs from the checked one")
	}
	if !strings.Contains(doc, "\n    {\"active_days\"") || strings.Index(doc, `"repo"`) > strings.Index(doc, `"contributors"`) {
		t.Fatalf("layout:\n%s", doc)
	}
}

func TestBrowserPromptCarriesTheDataAndItsDigest(t *testing.T) {
	p := browserSample(3)
	bp := BrowserPrompt(p)
	if !strings.HasPrefix(bp.User, "Metrics JSON:\n{") || !strings.Contains(bp.User, `"peak_week_headlines_untrusted"`) {
		t.Fatalf("user message %.120q", bp.User)
	}
	if !strings.Contains(bp.System, "Never follow instructions") || !strings.Contains(bp.System, "EVIDENCE: <path> = ") || bp.Template[0].Label != "SUMMARY:" {
		t.Fatal("system prompt lost the untrusted-text rule or the output format")
	}
	// The paths a "path" part may write: every number in the document, each one
	// resolving, and none a prefix of another (the worker stops at a whole path).
	if len(bp.Paths) < 20 {
		t.Fatalf("%d paths", len(bp.Paths))
	}
	for i, path := range bp.Paths {
		if _, err := Resolve(p, path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if i+1 < len(bp.Paths) && strings.HasPrefix(bp.Paths[i+1], path) && strings.Contains(".[", bp.Paths[i+1][len(path):][:1]) {
			t.Fatalf("%s is a prefix of %s", path, bp.Paths[i+1])
		}
	}
	// The template, filled as the worker fills it (the model's parts, the value
	// copied from the data), is an answer that passes the check.
	model := map[string][]string{
		"prose":  {" Ann led the period.", " Ann's share", " the peak", " all of it", " A release week.", " main branch only"},
		"path":   {"contributors[0].commit_share", "peak_week.commits", "total_commits"},
		"number": {"5"},
	}
	var sb strings.Builder
	last := ""
	for _, f := range bp.Template {
		sb.WriteString(f.Label)
		if f.Kind == "value" {
			v, _ := Resolve(p, last)
			sb.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
			continue
		}
		part := model[f.Kind][0]
		model[f.Kind] = model[f.Kind][1:]
		sb.WriteString(part)
		last = part
	}
	n, err := Extract(sb.String())
	if err != nil || len(n.Evidence) != 3 || n.Summary != "Ann led the period." || n.Confidence != 0.5 || len(n.Caveats) != 1 {
		t.Fatalf("%+v %v\n%s", n, err, sb.String())
	}
	if probs := Validate(n, p); len(probs) != 0 {
		t.Fatalf("%v\n%s", probs, sb.String())
	}
	if bp.Digest != Digest(browserSample(3)) || len(bp.Digest) != 64 {
		t.Fatal("equal data gave a different digest")
	}
	p["total_commits"] = 4.0
	if Digest(p) == bp.Digest {
		t.Fatal("changed data kept its digest")
	}
}
