// The narrative model, in a worker so the scene keeps its frame rate. It runs a
// small reasoning model on the viewer's GPU (WebGPU) with transformers.js. The
// weights come from Hugging Face at the commit the server pins; the browser
// caches them after the first visit. The server trusts none of this: the page
// posts the model's text to /verify, which checks it against the data.
//
// Two loading details, both found live (2026-10-30, Chromium 156):
//   - import(), not a static import: a module worker's static import of this
//     bundle fails before any code runs, with an error that carries no message;
//   - useWasmCache off: on, ONNX Runtime's loader is re-served as a blob: URL,
//     which the CSP refuses; off, it comes from jsdelivr, which the CSP allows.
const T = import('https://cdn.jsdelivr.net/npm/@huggingface/transformers@4.3.1/dist/transformers.min.js')
  .then((m) => { m.env.allowLocalModels = false; m.env.useWasmCache = false; return m; });

// Qwen2 chat turns end with <|im_end|> (151645); the repo's generation config
// stops on <|endoftext|> (151643) alone (VERIFIED 2026-10-30), so both are given.
const EOS = [151643, 151645];
let tok, model, loadedKey = '', stopper = null;
// The conversation so far: every token id, and the model's cache of all but the
// last (the last sampled token is never run through the model). Every later part
// extends it, so only new tokens are read: re-reading a ~7,000-token conversation
// in one pass overflowed ONNX Runtime (VERIFIED 2026-10-30).
let ids = [], cache = null;

// load fetches the model once. The page asks for it when it opens, so the
// download runs while the viewer looks around; a narrative asked for mid-download
// waits on the same load.
let loading = null;
function load(m) {
  const key = `${m.id}@${m.revision}/${m.dtype}`;
  if (key === loadedKey) return Promise.resolve();
  if (loading && loading.key === key) return loading.p;
  const p = (async () => {
    const { AutoTokenizer, AutoModelForCausalLM } = await T;
    const progress_callback = (x) => {
      if (x.status === 'progress' || x.status === 'done') {
        postMessage({ type: 'progress', file: x.file, loaded: x.loaded ?? 0, total: x.total ?? 0 });
      }
    };
    const opts = { revision: m.revision, progress_callback };
    tok = await AutoTokenizer.from_pretrained(m.id, opts);
    model = await AutoModelForCausalLM.from_pretrained(m.id, { ...opts, dtype: m.dtype, device: 'webgpu' });
    sortVocabulary();
    loadedKey = key;
    postMessage({ type: 'ready' });
  })();
  loading = { key, p };
  p.catch(() => { if (loading && loading.p === p) loading = null; });
  return p;
}

const encode = (text) => tok.encode(text, { add_special_tokens: false });
const pieces = new Map(); // token id -> its text
const piece = (id) => { let s = pieces.get(id); if (s === undefined) pieces.set(id, (s = tok.decode([id]))); return s; };

// The vocabulary, sorted once after the model loads:
//   - bannedInProse: tokens holding a number symbol other than the numerals 0-9
//     ("⁴", "½": the server reads numerals, so it checks every number prose
//     writes), or "=", "|", "[", "]", "{", "}", so a claim stays words, not a
//     pasted object (one was, live). (Banning numerals
//     too made the model write runs of stray symbols where it wanted a number;
//     seen live, 2026-10-30.)
//   - numberIds: tokens of ASCII digits only, or a newline (all a number part
//     may write; its label already wrote "0.").
let bannedInProse = [], numberIds = [];
function sortVocabulary() {
  bannedInProse = []; numberIds = [...EOS];
  for (let id = 0; id < model.config.vocab_size; id++) {
    const s = piece(id);
    if (/[=|[\]{}]|(?![0-9])\p{N}/u.test(s)) bannedInProse.push(id);
    if (/^[0-9]+$|^\n+$/.test(s)) numberIds.push(id);
  }
}

// keepOnly leaves the logits of ids as they are and rules out every other token.
function keepOnly(data, ids) {
  const kept = ids.map((id) => data[id]);
  data.fill(-Infinity);
  ids.forEach((id, i) => { data[id] = kept[i]; });
}

// partLock is the logits processor that holds one part of the template
// (narrate.Template) to its kind. start is where the part's tokens begin.
async function partLock(kind, start, paths) {
  const { LogitsProcessor } = await T;
  let rule;
  if (kind === 'path') {
    // A token is let through only when the text stays the start of a numeric
    // path. Candidates: the tokens of every path and of each character a path
    // uses, so every path can be spelled. Once a path is whole only the end
    // token fits, and the part ends: no numeric path is the start of another
    // (narrate's TestBrowserPromptCarriesTheDataAndItsDigest asserts it).
    const prefixes = new Set(), cand = new Set();
    for (const p of paths) {
      for (let i = 1; i <= p.length; i++) prefixes.add(p.slice(0, i));
      for (const id of encode(p)) cand.add(id);
      for (const ch of p) for (const id of encode(ch)) cand.add(id);
    }
    const candidates = [...cand];
    rule = (ids, data) => {
      const part = ids.slice(start).map(Number);
      const sofar = part.length ? tok.decode(part) : ''; // decode([]) throws
      const fit = candidates.filter((id) => prefixes.has(sofar + piece(id)));
      keepOnly(data, fit.length ? fit : EOS);
    };
  } else if (kind === 'number') {
    rule = (_, data) => keepOnly(data, numberIds);
  } else {
    rule = (_, data) => { for (const id of bannedInProse) data[id] = -Infinity; };
  }
  return new (class extends LogitsProcessor {
    _call(inputIds, logits) { rule(inputIds[0], logits[0].data); return logits; }
  })();
}

// run continues the conversation by the `more` token ids and generates up to
// maxNew tokens, or until until(text so far) is true.
async function run(more, maxNew, phase, until, processors = []) {
  const { TextStreamer, Tensor, InterruptableStoppingCriteria } = await T;
  const enough = new InterruptableStoppingCriteria();
  const start = ids.length + more.length;
  const all = [...ids, ...more.map(BigInt)];
  let n = 0, seen = '';
  const t0 = performance.now();
  const streamer = new TextStreamer(tok, {
    skip_prompt: true,
    skip_special_tokens: true,
    token_callback_function: () => { n++; },
    callback_function: (text) => {
      postMessage({ type: 'token', text, phase, n, ms: performance.now() - t0 });
      seen += text;
      if (until(seen)) enough.interrupt();
    },
  });
  const out = await model.generate({
    input_ids: new Tensor('int64', all, [1, all.length]),
    attention_mask: new Tensor('int64', all.map(() => 1n), [1, all.length]),
    past_key_values: cache, return_dict_in_generate: true,
    max_new_tokens: maxNew, do_sample: true, temperature: 0.6, top_p: 0.95,
    eos_token_id: EOS, streamer, stopping_criteria: [stopper, enough], logits_processor: processors,
  });
  ids = out.sequences.tolist()[0];
  cache = out.past_key_values;
  // A part the model ended itself ends on an end token. It is the last sampled
  // token, outside the cache, so it is dropped and the conversation continues.
  if (EOS.includes(Number(ids.at(-1)))) ids.pop();
  const gen = ids.slice(start);
  return { text: tok.decode(gen, { skip_special_tokens: true }), tokens: gen.length, ms: performance.now() - t0 };
}

// resolve reads the number at a path such as "contributors[0].commits".
function resolve(doc, path) {
  let cur = doc;
  for (const [, key, idx] of path.matchAll(/([A-Za-z0-9_]+)|\[(\d+)\]/g)) cur = cur?.[key ?? Number(idx)];
  return typeof cur === 'number' ? cur : undefined;
}

// turn writes one answer. The model reasons until it closes </think>, ends its
// turn or spends its budget. Then the worker leads it through the server's
// template: it writes each label, and the model writes each part under its lock;
// a value is copied from the data at the path the model chose.
async function turn(more, m) {
  stopper.reset();
  const think = await run(more, m.max_new_tokens, 'think', (s) => s.includes('</think>'));
  let { text, tokens, ms } = think;
  const cut = think.tokens >= m.max_new_tokens; // the reasoning ran out of budget
  // carry: text written for the model that it has not read yet. It is part of
  // the answer too: without the lead-in, SUMMARY: was not at a line start and the
  // server found no answer (seen live, 2026-10-30).
  let carry = text.includes('</think>') ? '\n\n' : '\n</think>\n\n';
  text += carry;
  let path = '';
  const used = new Set(); // an answer cites each path once (one cited the same path twice, live)
  for (const f of m.template) {
    if (stopper.interrupted) break;
    if (f.kind === 'value') {
      const v = resolve(m.data, path);
      const s = f.label + (v === undefined ? '?' : String(v));
      carry += s; text += s;
      postMessage({ type: 'token', text: s, phase: 'answer', n: 0, ms: 0 });
      continue;
    }
    postMessage({ type: 'token', text: f.label, phase: 'answer', n: 0, ms: 0 });
    const more = encode(carry + f.label);
    carry = '';
    const start = ids.length + more.length;
    const until = f.kind === 'path' ? () => false : (s) => s.trim() !== '' && s.includes('\n');
    const r = await run(more, f.max_tokens, 'answer', until, [await partLock(f.kind, start, m.paths.filter((p) => !used.has(p)))]);
    const part = r.text.replace(/\n+$/, '');
    if (f.kind === 'path') used.add(path = part);
    text += f.label + part; tokens += r.tokens; ms += r.ms;
  }
  postMessage({ type: 'done', text, tokens, ms, cut, interrupted: stopper.interrupted });
}

// generate starts a conversation from the server's system and user messages.
async function generate({ model: m, messages }) {
  stopper ??= new (await T).InterruptableStoppingCriteria();
  postMessage({ type: 'status', text: 'loading the model' });
  await load(m);
  await cache?.dispose();
  ids = []; cache = null;
  postMessage({ type: 'status', text: 'reading the data' });
  await turn(encode(tok.apply_chat_template(messages, { add_generation_prompt: true, tokenize: false })), m);
}

// retry adds the server's correction as a user turn and answers again. The
// correction names what to fix, so the reasoning budget is short: a full one
// re-argued the whole document for minutes (seen live, 2026-10-30).
async function retry({ model: m, text }) {
  if (!cache) throw new Error('nothing to retry');
  postMessage({ type: 'status', text: 'reading the correction' });
  await turn(encode(`<|im_end|>\n<|im_start|>user\n${text}<|im_end|>\n<|im_start|>assistant\n`),
    { ...m, max_new_tokens: Math.min(512, m.max_new_tokens) });
}

onmessage = async ({ data }) => {
  if (data.type === 'stop') { stopper?.interrupt(); return; }
  try {
    if (!navigator.gpu) throw new Error('WebGPU is off in this browser (a current Chrome, Edge or Safari has it)');
    if (data.type === 'load') await load(data.model);
    else if (data.type === 'generate') await generate(data);
    else if (data.type === 'retry') await retry(data);
  } catch (err) {
    postMessage({ type: data.type === 'load' ? 'load-error' : 'error', text: String(err && err.message || err) });
  }
};
