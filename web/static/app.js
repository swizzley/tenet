// Tenet: reconstruct a repository from its diffs, as a temporal pincer.
// Phase 1 (red, inverted): the server walks back through history; each commit
// arrives as a ring receding down a tunnel, its headline mirrored, its diff
// un-writing itself, while particles fall inward (entropy running backward). The
// turnstile flips time. Phase 2 (blue, forward): each diff replays; each rebuilt
// file paints itself, then folds into its tower in a city of directories, a tower
// built of one layer per change. The viewer turns, zooms, hovers and opens files.
// All server text is untrusted: it is drawn on canvases or set with textContent.
import * as THREE from 'https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js';

const RED = new THREE.Color(0xff3b3b), BLUE = new THREE.Color(0x3d8bff);
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
// tower in its district (a smaller spiral). A tower is its history: one layer per
// change, as thick as the lines it changed, colored from red (lines removed) to
// blue (lines added). Its cap shows the file's proof: white exact, amber
// unproven, magenta mismatch.
const dirs = new Map();   // dir -> {x, z, next, files: Map(path -> tower)}
const shards = [];
const pickables = [];     // the layers and caps a pointer can hover or click
const ADDED = new THREE.Color(0x3d8bff), REMOVED = new THREE.Color(0xff3b3b);
const CAP = { exact: 0xeef4ff, unknown: 0xffb547, mismatch: 0xff2bd6 };
const MAX_LAYERS = 120;   // a tower keeps its newest changes; older ones count in tw.dropped
let commitNow = null;     // the commit being replayed, for its layers
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
function newTower(slot, path) {
  const p = lot(slot);
  const group = new THREE.Group();
  group.position.set(p.x, 0, p.z);
  city.add(group);
  const cap = new THREE.Mesh(box, new THREE.MeshStandardMaterial({ emissiveIntensity: 0.9, roughness: 0.3 }));
  cap.scale.set(1.42, 0.09, 1.42);
  group.add(cap);
  const tw = { path, group, cap, layers: [], top: 0, versions: 0, dropped: 0, status: '', lines: 0 };
  cap.userData.tower = tw;
  pickables.push(cap);
  slot.files.set(path, tw);
  return tw;
}
function forwardFile(e) {
  if (e.op === 'renamed' && e.from) removeFile(e.from, false);
  if (e.status === 'deleted') { removeFile(e.path, true); return; }
  const slot = dirSlot(dirOf(e.path));
  const tw = slot.files.get(e.path) || newTower(slot, e.path);
  const adds = e.adds || 0, dels = e.dels || 0;
  const col = REMOVED.clone().lerp(ADDED, adds + dels ? adds / (adds + dels) : 0.5);
  const th = Math.min(1.4, 0.1 + Math.log2(1 + adds + dels) * 0.1);
  const m = new THREE.Mesh(box, new THREE.MeshStandardMaterial({ color: col, emissive: col, emissiveIntensity: 0.45, roughness: 0.4, metalness: 0.15 }));
  m.scale.set(1.3, 0.001, 1.3);
  m.position.y = tw.top;
  m.userData = { tower: tw, th, y: tw.top + th / 2, flash: 1, adds, dels, commit: commitNow };
  tw.group.add(m);
  tw.layers.push(m);
  pickables.push(m);
  tw.top += th;
  if (tw.layers.length > MAX_LAYERS) { // drop the oldest layer; the rest settle down
    const old = tw.layers.shift();
    pickables.splice(pickables.indexOf(old), 1);
    dispose(old, tw.group);
    tw.dropped++;
    tw.top -= old.userData.th;
    for (const l of tw.layers) l.userData.y -= old.userData.th;
  }
  tw.versions++;
  tw.status = e.status;
  tw.lines = e.lines || 0;
  tw.cap.material.color.setHex(CAP[e.status] ?? CAP.unknown);
  tw.cap.material.emissive.setHex(CAP[e.status] ?? CAP.unknown);
  // un-shatter: shards start scattered and converge on the new layer
  const to = new THREE.Vector3(tw.group.position.x, tw.top, tw.group.position.z);
  const n = reduced ? 2 : Math.min(14, 3 + adds / 6);
  for (let k = 0; k < n && shards.length < 700; k++) {
    const s = new THREE.Mesh(shardGeo, new THREE.MeshBasicMaterial({ color: col, transparent: true, opacity: 0.95 }));
    const a = Math.random() * Math.PI * 2, r = 8 + Math.random() * 14;
    s.position.set(slot.x + Math.cos(a) * r, to.y + (Math.random() - 0.2) * 12, slot.z + Math.sin(a) * r);
    s.userData = { from: s.position.clone(), dest: to, t: 0, dur: 0.6 + Math.random() * 0.5, inward: true };
    city.add(s);
    shards.push(s);
  }
}
function removeFile(path, burst) {
  const slot = dirs.get(dirOf(path));
  const tw = slot && slot.files.get(path);
  if (!tw) return;
  slot.files.delete(path);
  if (burst) { // deletion runs forward: the tower shatters outward
    const at = tw.group.position.clone().setY(tw.top / 2);
    for (let k = 0; k < 8 && shards.length < 700; k++) {
      const s = new THREE.Mesh(shardGeo, new THREE.MeshBasicMaterial({ color: 0x8a96b0, transparent: true, opacity: 0.9 }));
      s.position.copy(at);
      const a = Math.random() * Math.PI * 2;
      s.userData = { from: at.clone(), dest: at.clone().add(new THREE.Vector3(Math.cos(a) * 9, Math.random() * 6, Math.sin(a) * 9)), t: 0, dur: 0.9, inward: false };
      city.add(s);
      shards.push(s);
    }
  }
  for (const m of [...tw.layers, tw.cap]) pickables.splice(pickables.indexOf(m), 1);
  dispose(tw.group, city);
}
function dispose(o, parent) {
  parent.remove(o);
  o.traverse((x) => { if (x.material) { if (x.material.map) x.material.map.dispose(); x.material.dispose(); } });
}

// ---------- code sheets: the code itself, painted ----------
// Inverted, a commit's diff is shown mirrored and un-writes itself (its added
// lines erase, its removed lines come back), then recedes down the tunnel to its
// ring. Forward, each rebuilt file is painted (context fades in, the lines the
// patch added type themselves out), then the sheet folds into its tower: the code
// layered into the city. Under a backlog only a sample is painted. The text is
// the repository's, so it only ever reaches a canvas.
const SW = 1024, SH = 560, LH = 30;
const sheetGeo = new THREE.PlaneGeometry(7.2, 7.2 * SH / SW);
const sheets = [];
function makeSheet(kind, e, dur) {
  const c = document.createElement('canvas');
  c.width = SW; c.height = SH;
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  const m = new THREE.Mesh(sheetGeo, new THREE.MeshBasicMaterial({ map: tex, transparent: true, opacity: 0, depthWrite: false, side: THREE.DoubleSide }));
  m.userData = { kind, e, g: c.getContext('2d'), tex, t: 0, dur, state: 'paint', order: [...(e.added || [])].sort((a, b) => a - b) };
  return m;
}
const clamp01 = (x) => Math.max(0, Math.min(1, x));
function paintSheet(u, p) {
  const g = u.g, e = u.e, inv = u.kind === 'inv';
  g.setTransform(1, 0, 0, 1, 0, 0);
  g.clearRect(0, 0, SW, SH);
  if (inv) { g.translate(SW, 0); g.scale(-1, 1); } // read from the other side of the turnstile
  g.fillStyle = inv ? 'rgba(28, 6, 8, 0.86)' : 'rgba(6, 10, 22, 0.86)';
  g.fillRect(0, 0, SW, SH);
  g.strokeStyle = inv ? 'rgba(255, 59, 59, 0.7)' : e.status === 'exact' ? 'rgba(61, 139, 255, 0.8)' : 'rgba(255, 181, 71, 0.8)';
  g.lineWidth = 3;
  g.strokeRect(1.5, 1.5, SW - 3, SH - 3);
  g.font = '20px ui-monospace, Menlo, monospace';
  g.textBaseline = 'middle';
  g.fillStyle = inv ? '#ff8a8a' : '#9cc3ff';
  g.fillText(`${inv ? '◂ ' + (e.sha || '').slice(0, 8) : '▸ ' + e.status}  ${e.path || ''}`.slice(0, 84), 18, 26);
  g.font = '22px ui-monospace, Menlo, monospace';
  const lines = inv ? e.diff : e.code;
  for (let i = 0; i < lines.length; i++) {
    const y = 66 + i * LH;
    if (inv) {
      const mark = lines[i][0], text = lines[i].slice(1);
      // running backward: what the commit added is erased, what it removed returns
      const f = mark === '+' ? 1 - p : mark === '-' ? p : 1;
      const shown = text.slice(0, Math.round(text.length * f));
      g.fillStyle = mark === '+' ? '#ff6b6b' : mark === '-' ? '#ffd6d6' : 'rgba(255, 160, 160, 0.45)';
      g.fillText(`${mark} ${shown}`, 18, y);
      if (mark !== ' ' && f > 0 && f < 1) g.fillRect(18 + g.measureText(`${mark} ${shown}`).width + 2, y - 11, 12, 22);
      continue;
    }
    g.fillStyle = 'rgba(124, 133, 151, 0.8)';
    g.fillText(String(e.at + i).padStart(4), 12, y);
    const k = u.order.indexOf(i);
    if (k < 0) { // context: fades in
      g.fillStyle = `rgba(150, 165, 190, ${0.75 * clamp01(p * 4)})`;
      g.fillText(lines[i], 84, y);
      continue;
    }
    // an added line types itself out, in order
    const n = u.order.length, start = 0.12 + (0.72 * k) / n;
    const f = clamp01((p - start) / (0.72 / n + 0.06));
    if (f <= 0) continue;
    const shown = lines[i].slice(0, Math.round(lines[i].length * f));
    g.fillStyle = 'rgba(61, 139, 255, 0.16)';
    g.fillRect(78, y - 14, Math.max(8, g.measureText(shown).width + 12), 28);
    g.fillStyle = '#e3eeff';
    g.fillText(shown, 84, y);
    if (f < 1) { g.fillStyle = '#3d8bff'; g.fillRect(84 + g.measureText(shown).width + 2, y - 11, 12, 22); }
  }
  u.tex.needsUpdate = true;
}
// Both kinds are painted large before the camera. (Hung in the tunnel instead,
// the inverted sheets sat far ahead of a camera that trails a backlog, too small
// to read: seen live, google/uuid.)
function newSheet(kind, e, dur) {
  const busy = sheets.find((s) => s.userData.kind === kind && s.userData.state === 'paint');
  if (busy && busy.userData.t < 0.45) return null; // under a backlog, paint a sample, not every change
  if (busy) fold(busy);
  if (sheets.filter((s) => s.userData.kind === kind).length >= 8) retire(sheets.find((s) => s.userData.kind === kind));
  const s = makeSheet(kind, e, dur);
  scene.add(s);
  sheets.push(s);
  placeBeforeCamera(s);
  return s;
}
function invSheet(e) {
  const s = newSheet('inv', e, Math.max(0.4, Math.min(1.5, 1.5 - queue.length / 150)));
  if (s) s.userData.to = new THREE.Vector3((e.i % 2 ? -1 : 1) * 2.5, (e.i % 3 - 1) * 1.2, -depth);
}
function fwdSheet(e) {
  const s = newSheet('fwd', e, Math.max(0.4, Math.min(1.8, 1.8 - queue.length / 120)));
  const slot = dirs.get(dirOf(e.path));
  if (s) s.userData.tower = slot && slot.files.get(e.path);
}
const _v = new THREE.Vector3(), _r = new THREE.Vector3();
function placeBeforeCamera(s) { // forward sheets to the left, inverted to the right
  camera.getWorldDirection(_v);
  _r.crossVectors(_v, camera.up).normalize();
  const side = s.userData.kind === 'inv' ? 2.6 : -2.6;
  s.position.copy(camera.position).addScaledVector(_v, 9).addScaledVector(_r, side).addScaledVector(camera.up, s.userData.kind === 'inv' ? -0.6 : 1.1);
  s.quaternion.copy(camera.quaternion);
}
function fold(s) { // paint it whole, then send it into its tower, or down the tunnel
  const u = s.userData;
  paintSheet(u, 1);
  u.state = 'fold';
  u.t = 0;
  u.from = s.position.clone();
}
function retire(s) {
  if (!s) return;
  sheets.splice(sheets.indexOf(s), 1);
  dispose(s, s.parent || scene);
}
function stepSheets(dt) {
  for (const s of [...sheets]) {
    const u = s.userData;
    if (u.state === 'paint') {
      u.t = Math.min(1, u.t + dt / u.dur);
      s.material.opacity = Math.min(0.96, s.material.opacity + dt * 4);
      paintSheet(u, ease(u.t));
      placeBeforeCamera(s);
      if (u.t >= 1) fold(s);
    } else if (u.state === 'fold') {
      u.t = Math.min(1, u.t + dt / (u.kind === 'inv' ? 1.1 : 0.8));
      const tw = u.tower && u.tower.group.parent ? u.tower : null;
      const to = u.kind === 'inv' ? u.to : tw ? _v.copy(tw.group.position).setY(tw.top + 0.3) : u.from;
      s.position.lerpVectors(u.from, to, ease(u.t));
      s.scale.setScalar(1 - 0.94 * ease(u.t));
      s.material.opacity = 0.96 * (1 - u.t * u.t);
      if (u.t >= 1) { if (tw && tw.layers.length) tw.layers.at(-1).userData.flash = 1; retire(s); }
    }
  }
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
      if (e.diff && !reduced) invSheet(e);
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
      commitNow = { sha: e.sha, headline: e.headline || '', author: e.author || '', date: (e.date || '').slice(0, 10) };
      tick(`▸ ${e.sha.slice(0, 8)} ${e.headline || ''}`, 'fwd');
      break;
    case 'file':
      if (e.status !== 'deleted') {
        stats.files++;
        stats[e.status] = (stats[e.status] || 0) + 1;
      }
      forwardFile(e);
      if (e.code && !reduced) fwdSheet(e);
      tick(`${e.status.padEnd(8)} ${e.path}${e.why ? ' · ' + e.why : ''}`, e.status);
      break;
    case 'done':
      runDone = true;
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
  $('summary').onclick = () => { $('summary').hidden = true; }; // out of the way of the city
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
  for (const s of [...sheets]) retire(s);
  dirs.clear(); shards.length = 0; pickables.length = 0; queue = []; received = 0; depth = 0; clock = 0; extent = 6;
  commitNow = null; runDone = false; hover = null; tip.hidden = true; closeFile();
  Object.assign(view, { theta: 0.8, phi: 1.0, zoomed: false });
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

// ---------- the viewer's hands: turn, zoom, hover, open ----------
const view = { theta: 0.8, phi: 1.0, r: 30, zoomed: false, target: new THREE.Vector3(0, 1.5, 0) };
let hover = null, runDone = false, drag = null;
const canvas = $('scene'), tip = $('tip');
const raycaster = new THREE.Raycaster(), ndc = new THREE.Vector2();
canvas.addEventListener('contextmenu', (ev) => ev.preventDefault()); // right button turns the camera
canvas.addEventListener('pointerdown', (ev) => {
  drag = { x: ev.clientX, y: ev.clientY, button: ev.button, moved: 0 };
  canvas.setPointerCapture(ev.pointerId);
});
canvas.addEventListener('pointermove', (ev) => {
  if (drag) {
    const dx = ev.clientX - drag.x, dy = ev.clientY - drag.y;
    drag.moved += Math.abs(dx) + Math.abs(dy);
    drag.x = ev.clientX; drag.y = ev.clientY;
    if (drag.button === 2 && phase === 'fwd') {
      view.theta -= dx * 0.006;
      view.phi = Math.min(1.45, Math.max(0.15, view.phi - dy * 0.006));
    }
  }
  pick(ev.clientX, ev.clientY);
});
canvas.addEventListener('pointerup', (ev) => {
  if (drag && drag.button === 0 && drag.moved < 5 && hover) openFile(hover.tower);
  drag = null;
});
canvas.addEventListener('pointerleave', () => { hover = null; tip.hidden = true; });
canvas.addEventListener('wheel', (ev) => {
  if (phase !== 'fwd') return;
  ev.preventDefault();
  view.r = Math.min(400, Math.max(4, view.r * Math.exp(ev.deltaY * 0.001)));
  view.zoomed = true;
}, { passive: false });

// pick finds the layer or cap under the pointer and fills the tooltip.
function pick(x, y) {
  if (phase !== 'fwd' || (drag && drag.button === 2)) { hover = null; tip.hidden = true; return; }
  ndc.set((x / innerWidth) * 2 - 1, -(y / innerHeight) * 2 + 1);
  raycaster.setFromCamera(ndc, camera);
  const hit = raycaster.intersectObjects(pickables, false)[0];
  if (!hit) { hover = null; tip.hidden = true; canvas.style.cursor = ''; return; }
  const u = hit.object.userData, tw = u.tower;
  hover = { tower: tw, layer: u.commit !== undefined ? hit.object : null };
  const status = { exact: 'proven exact', unknown: 'unproven', mismatch: 'mismatch' }[tw.status] || tw.status;
  const rows = [tw.path, `${status} · ${tw.lines} lines · ${tw.versions} versions${tw.dropped ? ` (${tw.dropped} oldest not drawn)` : ''}`];
  if (hover.layer && u.commit) rows.push(`${u.commit.sha.slice(0, 8)} ${u.commit.date} ${u.commit.author}`, u.commit.headline, `+${u.adds} −${u.dels} lines`);
  rows.push('click to open the file');
  tip.replaceChildren(...rows.map((r, i) => { const d = document.createElement('div'); d.textContent = r; if (i === 0) d.className = 'path'; return d; }));
  tip.style.left = `${Math.min(x + 16, innerWidth - 340)}px`;
  tip.style.top = `${Math.min(y + 16, innerHeight - 140)}px`;
  tip.hidden = false;
  canvas.style.cursor = 'pointer';
}

// openFile shows a rebuilt file, from the session, once the run is done.
async function openFile(tw) {
  const panel = $('file');
  $('f-path').textContent = tw.path;
  $('f-code').textContent = '';
  panel.hidden = false;
  document.body.classList.add('filing');
  if (!runDone) { $('f-meta').textContent = 'the files open when the reconstruction is done'; return; }
  $('f-meta').textContent = 'opening…';
  try {
    const r = await fetch(`/api/v1/reconstructions/${sessionId}/file?path=${encodeURIComponent(tw.path)}`);
    const f = await r.json();
    if (!r.ok) throw new Error(f.error ? f.error.message : r.statusText);
    if (!f.known) { $('f-meta').textContent = `unproven: the rebuilt history never held this file's content (${tw.status})`; return; }
    if (f.binary) { $('f-meta').textContent = `${f.bytes} bytes, binary`; return; }
    const lines = f.content.replace(/\n$/, '').split('\n');
    const w = String(lines.length).length;
    $('f-meta').textContent = `rebuilt from diffs alone and proven against GitHub's blob id · ${lines.length} lines · ${tw.versions} versions${f.truncated ? ' · first 1 MB shown' : ''}`;
    $('f-code').textContent = lines.map((l, i) => `${String(i + 1).padStart(w)}  ${l}`).join('\n');
  } catch (err) {
    $('f-meta').textContent = err.message;
  }
}
function closeFile() { $('file').hidden = true; document.body.classList.remove('filing'); }
$('f-close').addEventListener('click', closeFile);
addEventListener('keydown', (ev) => { if (ev.key === 'Escape') closeFile(); });

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
    if (t >= 1) {
      flyThrough = null; tunnel.visible = true;
      while (tunnel.children.length) dispose(tunnel.children[0], tunnel);
      for (const s of sheets.filter((x) => x.userData.kind === 'inv')) retire(s);
    }
  } else if (phase === 'fwd') {
    // The camera holds still: right-drag turns it, the wheel zooms. Until the
    // viewer zooms, its distance follows the city so all of it stays in view.
    if (!view.zoomed) view.r = 10 + extent * 1.35;
    const want = new THREE.Vector3().setFromSphericalCoords(view.r, view.phi, view.theta).add(view.target);
    camera.position.lerp(want, Math.min(1, dt * 4));
    camera.lookAt(view.target);
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

  // layers grow to their thickness and settle; a fresh one flashes; the hovered
  // tower glows
  const k6 = Math.min(1, dt * 6);
  for (const slot of dirs.values()) {
    for (const tw of slot.files.values()) {
      const lit = hover && hover.tower === tw ? 0.5 : 0;
      for (const m of tw.layers) {
        const u = m.userData;
        m.scale.y += (u.th - m.scale.y) * k6;
        m.position.y += (u.y - m.position.y) * k6;
        u.flash = Math.max(0, u.flash - dt * 1.5);
        m.material.emissiveIntensity = 0.45 + lit + u.flash * 1.8 + (hover && hover.layer === m ? 0.8 : 0);
      }
      tw.cap.position.y += (tw.top + 0.045 - tw.cap.position.y) * k6;
    }
  }

  // shards: inward ones converge on their layer (un-shattering), outward ones fly off
  for (let k = shards.length - 1; k >= 0; k--) {
    const s = shards[k], u = s.userData;
    u.t += dt / u.dur;
    const t = Math.min(1, u.t);
    s.position.lerpVectors(u.from, u.dest, u.inward ? ease(t) : t);
    s.rotation.x += dt * 6; s.rotation.y += dt * 4;
    s.material.opacity = u.inward ? 0.95 : 0.9 * (1 - t);
    if (t >= 1) { dispose(s, city); shards.splice(k, 1); }
  }

  stepSheets(dt);

  // the clock counts down while inverted, up once forward
  const c = Math.abs(clock);
  $('clock').textContent = `${clock < 0 ? '−' : ' '}${String(Math.floor(c / 60)).padStart(2, '0')}:${String(c % 60).padStart(2, '0')}`;

  renderer.render(scene, camera);
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

const fromURL = new URLSearchParams(location.search).get('repo');
if (fromURL) { $('repo').value = fromURL; $('query').requestSubmit(); }
