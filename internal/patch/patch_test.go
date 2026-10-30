package patch

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobSHAMatchesGit(t *testing.T) {
	// Known ids: `git hash-object` of the empty file and of "hello\n".
	if got := BlobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty blob = %s", got)
	}
	if got := BlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hello blob = %s", got)
	}
}

func TestApplyBasics(t *testing.T) {
	cases := []struct {
		name, old, patch, want string
	}{
		{"add to empty", "", "@@ -0,0 +1,2 @@\n+a\n+b", "a\nb\n"},
		{"modify middle", "a\nb\nc\n", "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c", "a\nB\nc\n"},
		{"delete all", "a\nb\n", "@@ -1,2 +0,0 @@\n-a\n-b", ""},
		{"no newline added at end", "a\n", "@@ -1 +1 @@\n-a\n+a\n\\ No newline at end of file", "a"},
		{"newline added at end", "a", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+a", "a\n"},
		{"insert after line 2", "a\nb\nc\n", "@@ -2,0 +3 @@\n+x", "a\nb\nx\nc\n"},
		{"two hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "@@ -1,2 +1,2 @@\n-1\n+one\n 2\n@@ -8,2 +8,3 @@\n 8\n 9\n+ten", "one\n2\n3\n4\n5\n6\n7\n8\n9\nten\n"},
		{"crlf kept", "a\r\nb\r\n", "@@ -1,2 +1,2 @@\n a\r\n-b\r\n+c\r", "a\r\nc\r\n"},
	}
	for _, tc := range cases {
		got, err := Apply([]byte(tc.old), tc.patch)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// Apply refuses a patch that does not match, instead of producing a plausible file.
func TestApplyRefusesMismatch(t *testing.T) {
	_, err := Apply([]byte("a\nX\nc\n"), "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c")
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Line != 2 {
		t.Fatalf("err = %v, want a conflict at line 2", err)
	}
	if _, err := Apply([]byte("a\n"), "@@ -1,3 +1,3 @@\n a\n-b"); err == nil {
		t.Fatal("accepted a truncated hunk")
	}
	if _, err := Apply([]byte("a\n"), "garbage"); err == nil {
		t.Fatal("accepted text with no hunk header")
	}
	if _, err := Apply([]byte("a\n"), "@@ -5,1 +5,1 @@\n-z\n+y"); err == nil {
		t.Fatal("accepted a hunk past the end of the file")
	}
}

// The real proof: random histories built with git itself. For every file of every
// commit, the hunks git prints (in GitHub's format: headers stripped) applied to the
// parent's content must give a blob whose id equals git's own id for that file.
func TestApplyAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	rng := rand.New(rand.NewSource(42))
	files := map[string][]string{}
	words := []string{"alpha", "beta", "gamma", "delta", "", "  indented", "\ttab", "x"}
	write := func(name string, lines []string, trailingNL bool) {
		s := strings.Join(lines, "\n")
		if trailingNL && len(lines) > 0 {
			s += "\n"
		}
		os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644)
	}
	const commits = 60
	for c := range commits {
		for k := 0; k < 1+rng.Intn(3); k++ {
			name := fmt.Sprintf("f%d.txt", rng.Intn(6))
			lines := files[name]
			for e := 0; e < 1+rng.Intn(6); e++ {
				switch op := rng.Intn(3); {
				case op == 0 || len(lines) == 0:
					at := rng.Intn(len(lines) + 1)
					lines = append(lines[:at], append([]string{words[rng.Intn(len(words))] + fmt.Sprint(rng.Intn(9))}, lines[at:]...)...)
				case op == 1:
					at := rng.Intn(len(lines))
					lines = append(lines[:at], lines[at+1:]...)
				default:
					lines[rng.Intn(len(lines))] = words[rng.Intn(len(words))]
				}
			}
			files[name] = lines
			write(name, lines, rng.Intn(5) != 0)
		}
		if c > 5 && rng.Intn(8) == 0 {
			for name := range files {
				os.Remove(filepath.Join(dir, name))
				delete(files, name)
				break
			}
		}
		git("add", "-A")
		git("commit", "-q", "--allow-empty", "-m", fmt.Sprint("c", c))
	}

	shas := strings.Fields(git("rev-list", "--reverse", "main"))
	state := map[string][]byte{}
	checked := 0
	for i, sha := range shas {
		var changed []string
		if i == 0 {
			changed = strings.Fields(git("ls-tree", "--name-only", sha))
		} else {
			changed = strings.Fields(git("diff", "--name-only", shas[i-1], sha))
		}
		for _, name := range changed {
			var diff string
			if i == 0 {
				diff = git("diff", "--no-color", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", sha, "--", name)
			} else {
				diff = git("diff", "--no-color", shas[i-1], sha, "--", name)
			}
			p := githubPatch(diff)
			if strings.Contains(diff, "\ndeleted file mode") {
				delete(state, name)
				continue
			}
			got, err := Apply(state[name], p)
			if err != nil {
				t.Fatalf("commit %d %s: %v\npatch:\n%s", i, name, err, p)
			}
			want := strings.TrimSpace(git("rev-parse", sha+":"+name))
			if BlobSHA(got) != want {
				t.Fatalf("commit %d %s: blob %s, git says %s", i, name, BlobSHA(got), want)
			}
			state[name] = got
			checked++
		}
	}
	if checked < commits {
		t.Fatalf("only %d file versions checked", checked)
	}
	t.Logf("%d file versions over %d commits rebuilt byte-exact", checked, len(shas))
}

// githubPatch turns `git diff` output into the form GitHub's API returns: the hunks
// only, and no newline after the last line.
func githubPatch(diff string) string {
	i := strings.Index(diff, "\n@@ ")
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(diff[i+1:], "\n")
}
