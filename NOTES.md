# Notes

## 1. How to run it locally

```sh
cp config.example.json config.json    # github_token + anthropic_api_key
go run ./cmd/tenet                    # http://127.0.0.1:8080
```

- **Prerequisites:** Go 1.26: the pure-Go SQLite driver needs it, and any Go 1.21 or later downloads it automatically. `git` too, but only for the tests.
- **Credentials:** a GitHub token with read-only access to public repositories, and an Anthropic API key. Each one may come from `config.json` or from its environment variable (`GITHUB_TOKEN`, `ANTHROPIC_API_KEY`). The service refuses to start, naming what is missing, if either is absent.
- **Settings:** every other setting has a default (listed in `config.example.json`). An unknown key in `config.json` is an error, so a typo fails loudly. The SQLite file (`tenet.db`) is created on first run.
- **Why Go, not TypeScript:** the work here is concurrency: a double-buffered writer, a rate-limited fan-out of requests, and a stream to many viewers. Go does that in one process, with one binary, a race detector, and a pure-Go SQLite (no cgo, no native build step for the reviewer).

## 2. Architecture

```
            ┌──────────────── one Go process ────────────────────────────────────┐
 browser ──►│ api: REST + WebSocket + page (embedded)                           │
  curl  ──►│   │                                                                │
            │   ├─ insights ─► Service ─(covered?)─► store (SQLite) ◄────────┐  │
            │   │                 │ no                                      │  │
            │   │                 ▼                                         │  │
            │   │            collect ─► GitHub GraphQL history, 100/page     │  │
            │   │                 │ Put                                     │  │
            │   │                 ▼                                         │  │
            │   │      dbuf: [buffer A | buffer B] ─ flusher ─ 1 tx/buffer ──┘  │
            │   │                                                            │
            │   ├─ narrative ─► insight JSON ─► Claude ─► evidence check     │
            │   │                                                            │
            │   └─ reconstructions ─► walker ─(diff stream)─► replayer ─► events ─► WS
            │                           │ REST commits, chunked by rate budget │
            └───────────────────────────┴────────────────────────────────────────┘
```

**Insights.** The contributor insight reads the GitHub GraphQL commit history. One page holds 100 commits, with their line counts, for one rate-limit point; REST would need one request per commit for the same numbers. The page size equals the capacity of one half of the double-buffered writer (`internal/dbuf`). So a page fills one half exactly. The single flusher goroutine commits that half to SQLite as one transaction, while the collector already fetches the next page into the other half. Producers wait only when both halves are full, and every wait is counted (`/api/v1/status`). SQLite sees one writer and WAL readers, so no lock is contended. A period that a stored fetch covers is answered from SQLite without touching GitHub:

- A period that ended at least an hour before its fetch is history, and is served from disk for good.
- A period that reaches into the last hour is served from disk only within the cache TTL.

Concurrent identical requests share one fetch (`singleflight`). The narrative endpoint hands Claude the exact JSON of the insight. Every evidence item must name a path in that JSON (`contributors[0].commit_share`) and the number found there. `narrate.Validate` resolves each path and compares the number. A narrative that fails goes back once with the list of problems, then is withheld (`502 narrative_unsupported`, with the rejected text in the details). It is never returned as if it were sound.

**Reconstruction.** A reconstruction is a pincer:

1. **The walker** goes back from the branch head along first parents. The commit list gives 100 commits and their parents per request, so finding the chain is cheap.
2. **Fetching diffs.** The walker fetches each commit's diff in chunks: as many requests at once as fill one buffer, capped by the rate-limit budget the limiter reads from `X-RateLimit-*` (`Limiter.Chunk`). Diffs come from SQLite when they are already stored. A commit id never changes, so a stored diff never goes stale.
3. **The stream.** The walker publishes the diffs newest-first on a channel, the way a WebSocket would deliver real-time diffs. The replayer reads that inverted stream to its end (the turnstile), then plays it forward from an empty tree.
4. **Applying patches.** `internal/patch` applies every patch strictly. Every resulting file must hash to the blob id GitHub reports for it at that commit, and the final tree is checked against the head tree.
5. **Unproven files.** A file the run cannot prove is reported as unknown, with the reason, never guessed. Three causes:
   - a binary;
   - a patch GitHub truncated;
   - a file older than the walked window.
6. **Viewing.** Events go to viewers over a WebSocket. A late or reconnecting viewer replays from any index.

## 3. Decisions and trade-offs

- **SQLite, not Postgres.** It keeps "runnable in 60 seconds" true. The store is behind four small methods, so moving to Postgres is a contained change. The double buffer is why one writer suffices.
- **The double buffer's sink was first `/dev/null`.** It simulated the database while the pipeline was built. The assignment prefers a real store, so each buffer now becomes one SQLite transaction. The mechanism is the same; only the flush function changed.
- **GraphQL for insights, REST for diffs.** GraphQL is 100 times cheaper for line counts. Only REST returns per-file patches and blob ids.
- **Default branch only, first parent only.** The metric counts what landed. The reconstruction replays the history the head was built from. Merged branches arrive as the merge's diff against its first parent.
- **The proof is a hash, not a heuristic.** If a patch does not apply byte for byte, the file is unproven. If the result's blob id differs from GitHub's, it is a mismatch. Both are shown, and neither is hidden.
- **The narrative is checked, not trusted.** The confidence score is the model's, and is clamped and validated to the range 0 to 1. The numbers it cites are verified. Commit headlines and author names reach the model only as data, under a key named `..._untrusted`, with a system rule to never follow instructions inside them.
- **Security.**
  - **SSRF:** the repository is validated (`owner/name` only) and used only as escaped path segments under a fixed host, so a request cannot be steered elsewhere.
  - **Bodies and fields:** request bodies are capped at 4 KB, and unknown JSON fields are rejected.
  - **Headers:** responses carry a strict CSP, `nosniff`, `DENY` framing and `no-referrer`.
  - **WebSocket:** the stream checks the Origin, so another site cannot open it.
  - **Untrusted text on the page:** server text is drawn on canvases or set with `textContent`, never as HTML.
  - **Credentials:** they never appear in a log line, an error or a response. Tests check this, and so did a scan of the live run's log.

## 4. What I would do next with another day

- **Rate limiting of our own API, per client.** Today the service is bounded by caches, `singleflight` and a cap on live reconstructions, not by a per-client limit.
- **Warmer starts:** pre-fetch the commit list's ETags into SQLite, so a restart keeps the 304 savings that live only in memory now.
- **More signals:** pull requests merged and reviews, from the same GraphQL query family, so the narrative can tell "one author" from "one reviewer".
- **Partial windows:** for a reconstruction that ends at the window edge, fetch the window-base tree once (one request) and seed the files that existed before the window. That would turn most of its "unproven" files into proven ones.
- **Metrics:** Prometheus counters next to `/api/v1/status`.

## 5. How AI was used

I built this with Claude Code (Opus) as the pair, directing it, reviewing what it wrote, and making it prove each claim.

**Tests.** Each package has a test suite that runs the real path:

- The patch engine and the reconstruction are tested against **real git repositories** built during the test. They include merges, renames, deletes, a binary, an empty file, files without a final newline, and a mode-only change. Each one's served data comes from git itself, so "byte-exact" is checked against git's own blob ids.
- The API tests run the whole service in one process: the real store, double buffer, collector, GitHub client and reconstruction, against a git-backed fake GitHub and a fake Anthropic endpoint. They check that the API's totals and per-author counts equal an independent `git log --numstat`.

**Mutation check.** I broke each key mechanism on a throwaway copy and confirmed that a test fails. The 12 mechanisms:

- the cache;
- `singleflight`;
- path validation;
- the rate-limit chunk;
- the evidence check;
- the blob check;
- the double buffer;
- the credential rule;
- strict patching;
- coverage expiry;
- the token-in-log rule;
- window honesty.

At first 11 of 12 failed as they should. The window-honesty test passed on broken code because it checked only the final status. I sharpened it to check the reason too; now all 12 fail.

**Live runs** against GitHub found four defects that no fake had shown. All are fixed, each with a regression test:

1. **ETag 304.** GitHub's `304 Not Modified` carries no `Link` header. So a repeat reconstruction read "last page" and stopped at 70 of google/uuid's 109 commits. The client now caches `Link` with the body.
2. **Mode-only changes.** A mode-only change arrives with no patch and `sha: null`. It was reported as 4 mismatches; it is now carried as unchanged content.
3. **The repository regex.** The search field's `pattern` attribute was invalid under the browser's `v` regex flag, which stopped the auto-start.
4. **The event ticker.** It overlapped its own rows once it held more than about 15.

**Live measurements** (2026-10-30, this laptop, real GitHub):

- **golang/go, 1 to 29 October 2026.**
  - 461 commits in 5 GraphQL pages (100, 100, 100, 100, 61), with 0 producer waits. All 5 transactions committed in 7.9 ms in total.
  - The repeat query came from SQLite in 14 ms, against 14.3 s for the first fetch.
  - The independent REST list returned the identical set of 461 commit ids.
  - The top contributor (aclements, 52 commits, 57,269 lines changed) matches the sum of 52 separate REST commit-detail calls.
- **Reconstructions**, every head file rebuilt byte-exact from diffs alone:

  | Repository | Commits walked back to the root | Head files exact |
  |---|---|---|
  | rsc/quote | 17 | 10 of 10 |
  | google/uuid | 109 | 33 of 33 |
  | spf13/pflag | 233 | 91 of 91 |

  A repeat google/uuid run made 5 GitHub calls instead of 114.

**Not yet verified live: the narrative endpoint against the real Anthropic API.** No key was available where this was built. The tests prove the request shape, the structured-output schema, the evidence check, the retry and the error mapping against a fake endpoint. With a key, the first live call is the README's narrative `curl`.
