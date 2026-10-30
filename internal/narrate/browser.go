package narrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/swizzley/tenet/internal/collect"
	"github.com/swizzley/tenet/internal/insight"
)

// The browser path: the page runs a small reasoning model over WebGPU. The server
// hands it the prompt and later checks its raw output with the same Validate the
// Claude path uses. The model's text is untrusted input to the server, like any
// request body: Extract parses it, and nothing in it is executed or rendered as HTML.

// BrowserPayload is the small model's document: the same metrics as Payload,
// trimmed so a 1.5B model can hold it. The top 8 contributors with the ranking
// fields only, the last 12 weeks with plain dates, 10 headlines. With the full
// payload VibeThinker-1.5B spent its whole budget re-reading the 15-contributor
// list, misread array indices, and took contributors_total (people) for a commit
// count (VERIFIED live 2026-10-30, golang/go October, two attempts withheld).
func BrowserPayload(in insight.Insight, recs []collect.Record) map[string]any {
	m := Payload(in, recs)
	keep := []string{"id", "commits", "lines_changed", "commit_share", "lines_share", "large_commits", "active_days"}
	cs, _ := m["contributors"].([]any)
	cs = cs[:min(8, len(cs))]
	for i, c := range cs {
		full, _ := c.(map[string]any)
		slim := map[string]any{}
		for _, k := range keep {
			slim[k] = full[k]
		}
		cs[i] = slim
	}
	m["contributors"] = cs
	m["contributors_shown"] = float64(len(cs))
	ws, _ := m["weekly"].([]any)
	ws = ws[max(0, len(ws)-12):]
	for _, w := range ws {
		if w, ok := w.(map[string]any); ok {
			w["start"] = dateOnly(w["start"])
		}
	}
	if pw, ok := m["peak_week"].(map[string]any); ok {
		pw["start"] = dateOnly(pw["start"])
	}
	m["weekly"] = ws
	m["since"], m["until"] = dateOnly(m["since"]), dateOnly(m["until"])
	if h, ok := m["peak_week_headlines_untrusted"].([]any); ok {
		m["peak_week_headlines_untrusted"] = h[:min(10, len(h))]
	}
	return roundTrip(m)
}

func dateOnly(v any) any {
	if s, ok := v.(string); ok && len(s) >= 10 {
		return s[:10]
	}
	return v
}

// browserDoc prints the payload for a small model: valid JSON, one array element
// a line, so "contributors[3]" is the fourth line of the list.
func browserDoc(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	order := map[string]int{"repo": 0, "since": 1, "until": 2, "total_commits": 3, "total_lines_changed": 4,
		"contributors_total": 5, "contributors_shown": 6, "contributors": 7}
	slices.SortFunc(keys, func(a, b string) int {
		oa, okA := order[a]
		ob, okB := order[b]
		switch {
		case okA && okB:
			return oa - ob
		case okA:
			return -1
		case okB:
			return 1
		}
		return strings.Compare(a, b)
	})
	var sb strings.Builder
	sb.WriteString("{\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		sb.WriteString("  " + string(kb) + ": ")
		if arr, ok := payload[k].([]any); ok && len(arr) > 0 {
			sb.WriteString("[\n")
			for j, e := range arr {
				eb, _ := json.Marshal(e)
				sb.WriteString("    " + string(eb))
				if j < len(arr)-1 {
					sb.WriteString(",")
				}
				sb.WriteString("\n")
			}
			sb.WriteString("  ]")
		} else {
			vb, _ := json.Marshal(payload[k])
			sb.Write(vb)
		}
		if i < len(keys)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}")
	return sb.String()
}

// browserFormat tells a model without structured output how to end its answer.
const browserFormat = `

How to read the JSON: "contributors" lists the top contributors_shown of the contributors_total people, one a line, ranked by commits; contributors[0] is the first line. "weekly" holds one week a line.

Output format: reason briefly. Then the answer is written in these lines:
SUMMARY: <2 to 4 sentences>
EVIDENCE: <path> = <the number at that path> | <the claim it supports>
HYPOTHESIS: <one or two sentences>
CONFIDENCE: <one number from 0 to 1: your own probability that the hypothesis is right>
CAVEAT: <one line>
Every number in any line is checked against the JSON. Copy numbers from it as numerals; never compute a new one (the shares and means you may want are in it).
A path names one number: for example contributors[0].commits, peak_week.lines_changed, weekly[2].commits or total_commits.`

// Field is one part of the answer. The page's worker writes Label, then the
// part by its Kind:
//   - "prose": the model writes to the end of the line (no "=", "|", "[",
//     "]", and numbers only as numerals, which Validate checks);
//   - "path": the model writes a path, held to the document's numeric paths;
//   - "value": the worker writes the number at the path just written;
//   - "number": the model writes digits (the confidence, after its label's "0.").
type Field struct {
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	MaxTokens int    `json:"max_tokens"`
}

// Template is the answer the worker leads a small model through after it
// reasons. Free answers from VibeThinker-1.5B failed to parse, and its paths and
// numbers were often wrong (2026-10-30, eleven live runs). Led and held this
// way, every path exists and every value is the data's. The server still checks
// all of it, the numbers in the prose included.
var Template = []Field{
	{"SUMMARY:", "prose", 160},
	{"\nEVIDENCE: ", "path", 24}, {" = ", "value", 0}, {" | ", "prose", 40},
	{"\nEVIDENCE: ", "path", 24}, {" = ", "value", 0}, {" | ", "prose", 40},
	{"\nEVIDENCE: ", "path", 24}, {" = ", "value", 0}, {" | ", "prose", 40},
	{"\nHYPOTHESIS:", "prose", 100},
	{"\nCONFIDENCE: 0.", "number", 3},
	{"\nCAVEAT:", "prose", 60},
}

// Prompt is everything a client-side model needs to write the narrative, and the
// digest of the data it reads, so the check later runs against the same data.
type Prompt struct {
	System   string   `json:"system"`
	User     string   `json:"user"`
	Template []Field  `json:"template"`
	Paths    []string `json:"paths"` // every path in the document that holds a number
	Digest   string   `json:"digest"`
}

// BrowserPrompt builds the prompt for a client-side model over payload (a
// BrowserPayload).
func BrowserPrompt(payload map[string]any) Prompt {
	return Prompt{System: system + browserFormat, User: userMessage([]byte(browserDoc(payload))),
		Template: Template, Paths: NumericPaths(payload), Digest: Digest(payload)}
}

// NumericPaths lists every path in doc that holds a number, in Resolve's form.
func NumericPaths(doc map[string]any) []string {
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case float64:
			out = append(out, prefix)
		case map[string]any:
			for k, x := range v {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, x)
			}
		case []any:
			for i, x := range v {
				walk(fmt.Sprintf("%s[%d]", prefix, i), x)
			}
		}
	}
	walk("", doc)
	slices.Sort(out)
	return out
}

func userMessage(doc []byte) string { return "Metrics JSON:\n" + string(doc) }

// RetryMessage asks the model to answer again, listing what failed the check.
func RetryMessage(problems []string) string {
	return "Your answer failed the evidence check:\n- " + strings.Join(problems, "\n- ") +
		"\nAnswer again. Cite only paths that exist in the metrics JSON, with the exact numbers found there."
}

// Digest identifies a payload. encoding/json sorts map keys, so equal data gives
// equal bytes.
func Digest(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ErrNoAnswer: the output holds no answer.
var ErrNoAnswer = errors.New("the output holds no answer: no line starts with SUMMARY:")

var (
	summaryRe  = regexp.MustCompile(`(?m)^SUMMARY:`)
	lineRe     = regexp.MustCompile(`^(SUMMARY|EVIDENCE|HYPOTHESIS|CONFIDENCE|CAVEAT):\s*(.*)$`)
	evidenceRe = regexp.MustCompile(`^(\S+) = (\S+) \| (.*)$`)
)

// Extract reads the answer the page's worker led the model through (Template),
// from the last line that starts with SUMMARY: (the reasoning before it may
// hold drafts). A malformed line is named, and the confidence is never NaN, which
// encoding/json refuses.
func Extract(output string) (Narrative, error) {
	locs := summaryRe.FindAllStringIndex(output, -1)
	if locs == nil {
		return Narrative{}, ErrNoAnswer
	}
	var n Narrative
	var probs []string
	for _, ln := range strings.Split(output[locs[len(locs)-1][0]:], "\n") {
		m := lineRe.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		val := strings.TrimSpace(m[2])
		switch m[1] {
		case "SUMMARY":
			n.Summary = val
		case "HYPOTHESIS":
			n.Hypothesis = val
		case "CAVEAT":
			n.Caveats = append(n.Caveats, val)
		case "CONFIDENCE":
			c, err := strconv.ParseFloat(val, 64)
			if err != nil {
				probs = append(probs, fmt.Sprintf("CONFIDENCE %q is not a number", val))
				continue
			}
			n.Confidence = c
		case "EVIDENCE":
			e := evidenceRe.FindStringSubmatch(val)
			var v float64
			var err error
			if e != nil {
				v, err = strconv.ParseFloat(e[2], 64)
			}
			if e == nil || err != nil {
				probs = append(probs, fmt.Sprintf("EVIDENCE %q is not <path> = <number> | <claim>", val))
				continue
			}
			n.Evidence = append(n.Evidence, Evidence{Claim: strings.TrimSpace(e[3]), Metric: e[1], Value: v})
		}
	}
	if len(probs) > 0 {
		return n, errors.New(strings.Join(probs, "; "))
	}
	return n, nil
}
