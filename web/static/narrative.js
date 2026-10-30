// The narrative, written in the browser. The server builds the prompt over the
// period's data; a small reasoning model runs on this GPU (narrate-worker.js);
// the server then checks every number in the answer against the same data.
// The red text is the model reasoning; the blue card is what passed the check.
// All model and server text is set with textContent, never as HTML.
const $ = (id) => document.getElementById(id);
const panel = $('narr');
let worker = null, running = false;

function repoPath() {
  const v = $('repo').value.trim().replace(/^https:\/\/github\.com\//, '').replace(/\/+$/, '');
  const m = /^([A-Za-z0-9][A-Za-z0-9-]{0,38})\/([A-Za-z0-9._-]{1,100})$/.exec(v);
  if (!m) throw new Error('enter a repository as owner/name');
  return `${encodeURIComponent(m[1])}/${encodeURIComponent(m[2])}`;
}

function periodQuery() {
  const q = new URLSearchParams(location.search), out = new URLSearchParams();
  for (const k of ['since', 'until']) if (q.get(k)) out.set(k, q.get(k));
  return out.toString();
}

function status(text, err) { const s = $('n-status'); s.textContent = text; s.className = err ? 'red' : ''; }
function bar(f) { $('n-bar').style.width = `${Math.round(Math.max(0, Math.min(1, f)) * 100)}%`; }
const mb = (b) => (b / 1e6).toLocaleString(undefined, { maximumFractionDigits: 0 });

async function api(url, opts) {
  const r = await fetch(url, opts);
  const body = await r.json().catch(() => ({}));
  return { r, body };
}

// The model download: the worker starts it when the page opens, and the bar
// under the search box shows it. The weights come from Hugging Face at the
// commit the server pins, and the browser caches them; the binary embeds only
// the page code (VERIFIED 2026-10-30: web/static is 60 KB).
const files = new Map();
let modelReady = false, onWorker = null, modelName = 'the narrative model';
function downloaded() {
  let got = 0, all = 0;
  for (const f of files.values()) { got += f.loaded; all += f.total; }
  return { got, all };
}
function ensureWorker() {
  if (worker) return worker;
  worker = new Worker('narrate-worker.js', { type: 'module' });
  worker.onmessage = ({ data: d }) => {
    if (d.type === 'progress') {
      files.set(d.file, d);
      const { got, all } = downloaded();
      if (all > 1e6) {
        $('model-label').textContent = `${modelName}: downloading from Hugging Face, ${mb(got)} of ${mb(all)} MB (cached after the first visit)`;
        $('model-bar').style.width = `${Math.round((got / all) * 100)}%`;
      }
    } else if (d.type === 'ready') {
      modelReady = true;
      $('model-label').textContent = `${modelName}: ready on this GPU`;
      $('model-bar').style.width = '100%';
      $('model').classList.add('ready');
    } else if (d.type === 'load-error') {
      $('model-label').textContent = `${modelName}: ${d.text}`;
      $('model').classList.add('failed');
    }
    onWorker?.(d);
  };
  worker.onerror = (e) => worker.onmessage({ data: { type: 'load-error', text: e.message || 'the model worker failed to start' } });
  return worker;
}
async function preload() {
  const { r, body } = await api('/api/v1/status');
  const m = r.ok && body.narrative && body.narrative.browser;
  if (!m || !m.id) return;
  modelName = m.id.split('/').pop().replace(/-ONNX$/, '');
  $('model').hidden = false;
  if (!navigator.gpu) {
    $('model-label').textContent = `${modelName}: WebGPU is off in this browser, so EXPLAIN is off (a current Chrome, Edge or Safari has it)`;
    $('model').classList.add('failed');
    $('explain').disabled = true;
    return;
  }
  $('model-label').textContent = `${modelName}: starting the download from Hugging Face`;
  ensureWorker().postMessage({ type: 'load', model: m });
}
preload().catch(() => {});

// One answer from the worker: msg is {type:'generate', messages} for the first,
// {type:'retry', text} for a correction. Resolves with {text, tokens, ms, cut}.
function generate(model, msg) {
  ensureWorker();
  const think = $('n-think');
  let first = null;
  return new Promise((resolve, reject) => {
    onWorker = (d) => {
      switch (d.type) {
        case 'status': status(modelReady || d.text !== 'loading the model' ? d.text : 'waiting for the model download (the bar under the search box)'); break;
        case 'progress': {
          const { got, all } = downloaded();
          if (all > 1e6) { status(`downloading the model: ${mb(got)} of ${mb(all)} MB (cached after this)`); bar(got / all); }
          break;
        }
        case 'token': {
          const span = document.createElement('span');
          span.textContent = d.text;
          if (d.phase === 'answer') span.className = 'answer';
          think.append(span);
          think.scrollTop = think.scrollHeight;
          // Decode speed, timed from the first token so the prefill does not dilute it.
          if (!first || d.n < first.n) first = d;
          const rate = d.ms > first.ms ? ((d.n - first.n) / (d.ms - first.ms) * 1000).toFixed(1) : '…';
          status(`${d.phase === 'answer' ? 'answering' : 'reasoning'} · ${d.n} tokens · ${rate} tok/s`);
          bar(d.n / model.max_new_tokens);
          break;
        }
        case 'done': onWorker = null; resolve(d); break;
        case 'error': case 'load-error': onWorker = null; reject(new Error(d.text)); break;
      }
    };
    worker.postMessage({ ...msg, model });
  });
}

function render(res) {
  const n = res.narrative;
  $('n-summary').textContent = n.summary;
  $('n-hyp').textContent = n.hypothesis;
  $('n-conf').textContent = `${Math.round(n.confidence * 100)}%`;
  $('n-conf-bar').style.width = `${Math.round(n.confidence * 100)}%`;
  const ev = $('n-evidence');
  ev.replaceChildren(...n.evidence.map((e, i) => {
    const li = document.createElement('li');
    const claim = document.createElement('span'), at = document.createElement('code');
    claim.textContent = e.claim;
    at.textContent = `${e.metric} = ${e.value}`;
    li.append(claim, at);
    li.style.animationDelay = `${0.15 * i}s`;
    return li;
  }));
  $('n-caveats').replaceChildren(...(n.caveats || []).map((c) => { const li = document.createElement('li'); li.textContent = c; return li; }));
  $('n-fine').textContent = `${n.model} · attempt ${n.attempts} · the server checked every number here against the period's data (${res.source}): ` +
    'each evidence value at its path, each number in the text as one of the data\'s own. The reading of them is the model\'s.';
  $('n-result').hidden = false;
  panel.classList.add('proven');
}

async function explain() {
  if (running) return;
  running = true;
  $('explain').disabled = true;
  $('n-stop').hidden = false;
  panel.hidden = false;
  document.body.classList.add('narrating');
  panel.classList.remove('proven');
  $('n-result').hidden = true;
  $('n-think').replaceChildren();
  bar(0);
  try {
    const base = `/api/v1/repos/${repoPath()}/insights/narrative`;
    const q = periodQuery();
    status('reading the period\'s commits');
    for (let fresh = 0; fresh < 2; fresh++) {
      const { r, body: p } = await api(`${base}/prompt${q ? '?' + q : ''}`);
      if (!r.ok) throw new Error(p.error ? p.error.message : r.statusText);
      $('n-model').textContent = `${p.model.id.split('/').pop().replace(/-ONNX$/, '')} · on this GPU`;
      $('n-data').textContent = `${p.data.total_commits} commits by ${p.data.contributors_total} contributors · ${p.data.since.slice(0, 10)} to ${p.data.until.slice(0, 10)}`;
      let msg = { type: 'generate', messages: [{ role: 'system', content: p.system }, { role: 'user', content: p.user }] };
      const model = { ...p.model, template: p.template, paths: p.paths, data: p.data };
      for (let attempt = 1; attempt <= 2; attempt++) {
        const out = await generate(model, msg);
        if (out.interrupted) { status('stopped'); return; }
        status(`checking ${out.tokens} tokens of output against the data`);
        const { r: vr, body: v } = await api(`${base}/verify${q ? '?' + q : ''}`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ digest: p.digest, output: out.text, attempt }),
        });
        if (vr.ok) {
          status(`proven · ${out.tokens} tokens in ${(out.ms / 1000).toFixed(0)} s${out.cut ? ' · reasoning cut at the budget' : ''}`);
          bar(1);
          render(v);
          return;
        }
        if (vr.status === 409) break; // the data moved; fetch the prompt again
        if (vr.status !== 422) throw new Error(v.error ? v.error.message : vr.statusText);
        const probs = v.error.details.problems;
        const note = document.createElement('div');
        note.className = 'problems';
        note.textContent = `\n✗ the check refused attempt ${attempt}:\n  ${probs.join('\n  ')}\n`;
        $('n-think').append(note);
        if (attempt === 2) { status(`withheld after 2 attempts: ${probs[0]}`, true); return; }
        msg = { type: 'retry', text: v.error.details.retry_prompt };
      }
    }
    throw new Error('the period\'s data kept changing; try again');
  } catch (err) {
    status(err.message, true);
  } finally {
    running = false;
    $('explain').disabled = false;
    $('n-stop').hidden = true;
  }
}

$('explain').addEventListener('click', explain);
$('n-stop').addEventListener('click', () => worker && worker.postMessage({ type: 'stop' }));
$('n-close').addEventListener('click', () => {
  if (running && worker) worker.postMessage({ type: 'stop' });
  panel.hidden = true;
  document.body.classList.remove('narrating');
});
