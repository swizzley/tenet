// Tenet: reconstruct a repository from its diffs, as a temporal pincer.
// Phase 1 (red, inverted): the server walks back through history; each commit
// arrives as a ring receding down a tunnel, its headline mirrored, while particles
// fall inward (entropy running backward). The turnstile flips time. Phase 2 (blue,
// forward): each diff replays; every file's shards un-shatter into its slab in a
// city of directories, colored by proof: exact (blue), unproven (amber), mismatch (red).
// All server text is untrusted: it is drawn on canvases or set with textContent.
import * as THREE from 'https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js';

const RED = new THREE.Color(0xff3b3b), BLUE = new THREE.Color(0x3d8bff), AMBER = new THREE.Color(0xffb547);
const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;
const $ = (id) => document.getElementById(id);

// ---------- scene ----------
const renderer = new THREE.WebGLRenderer({ canvas: $('scene'), antialias: true });
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
const scene = new THREE.Scene();
scene.background = new THREE.Color(0x030407);
scene.fog = new THREE.FogExp2(0x030407, 0.012);
const camera = new THREE.PerspectiveCamera(60, 1, 0.1, 2000);
camera.position.set(0, 0, 12);
scene.add(new THREE.AmbientLight(0x404858, 1.2));
const key = new THREE.PointLight(0xffffff, 60, 0, 1.6);
key.position.set(6, 18, 10);
scene.add(key);

function resize() {
  renderer.setSize(innerWidth, innerHeight, false);
  camera.aspect = innerWidth / innerHeight;
  camera.updateProjectionMatrix();
}
addEventListener('resize', resize);
resize();

// The particle field: in the inverted phase points fall toward the axis, in the
// forward phase they fly outward. Same motion, opposite sign: inversion.
const N = reduced ? 600 : 2600;
const pGeo = new THREE.BufferGeometry();
const pos = new Float32Array(N * 3), vel = new Float32Array(N * 3);
for (let i = 0; i < N; i++) seedParticle(i, true);
pGeo.setAttribute('position', new THREE.BufferAttribute(pos, 3));
const pMat = new THREE.PointsMaterial({ size: 0.09, color: 0x8a96b0, transparent: true, opacity: 0.8, blending: THREE.AdditiveBlending, depthWrite: false });
scene.add(new THREE.Points(pGeo, pMat));
function seedParticle(i, anywhere) {
  const r = anywhere ? 4 + Math.random() * 40 : 30 + Math.random() * 15;
  const a = Math.random() * Math.PI * 2;
  pos[i * 3] = Math.cos(a) * r;
  pos[i * 3 + 1] = Math.sin(a) * r * 0.7;
  pos[i * 3 + 2] = camera.position.z - Math.random() * 160;
  vel[i * 3] = -Math.cos(a); vel[i * 3 + 1] = -Math.sin(a) * 0.7; vel[i * 3 + 2] = 0;
}
let entropy = -1; // -1: inverted (implode), +1: forward (explode)

// ---------- text on canvas ----------
function label(text, color, mirrored, w = 512, h = 64, size = 26) {
  const c = document.createElement('canvas');
  c.width = w; c.height = h;
  const g = c.getContext('2d');
  if (mirrored) { g.translate(w, 0); g.scale(-1, 1); }
  g.font = `${size}px ui-monospace, Menlo, monospace`;
  g.fillStyle = color;
  g.textBaseline = 'middle';
  g.fillText(String(text).slice(0, 48), 8, h / 2);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

// ---------- phase 1: the inverted tunnel ----------
const tunnel = new THREE.Group();
scene.add(tunnel);
const ringGeo = new THREE.TorusGeometry(5, 0.035, 8, 96);
let depth = 0;
function invertCommit(e) {
  depth = e.i * 3.2;
  const ring = new THREE.Mesh(ringGeo, new THREE.MeshBasicMaterial({ color: RED, transparent: true, opacity: 0.9 }));
  ring.position.z = -depth;
  ring.rotation.z = Math.random() * Math.PI;
  ring.userData.spin = (Math.random() - 0.5) * 0.02;
  tunnel.add(ring);
  // the headline, mirrored: read from the other side of the turnstile
  const sp = new THREE.Sprite(new THREE.SpriteMaterial({ map: label(e.headline || e.sha.slice(0, 12), '#ff6b6b', true), transparent: true, opacity: 0.85, depthWrite: false }));
  sp.scale.set(8, 1, 1);
  sp.position.set((e.i % 2 ? 1 : -1) * 3.2, (e.i % 3 - 1) * 1.6, -depth);
  tunnel.add(sp);
  while (tunnel.children.length > 160) dispose(tunnel.children[0], tunnel);
}

// ---------- the turnstile ----------
const turnstile = new THREE.Group();
for (let k = 0; k < 3; k++) {
  const t = new THREE.Mesh(new THREE.TorusGeometry(6.5 + k * 0.6, 0.06, 8, 160), new THREE.MeshBasicMaterial({ color: k === 1 ? 0xffffff : RED, transparent: true, opacity: 0.7 }));
  turnstile.add(t);
}
for (let k = 0; k < 8; k++) {
  const spoke = new THREE.Mesh(new THREE.BoxGeometry(0.06, 6.2, 0.06), new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.5 }));
  spoke.rotation.z = (k / 8) * Math.PI;
  turnstile.add(spoke);
}
turnstile.visible = false;
scene.add(turnstile);

// ---------- phase 2: the forward city ----------
const city = new THREE.Group();
scene.add(city);
const box = new THREE.BoxGeometry(1, 1, 1);
const shardGeo = new THREE.TetrahedronGeometry(0.16);
// The city: each directory is a district on a golden-angle spiral; each file is a
// tower in its district (a smaller spiral), standing on the ground, its height
// growing with its line count.
const dirs = new Map();   // dir -> {x, z, next, files: Map(path -> mesh)}
const shards = [];
let extent = 6;           // how far the city reaches, for the camera
function dirOf(path) { const i = path.lastIndexOf('/'); return i < 0 ? '/' : path.slice(0, i); }
function dirSlot(d) {
  if (!dirs.has(d)) {
    const k = dirs.size, r = k ? 9 * Math.sqrt(k) : 0, a = k * 2.39996;
    const x = Math.cos(a) * r, z = Math.sin(a) * r;
    const base = new THREE.Sprite(new THREE.SpriteMaterial({ map: label(d, '#7c8597', false, 512, 48, 22), transparent: true, opacity: 0.8, depthWrite: false }));
    base.scale.set(5, 0.47, 1);
    base.position.set(x, -0.6, z + 3.2);
    city.add(base);
    dirs.set(d, { x, z, next: 0, files: new Map() });
  }
  return dirs.get(d);
}
function lot(slot) { // the next free tower position in a district
  const j = slot.next++, r = j ? 1.9 * Math.sqrt(j) : 0, a = j * 2.39996;
  const p = { x: slot.x + Math.cos(a) * r, z: slot.z + Math.sin(a) * r };
  extent = Math.max(extent, Math.hypot(p.x, p.z) + 4);
  return p;
}
const statusColor = (s) => (s === 'exact' ? BLUE : s === 'mismatch' ? RED : AMBER);
function forwardFile(e) {
  if (e.op === 'renamed' && e.from) removeFile(e.from, false);
  if (e.status === 'deleted') { removeFile(e.path, true); return; }
  const slot = dirSlot(dirOf(e.path));
  let m = slot.files.get(e.path);
  const h = Math.min(9, 0.2 + Math.log2((e.lines || e.adds || 1) + 1) * 0.6);
  const col = statusColor(e.status);
  if (!m) {
    const p = lot(slot);
    m = new THREE.Mesh(box, new THREE.MeshStandardMaterial({ color: col, emissive: col, emissiveIntensity: 0.55, roughness: 0.35, metalness: 0.2, transparent: true, opacity: 0.92 }));
    m.scale.set(1.3, 0.001, 1.3);
    m.position.set(p.x, 0, p.z);
    city.add(m);
    slot.files.set(e.path, m);
  } else {
    m.material.color.copy(col);
    m.material.emissive.copy(col);
  }
  m.userData.h = h;
  m.userData.ty = h / 2;
  m.userData.flash = 1;
  // un-shatter: shards start scattered and converge on the slab
  const n = reduced ? 2 : Math.min(14, 3 + (e.adds || 0) / 6);
  for (let k = 0; k < n && shards.length < 700; k++) {
    const s = new THREE.Mesh(shardGeo, new THREE.MeshBasicMaterial({ color: col, transparent: true, opacity: 0.95 }));
    const a = Math.random() * Math.PI * 2, r = 8 + Math.random() * 14;
    s.position.set(slot.x + Math.cos(a) * r, m.userData.ty + (Math.random() - 0.2) * 12, slot.z + Math.sin(a) * r);
    s.userData = { from: s.position.clone(), to: m, t: 0, dur: 0.6 + Math.random() * 0.5, inward: true };
    city.add(s);
    shards.push(s);
  }
}
function removeFile(path, burst) {
  const slot = dirs.get(dirOf(path));
  const m = slot && slot.files.get(path);
  if (!m) return;
  slot.files.delete(path);
  if (burst) { // deletion runs forward: the slab shatters outward
    for (let k = 0; k < 8 && shards.length < 700; k++) {
      const s = new THREE.Mesh(shardGeo, new THREE.MeshBasicMaterial({ color: 0x8a96b0, transparent: true, opacity: 0.9 }));
      s.position.copy(m.position);
      const a = Math.random() * Math.PI * 2;
      s.userData = { from: m.position.clone(), dest: m.position.clone().add(new THREE.Vector3(Math.cos(a) * 9, Math.random() * 6, Math.sin(a) * 9)), t: 0, dur: 0.9, inward: false };
      city.add(s);
      shards.push(s);
    }
  }
  dispose(m, city);
}
function dispose(o, parent) {
  parent.remove(o);
  if (o.material) { if (o.material.map) o.material.map.dispose(); o.material.dispose(); }
}

// ---------- HUD ----------
const stats = { commits: 0, files: 0, exact: 0, unknown: 0, mismatch: 0 };
function hud() {
  $('s-commits').textContent = stats.commits;
  $('s-files').textContent = stats.files;
  $('s-exact').textContent = stats.exact;
  $('s-unknown').textContent = stats.unknown;
  $('s-mismatch').textContent = stats.mismatch;
}
function tick(text, cls) {
  const d = document.createElement('div');
  d.textContent = text;
  d.className = cls;
  const t = $('ticker');
  t.prepend(d);
  while (t.childNodes.length > 60) t.lastChild.remove();
}
function setPhase(name, cls) { const p = $('phase'); p.textContent = name; p.className = 'phase ' + cls; }
function say(text, err) { const m = $('msg'); m.textContent = text; m.className = err ? 'err' : ''; }

// ---------- the event queue ----------
let queue = [], holdUntil = 0, phase = 'idle', clock = 0;
function handle(e) {
  switch (e.t) {
    case 'meta':
      say(`${e.repo} · ${e.branch} @ ${e.sha.slice(0, 12)} · walking back up to ${e.commits} commits`);
      break;
    case 'invert':
      stats.commits = e.i; clock = -e.i;
      invertCommit(e);
      tick(`◂ ${e.sha.slice(0, 8)} ${e.headline || ''}`, 'inv');
      break;
    case 'turnstile':
      turnstile.position.set(0, 0, -depth - 6);
      turnstile.visible = true;
      setPhase(e.root ? 'turnstile · root reached' : 'turnstile · window edge', 'inv');
      holdUntil = performance.now() + (reduced ? 300 : 2200);
      flyThrough = { t0: performance.now(), from: camera.position.clone() };
      break;
    case 'commit':
      if (phase !== 'fwd') {
        phase = 'fwd';
        setPhase('forward', 'fwd');
        entropy = 1;
        pMat.color.copy(BLUE);
        scene.fog.density = 0.008;
      }
      clock = e.i;
      tick(`▸ ${e.sha.slice(0, 8)} ${e.headline || ''}`, 'fwd');
      break;
    case 'file':
      if (e.status !== 'deleted') {
        stats.files++;
        stats[e.status] = (stats[e.status] || 0) + 1;
      }
      forwardFile(e);
      tick(`${e.status.padEnd(8)} ${e.path}${e.why ? ' · ' + e.why : ''}`, e.status);
      break;
    case 'done':
      showSummary(e.summary);
      break;
    case 'error':
      say(e.msg, true);
      $('go').disabled = false;
      break;
  }
  hud();
}

function showSummary(s) {
  const pct = s.head_files ? Math.round((100 * s.head_exact) / s.head_files) : 0;
  $('sum-title').textContent = s.mismatch ? 'Reconstruction · mismatches found' : 'Reconstruction proven';
  $('sum-line').textContent = `${s.head_exact} of ${s.head_files} files at the head rebuilt byte-exact from diffs alone (${pct}%).`;
  const rows = [
    ['commits walked', s.commits], ['root reached', s.root_reached ? 'yes' : 'no (window edge)'],
    ['file versions built', s.file_versions], ['proven exact', s.exact], ['unproven', s.unknown], ['mismatch', s.mismatch],
    ['head files unproven', s.head_unknown], ['head files never built', s.head_missing],
    ['GitHub calls', s.api_calls], ['from the database', s.cache_hits], ['time', (s.millis / 1000).toFixed(1) + ' s'],
  ];
  const grid = $('sum-grid');
  grid.replaceChildren();
  for (const [k, v] of rows) {
    const dt = document.createElement('dt'); dt.textContent = k;
    const dd = document.createElement('dd'); dd.textContent = v;
    grid.append(dt, dd);
  }
  $('sum-fine').textContent = 'Each file version is checked against the blob id GitHub reports for it; the final tree against the head commit. Unproven means the content was never streamed (a binary, a truncated patch, or a file older than the window), never guessed.';
  $('summary').hidden = false;
  $('go').disabled = false;
  say('done · enter another repository to run again');
}

// ---------- the stream ----------
let ws = null, received = 0, sessionId = null;
function connect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/api/v1/reconstructions/${sessionId}/events?from=${received}`);
  ws.onmessage = (m) => { received++; queue.push(JSON.parse(m.data)); };
  ws.onclose = (ev) => {
    if (ev.code !== 1000 && sessionId) setTimeout(connect, 1000); // resume where it broke
  };
}

function reset() {
  for (const g of [tunnel, city]) while (g.children.length) dispose(g.children[0], g);
  dirs.clear(); shards.length = 0; queue = []; received = 0; depth = 0; clock = 0; extent = 6;
  Object.keys(stats).forEach((k) => (stats[k] = 0));
  phase = 'inv'; entropy = -1; turnstile.visible = false; flyThrough = null;
  pMat.color.copy(RED); scene.fog.density = 0.012;
  camera.position.set(0, 0, 12); camera.lookAt(0, 0, 0);
  for (let i = 0; i < N; i++) seedParticle(i, true);
  $('ticker').replaceChildren();
  $('summary').hidden = true;
  $('stats').hidden = false;
  setPhase('inverted', 'inv');
  hud();
}

$('query').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const repo = $('repo').value.trim();
  $('go').disabled = true;
  say('requesting inversion…');
  try {
    const r = await fetch('/api/v1/reconstructions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ repo }) });
    const body = await r.json();
    if (!r.ok) throw new Error(body.error ? body.error.message : r.statusText);
    if (ws) { sessionId = null; ws.close(1000); }
    reset();
    sessionId = body.id;
    connect();
  } catch (err) {
    say(err.message, true);
    $('go').disabled = false;
  }
});

// ---------- the loop ----------
let flyThrough = null, last = performance.now(), tokens = 0;
const ease = (t) => 1 - Math.pow(1 - t, 3);
function frame(now) {
  const dt = Math.min(0.05, (now - last) / 1000);
  last = now;

  // drain the queue at a rate that scales with its length (about 10 s to clear a
  // backlog, never slower than 15 events a second), paused through the turnstile
  if (now >= holdUntil && queue.length) {
    tokens += Math.min(600, Math.max(15, queue.length / 10)) * dt * (reduced ? 4 : 1);
    while (tokens >= 1 && queue.length && now >= holdUntil) { tokens--; handle(queue.shift()); }
  } else {
    tokens = 0;
  }

  // camera
  if (phase === 'inv' && !flyThrough) {
    const targetZ = 12 - depth;
    camera.position.z += (targetZ - camera.position.z) * Math.min(1, dt * 2.5);
    camera.position.x = Math.sin(now / 2400) * 0.6;
    camera.lookAt(0, 0, camera.position.z - 20);
  } else if (flyThrough) {
    const t = Math.min(1, (now - flyThrough.t0) / (reduced ? 300 : 2200));
    camera.position.z = flyThrough.from.z + (turnstile.position.z - 4 - flyThrough.from.z) * ease(t);
    camera.lookAt(0, 0, camera.position.z - 20);
    turnstile.rotation.z += dt * (1 + t * 10);
    if (t > 0.72 && !flyThrough.flashed) {
      flyThrough.flashed = true;
      const f = $('flash'); f.classList.add('on');
      requestAnimationFrame(() => requestAnimationFrame(() => f.classList.remove('on')));
      tunnel.visible = false; turnstile.visible = false;
    }
    if (t >= 1) { flyThrough = null; tunnel.visible = true; while (tunnel.children.length) dispose(tunnel.children[0], tunnel); }
  } else if (phase === 'fwd') {
    // orbit the whole city, low enough to read a skyline, far enough to hold it all
    const r = 10 + extent * 1.35;
    const a = now / 9000;
    camera.position.lerp(new THREE.Vector3(Math.cos(a) * r, 4 + r * 0.42, Math.sin(a) * r), Math.min(1, dt * 1.5));
    camera.lookAt(0, 1.5, 0);
  }

  // tunnel rings spin; the newest glow
  for (const o of tunnel.children) if (o.userData.spin) o.rotation.z += o.userData.spin;

  // particles: implode while inverted, explode once time runs forward
  // vel points at the axis: moving along it implodes, against it explodes
  const cz = camera.position.z, s = 6 * dt;
  for (let i = 0; i < N; i++) {
    const j = i * 3;
    pos[j] -= vel[j] * s * entropy;
    pos[j + 1] -= vel[j + 1] * s * entropy;
    const r2 = pos[j] * pos[j] + pos[j + 1] * pos[j + 1];
    if (entropy < 0 && (r2 < 1 || pos[j + 2] > cz + 5 || pos[j + 2] < cz - 200)) {
      seedParticle(i, false); // arrives from the rim, falls inward
    } else if (entropy > 0 && r2 > 2600) {
      seedParticle(i, false); // leaves from the core, flies outward
      pos[j] *= 0.03; pos[j + 1] *= 0.03; pos[j + 2] = (Math.random() - 0.5) * 80;
    }
  }
  pGeo.attributes.position.needsUpdate = true;

  // slabs grow to their height and settle; a fresh change flashes
  for (const slot of dirs.values()) {
    for (const m of slot.files.values()) {
      m.scale.y += (m.userData.h - m.scale.y) * Math.min(1, dt * 6);
      m.position.y += (m.userData.ty - m.position.y) * Math.min(1, dt * 6);
      if (m.userData.flash > 0) {
        m.userData.flash = Math.max(0, m.userData.flash - dt * 1.5);
        m.material.emissiveIntensity = 0.55 + m.userData.flash * 1.8;
      }
    }
  }

  // shards: inward ones converge on their slab (un-shattering), outward ones fly off
  for (let k = shards.length - 1; k >= 0; k--) {
    const s = shards[k], u = s.userData;
    u.t += dt / u.dur;
    const t = Math.min(1, u.t);
    const dest = u.inward ? u.to.position : u.dest;
    s.position.lerpVectors(u.from, dest, u.inward ? ease(t) : t);
    s.rotation.x += dt * 6; s.rotation.y += dt * 4;
    s.material.opacity = u.inward ? 0.95 : 0.9 * (1 - t);
    if (t >= 1) { dispose(s, city); shards.splice(k, 1); }
  }

  // the clock counts down while inverted, up once forward
  const c = Math.abs(clock);
  $('clock').textContent = `${clock < 0 ? '−' : ' '}${String(Math.floor(c / 60)).padStart(2, '0')}:${String(c % 60).padStart(2, '0')}`;

  renderer.render(scene, camera);
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

const fromURL = new URLSearchParams(location.search).get('repo');
if (fromURL) { $('repo').value = fromURL; $('query').requestSubmit(); }
