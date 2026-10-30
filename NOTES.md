# Notes

## 1. How to run it locally

```sh
cp config.example.json config.json    # github_token (anthropic_api_key optional)
go run ./cmd/tenet                    # http://127.0.0.1:8080
```

- **Prerequisites:** Go 1.26: the pure-Go SQLite driver needs it, and any Go 1.21 or later downloads it automatically. `git` too, but only for the tests.
- **Credentials:** a GitHub token with read-only access to public repositories. It may come from `config.json` or from `GITHUB_TOKEN`; the service refuses to start, naming both places, without it. An Anthropic API key (`anthropic_api_key` or `ANTHROPIC_API_KEY`) is optional: it turns on the server-side Claude narrative. Without it, the narrative runs in the browser.
- **Browser narrative:** a browser with WebGPU (current Chrome, Edge or Safari). The first run downloads the model, 1.24 GB, and the browser caches it.
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
            │   ├─ narrative/prompt ─► trimmed insight JSON ─► browser model │
            │   ├─ narrative/verify ◄─ model's text ─► the check (Validate)  │
            │   ├─ narrative (key set) ─► insight JSON ─► Claude ─► check    │
            │   ├─ reconstructions/{id}/file ─► one rebuilt file             │
            │   │                                                            │
            │   └─ reconstructions ─► walker ─(diff stream)─► replayer ─► events ─► WS
            │                           │ REST commits, chunked by rate budget │
            └───────────────────────────┴────────────────────────────────────────┘
```

**Insights.** The contributor insight reads the GitHub GraphQL commit history. One page holds 100 commits, with their line counts, for one rate-limit point; REST would need one request per commit for the same numbers. The page size equals the capacity of one half of the double-buffered writer (`internal/dbuf`). So a page fills one half exactly. The single flusher goroutine commits that half to SQLite as one transaction, while the collector already fetches the next page into the other half. Producers wait only when both halves are full, and every wait is counted (`/api/v1/status`). SQLite sees one writer and WAL readers, so no lock is contended. A period that a stored fetch covers is answered from SQLite without touching GitHub:

- A period that ended at least an hour before its fetch is history, and is served from disk for good.
- A period that reaches into the last hour is served from disk only within the cache TTL.

Concurrent identical requests share one fetch (`singleflight`).

**The narrative in the browser.** By default the narrative needs no LLM key. The page runs [VibeThinker-1.5B](https://huggingface.co/WeiboAI/VibeThinker-1.5B), a 1.5B reasoning model, on the viewer's GPU: the ONNX build at 4 bits, through transformers.js on WebGPU, in a Web Worker.

1. **Download.** When the page opens, the worker starts downloading the model from Hugging Face, pinned to one commit, and a bar under the search box shows it (1.24 GB the first time; then the browser cache).
2. **Prompt.** The page asks `GET .../narrative/prompt`. The server builds the prompt over a trimmed insight document: the top 8 contributors, the ranking fields, one contributor a line, and the shares and means a narrative reaches for. It returns the prompt with the document's digest, the answer template, and the list of the document's numeric paths.
3. **Reasoning.** The model reasons until it closes `</think>`, ends its turn, or spends its budget (4,096 tokens).
4. **The answer, led and held.** The worker leads the model through the template (`narrate.Template`). It writes each label, and the model writes each part under a logits lock:
   - an evidence **path** may only be the start of one of the document's numeric paths, so every path exists (and each is cited once);
   - its **value** is written by the worker, copied from the data at that path;
   - **prose** (summary, claims, hypothesis, caveat) may hold numerals but no number symbols (`⁴`, `½`) and no `= | [ ] { }`;
   - the **confidence** is digits after a written `0.`.

   Each part continues from the model's cache, so nothing is re-read.
5. **The check.** The page posts the raw text to `POST .../narrative/verify`. The server rebuilds the document itself, checks the digest, parses the answer strictly, and runs `narrate.Validate`:
   - every evidence path must resolve to a number equal to its value;
   - every number in the prose must be one of the document's numbers, as written or rounded (a percentage must be 100 times one).

   The page never sends numbers; it sends only text.
6. **Retry.** A failed check returns `422` with the problems and a retry message. The model gets one retry, continued from its cache with a short reasoning budget. Then the narrative is withheld.

The check proves that every number shown is the data's. It cannot prove that a sentence reads the number rightly: a live caveat called contributors_total (92) "contributors shown". The page says what was checked under every narrative.

The Claude path is the same check without the template: with a key, `GET .../narrative` hands Claude the full insight JSON with a structured-output schema, `narrate.Validate` checks the answer, a failure goes back once with the problems, and then it is withheld (`502 narrative_unsupported`, with the rejected text in the details). It is never returned as if it were sound.

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
7. **The code, painted.** An inverted event carries the start of one file's patch. The page shows it mirrored and red, and un-writes it: the lines the commit added erase, the lines it removed come back. A forward file event carries up to 16 lines of the rebuilt file around the change, with the lines the patch added marked. The page paints them on a sheet (context fades in, added lines type themselves out), then folds the sheet into the file's tower. A test checks that every painted line equals git's own content of that file at that commit and line number.
8. **The city.** A tower is a file's history: one layer per change, as thick as the lines it changed, colored from red (lines removed) to blue (lines added). Its cap is the file's proof: white exact, amber unproven, magenta mismatch.
   - The camera holds still. Right-drag turns it, and the wheel zooms.
   - Hovering a layer names the file, its proof and size, and the commit that wrote that layer, with its lines added and removed.
   - A click opens the file as the run rebuilt it (`GET /api/v1/reconstructions/{id}/file?path=`, served once the run is done).
9. **Length.** A run walks to the root, up to `max_reconstruct` commits (default 1,000), paging the commit list 100 at a time. A test walks a real 230-commit history across three pages.

## 3. Decisions and trade-offs

- **SQLite, not Postgres.** It keeps "runnable in 60 seconds" true. The store is behind four small methods, so moving to Postgres is a contained change. The double buffer is why one writer suffices.
- **The double buffer's sink was first `/dev/null`.** It simulated the database while the pipeline was built. The assignment prefers a real store, so each buffer now becomes one SQLite transaction. The mechanism is the same; only the flush function changed.
- **GraphQL for insights, REST for diffs.** GraphQL is 100 times cheaper for line counts. Only REST returns per-file patches and blob ids.
- **Default branch only, first parent only.** The metric counts what landed. The reconstruction replays the history the head was built from. Merged branches arrive as the merge's diff against its first parent.
- **The proof is a hash, not a heuristic.** If a patch does not apply byte for byte, the file is unproven. If the result's blob id differs from GitHub's, it is a mismatch. Both are shown, and neither is hidden.
- **A small model in the browser, not a key on the server.** It costs nothing to run and needs no secret, and the viewer's data stays with the viewer and this server. It reasons far worse than Claude. The check is what makes that safe: a number the model invents is refused, never shown. Expect plain narratives.
- **Constrained, not trusted to follow a format.** Free answers from the small model failed in every way a format can fail: JSON with a stray quote, several pairs on one line, pairs quoted in prose, invented paths, miscounted indices, numbers written as superscripts. Leniency in the parser chased each one and found the next. So the worker now writes the structure and holds each part with a logits lock, and the parser is strict. Banning numerals in prose was tried and dropped: the model then wrote runs of stray symbols where it wanted a number.
- **The narrative is checked, not trusted.** The confidence score is the model's, and is clamped and validated to the range 0 to 1. The numbers it cites are verified. Commit headlines and author names reach the model only as data, under a key named `..._untrusted`, with a system rule to never follow instructions inside them.
- **Security.**
  - **SSRF:** the repository is validated (`owner/name` only) and used only as escaped path segments under a fixed host, so a request cannot be steered elsewhere.
  - **Bodies and fields:** request bodies are capped at 4 KB, and unknown JSON fields are rejected.
  - **Headers:** responses carry a strict CSP, `nosniff`, `DENY` framing and `no-referrer`.
  - **WebSocket:** the stream checks the Origin, so another site cannot open it.
  - **The browser model:** the CSP allows scripts only from this server and jsdelivr (transformers.js and ONNX Runtime, at pinned versions), plus `'wasm-unsafe-eval'` for the wasm. Model downloads may come only from Hugging Face. The model's text is untrusted input to `/verify`: capped at 512 KB, parsed, never run or shown as HTML.
  - **Untrusted text on the page:** server text is drawn on canvases or set with `textContent`, never as HTML.
  - **Credentials:** they never appear in a log line, an error or a response. Tests check this, and so did a scan of the live run's log.

## 4. What I would do next with another day

- **Rate limiting of our own API, per client.** Today the service is bounded by caches, `singleflight` and a cap on live reconstructions, not by a per-client limit.
- **Warmer starts:** pre-fetch the commit list's ETags into SQLite, so a restart keeps the 304 savings that live only in memory now.
- **More signals:** pull requests merged and reviews, from the same GraphQL query family, so the narrative can tell "one author" from "one reviewer".
- **A stronger browser model.** The locks make every number true; the sentences around them are still a 1.5B model's. A larger model drops into the same pipeline (`browser_model`), with the same checks.
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

The later work added five more mechanisms, each broken the same way:
- the prose-number check;
- the percentage scaling;
- the `/verify` digest check;
- the strict evidence parse;
- the file endpoint's wait for the end of the run.

At first 4 of 5 failed as they should. The file wait survived, because no test asked for a file mid-run. A test that holds the fake GitHub mid-run now covers it; all 5 fail.

**Live runs** against GitHub found four defects that no fake had shown. All are fixed, each with a regression test:

1. **ETag 304.** GitHub's `304 Not Modified` carries no `Link` header. So a repeat reconstruction read "last page" and stopped at 70 of google/uuid's 109 commits. The client now caches `Link` with the body.
2. **Mode-only changes.** A mode-only change arrives with no patch and `sha: null`. It was reported as 4 mismatches; it is now carried as unchanged content.
3. **The repository regex.** The search field's `pattern` attribute was invalid under the browser's `v` regex flag, which stopped the auto-start.
4. **The event ticker.** It overlapped its own rows once it held more than about 15.

The browser narrative's own live defects are listed with its measurements below.

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

  A repeat google/uuid run made 5 GitHub calls instead of 114. spf13/pflag ran to its root past the old 200-commit limit: 233 commits, 589 file versions, all exact.

**The browser narrative, live** (2026-10-30, Chromium on WebGPU, Apple M1 Pro, golang/go October):

- **It proves end to end in the page.** With the final design, two full-budget runs proved: one on its first attempt, one on its retry (the first attempt cited a garbled year, and was refused for it). Short-budget runs also proved on their first attempt.
- **Speed:** the model is ready 7 to 9 s after the page opens, from the browser cache. It reasons at 18 to 31 tokens/s, so a full narrative takes 3 to 4 minutes.
- **The live runs found these defects of mine,** all fixed and, where the code allows, tested:
  - a static import of transformers.js failing inside a worker;
  - its wasm cache served as a `blob:` URL the CSP refuses;
  - an ONNX Runtime integer overflow when the whole conversation was re-read at about 7,000 tokens, fixed by continuing from the cache;
  - a budget cut that never fired, because the reasoning quoted the format;
  - a NaN confidence that made the `422` body empty;
  - the narrative panel visible on every page load;
  - the answer's lead-in missing from the text sent to the server, so `SUMMARY:` was not at a line start;
  - number symbols (`⁴`) slipping past an ASCII-only prose check, and passing as "proven";
  - "0.3926%" accepted for a share of 0.3926;
  - a claim written as a pasted JSON object, and one path cited twice.
- **The small model's own errors,** shown by the server's refusals:
  - invented keys (`no key "commits"`);
  - miscounted indices (`weekly[2].commits says 31, the data holds 89`);
  - wrong arithmetic (40.7% for 39.3%);
  - a garbled year (202026).

  The locks now make the paths and values right by construction. Prose numbers are checked.
- **What a proven narrative is.** Its numbers are all the data's; its sentences are a 1.5B model's, and some misread the numbers they use.

**Not yet verified live: the narrative endpoint against the real Anthropic API.** No key was available where this was built. The tests prove the request shape, the structured-output schema, the evidence check, the retry and the error mapping against a fake endpoint. With a key, the first live call is the README's narrative `curl`.
