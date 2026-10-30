# tenet

*The best worst way to do git archaeology.* It rebuilds a repository from nothing but its diffs, runs time backward down a neon avenue to the first commit, then grows the code forward as a night city you can walk through. Completely pointless, which probably makes it art.

A Go service that reads a GitHub repository's commit history and does three things with it:

1. **Insight.** It ranks the top contributors over a period, by commits and by lines changed.
2. **Narrative.** A reasoning model writes a short account of what is unusual, with a root-cause hypothesis, a confidence score, and an evidence chain. By default the model is [VibeThinker-1.5B](https://huggingface.co/WeiboAI/VibeThinker-1.5B), and it runs in the viewer's browser on WebGPU, so no LLM key is needed. With an Anthropic key, the server can also ask Claude. Either way, the server checks every number in it against the data before it shows the narrative.
3. **Reconstruction.** It rebuilds the repository's files from their diffs. It walks back to the root commit (the inverted pass), then replays every diff forward, and proves each file against GitHub's blob id. Where GitHub sends no patch (a binary, a very large diff), it fetches the blob by that id and proves it by its hash.

   A three.js page plays the run as a Tenet-style temporal pincer through a seeded night city:
   - **Backward:** down a neon avenue, each commit raises an arch, its diff un-writes itself, and the rain falls up.
   - **Forward:** each file grows as a skyscraper, one band of floors per change. A click on a roof opens the file; a click on a floor band opens that change's diff.

One process runs all three: the collector, the REST API, the WebSocket stream and the page.

## 60-second quickstart

Prerequisites: Go 1.26 (any Go 1.21 or later downloads it automatically) and `git` (git is needed by the tests only).

```sh
cd tenet
cp config.example.json config.json   # set github_token (anthropic_api_key is optional)
# or skip the file and export it instead:
#   export GITHUB_TOKEN=...
go run ./cmd/tenet                   # listens on http://127.0.0.1:8080
```

Then try it:

```sh
open http://127.0.0.1:8080/?repo=spf13/pflag          # the reconstruction, animated
# then: right-drag turns the city, the wheel zooms, hover a tower, click it to open the file
# type golang/go and press EXPLAIN: the narrative, written on your GPU

curl 'http://127.0.0.1:8080/api/v1/repos/golang/go/insights/contributors?since=2026-10-01&until=2026-10-28&limit=5'
curl 'http://127.0.0.1:8080/api/v1/repos/golang/go/insights/narrative/prompt?since=2026-10-01&until=2026-10-28'
curl -X POST http://127.0.0.1:8080/api/v1/reconstructions -d '{"repo":"rsc/quote"}'
```

The service will not start without a GitHub token. It says where to put it:

```
tenet: config: missing credential: github_token (in config.json) or GITHUB_TOKEN
```

Each credential comes from `config.json` or from its environment variable, and the environment wins. Use a GitHub token with **read-only** access: a fine-grained token with "Public repositories (read-only)" is enough. `config.json` and the database are in `.gitignore`.

**The browser narrative** needs WebGPU: a current Chrome, Edge or Safari. When the page opens, it starts downloading the model from Hugging Face, 1.24 GB at 4-bit, with a progress bar under the search box; the browser then caches it. The model is pinned to one commit (`browser_model_revision`), so every viewer runs the same weights. A narrative takes 3 to 4 minutes on an M1 Pro.

## API

Every error body has the same shape: `{"error":{"code":"...","message":"..."}}`.

| Method and path | What it does |
|---|---|
| `GET /api/v1/repos/{owner}/{repo}/insights/contributors?since=&until=&by=commits\|lines&limit=10` | The contributor ranking over a period (see Metric). |
| `GET /api/v1/repos/{owner}/{repo}/insights/narrative/prompt?since=&until=` | The browser model's input: system and user messages, the pinned model, `data` (the JSON its evidence must point into) and its `digest`. |
| `POST /api/v1/repos/{owner}/{repo}/insights/narrative/verify?since=&until=` with `{"digest":"…","output":"<the model's text>","attempt":1}` | Checks the model's answer against data the server rebuilds itself. `200` with the narrative, or `422` with the problems and a `retry_prompt`. |
| `GET /api/v1/repos/{owner}/{repo}/insights/narrative?since=&until=` | Claude's narrative, written on the server, when `anthropic_api_key` is set (`501` otherwise). |
| `POST /api/v1/reconstructions` with `{"repo":"owner/name","max_commits":200}` | Starts a reconstruction. Without `max_commits` it walks to the root, up to `max_reconstruct` (default 1,000). Returns `202`, a `Location` and the stream URL. |
| `GET /api/v1/reconstructions/{id}` | The run's state and, when it is done, the proof summary. |
| `GET /api/v1/reconstructions/{id}/events?from=N` | A WebSocket of the run's events. `from` resumes after a reconnect. |
| `GET /api/v1/reconstructions/{id}/file?path=` | One file as the run rebuilt it, with whether it is proven, or why not (`409` while the run is going). |
| `GET /api/v1/reconstructions/{id}/diff?sha=&path=` | One file's change in one commit of the run, from the stored commit details. |
| `GET /api/v1/status` | GitHub rate-limit budgets and the double-buffered writer's counters. |
| `GET /healthz` | Liveness. |

- **Periods.** `since` and `until` take RFC 3339 (`2026-10-01T00:00:00Z`) or a date (`2026-10-01`). A date for `until` means the end of that day. With no `until`, the period ends at the next full hour. The default `since` is 30 days before `until`. The longest period is 366 days.
- **Status codes:**
  - `400`: bad input;
  - `404`: the repository is unknown or private;
  - `409`: the period's data changed between the prompt and the check, or a file was asked for while its run is going;
  - `422`: the repository is empty, the model declined, or a browser answer failed the check;
  - `501`: the server narrative is off (no `anthropic_api_key`);
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

## Build, run, debug

### Build

```sh
go build -o bin/tenet ./cmd/tenet    # one static binary; bin/ is git-ignored
go vet ./...
```

The page (`web/static`) is embedded in the binary. After editing it, rebuild or use `go run`, then reload the page. There is no front-end build step: the page is plain ES modules. Its libraries load from jsdelivr at pinned versions:
- three.js 0.186.1, with its OrbitControls, EffectComposer, RenderPass, UnrealBloomPass and OutputPass;
- simplex-noise 4.0.3;
- transformers.js 4.3.1.

### Run

```sh
GITHUB_TOKEN=$(gh auth token) ./bin/tenet                 # settings from ./config.json, if present
./bin/tenet -config /path/to/config.json                 # another settings file
```

- **Settings:** every setting and its default is in `config.example.json`. `addr` sets the port and `db_path` the SQLite file.
- **Resetting:** delete the database file to start with empty caches.
- **Stopping:** Ctrl-C stops the server cleanly, and flushes the double-buffered writer first.

### Debug

- **Logs.** The server logs JSON to stdout, one line per request (method, path, status, ms), plus one line per finished reconstruction. Credentials never appear in them.

  ```sh
  ./bin/tenet | jq -c 'select(.status >= 400 or .msg == "reconstruction finished")'
  ```

- **Live state.** `GET /api/v1/status` shows the GitHub rate-limit budgets, the double-buffered writer's counters (batches, producer waits, write time), and whether each narrative path is on.
- **A reconstruction.**
  - `GET /api/v1/reconstructions/{id}` gives its state and proof summary.
  - Its event stream holds every file version, and each one that is not exact carries a `why`:

    ```sh
    websocat -H 'Origin: http://127.0.0.1:8080' ws://127.0.0.1:8080/api/v1/reconstructions/$ID/events | jq -c 'select(.t == "file" and .status != "exact")'
    ```

  - `GET .../file?path=` gives a rebuilt file, or the reason it is unproven. `GET .../diff?sha=&path=` gives one change, which is what clicking a floor band shows.
- **The narrative.**
  - `GET .../insights/narrative/prompt` shows exactly what the browser model reads.
  - Save a model's text and replay the check with `POST .../insights/narrative/verify`, using the digest from the prompt:

    ```sh
    curl -s -X POST "http://127.0.0.1:8080/api/v1/repos/golang/go/insights/narrative/verify?since=2026-10-01&until=2026-10-29" \
      -d "$(jq -n --arg o "$(cat answer.txt)" --arg d "$DIGEST" '{digest:$d, output:$o, attempt:1}')" | jq .
    ```

    A `422` lists every problem the check found.
- **The page.** Open the browser's developer tools; the console shows any module or WebGPU error. The page needs WebGPU only for the narrative. To watch the model's download and cache, look at the network panel and Application → Cache Storage (`transformers-cache`).
- **The database.**

  ```sh
  sqlite3 tenet.db '.tables'
  sqlite3 tenet.db "select repo, count(*) from commits group by repo"
  ```

  It holds:
  - `commits`: the insight data;
  - `coverage`: which periods were fetched;
  - `commit_details`: diffs, and the blobs fetched for patchless changes, stored as `blob:<id>`.

## Tests

```sh
go test ./...          # about a minute; the reconstruction tests drive real git
go test -race ./...
go test ./internal/reconstruct/ -run Withheld -v    # one test, verbose
```

See NOTES.md for what the tests prove and how they were checked.
