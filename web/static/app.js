// Tenet: reconstruct a repository from its diffs, as a temporal pincer, in a
// night city.
// Phase 1 (red, inverted): the server walks back through history. The camera
// runs down an avenue at street level; each commit raises a red neon arch with
// its headline mirrored, its diff un-writes itself, rain falls upward and the
// traffic flies backward. The turnstile flips time.
// Phase 2 (blue, forward): each diff replays. Each directory is a city block and
// each file a skyscraper: one band of floors per change, cyan for lines added,
// magenta for lines removed, a roof beacon for its proof. Billboards flash the
// headlines; the viewer orbits (right-drag), hovers and opens files and diffs.
// The world is procedural and seeded by the repository's name.
// All server text is untrusted: it is drawn on canvases or set with textContent.
//
// Libraries (pinned): three.js 0.186.1 with its OrbitControls and the bloom
// post-processing chain, and simplex-noise 4.0.3. jsdelivr's +esm builds import
// three from one URL, so the addons share the page's copy without an importmap.
import * as THREE from 'https://cdn.jsdelivr.net/npm/three@0.186.1/+esm';
import { OrbitControls } from 'https://cdn.jsdelivr.net/npm/three@0.186.1/examples/jsm/controls/OrbitControls.js/+esm';
import { EffectComposer } from 'https://cdn.jsdelivr.net/npm/three@0.186.1/examples/jsm/postprocessing/EffectComposer.js/+esm';
import { RenderPass } from 'https://cdn.jsdelivr.net/npm/three@0.186.1/examples/jsm/postprocessing/RenderPass.js/+esm';
import { UnrealBloomPass } from 'https://cdn.jsdelivr.net/npm/three@0.186.1/examples/jsm/postprocessing/UnrealBloomPass.js/+esm';
import { OutputPass } from 'https://cdn.jsdelivr.net/npm/three@0.186.1/examples/jsm/postprocessing/OutputPass.js/+esm';
import { createNoise2D } from 'https://cdn.jsdelivr.net/npm/simplex-noise@4.0.3/+esm';

const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;
const $ = (id) => document.getElementById(id);
const CYAN = new THREE.Color(0x22e5ff), MAGENTA = new THREE.Color(0xff2bd6), RED = new THREE.Color(0xff3b3b);
const NEON = [0x22e5ff, 0xff2bd6, 0xffb547, 0x7cff6b, 0x9b5cff];
const BEACON = { exact: 0x3dff8a, unknown: 0xffb547, mismatch: 0xff3b3b };

// ---------- seeded randomness: one city per repository ----------
function mulberry32(a) {
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function hashStr(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}
let rand = mulberry32(20261030), noise = createNoise2D(mulberry32(7));

// ---------- renderer, camera, post ----------
const canvas = $('scene');
const renderer = new THREE.WebGLRenderer({ canvas, antialias: true });
renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
renderer.toneMapping = THREE.ACESFilmicToneMapping;
renderer.toneMappingExposure = 1.05;
const scene = new THREE.Scene();
scene.fog = new THREE.FogExp2(0x160a26, 0.0075);
const camera = new THREE.PerspectiveCamera(55, 1, 0.1, 3000);
camera.position.set(0, 3.2, 12);
const composer = new EffectComposer(renderer);
composer.addPass(new RenderPass(scene, camera));
const bloom = new UnrealBloomPass(new THREE.Vector2(1, 1), 1.1, 0.55, 0.6);
composer.addPass(bloom);
composer.addPass(new OutputPass());

// The camera orbits on the right button; the left one is for picking a building.
const controls = new OrbitControls(camera, canvas);
controls.enableDamping = true;
controls.dampingFactor = 0.07;
controls.mouseButtons = { LEFT: null, MIDDLE: THREE.MOUSE.PAN, RIGHT: THREE.MOUSE.ROTATE };
controls.minDistance = 6;
controls.maxDistance = 700;
controls.maxPolarAngle = 1.48;
controls.enabled = false;
let viewerMoved = false; // once the viewer turns or zooms, the camera stops following the city's growth
controls.addEventListener('start', () => { viewerMoved = true; });

function resize() {
  renderer.setSize(innerWidth, innerHeight, false);
  composer.setSize(innerWidth, innerHeight);
  camera.aspect = innerWidth / innerHeight;
  camera.updateProjectionMatrix();
}
addEventListener('resize', resize);
resize();

// ---------- sky, light, ground ----------
const sky = new THREE.Mesh(new THREE.SphereGeometry(1500, 32, 16), new THREE.ShaderMaterial({
  side: THREE.BackSide, depthWrite: false, fog: false,
  uniforms: { top: { value: new THREE.Color(0x04020a) }, horizon: { value: new THREE.Color(0x3a0f22) }, glow: { value: new THREE.Color(0xff3b3b) } },
  vertexShader: 'varying vec3 vp; void main(){ vp = normalize(position); gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0); }',
  fragmentShader: 'uniform vec3 top, horizon, glow; varying vec3 vp; void main(){ float h = max(vp.y, 0.0); vec3 c = mix(horizon, top, pow(h, 0.42)); c += glow * 0.10 * pow(1.0 - h, 10.0); gl_FragColor = vec4(c, 1.0); }',
}));
scene.add(sky);
const SKY = { inv: [0x04020a, 0x3a0f22, 0xff3b3b], fwd: [0x03030c, 0x1c1446, 0x22e5ff] };
let skyMood = SKY.inv;
const _c = new THREE.Color();
function stepSky(dt) {
  const u = sky.material.uniforms, k = Math.min(1, dt * 1.5);
  u.top.value.lerp(_c.setHex(skyMood[0]), k);
  u.horizon.value.lerp(_c.setHex(skyMood[1]), k);
  u.glow.value.lerp(_c.setHex(skyMood[2]), k);
  scene.fog.color.copy(u.horizon.value).multiplyScalar(0.55);
}

scene.add(new THREE.HemisphereLight(0x6a5cff, 0x08050f, 0.9));
const moon = new THREE.DirectionalLight(0x8aa0ff, 0.45);
moon.position.set(-80, 120, 60);
scene.add(moon);
const neonLights = NEON.slice(0, 3).map((c) => { const l = new THREE.PointLight(c, 40, 90, 1.4); scene.add(l); return l; });

const ground = new THREE.Mesh(new THREE.PlaneGeometry(6000, 6000), new THREE.MeshStandardMaterial({ color: 0x07070d, roughness: 0.32, metalness: 0.55 }));
ground.rotation.x = -Math.PI / 2;
scene.add(ground);

// ---------- textures ----------
// Lit windows: a grid of panes, some dark, as an emissive map. One texture
// for the skyline's tall blocks, one with fewer rows for a building's floor band.
function windowsTexture(rows, seed) {
  const c = document.createElement('canvas');
  c.width = 64; c.height = rows * 8;
  const g = c.getContext('2d');
  const r = mulberry32(seed);
  g.fillStyle = '#000';
  g.fillRect(0, 0, c.width, c.height);
  for (let y = 0; y < rows; y++) {
    for (let x = 0; x < 4; x++) {
      if (r() < 0.38) continue;
      const v = Math.floor(110 + r() * 145);
      g.fillStyle = `rgb(${v},${v},${v})`;
      g.fillRect(4 + x * 15, 2 + y * 8, 10, 5);
    }
  }
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}
const tallWindows = windowsTexture(48, 11), floorWindows = windowsTexture(4, 23);

// label draws neon text on a canvas, mirrored when it is read from the far side of time.
function label(text, color, mirrored, w = 512, h = 72, size = 30) {
  const c = document.createElement('canvas');
  c.width = w; c.height = h;
  const g = c.getContext('2d');
  if (mirrored) { g.translate(w, 0); g.scale(-1, 1); }
  g.font = `600 ${size}px ui-monospace, Menlo, monospace`;
  g.textBaseline = 'middle';
  g.shadowColor = color;
  g.shadowBlur = 14;
  g.fillStyle = color;
  g.fillText(String(text).slice(0, 44), 10, h / 2);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}
// dispose frees an object's materials and textures, except materials many
// objects share (marked userData.shared), which stay for the others.
function dispose(o, parent) {
  parent.remove(o);
  o.traverse((x) => {
    if (x.material && !x.material.userData.shared) { if (x.material.map) x.material.map.dispose(); x.material.dispose(); }
  });
}

// ---------- the skyline: a seeded ring of distant towers ----------
const box = new THREE.BoxGeometry(1, 1, 1);
const SKYLINE = reduced ? 220 : 520;
const skyline = new THREE.InstancedMesh(box, new THREE.MeshBasicMaterial({ map: tallWindows }), SKYLINE);
scene.add(skyline);
const _o = new THREE.Object3D();
function buildSkyline() {
  for (let i = 0; i < SKYLINE; i++) {
    const a = rand() * Math.PI * 2, r = 150 + rand() * 300;
    const x = Math.cos(a) * r, z = Math.sin(a) * r;
    const n = 0.5 + 0.5 * noise(x * 0.006, z * 0.006);
    const h = 10 + 110 * n * n * (1.25 - (r - 150) / 400), w = 5 + rand() * 9;
    _o.position.set(x, h / 2, z);
    _o.rotation.set(0, rand() * Math.PI, 0);
    _o.scale.set(w, h, w);
    _o.updateMatrix();
    skyline.setMatrixAt(i, _o.matrix);
    skyline.setColorAt(i, _c.setHex(NEON[Math.floor(rand() * NEON.length)]).multiplyScalar(0.25 + rand() * 0.35));
  }
  skyline.instanceMatrix.needsUpdate = true;
  skyline.instanceColor.needsUpdate = true;
}

// ---------- phase 1: the inverted avenue ----------
const avenue = new THREE.Group();
scene.add(avenue);
const arches = new THREE.Group();
avenue.add(arches);
const archMat = new THREE.MeshBasicMaterial({ color: RED });
archMat.userData.shared = true;
let depth = 0;
const SPACING = 5;
function invertCommit(e) {
  depth = e.i * SPACING;
  const arch = new THREE.Group();
  for (const x of [-5, 5]) {
    const p = new THREE.Mesh(box, archMat);
    p.scale.set(0.35, 8, 0.35);
    p.position.set(x, 4, 0);
    arch.add(p);
  }
  const beam = new THREE.Mesh(box, archMat);
  beam.scale.set(10.7, 0.35, 0.35);
  beam.position.y = 8;
  arch.add(beam);
  const sign = new THREE.Sprite(new THREE.SpriteMaterial({ map: label(e.headline || e.sha.slice(0, 12), '#ff5a5a', true), transparent: true, depthWrite: false }));
  sign.scale.set(9, 1.26, 1);
  sign.position.y = 9.3;
  arch.add(sign);
  arch.position.z = -depth;
  arches.add(arch);
  while (arches.children.length > 140) {
    const old = arches.children[0];
    old.children.at(-1).material.map.dispose();
    old.children.at(-1).material.dispose();
    arches.remove(old);
  }
}
// The avenue's walls: towers on both sides, every 10 units of the street. Each
// one's size and color follow from its place (seeded noise), so a building stays
// the same as the camera passes; the 100 slots a side are recycled around it.
const WALLS = 200;
const walls = new THREE.InstancedMesh(box, new THREE.MeshBasicMaterial({ map: tallWindows }), WALLS);
avenue.add(walls);
function stepWalls() {
  const base = Math.floor(camera.position.z / 10) * 10;
  for (let i = 0; i < WALLS; i++) {
    const side = i % 2 ? 1 : -1, z = base + 30 - (i >> 1) * 10;
    const n = 0.5 + 0.5 * noise(z * 0.037, side * 3.1), m = 0.5 + 0.5 * noise(z * 0.11 + 9, side);
    const h = 12 + 55 * n * n, w = 6 + 6 * m;
    _o.position.set(side * (12 + w / 2), h / 2, z);
    _o.rotation.set(0, 0, 0);
    _o.scale.set(w, h, 8);
    _o.updateMatrix();
    walls.setMatrixAt(i, _o.matrix);
    walls.setColorAt(i, _c.setHex(m > 0.35 ? 0xff2b4a : NEON[Math.floor(m * 13) % NEON.length]).multiplyScalar(0.3 + 0.3 * n));
  }
  walls.instanceMatrix.needsUpdate = true;
  walls.instanceColor.needsUpdate = true;
}
const laneLines = new THREE.LineSegments(new THREE.BufferGeometry().setFromPoints([-3, 3, -9, 9].flatMap((x) =>
  [new THREE.Vector3(x, 0.02, 40), new THREE.Vector3(x, 0.02, -8000)])), new THREE.LineBasicMaterial({ color: 0xff3b5a, transparent: true, opacity: 0.55 }));
avenue.add(laneLines);

const turnstile = new THREE.Group();
for (let k = 0; k < 3; k++) turnstile.add(new THREE.Mesh(new THREE.TorusGeometry(6.5 + k * 0.6, 0.07, 8, 160), new THREE.MeshBasicMaterial({ color: k === 1 ? 0xffffff : RED })));
for (let k = 0; k < 8; k++) {
  const spoke = new THREE.Mesh(box, new THREE.MeshBasicMaterial({ color: 0xffffff }));
  spoke.scale.set(0.07, 12.4, 0.07);
  spoke.rotation.z = (k / 8) * Math.PI;
  turnstile.add(spoke);
}
turnstile.visible = false;
scene.add(turnstile);

// ---------- phase 2: the city ----------
// Each directory is a city block on a square spiral of the street grid; a block
// holds 25 lots, and a directory with more files takes the next free block too.
// Each file is a skyscraper: one band of floors per change, as tall as the lines
// it changed, its windows lit cyan (added) through magenta (removed), a neon
// floor line on top of the band, and a roof with an antenna and a beacon for the
// file's proof.
const city = new THREE.Group();
scene.add(city);
const BLOCK = 14;
const CELLS = (() => {
  const out = [];
  for (let r = 0; r <= 14; r++) {
    const ring = [];
    for (let i = -r; i <= r; i++) for (let j = -r; j <= r; j++) if (Math.max(Math.abs(i), Math.abs(j)) === r) ring.push([i, j]);
    ring.sort((a, b) => Math.atan2(a[1], a[0]) - Math.atan2(b[1], b[0]));
    out.push(...ring);
  }
  return out;
})();
// A block's 25 lots: 16 on its street frontage, 8 behind them, 1 in the middle.
const LOTS = [];
for (let i = -2; i <= 2; i++) for (let j = -2; j <= 2; j++) LOTS.push({ dx: i * 2.5, dz: j * 2.5, ring: Math.max(Math.abs(i), Math.abs(j)) });
// lotOrder is the order a block at (x, z) fills in: the street frontage first,
// and along it the lots nearest the city's center first, so a sparse directory
// lines the street on the side toward the center square instead of standing in
// the middle of an empty block.
function lotOrder(x, z) {
  return [...LOTS].sort((a, b) => (b.ring - a.ring) || (Math.hypot(x + a.dx, z + a.dz) - Math.hypot(x + b.dx, z + b.dz)));
}
const streets = new THREE.LineSegments(new THREE.BufferGeometry().setFromPoints((() => {
  const pts = [], L = 14 * BLOCK + BLOCK / 2;
  for (let i = -14; i <= 15; i++) {
    const v = (i - 0.5) * BLOCK;
    pts.push(new THREE.Vector3(v, 0.03, -L), new THREE.Vector3(v, 0.03, L), new THREE.Vector3(-L, 0.03, v), new THREE.Vector3(L, 0.03, v));
  }
  return pts;
})()), new THREE.LineBasicMaterial({ color: 0x2a6bff, transparent: true, opacity: 0.35 }));
city.add(streets);
const plateMat = new THREE.MeshStandardMaterial({ color: 0x0d0e18, roughness: 0.6, metalness: 0.3 });
const shaftMat = new THREE.MeshStandardMaterial({ color: 0x1a1c26, roughness: 0.5, metalness: 0.6 });
plateMat.userData.shared = shaftMat.userData.shared = true;

const dirs = new Map(); // dir -> {cells: [[x, z]], next, files: Map(path -> tower), hue}
let nextCell = 0;
const shards = [];
const pickables = []; // floor bands, their neon lines, roofs and beacons
const MAX_LAYERS = 120; // a tower keeps its newest changes; older ones count in tw.dropped
let commitNow = null;   // the commit being replayed, for its floor bands
let extent = 10;        // how far the city reaches, for the camera
function dirOf(path) { const i = path.lastIndexOf('/'); return i < 0 ? '/' : path.slice(0, i); }
function dirSlot(d) {
  if (!dirs.has(d)) dirs.set(d, { cells: [], blocks: [], next: 0, files: new Map(), hue: NEON[hashStr(d) % NEON.length], name: d });
  return dirs.get(d);
}
// freeCells are blocks given back by directories that lost their last file.
// A new block takes the free one nearest the center square before it claims new
// land at the edge, so the city stays packed around its center.
let freeCells = [];
function takeCell() {
  const next = CELLS[Math.min(nextCell, CELLS.length - 1)];
  freeCells.sort((a, b) => Math.hypot(...a) - Math.hypot(...b));
  if (freeCells.length && Math.hypot(...freeCells[0]) <= Math.hypot(...next)) return freeCells.shift();
  nextCell++;
  return next;
}
function newBlock(slot) {
  const [i, j] = takeCell();
  const x = i * BLOCK, z = j * BLOCK;
  const plate = new THREE.Mesh(box, plateMat);
  plate.scale.set(BLOCK - 1, 0.18, BLOCK - 1);
  plate.position.set(x, 0.09, z);
  const h = (BLOCK - 1) / 2;
  const edge = new THREE.LineLoop(new THREE.BufferGeometry().setFromPoints([[-h, -h], [h, -h], [h, h], [-h, h]].map(([a, b]) => new THREE.Vector3(x + a, 0.2, z + b))),
    new THREE.LineBasicMaterial({ color: slot.hue }));
  const parts = [plate, edge];
  if (!slot.cells.length) {
    const sign = new THREE.Sprite(new THREE.SpriteMaterial({ map: label(slot.name, '#' + slot.hue.toString(16).padStart(6, '0'), false, 512, 64, 26), transparent: true, depthWrite: false }));
    sign.scale.set(6.5, 0.8, 1);
    sign.position.set(x, 0.9, z + h + 0.6);
    parts.push(sign);
  }
  city.add(...parts);
  slot.blocks.push({ cell: [i, j], parts });
  slot.cells.push([x, z]);
  extent = Math.max(extent, Math.hypot(x, z) + BLOCK);
}
// clearBlocks gives an emptied directory's land back to the city.
function clearBlocks(slot) {
  for (const b of slot.blocks) {
    for (const p of b.parts) {
      city.remove(p);
      if (!p.material.userData.shared) { if (p.material.map) p.material.map.dispose(); p.material.dispose(); }
    }
    freeCells.push(b.cell);
  }
  dirs.delete(slot.name);
}
function lot(slot) {
  if (slot.next % LOTS.length === 0) newBlock(slot);
  const [x, z] = slot.cells.at(-1);
  slot.order ??= [];
  if (slot.order.length !== slot.cells.length) slot.order.push(lotOrder(x, z));
  const { dx, dz } = slot.order.at(-1)[slot.next++ % LOTS.length];
  return { x: x + dx, z: z + dz };
}
// The neon on a building: tubes up its four corners, a vertical blade sign with
// the file's name on one face, and a roof sign once it stands tall. Each sign's
// color and its flicker follow from the file's path (seeded, so a repository's
// city always lights the same way). Signs are not pickable, so they never block
// a click on the floors behind them.
const BLADE_W = 0.85, GLYPH = 0.56;
function bladeTexture(text, color) {
  const chars = [...text.toUpperCase()].slice(0, 14);
  const c = document.createElement('canvas');
  c.width = 64; c.height = 56 * chars.length + 16;
  const g = c.getContext('2d');
  g.fillStyle = 'rgba(6, 4, 14, 0.85)';
  g.fillRect(0, 0, c.width, c.height);
  g.strokeStyle = color; g.lineWidth = 4; g.shadowColor = color; g.shadowBlur = 12;
  g.strokeRect(4, 4, c.width - 8, c.height - 8);
  g.font = '700 40px ui-monospace, Menlo, monospace';
  g.textAlign = 'center'; g.textBaseline = 'middle'; g.fillStyle = color;
  chars.forEach((ch, i) => g.fillText(ch, 32, 36 + i * 56));
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return { t, n: chars.length };
}
function neonFor(tw, path) {
  const h = hashStr(path), name = path.slice(path.lastIndexOf('/') + 1);
  const hex = NEON[h % NEON.length], css = '#' + hex.toString(16).padStart(6, '0');
  const tube = new THREE.MeshBasicMaterial({ color: hex });
  tw.tubes = [-1, 1].flatMap((sx) => [-1, 1].map((sz) => {
    const m = new THREE.Mesh(box, tube);
    m.position.set(sx * 1.02, 0, sz * 1.02);
    m.scale.set(0.06, 0.001, 0.06);
    tw.group.add(m);
    return m;
  }));
  const { t, n } = bladeTexture(name, css);
  const blade = new THREE.Mesh(new THREE.PlaneGeometry(BLADE_W, GLYPH * n + 0.12), new THREE.MeshBasicMaterial({ map: t, transparent: true, side: THREE.DoubleSide, toneMapped: false }));
  const face = h >>> 3 & 3; // which face carries it
  blade.rotation.y = face * Math.PI / 2 + Math.PI / 2;
  blade.position.set(Math.cos(face * Math.PI / 2) * 1.35, 0, -Math.sin(face * Math.PI / 2) * 1.35);
  blade.visible = false;
  tw.group.add(blade);
  const roof = new THREE.Mesh(new THREE.PlaneGeometry(2.6, 0.45), new THREE.MeshBasicMaterial({ map: label(name, css, false, 512, 88, 40), transparent: true, side: THREE.DoubleSide, toneMapped: false }));
  roof.visible = false;
  tw.group.add(roof);
  tw.neon = { tube, blade, bladeH: GLYPH * n + 0.12, roof, flicker: (h >>> 7) % 5 === 0, phase: (h % 1000) / 159 };
}
function stepNeon(tw, now) {
  const nn = tw.neon;
  for (const m of tw.tubes) { m.scale.y = Math.max(0.001, tw.top); m.position.y = tw.top / 2; }
  nn.blade.visible = tw.top > nn.bladeH + 0.8;
  if (nn.blade.visible) nn.blade.position.y = Math.min(tw.top - nn.bladeH / 2 - 0.3, nn.bladeH / 2 + 0.6 + (tw.top - nn.bladeH) * 0.4);
  nn.roof.visible = tw.top > 7;
  nn.roof.position.y = tw.top + 2.3;
  // a flickering sign stutters like a failing tube; the others breathe slowly
  const s = nn.flicker ? (Math.sin(now / 37 + nn.phase * 9) > 0.2 || Math.sin(now / 900 + nn.phase) > 0 ? 1 : 0.15) : 0.82 + 0.18 * Math.sin(now / 700 + nn.phase);
  nn.blade.material.opacity = s;
  nn.roof.material.opacity = s;
  nn.roof.rotation.y = Math.atan2(camera.position.x - tw.group.position.x, camera.position.z - tw.group.position.z);
}
function newTower(slot, path) {
  const p = lot(slot);
  const group = new THREE.Group();
  group.position.set(p.x, 0.18, p.z);
  city.add(group);
  const roof = new THREE.Mesh(box, shaftMat);
  roof.scale.set(2.1, 0.22, 2.1);
  const antenna = new THREE.Mesh(box, shaftMat);
  antenna.scale.set(0.06, 1.6, 0.06);
  const beacon = new THREE.Mesh(new THREE.SphereGeometry(0.17, 12, 8), new THREE.MeshBasicMaterial({ color: BEACON.unknown }));
  const cap = new THREE.Group();
  antenna.position.y = 0.9;
  beacon.position.y = 1.75;
  cap.add(roof, antenna, beacon);
  group.add(cap);
  const tw = { path, group, cap, beacon, layers: [], top: 0, versions: 0, dropped: 0, status: '', lines: 0 };
  for (const m of [roof, beacon]) { m.userData = { tower: tw, cap: true }; pickables.push(m); }
  neonFor(tw, path);
  slot.files.set(path, tw);
  return tw;
}
function forwardFile(e) {
  if (e.op === 'renamed' && e.from) removeFile(e.from, false);
  if (e.status === 'deleted') { removeFile(e.path, true); return; }
  const slot = dirSlot(dirOf(e.path));
  const tw = slot.files.get(e.path) || newTower(slot, e.path);
  const adds = e.adds || 0, dels = e.dels || 0;
  const col = MAGENTA.clone().lerp(CYAN, adds + dels ? adds / (adds + dels) : 0.5);
  const th = Math.min(2.4, 0.4 + Math.log2(1 + adds + dels) * 0.24);
  const band = new THREE.Mesh(box, new THREE.MeshStandardMaterial({ color: 0x0b0d16, roughness: 0.3, metalness: 0.7, emissive: col, emissiveMap: floorWindows, emissiveIntensity: 1.1 }));
  band.scale.set(2, 0.001, 2);
  band.position.y = tw.top;
  const line = new THREE.Mesh(box, new THREE.MeshBasicMaterial({ color: col }));
  line.scale.set(2.06, 0.05, 2.06);
  line.position.y = tw.top;
  band.userData = { tower: tw, th, y: tw.top + th / 2, flash: 1, adds, dels, commit: commitNow, fetched: !!e.fetched, line };
  line.userData = band.userData;
  tw.group.add(band, line);
  tw.layers.push(band);
  pickables.push(band, line);
  tw.top += th;
  if (tw.layers.length > MAX_LAYERS) { // drop the oldest band; the rest settle down
    const old = tw.layers.shift();
    for (const m of [old, old.userData.line]) { pickables.splice(pickables.indexOf(m), 1); dispose(m, tw.group); }
    tw.dropped++;
    tw.top -= old.userData.th;
    for (const l of tw.layers) l.userData.y -= old.userData.th;
  }
  tw.versions++;
  tw.status = e.status;
  tw.lines = e.lines || 0;
  tw.beacon.material.color.setHex(BEACON[e.status] ?? BEACON.unknown);
  // data motes converge on the new band
  const to = new THREE.Vector3(tw.group.position.x, tw.top, tw.group.position.z);
  const n = reduced ? 2 : Math.min(12, 3 + adds / 8);
  for (let k = 0; k < n && shards.length < 600; k++) {
    const s = new THREE.Mesh(box, new THREE.MeshBasicMaterial({ color: col, transparent: true, opacity: 0.95 }));
    s.scale.setScalar(0.14);
    const a = rand() * Math.PI * 2, r = 8 + rand() * 14;
    s.position.set(to.x + Math.cos(a) * r, to.y + (rand() - 0.2) * 12, to.z + Math.sin(a) * r);
    s.userData = { from: s.position.clone(), dest: to, t: 0, dur: 0.6 + rand() * 0.5, inward: true };
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
    for (let k = 0; k < 10 && shards.length < 600; k++) {
      const s = new THREE.Mesh(box, new THREE.MeshBasicMaterial({ color: 0x8a96b0, transparent: true, opacity: 0.9 }));
      s.scale.setScalar(0.2);
      s.position.copy(at);
      const a = rand() * Math.PI * 2;
      s.userData = { from: at.clone(), dest: at.clone().add(new THREE.Vector3(Math.cos(a) * 9, rand() * 6, Math.sin(a) * 9)), t: 0, dur: 0.9, inward: false };
      city.add(s);
      shards.push(s);
    }
  }
  tw.group.traverse((m) => { const i = pickables.indexOf(m); if (i >= 0) pickables.splice(i, 1); });
  dispose(tw.group, city);
  if (!slot.files.size) clearBlocks(slot);
}

// Billboards: a commit's headline flashes on a tall building's face.
const billboards = [];
function billboard(e) {
  if (reduced || !e.headline) return;
  const tall = [...dirs.values()].flatMap((d) => [...d.files.values()]).filter((t) => t.top > 4);
  if (!tall.length) return;
  const tw = tall[Math.floor(rand() * tall.length)];
  const color = '#' + NEON[Math.floor(rand() * NEON.length)].toString(16).padStart(6, '0');
  const b = new THREE.Mesh(new THREE.PlaneGeometry(5, 0.7), new THREE.MeshBasicMaterial({ map: label(e.headline, color, false), transparent: true, depthWrite: false, side: THREE.DoubleSide }));
  const out = new THREE.Vector3(tw.group.position.x, 0, tw.group.position.z).normalize();
  if (!isFinite(out.x)) out.set(0, 0, 1);
  b.position.copy(tw.group.position).addScaledVector(out, 1.3).setY(tw.top * (0.5 + rand() * 0.4));
  b.lookAt(b.position.clone().add(out));
  b.userData = { born: performance.now() };
  city.add(b);
  billboards.push(b);
  while (billboards.length > 14) dispose(billboards.shift(), city);
}

// ---------- the life: traffic and rain ----------
const CARS = reduced ? 40 : 150;
const cars = new THREE.InstancedMesh(new THREE.BoxGeometry(0.35, 0.12, 0.8), new THREE.MeshBasicMaterial(), CARS);
scene.add(cars);
const car = Array.from({ length: CARS }, () => ({}));
function seedCars() {
  car.forEach((c, i) => {
    Object.assign(c, { f: rand(), h: 4 + rand() * 24, a: rand() * Math.PI * 2, w: (0.04 + rand() * 0.1) * (rand() < 0.5 ? 1 : -1), lane: (rand() < 0.5 ? -1 : 1) * (2 + rand() * 7), u: rand(), v: 0.12 + rand() * 0.2 });
    cars.setColorAt(i, _c.setHex(i % 3 === 0 ? 0xff3355 : i % 3 === 1 ? 0xffffff : NEON[i % NEON.length]));
  });
  cars.instanceColor.needsUpdate = true;
}
function stepCars(dt, inverted) {
  for (let i = 0; i < CARS; i++) {
    const c = car[i];
    if (inverted) { // down the avenue, toward the camera: traffic running backward
      c.u = (c.u - c.v * dt * 0.25 + 1) % 1;
      _o.position.set(c.lane, 3 + c.h * 0.45, camera.position.z + 10 - c.u * 170);
      _o.rotation.set(0, 0, 0);
    } else { // sky lanes around the city
      c.a += c.w * dt;
      const R = 12 + c.f * (extent + 30);
      _o.position.set(Math.cos(c.a) * R, c.h, Math.sin(c.a) * R);
      _o.rotation.set(0, -c.a + (c.w > 0 ? 0 : Math.PI), 0);
    }
    _o.scale.set(1, 1, 1);
    _o.updateMatrix();
    cars.setMatrixAt(i, _o.matrix);
  }
  cars.instanceMatrix.needsUpdate = true;
}
const DROPS = reduced ? 600 : 3000;
const rainPos = new Float32Array(DROPS * 6), drop = new Float32Array(DROPS * 3);
const rain = new THREE.LineSegments(new THREE.BufferGeometry(), new THREE.LineBasicMaterial({ color: 0x9fb4ff, transparent: true, opacity: 0.32 }));
rain.geometry.setAttribute('position', new THREE.BufferAttribute(rainPos, 3));
rain.frustumCulled = false;
scene.add(rain);
for (let i = 0; i < DROPS; i++) { drop[i * 3] = (rand() - 0.5) * 140; drop[i * 3 + 1] = rand() * 60; drop[i * 3 + 2] = (rand() - 0.5) * 140; }
function stepRain(dt, up) {
  const fx = controls.enabled ? controls.target.x : camera.position.x, fz = controls.enabled ? controls.target.z : camera.position.z - 40;
  const vy = (up ? 32 : -38) * dt;
  for (let i = 0; i < DROPS; i++) {
    let y = drop[i * 3 + 1] + vy;
    if (y < 0) y += 60; else if (y > 60) y -= 60;
    drop[i * 3 + 1] = y;
    const x = fx + drop[i * 3], z = fz + drop[i * 3 + 2], j = i * 6;
    rainPos[j] = x; rainPos[j + 1] = y; rainPos[j + 2] = z;
    rainPos[j + 3] = x + 0.04; rainPos[j + 4] = y + 0.75; rainPos[j + 5] = z;
  }
  rain.geometry.attributes.position.needsUpdate = true;
}

// ---------- code sheets: the code itself, painted ----------
// Inverted, a commit's diff is shown mirrored and un-writes itself (its added
// lines erase, its removed lines come back), then recedes down the avenue to its
// arch. Forward, each rebuilt file is painted (context fades in, the lines the
// patch added type themselves out), then the sheet folds into its building's
// roof. Under a backlog only a sample is painted. The text is the repository's,
// so it only ever reaches a canvas.
const SW = 1024, SH = 560, LH = 30;
const sheetGeo = new THREE.PlaneGeometry(7.2, 7.2 * SH / SW);
const sheets = [];
function makeSheet(kind, e, dur) {
  const c = document.createElement('canvas');
  c.width = SW; c.height = SH;
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  const m = new THREE.Mesh(sheetGeo, new THREE.MeshBasicMaterial({ map: tex, transparent: true, opacity: 0, depthWrite: false, side: THREE.DoubleSide, toneMapped: false }));
  m.userData = { kind, e, g: c.getContext('2d'), tex, t: 0, dur, state: 'paint', order: [...(e.added || [])].sort((a, b) => a - b) };
  return m;
}
const clamp01 = (x) => Math.max(0, Math.min(1, x));
function paintSheet(u, p) {
  const g = u.g, e = u.e, inv = u.kind === 'inv';
  g.setTransform(1, 0, 0, 1, 0, 0);
  g.clearRect(0, 0, SW, SH);
  if (inv) { g.translate(SW, 0); g.scale(-1, 1); } // read from the other side of the turnstile
  g.fillStyle = inv ? 'rgba(24, 4, 10, 0.88)' : 'rgba(4, 8, 20, 0.88)';
  g.fillRect(0, 0, SW, SH);
  g.strokeStyle = inv ? 'rgba(255, 59, 90, 0.8)' : e.status === 'exact' ? 'rgba(34, 229, 255, 0.85)' : 'rgba(255, 181, 71, 0.85)';
  g.lineWidth = 3;
  g.strokeRect(1.5, 1.5, SW - 3, SH - 3);
  g.font = '20px ui-monospace, Menlo, monospace';
  g.textBaseline = 'middle';
  g.fillStyle = inv ? '#ff8aa0' : '#8eeeff';
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
      g.fillStyle = mark === '+' ? '#ff6b8a' : mark === '-' ? '#ffd6de' : 'rgba(255, 160, 175, 0.45)';
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
    g.fillStyle = 'rgba(34, 229, 255, 0.16)';
    g.fillRect(78, y - 14, Math.max(8, g.measureText(shown).width + 12), 28);
    g.fillStyle = '#e3f9ff';
    g.fillText(shown, 84, y);
    if (f < 1) { g.fillStyle = '#22e5ff'; g.fillRect(84 + g.measureText(shown).width + 2, y - 11, 12, 22); }
  }
  u.tex.needsUpdate = true;
}
// Both kinds are painted large before the camera. (Hung far ahead instead, the
// inverted sheets sat beyond a camera that trails a backlog, too small to read:
// seen live, google/uuid.)
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
  if (s) s.userData.to = new THREE.Vector3((e.i % 2 ? -1 : 1) * 4, 9.3, -depth);
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
function fold(s) { // paint it whole, then send it to its building's roof, or down the avenue
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
      const to = u.kind === 'inv' ? u.to : tw ? _v.copy(tw.group.position).setY(tw.top + 0.5) : u.from;
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
function enterForward() {
  phase = 'fwd';
  setPhase('forward', 'fwd');
  skyMood = SKY.fwd;
  avenue.visible = false;
  city.visible = true;
  camera.position.set(34, 26, 40);
  controls.target.set(0, 4, 0);
  controls.enabled = true;
  viewerMoved = false;
}
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
      turnstile.position.set(0, 4.5, -depth - 8);
      turnstile.visible = true;
      setPhase(e.root ? 'turnstile · root reached' : 'turnstile · window edge', 'inv');
      holdUntil = performance.now() + (reduced ? 300 : 2200);
      flyThrough = { t0: performance.now(), from: camera.position.clone() };
      break;
    case 'commit':
      if (phase !== 'fwd') enterForward();
      clock = e.i;
      commitNow = { sha: e.sha, headline: e.headline || '', author: e.author || '', date: (e.date || '').slice(0, 10) };
      if (e.i % 5 === 0) billboard(e);
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
  $('sum-line').textContent = `${s.head_exact} of ${s.head_files} files at the head rebuilt byte-exact (${pct}%).`;
  const rows = [
    ['commits walked', s.commits], ['root reached', s.root_reached ? 'yes' : 'no (window edge)'],
    ['file versions built', s.file_versions], ['proven exact', s.exact], ['  of them from a fetched blob', s.fetched],
    ['unproven', s.unknown], ['mismatch', s.mismatch],
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
  $('sum-fine').textContent = 'Each file version is checked against the blob id GitHub reports for it; the final tree against the head commit. Where GitHub sends no patch (a binary, a very large diff), the blob is fetched by that id and proven by its hash. Unproven means the content was never streamed, never guessed.';
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

// reset clears the stage and grows a new world from the repository's name.
function reset(repo) {
  for (const g of [arches, city]) while (g.children.length) dispose(g.children[0], g);
  city.add(streets);
  for (const s of [...sheets]) retire(s);
  dirs.clear(); shards.length = 0; pickables.length = 0; billboards.length = 0;
  queue = []; received = 0; depth = 0; clock = 0; extent = 10; nextCell = 0; freeCells = [];
  commitNow = null; runDone = false; hover = null; tip.hidden = true; closeFile();
  rand = mulberry32(hashStr(repo));
  noise = createNoise2D(mulberry32(hashStr(repo) ^ 0x9e3779b9));
  buildSkyline(); seedCars();
  Object.keys(stats).forEach((k) => (stats[k] = 0));
  phase = 'inv'; skyMood = SKY.inv; turnstile.visible = false; flyThrough = null;
  avenue.visible = true; city.visible = false; controls.enabled = false;
  camera.position.set(0, 3.2, 12); camera.lookAt(0, 3.2, -20);
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
    reset(repo.toLowerCase());
    sessionId = body.id;
    connect();
  } catch (err) {
    say(err.message, true);
    $('go').disabled = false;
  }
});

// ---------- the viewer's hands: hover, open ----------
// The roof opens the file's current copy; a floor band opens that change's diff.
let hover = null, runDone = false, press = null;
const tip = $('tip');
const raycaster = new THREE.Raycaster(), ndc = new THREE.Vector2();
canvas.addEventListener('contextmenu', (ev) => ev.preventDefault()); // the right button orbits
canvas.addEventListener('pointerdown', (ev) => { if (ev.button === 0) press = { x: ev.clientX, y: ev.clientY }; });
canvas.addEventListener('pointermove', (ev) => pick(ev.clientX, ev.clientY, ev.buttons & 2));
canvas.addEventListener('pointerup', (ev) => {
  if (ev.button === 0 && press && Math.hypot(ev.clientX - press.x, ev.clientY - press.y) < 5 && hover) {
    if (hover.layer) openDiff(hover.tower, hover.layer); else openFile(hover.tower);
  }
  press = null;
});
canvas.addEventListener('pointerleave', () => { hover = null; tip.hidden = true; });

// pick finds the floor band or roof under the pointer and fills the tooltip.
function pick(x, y, turning) {
  if (phase !== 'fwd' || turning) { hover = null; tip.hidden = true; return; }
  ndc.set((x / innerWidth) * 2 - 1, -(y / innerHeight) * 2 + 1);
  raycaster.setFromCamera(ndc, camera);
  const hit = raycaster.intersectObjects(pickables, false)[0];
  if (!hit) { hover = null; tip.hidden = true; canvas.style.cursor = ''; return; }
  const u = hit.object.userData, tw = u.tower;
  const layer = u.cap ? null : tw.layers.find((l) => l.userData === u) || null;
  hover = { tower: tw, layer };
  const status = { exact: 'proven exact', unknown: 'unproven', mismatch: 'mismatch' }[tw.status] || tw.status;
  const rows = [tw.path, `${status} · ${tw.lines} lines · ${tw.versions} versions${tw.dropped ? ` (${tw.dropped} oldest not drawn)` : ''}`];
  if (layer && u.commit) rows.push(`${u.commit.sha.slice(0, 8)} ${u.commit.date} ${u.commit.author}`, u.commit.headline, `+${u.adds} −${u.dels} lines`, 'click to see this change');
  else rows.push('roof: click to open the current copy');
  tip.replaceChildren(...rows.map((r, i) => { const d = document.createElement('div'); d.textContent = r; if (i === 0) d.className = 'path'; return d; }));
  tip.style.left = `${Math.min(x + 16, innerWidth - 340)}px`;
  tip.style.top = `${Math.min(y + 16, innerHeight - 150)}px`;
  tip.hidden = false;
  canvas.style.cursor = 'pointer';
}

function showPanel(title, meta) {
  $('f-path').textContent = title;
  $('f-meta').textContent = meta;
  $('f-code').replaceChildren();
  $('file').hidden = false;
  document.body.classList.add('filing');
}
async function getJSON(url) {
  const r = await fetch(url);
  const b = await r.json();
  if (!r.ok) throw new Error(b.error ? b.error.message : r.statusText);
  return b;
}
// openFile shows the file's current copy as the run rebuilt it, once the run is done.
async function openFile(tw) {
  showPanel(tw.path, runDone ? 'opening…' : 'the files open when the reconstruction is done');
  if (!runDone) return;
  try {
    const f = await getJSON(`/api/v1/reconstructions/${sessionId}/file?path=${encodeURIComponent(tw.path)}`);
    if (!f.known) { $('f-meta').textContent = `unproven: ${f.why || 'the run never held this file\'s content'}`; return; }
    if (f.binary) { $('f-meta').textContent = `${f.bytes} bytes, binary · proven by its blob id`; return; }
    const lines = f.content.replace(/\n$/, '').split('\n');
    const w = String(lines.length).length;
    $('f-meta').textContent = `current copy, rebuilt and proven against GitHub's blob id · ${lines.length} lines · ${tw.versions} versions${f.truncated ? ' · first 1 MB shown' : ''}`;
    $('f-code').textContent = lines.map((l, i) => `${String(i + 1).padStart(w)}  ${l}`).join('\n');
  } catch (err) {
    $('f-meta').textContent = err.message;
  }
}
// openDiff shows the change one floor band stands for.
async function openDiff(tw, layer) {
  const c = layer.userData.commit;
  showPanel(`${tw.path} @ ${c.sha.slice(0, 8)}`, 'opening…');
  try {
    const d = await getJSON(`/api/v1/reconstructions/${sessionId}/diff?sha=${c.sha}&path=${encodeURIComponent(tw.path)}`);
    $('f-meta').textContent = `${c.date} ${c.author} · ${d.headline} · ${d.status}${d.from ? ' from ' + d.from : ''} · +${d.additions} −${d.deletions}`;
    if (!d.patch) {
      $('f-code').textContent = layer.userData.fetched
        ? 'GitHub sends no patch for this change (a binary, or a very large diff).\nThe file was proven here by fetching its blob by id.'
        : 'GitHub sends no patch for this change.';
      return;
    }
    const frag = document.createDocumentFragment();
    for (const l of d.patch.split('\n')) {
      const s = document.createElement('span');
      s.textContent = l + '\n';
      s.className = l[0] === '+' ? 'add' : l[0] === '-' ? 'del' : l.startsWith('@@') ? 'hunk' : '';
      frag.append(s);
    }
    $('f-code').replaceChildren(frag);
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
const want = new THREE.Vector3();
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

  const inverted = phase !== 'fwd';
  if (inverted && !flyThrough) { // run down the avenue, a little behind the newest arch
    const z = 14 - depth;
    camera.position.z += (z - camera.position.z) * Math.min(1, dt * 2.5);
    camera.position.x = Math.sin(now / 2600) * 0.8;
    camera.position.y = 3.2;
    camera.lookAt(0, 4, camera.position.z - 30);
  } else if (flyThrough) {
    const t = Math.min(1, (now - flyThrough.t0) / (reduced ? 300 : 2200));
    camera.position.z = flyThrough.from.z + (turnstile.position.z - 4 - flyThrough.from.z) * ease(t);
    camera.lookAt(0, 4.5, camera.position.z - 20);
    turnstile.rotation.z += dt * (1 + t * 10);
    if (t > 0.72 && !flyThrough.flashed) {
      flyThrough.flashed = true;
      const f = $('flash'); f.classList.add('on');
      requestAnimationFrame(() => requestAnimationFrame(() => f.classList.remove('on')));
      turnstile.visible = false;
      for (const s of sheets.filter((x) => x.userData.kind === 'inv')) retire(s);
    }
    if (t >= 1) flyThrough = null;
  } else {
    // Until the viewer turns or zooms, the camera backs off as the city grows.
    if (!viewerMoved) {
      want.copy(camera.position).sub(controls.target).setLength(16 + extent * 1.5).add(controls.target);
      camera.position.lerp(want, Math.min(1, dt * 1.5));
    }
    controls.update();
  }
  stepWalls();
  stepSky(dt);
  stepCars(dt, inverted);
  stepRain(dt, inverted);
  neonLights.forEach((l, i) => {
    const a = now / 4000 + (i * Math.PI * 2) / 3, R = inverted ? 10 : extent * 0.6;
    l.position.set(Math.cos(a) * R, inverted ? 6 : 12, (inverted ? camera.position.z - 30 : 0) + Math.sin(a) * R);
  });

  // floor bands grow to their height and settle; a fresh one flashes; the
  // hovered building glows; beacons pulse
  const k6 = Math.min(1, dt * 6), pulse = 0.75 + 0.25 * Math.sin(now / 300);
  for (const slot of dirs.values()) {
    for (const tw of slot.files.values()) {
      const lit = hover && hover.tower === tw ? 0.6 : 0;
      for (const m of tw.layers) {
        const u = m.userData;
        m.scale.y += (u.th - m.scale.y) * k6;
        m.position.y += (u.y - m.position.y) * k6;
        u.line.position.y = m.position.y + m.scale.y / 2;
        u.flash = Math.max(0, u.flash - dt * 1.5);
        m.material.emissiveIntensity = 1.1 + lit + u.flash * 2 + (hover && hover.layer === m ? 1.2 : 0);
      }
      tw.cap.position.y += (tw.top + 0.11 - tw.cap.position.y) * k6;
      stepNeon(tw, now);
      tw.beacon.scale.setScalar(pulse + (hover && hover.tower === tw && !hover.layer ? 0.5 : 0));
    }
  }
  for (const b of billboards) b.material.opacity = 0.75 + 0.25 * Math.sin((now - b.userData.born) / 90) * Math.sin(now / 1700);

  // motes: inward ones converge on their band, outward ones fly off
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

  composer.render();
  requestAnimationFrame(frame);
}

// The stage before any run: the seeded skyline, the avenue, the rain.
buildSkyline(); seedCars();
city.visible = false;
camera.lookAt(0, 3.2, -20);
requestAnimationFrame(frame);

const fromURL = new URLSearchParams(location.search).get('repo');
if (fromURL) { $('repo').value = fromURL; $('query').requestSubmit(); }
