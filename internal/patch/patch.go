// Package patch applies the unified-diff hunks GitHub returns for one file of one
// commit, exactly and strictly: every context and removed line must match the old
// content byte for byte, or Apply fails. It never guesses (no fuzz, no offsets
// beyond those the earlier hunks imply), so a file it produces is either exact or
// reported as broken. BlobSHA gives git's object id for the result, which the caller
// compares with GitHub's blob id to prove the reconstruction.
package patch

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// BlobSHA is git's object id for a blob with this content.
func BlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

type hunk struct {
	oldStart, oldLen int
	newStart, newLen int
	lines            []hline
}

type hline struct {
	op   byte   // ' ', '-', '+'
	text string // the line including its "\n", unless the file ends without one
}

// ConflictError means the patch does not fit the old content.
type ConflictError struct {
	Hunk int
	Line int // 1-based line in the old file
	Want string
	Got  string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("patch: hunk %d does not apply at old line %d: want %q, got %q", e.Hunk, e.Line, e.Want, e.Got)
}

func parse(p string) ([]hunk, error) {
	var hs []hunk
	raw := strings.SplitAfter(p, "\n")
	for i := 0; i < len(raw); i++ {
		l := raw[i]
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "@@ ") {
			h, err := parseHeader(strings.TrimRight(l, "\n"))
			if err != nil {
				return nil, err
			}
			hs = append(hs, h)
			continue
		}
		if len(hs) == 0 {
			// GitHub's patch starts at the first hunk; tolerate diff headers if present.
			if strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "index ") ||
				strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") ||
				strings.HasPrefix(l, "new file") || strings.HasPrefix(l, "deleted file") {
				continue
			}
			return nil, fmt.Errorf("patch: text before the first hunk: %q", l)
		}
		h := &hs[len(hs)-1]
		switch l[0] {
		case ' ', '-', '+':
			text := l[1:]
			if !strings.HasSuffix(text, "\n") {
				// GitHub drops the newline of the patch's very last line; the line
				// itself still had one unless a "\ No newline" marker follows.
				text += "\n"
			}
			h.lines = append(h.lines, hline{l[0], text})
		case '\\':
			// "\ No newline at end of file": the previous line has no "\n".
			if n := len(h.lines); n > 0 {
				h.lines[n-1].text = strings.TrimSuffix(h.lines[n-1].text, "\n")
			}
		default:
			return nil, fmt.Errorf("patch: bad line %q", l)
		}
	}
	for i, h := range hs {
		var o, n int
		for _, l := range h.lines {
			if l.op != '+' {
				o++
			}
			if l.op != '-' {
				n++
			}
		}
		if o != h.oldLen || n != h.newLen {
			return nil, fmt.Errorf("patch: hunk %d counts %d/%d lines, header says %d/%d (truncated patch?)", i+1, o, n, h.oldLen, h.newLen)
		}
	}
	return hs, nil
}

// parseHeader reads "@@ -a,b +c,d @@ section".
func parseHeader(l string) (hunk, error) {
	var h hunk
	rest, ok := strings.CutPrefix(l, "@@ -")
	if !ok {
		return h, fmt.Errorf("patch: bad hunk header %q", l)
	}
	end := strings.Index(rest, " @@")
	if end < 0 {
		return h, fmt.Errorf("patch: bad hunk header %q", l)
	}
	oldS, newS, ok := strings.Cut(rest[:end], " +")
	if !ok {
		return h, fmt.Errorf("patch: bad hunk header %q", l)
	}
	var err error
	if h.oldStart, h.oldLen, err = rng(oldS); err != nil {
		return h, fmt.Errorf("patch: bad hunk header %q", l)
	}
	if h.newStart, h.newLen, err = rng(newS); err != nil {
		return h, fmt.Errorf("patch: bad hunk header %q", l)
	}
	return h, nil
}

func rng(s string) (start, n int, err error) {
	a, b, has := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return
	}
	n = 1
	if has {
		n, err = strconv.Atoi(b)
	}
	return
}

// splitLines splits after each "\n"; a last line without one is kept as it is.
func splitLines(b []byte) []string {
	ls := strings.SplitAfter(string(b), "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// Apply applies patch p to old and returns the new content.
func Apply(old []byte, p string) ([]byte, error) {
	hs, err := parse(p)
	if err != nil {
		return nil, err
	}
	lines := splitLines(old)
	var out strings.Builder
	pos := 0 // next old line to copy, 0-based
	for i, h := range hs {
		start := h.oldStart - 1
		if h.oldLen == 0 {
			start = h.oldStart // "-a,0" inserts after line a
		}
		if start < pos || start > len(lines) {
			return nil, &ConflictError{Hunk: i + 1, Line: h.oldStart, Want: "a hunk in order", Got: fmt.Sprintf("old file has %d lines", len(lines))}
		}
		for _, l := range lines[pos:start] {
			out.WriteString(l)
		}
		pos = start
		for _, l := range h.lines {
			switch l.op {
			case ' ', '-':
				if pos >= len(lines) || lines[pos] != l.text {
					got := "<end of file>"
					if pos < len(lines) {
						got = lines[pos]
					}
					return nil, &ConflictError{Hunk: i + 1, Line: pos + 1, Want: l.text, Got: got}
				}
				if l.op == ' ' {
					out.WriteString(l.text)
				}
				pos++
			case '+':
				out.WriteString(l.text)
			}
		}
	}
	for _, l := range lines[pos:] {
		out.WriteString(l)
	}
	return []byte(out.String()), nil
}
