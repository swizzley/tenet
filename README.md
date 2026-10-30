# tenet

A Go service that reads a GitHub repository's commit history and does three things with it:

1. **Insight.** It ranks the top contributors over a period, by commits and by lines changed.
2. **Narrative.** Claude writes a short account of what is unusual, with a root-cause hypothesis, a confidence score, and an evidence chain. Each piece of evidence is checked against the numbers before it is returned.
3. **Reconstruction.** It rebuilds the repository's files from nothing but their diffs. It walks back to the root commit (the inverted pass), then replays every diff forward, and proves each file against GitHub's blob id. A three.js page plays the run as a Tenet-style temporal pincer.

One process runs all three: the collector, the REST API, the WebSocket stream and the page.

## 60-second quickstart

Prerequisites: Go 1.26 (any Go 1.21 or later downloads it automatically) and `git` (git is needed by the tests only).

```sh
cd tenet
cp config.example.json config.json   # set github_token and anthropic_api_key
# or skip the file and export them instead:
#   export GITHUB_TOKEN=...  ANTHROPIC_API_KEY=...
go run ./cmd/tenet                   # listens on http://127.0.0.1:8080
```

Then try it:

```sh
open http://127.0.0.1:8080/?repo=spf13/pflag          # the reconstruction, animated

curl 'http://127.0.0.1:8080/api/v1/repos/golang/go/insights/contributors?since=2026-10-01&until=2026-10-28&limit=5'
curl 'http://127.0.0.1:8080/api/v1/repos/golang/go/insights/narrative?since=2026-10-01&until=2026-10-28'
curl -X POST http://127.0.0.1:8080/api/v1/reconstructions -d '{"repo":"rsc/quote"}'
```

The service will not start without both credentials. It names each missing one and says where to put it:

```
tenet: config: missing credentials: anthropic_api_key (in config.json) or ANTHROPIC_API_KEY
```

Each credential comes from `config.json` or from its environment variable, and the environment wins. Use a GitHub token with **read-only** access: a fine-grained token with "Public repositories (read-only)" is enough. `config.json` and the database are in `.gitignore`.

## API

Every error body has the same shape: `{"error":{"code":"...","message":"..."}}`.

| Method and path | What it does |
|---|---|
| `GET /api/v1/repos/{owner}/{repo}/insights/contributors?since=&until=&by=commits\|lines&limit=10` | The contributor ranking over a period (see Metric). |
| `GET /api/v1/repos/{owner}/{repo}/insights/narrative?since=&until=` | Claude's narrative, plus `data`: the exact JSON its evidence paths point into. |
| `POST /api/v1/reconstructions` with `{"repo":"owner/name","max_commits":200}` | Starts a reconstruction. Returns `202`, a `Location` and the stream URL. |
| `GET /api/v1/reconstructions/{id}` | The run's state and, when it is done, the proof summary. |
| `GET /api/v1/reconstructions/{id}/events?from=N` | A WebSocket of the run's events. `from` resumes after a reconnect. |
| `GET /api/v1/status` | GitHub rate-limit budgets and the double-buffered writer's counters. |
| `GET /healthz` | Liveness. |

- **Periods.** `since` and `until` take RFC 3339 (`2026-10-01T00:00:00Z`) or a date (`2026-10-01`). A date for `until` means the end of that day. With no `until`, the period ends at the next full hour. The default `since` is 30 days before `until`. The longest period is 366 days.
- **Status codes:**
  - `400`: bad input;
  - `404`: the repository is unknown or private;
  - `422`: the repository is empty, or the model declined;
  - `429`: too many reconstructions are running;
  - `502`: an upstream failure, including a narrative whose evidence failed the check;
  - `503`: an upstream rate limit (sent with `Retry-After`);
  - `504`: an upstream timeout.

## Metric

**Who carried the period, measured two ways at once.**

- **Commits** count how often someone landed work on the default branch.
- **Lines changed** (additions + deletions) count how much they changed.

Either one alone misleads. Commits reward many small changes; lines reward one generated or vendored dump. So every contributor carries both, plus `large_commits`: the number of commits over 1,000 lines, which shows when a lines ranking rests on a few bulk changes. For example, in golang/go in October 2026, aclements leads by lines, but 10 of those 52 commits are over 1,000 lines each.

Around the ranking, the response also carries:

- `top1_commit_share`;
- `bus_factor_50`: the fewest people who make up half the commits;
- a weekly series with the top contributor of each week, and the peak week.

These give the narrative something to explain.

**Identity.** A contributor is their GitHub login. An author whose email maps to no account appears as `~Name`. Commits count once, on the default branch, by committed date.

## Tests

```sh
go test ./...          # about a minute; the reconstruction tests drive real git
go test -race ./...
```

See NOTES.md for what the tests prove and how they were checked.
