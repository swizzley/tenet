// Package testgit is test support: a real git repository with a varied history,
// served through the subset of the GitHub REST API this service reads. Every
// answer comes from git itself, so a test that rebuilds the repository from these
// answers is checked against git, not against hand-written fixtures.
package testgit

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Repo is a git repository in a temp dir.
type Repo struct {
	t      testing.TB
	Dir    string
	date   string // author and committer date of the next commit
	author string
}

// Login is the GitHub login the fake reports for an author name: Ann and Bob
// have accounts, Cy does not.
func Login(name string) string {
	if name == "Cy" {
		return ""
	}
	return strings.ToLower(name)
}

func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	who := r.author
	if who == "" {
		who = "Ann"
	}
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+who, "GIT_AUTHOR_EMAIL="+strings.ToLower(who)+"@example.com",
		"GIT_COMMITTER_NAME="+who, "GIT_COMMITTER_EMAIL="+strings.ToLower(who)+"@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if r.date != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+r.date, "GIT_COMMITTER_DATE="+r.date)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *Repo) write(name, content string) {
	p := filepath.Join(r.Dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit n is dated 2026-01-01 plus n hours, by Authors[n%3] (Ann twice as often).
func (r *Repo) commit(msg string, n int) {
	r.stamp(n)
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", msg)
}

func (r *Repo) stamp(n int) {
	r.date = fmt.Sprintf("2026-01-%02dT%02d:00:00Z", 1+n/24, n%24)
	r.author = []string{"Ann", "Bob", "Ann", "Cy"}[n%4]
}

// New builds a repository with commits commits: random edits across a few files,
// plus a rename, a delete, a binary file, an empty file, files without a final
// newline, and a merge from a side branch.
func New(t testing.TB, commits int) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &Repo{t: t, Dir: t.TempDir()}
	r.Git("init", "-q", "-b", "main")
	rng := rand.New(rand.NewSource(42))
	files := map[string][]string{}
	edit := func(name string) {
		lines := files[name]
		for range 1 + rng.Intn(5) {
			switch op := rng.Intn(3); {
			case op == 0 || len(lines) == 0:
				at := rng.Intn(len(lines) + 1)
				lines = append(lines[:at], append([]string{fmt.Sprintf("line %d %s", rng.Intn(1000), strings.Repeat("x", rng.Intn(8)))}, lines[at:]...)...)
			case op == 1:
				at := rng.Intn(len(lines))
				lines = append(lines[:at], lines[at+1:]...)
			default:
				lines[rng.Intn(len(lines))] = fmt.Sprintf("changed %d", rng.Intn(1000))
			}
		}
		files[name] = lines
		s := strings.Join(lines, "\n")
		if len(lines) > 0 && rng.Intn(4) != 0 {
			s += "\n"
		}
		r.write(name, s)
	}
	n := 0
	next := func(msg string) { r.commit(msg, n); n++ }
	edit("README.md")
	r.write("empty.txt", "")
	next("root: readme and an empty file")
	for i := 1; n < commits; i++ {
		switch {
		case i == 5:
			r.write("logo.bin", string([]byte{0, 1, 2, 0xff, 0, 0x89, 'P', 'N', 'G'}))
			next("add a binary")
		case i == 8 && files["src/a.go"] != nil:
			r.Git("mv", "src/a.go", "src/renamed.go")
			files["src/renamed.go"] = files["src/a.go"]
			delete(files, "src/a.go")
			next("rename a.go")
		case i == 11:
			r.Git("checkout", "-q", "-b", "side")
			edit("side/feature.txt")
			r.commit("side work", n)
			n++
			r.Git("checkout", "-q", "main")
			edit("README.md")
			next("main work")
			r.stamp(n)
			r.Git("merge", "-q", "--no-ff", "-m", "merge side", "side")
			n++
		case i == 17:
			if err := os.Chmod(filepath.Join(r.Dir, "README.md"), 0o755); err != nil {
				t.Fatal(err)
			}
			r.commit("make the readme executable (mode only)", n)
			n++
		case i == 14 && files["src/b.go"] != nil:
			r.Git("rm", "-q", "src/b.go")
			delete(files, "src/b.go")
			next("delete b.go")
		default:
			for range 1 + rng.Intn(2) {
				edit([]string{"README.md", "src/a.go", "src/b.go", "docs/guide.md", "src/renamed.go"}[rng.Intn(5)])
			}
			next(fmt.Sprintf("commit %d", i))
		}
	}
	return r
}

// Server serves the repository as owner/name on the GitHub REST paths.
type Server struct {
	R            *Repo
	Calls        atomic.Int64
	GraphQLCalls atomic.Int64
	InFlight     atomic.Int64
	MaxSeen      atomic.Int64
	// Tamper, when set, may change a commit's files before they are served.
	Tamper func(sha string, files []map[string]any)

	mu sync.Mutex
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.Calls.Add(1)
	n := s.InFlight.Add(1)
	defer s.InFlight.Add(-1)
	for {
		m := s.MaxSeen.Load()
		if n <= m || s.MaxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	s.mu.Lock() // git commands on one repo, one at a time
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := strings.TrimPrefix(req.URL.Path, "/repos/o/r")
	g := s.R.Git
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/graphql":
		s.GraphQLCalls.Add(1)
		s.history(w, req)
	case p == "":
		fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`)
	case p == "/branches/main":
		fmt.Fprintf(w, `{"commit":{"sha":%q}}`, strings.TrimSpace(g("rev-parse", "main")))
	case p == "/commits":
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		page = max(page, 1)
		per, _ := strconv.Atoi(req.URL.Query().Get("per_page"))
		lines := strings.Split(strings.TrimSpace(g("rev-list", "--date-order", "--parents", req.URL.Query().Get("sha"))), "\n")
		from, to := (page-1)*per, min(page*per, len(lines))
		if to < len(lines) {
			w.Header().Set("Link", `<x>; rel="next"`)
		}
		var out []map[string]any
		for _, l := range lines[min(from, len(lines)):to] {
			f := strings.Fields(l)
			ps := []map[string]string{}
			for _, p := range f[1:] {
				ps = append(ps, map[string]string{"sha": p})
			}
			out = append(out, map[string]any{"sha": f[0], "parents": ps})
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/commits/"):
		json.NewEncoder(w).Encode(s.commit(strings.TrimPrefix(p, "/commits/")))
	case strings.HasPrefix(p, "/git/trees/"):
		var tree []map[string]string
		for _, l := range strings.Split(strings.TrimSpace(g("ls-tree", "-r", strings.TrimPrefix(p, "/git/trees/"))), "\n") {
			meta, path, _ := strings.Cut(l, "\t")
			f := strings.Fields(meta)
			tree = append(tree, map[string]string{"path": path, "type": f[1], "sha": f[2]})
		}
		json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}

// history answers the GraphQL commit-history query from `git log` on main.
func (s *Server) history(w http.ResponseWriter, req *http.Request) {
	var q struct {
		Variables struct {
			Owner, Name, Since, Until, Cursor string
			First                             int
		} `json:"variables"`
	}
	json.NewDecoder(req.Body).Decode(&q)
	v := q.Variables
	if v.Owner != "o" || v.Name != "r" {
		fmt.Fprint(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"unknown repository"}]}`)
		return
	}
	out := s.R.Git("log", "--format=@%H\t%aN\t%aE\t%cI\t%s", "--numstat", "--since="+v.Since, "--until="+v.Until, "main")
	var nodes []map[string]any
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "@"):
			f := strings.SplitN(l[1:], "\t", 5)
			var user any
			if login := Login(f[1]); login != "" {
				user = map[string]string{"login": login}
			}
			nodes = append(nodes, map[string]any{"oid": f[0], "committedDate": f[3], "messageHeadline": f[4], "additions": 0, "deletions": 0,
				"author": map[string]any{"name": f[1], "email": f[2], "date": f[3], "user": user}})
		case l != "" && len(nodes) > 0:
			f := strings.Fields(l)
			a, _ := strconv.Atoi(f[0]) // "-" for a binary counts 0, as on GitHub
			d, _ := strconv.Atoi(f[1])
			n := nodes[len(nodes)-1]
			n["additions"] = n["additions"].(int) + a
			n["deletions"] = n["deletions"].(int) + d
		}
	}
	off, _ := strconv.Atoi(v.Cursor)
	end := min(off+v.First, len(nodes))
	page := nodes[min(off, len(nodes)):end]
	if page == nil {
		page = []map[string]any{}
	}
	info := map[string]any{"hasNextPage": end < len(nodes), "endCursor": strconv.Itoa(end)}
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
		"defaultBranchRef": map[string]any{"target": map[string]any{"history": map[string]any{"pageInfo": info, "nodes": page}}}}}})
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// commit builds GitHub's commit-detail shape from git: files diffed against the
// first parent, with GitHub's statuses, blob ids after the change, and hunks only.
func (s *Server) commit(sha string) map[string]any {
	g := s.R.Git
	parents := strings.Fields(g("log", "-1", "--format=%P", sha))
	base := emptyTree
	if len(parents) > 0 {
		base = parents[0]
	}
	var files []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(g("diff", "--name-status", "-M", base, sha)), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		st, name, prev := f[0], f[len(f)-1], ""
		status := map[byte]string{'A': "added", 'M': "modified", 'D': "removed", 'R': "renamed"}[st[0]]
		if st[0] == 'R' {
			prev = f[1]
		}
		file := map[string]any{"filename": name, "status": status}
		if prev != "" {
			file["previous_filename"] = prev
		}
		if status == "removed" {
			file["sha"] = strings.Repeat("0", 40)
		} else {
			file["sha"] = strings.TrimSpace(g("rev-parse", sha+":"+name))
		}
		paths := []string{name}
		if prev != "" {
			paths = append(paths, prev)
		}
		num := strings.Fields(g(append([]string{"diff", "--numstat", "-M", base, sha, "--"}, paths...)...))
		if len(num) >= 2 && num[0] != "-" {
			a, _ := strconv.Atoi(num[0])
			d, _ := strconv.Atoi(num[1])
			file["additions"], file["deletions"], file["changes"] = a, d, a+d
			diff := g(append([]string{"diff", "--no-color", "-M", base, sha, "--"}, paths...)...)
			if i := strings.Index(diff, "\n@@ "); i >= 0 {
				file["patch"] = strings.TrimSuffix(diff[i+1:], "\n")
			} else if strings.Contains(diff, "\nold mode ") && status == "modified" {
				file["sha"] = nil // a mode-only change: GitHub sends no blob id
			}
		} else {
			file["changes"] = 1 // binary: GitHub sends no patch
		}
		files = append(files, file)
	}
	if s.Tamper != nil {
		s.Tamper(sha, files)
	}
	ps := []map[string]string{}
	for _, p := range parents {
		ps = append(ps, map[string]string{"sha": p})
	}
	return map[string]any{
		"sha": sha, "parents": ps, "files": files,
		"commit": map[string]any{"message": strings.TrimSpace(g("log", "-1", "--format=%B", sha)),
			"author": map[string]string{"name": "Ann", "email": "ann@example.com", "date": strings.TrimSpace(g("log", "-1", "--format=%aI", sha))}},
	}
}
