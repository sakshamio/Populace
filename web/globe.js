// WebGL2 renderer. One draw call for the entire population.
//
// The instance buffer arrives from Go as packed binary and goes straight into
// a GPU buffer -- no JSON, no parse, no garbage. Positions are uploaded once
// and never move again; the only buffer that changes per tick is two bytes of
// state per persona.
//
// Layout must match internal/world.InstanceBytes exactly:
//   f32 lon | f32 lat | u16 palette | u16 shape | u16 arch | u16 flags   = 16B

const STRIDE = 16;

// The flags word is unpacked on the GPU, not in JS. It arrives from Go in the
// layout internal/sim.packFlags writes:
//   bits 0-1 adoption state, bits 2-7 opinion (64 levels), bit 8 broadcaster,
//   bit 9 feed-driven adopter, bit 10 reached by a feed this tick.
// Unpacking ten million of these per tick in JavaScript would cost more than
// the simulation tick that produced them; here it is four integer ops in a
// shader that was going to run anyway.
//
// The projection is a morph rather than two programs. Globe and map are the
// same points under a different mapping, and interpolating between them in the
// vertex shader means the transition is continuous -- you can watch a cascade
// keep spreading while the world unrolls underneath it. Two separate views
// would cut to a new frame and lose the thing being followed.
const VS = `#version 300 es
layout(location=0) in vec2 a_ll;      // lon, lat in radians
layout(location=1) in uint a_flags;   // packed state, see internal/sim
uniform float u_rot, u_radius, u_morph, u_zoom, u_mapy;
uniform vec2  u_res, u_pan;
uniform float u_size;
out float v_state, v_op, v_media, v_face, v_feed, v_fresh;
const float PI = 3.14159265359;
void main(){
  float lon = a_ll.x + u_rot;
  // Wrap into [-PI, PI] so the map does not smear a seam of points across the
  // whole width as the rotation accumulates.
  lon = mod(lon + PI, 2.0*PI) - PI;
  float lat = a_ll.y;
  float cl  = cos(lat);

  vec3  sp   = vec3(cl*sin(lon), sin(lat), cl*cos(lon));
  vec2  sNDC = sp.xy * u_radius / (u_res*0.5);
  vec2  mNDC = vec2(lon/PI * 0.92, lat/(PI*0.5) * u_mapy);

  vec2 ndc = mix(sNDC, mNDC, u_morph);
  // The far hemisphere is culled on the globe and must not be on the map,
  // where every point faces the viewer. Interpolating the facing term with the
  // projection is what keeps the back of the world fading in as it unrolls
  // instead of popping.
  v_face  = mix(sp.z, 1.0, u_morph);

  v_state = float(a_flags & 3u);
  v_op    = float((a_flags >> 2) & 63u) / 63.0 * 2.0 - 1.0;
  v_media = ((a_flags & 256u)  != 0u) ? 1.0 : 0.0;
  v_feed  = ((a_flags & 512u)  != 0u) ? 1.0 : 0.0;
  v_fresh = ((a_flags & 1024u) != 0u) ? 1.0 : 0.0;

  gl_Position  = vec4(ndc * u_zoom + u_pan, 0.0, 1.0);
  gl_PointSize = (u_size + v_media * 1.6) * max(1.0, u_zoom * 0.75);
}`;

// Three channels in one dot, which is why opinion, adoption, and channel are
// separate fields rather than one "activated" bit: hue carries which way a
// person leans, brightness carries whether the story has reached them, and a
// warm rim marks the ones the algorithm reached rather than their friends. A
// population can be uniformly aware and completely split, and that is exactly
// the state worth being able to see.
// Colours match the CSS tokens exactly (--against, --favour, --feed) so a dot
// on the globe and a chip in the sidebar read as the same fact rather than as
// two designers' guesses at "roughly blue" and "roughly orange".
const FS = `#version 300 es
precision highp float;
in float v_state, v_op, v_media, v_face, v_feed, v_fresh;
out vec4 o;
void main(){
  if (v_face <= 0.02) discard;         // backface cull: one dot product
  vec3 against = vec3(0.298,0.494,1.000);   // #4C7EFF
  vec3 neutral = vec3(0.400,0.450,0.580);
  vec3 favour  = vec3(1.000,0.604,0.239);   // #FF9A3D
  float t = clamp(abs(v_op) * 1.6, 0.0, 1.0);
  vec3 c = mix(neutral, v_op < 0.0 ? against : favour, t);

  // Unaware people stay dim; carriers burn; the fatigued keep the colour they
  // ended up with but stop drawing the eye.
  float lum = 0.34;
  if      (v_state > 1.5) lum = 0.66;  // fatigued: adopted, no longer spreading
  else if (v_state > 0.5) lum = 1.45;  // actively transmitting
  if (v_media > 0.5) lum *= 1.4;

  // Feed-driven adopters are tinted toward the algorithmic colour. Peer spread
  // and feed spread produce identical adoption states, so without this the one
  // thing the media layer demonstrates is invisible on the thing it changed.
  if (v_feed > 0.5)  c = mix(c, vec3(0.761,0.310,0.820), 0.55);   // #C24FD1
  if (v_fresh > 0.5) lum *= 1.9;       // touched by a feed on this very tick

  o = vec4(c * lum * (0.42 + 0.58*v_face), 1.0);
}`;

const BODY_VS = `#version 300 es
layout(location=0) in vec2 a_p;
uniform float u_radius, u_morph, u_zoom, u_mapy; uniform vec2 u_res, u_pan;
out vec2 v_p;
void main(){
  v_p = a_p;
  vec2 sNDC = a_p * u_radius / (u_res*0.5);
  vec2 mNDC = vec2(a_p.x * 0.92, a_p.y * u_mapy);
  gl_Position = vec4(mix(sNDC, mNDC, u_morph) * u_zoom + u_pan, 0.0, 1.0);
}`;

const BODY_FS = `#version 300 es
precision highp float;
in vec2 v_p; out vec4 o;
uniform float u_morph;
void main(){
  float d = length(v_p);
  // The sphere is a lit disc; the map is a flat plate. Crossfading the two
  // shadings rather than switching keeps the ground continuous through the
  // morph, which is the only reason the transition reads as one object.
  if (d > 1.0 && u_morph < 0.5) discard;
  float z = sqrt(max(0.0, 1.0 - min(d*d, 1.0)));
  float lam = clamp(dot(normalize(vec3(v_p,z)), normalize(vec3(-0.35,0.25,0.9))), 0.0, 1.0);
  // Deep indigo-teal jewel tones, matched to the CSS --deep/--deep2 tokens so
  // the WebGL canvas and the light chrome around it read as one deliberate
  // palette rather than a dark widget dropped into a bright page.
  vec3 globe = mix(vec3(0.055,0.125,0.220), vec3(0.071,0.188,0.286), lam);
  vec3 plate = vec3(0.059,0.145,0.251);
  o = vec4(mix(globe, plate, u_morph), 1.0);
}`;

// Country borders: real Natural Earth geometry (via web/borders.bin, decoded
// offline from the public-domain world-atlas 110m dataset -- see
// scratchpad/borders/decode.js for the one-time conversion), not a texture.
// Same lon/lat-radians convention and the same sphere/map morph as the
// population points, so a border line and a persona dot always agree about
// where the coastline actually is.
//
// Rotation is where line rendering gets genuinely harder than point
// rendering. The population points wrap each vertex independently with
// mod(lon+rot+PI, 2PI)-PI, which is fine for points -- each one just teleports
// a hair, invisible for a single pixel with nothing attached to it. A LINE
// STRIP is not independent: whenever that per-vertex wrap sends one endpoint
// of a segment to +180 degrees and its neighbour to -180, the two are still
// numerically far apart even though the true points are right next to each
// other, and the rasterizer draws the "short way" as a line clear across the
// screen. This is not particular to rings that happen to cross the real
// antimeridian -- since rotation is unbounded, *any* ring's own wrap point
// sweeps past every one of its edges in turn as it spins.
//
// A per-fragment derivative test (discard where fwidth() is large) looks
// like the fix and is not: a spurious line spans many pixels, so its
// *per-fragment* derivative is small even though the segment end to end is
// huge -- confirmed empirically (see the panned-map screenshots this
// comment replaced). The actual fix has to compare a segment's two
// endpoints, which one vertex-shader invocation never sees on its own.
//
// The fix: every vertex in a ring also carries a_anchor, that ring's own
// first raw longitude (set once at load time, identical for every vertex in
// the ring). Each vertex computes its position not by wrapping itself
// independently, but by wrapping the *anchor* and adding its own
// rotation-invariant shortest-path offset from that anchor. Two vertices
// anchored to the same reference can never land on opposite sides of the
// branch cut from each other, because they are both measured from one
// consistently-wrapped point -- and no real country's own longitude span
// (even Russia's) exceeds the +/-180 degree reach of one shortest-path
// offset, so nothing is ever cut off by this on the sphere. On the flat map
// a ring anchored near the map's own edge can draw a few degrees past that
// edge rather than wrapping to the other side, which is a country clipped
// at the seam instead of a line drawn through the world -- the failure mode
// worth having.
const BORDER_VS = `#version 300 es
layout(location=0) in vec2  a_ll;      // this vertex, raw lon/lat radians
layout(location=1) in float a_anchor;  // this ring's own raw lon, radians
uniform float u_rot, u_radius, u_morph, u_zoom, u_mapy;
uniform vec2  u_res, u_pan;
out float v_face;
const float PI = 3.14159265359;
void main(){
  float lonRef = mod(a_anchor + u_rot + PI, 2.0*PI) - PI;
  float delta  = mod(a_ll.x - a_anchor + PI, 2.0*PI) - PI;
  float lon = lonRef + delta;
  float lat = a_ll.y;
  float cl  = cos(lat);

  vec3  sp   = vec3(cl*sin(lon), sin(lat), cl*cos(lon));
  vec2  sNDC = sp.xy * u_radius / (u_res*0.5);
  vec2  mNDC = vec2(lon/PI * 0.92, lat/(PI*0.5) * u_mapy);

  vec2 ndc = mix(sNDC, mNDC, u_morph);
  v_face = mix(sp.z, 1.0, u_morph);
  gl_Position = vec4(ndc * u_zoom + u_pan, 0.0, 1.0);
}`;

const BORDER_FS = `#version 300 es
precision highp float;
in float v_face;
out vec4 o;
void main(){
  if (v_face <= 0.02) discard;
  // Thin and quiet on purpose -- this is context for the population, not data.
  o = vec4(1.0, 1.0, 1.0, 0.22 * (0.3 + 0.7 * v_face));
}`;

function compile(gl, type, src) {
  const s = gl.createShader(type);
  gl.shaderSource(s, src);
  gl.compileShader(s);
  if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) {
    throw new Error(gl.getShaderInfoLog(s) || 'shader compile failed');
  }
  return s;
}

function link(gl, vs, fs) {
  const p = gl.createProgram();
  gl.attachShader(p, compile(gl, gl.VERTEX_SHADER, vs));
  gl.attachShader(p, compile(gl, gl.FRAGMENT_SHADER, fs));
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) {
    throw new Error(gl.getProgramInfoLog(p) || 'program link failed');
  }
  return p;
}

// Uniform locations are looked up once and cached. getUniformLocation is a
// string lookup into the driver; calling it six times per uniform per frame
// showed up as a measurable cost at 60fps long before the draw call did.
function uniforms(gl, prog, names) {
  const u = {};
  for (const n of names) u[n] = gl.getUniformLocation(prog, n);
  return u;
}

export class Globe {
  constructor(canvas, onStats) {
    this.cv = canvas;
    this.onStats = onStats || (() => {});
    const gl = canvas.getContext('webgl2', { antialias: true, alpha: false });
    if (!gl) throw new Error('WebGL2 is required and is not available here.');
    this.gl = gl;

    this.pPts = link(gl, VS, FS);
    this.pBody = link(gl, BODY_VS, BODY_FS);
    this.pBorder = link(gl, BORDER_VS, BORDER_FS);
    this.uPts = uniforms(gl, this.pPts,
      ['u_rot','u_radius','u_res','u_size','u_morph','u_zoom','u_pan','u_mapy']);
    this.uBody = uniforms(gl, this.pBody,
      ['u_radius','u_res','u_morph','u_zoom','u_pan','u_mapy']);
    this.uBorder = uniforms(gl, this.pBorder,
      ['u_rot','u_radius','u_res','u_morph','u_zoom','u_pan','u_mapy']);
    this.borderCount = 0;
    this.loadBorders();

    this.quad = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.quad);
    gl.bufferData(gl.ARRAY_BUFFER,
      new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);

    this.vaoBody = gl.createVertexArray();
    gl.bindVertexArray(this.vaoBody);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.quad);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    this.n = 0;
    this.rot = 0;
    this.dirty = false;
    this.fps = 60;
    this.reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    // View state. morph 0 = globe, 1 = map; morphTo is the target and the two
    // are eased together each frame.
    this.morph = 0;
    this.morphTo = 0;
    this.zoom = 1;
    this.pan = [0, 0];
    this.spin = !this.reduce;

    this._resize = this.resize.bind(this);
    window.addEventListener('resize', this._resize);
    this.resize();
    this.bindPointer();
  }

  // Drag to turn the world, wheel to zoom. Dragging sets spin off, because a
  // world that keeps rotating out from under the cursor after you have grabbed
  // it is the single most irritating thing a globe can do.
  bindPointer() {
    const cv = this.cv;
    let down = false, lastX = 0, lastY = 0, moved = 0;

    cv.addEventListener('pointerdown', (e) => {
      down = true; moved = 0;
      lastX = e.clientX; lastY = e.clientY;
      cv.setPointerCapture(e.pointerId);
      this.spin = false;
    });
    cv.addEventListener('pointermove', (e) => {
      if (!down) return;
      const dx = e.clientX - lastX, dy = e.clientY - lastY;
      lastX = e.clientX; lastY = e.clientY;
      moved += Math.abs(dx) + Math.abs(dy);
      this.rot += dx * 0.005;
      // Vertical drag pans rather than tilts. Tilting would need a full camera
      // and a matrix; panning gives the same "look at another part" affordance
      // for two lines, and on the map it is the only thing that makes sense.
      this.pan[1] = Math.max(-1.2, Math.min(1.2, this.pan[1] - dy * 0.002 * this.zoom));
    });
    const end = (e) => {
      if (!down) return;
      down = false;
      try { cv.releasePointerCapture(e.pointerId); } catch (_) {}
      // A drag is not a click. Without this threshold every attempt to turn
      // the globe also opens the inspector on whoever was under the finger
      // when it stopped.
      this.suppressClick = moved > 6;
    };
    cv.addEventListener('pointerup', end);
    cv.addEventListener('pointercancel', end);

    cv.addEventListener('wheel', (e) => {
      e.preventDefault();
      const k = Math.exp(-e.deltaY * 0.0016);
      this.zoom = Math.max(1, Math.min(14, this.zoom * k));
      if (this.zoom === 1) this.pan = [0, 0];
    }, { passive: false });
  }

  setView(v) { this.morphTo = v === 'map' ? 1 : 0; }
  setSpin(on) { this.spin = on && !this.reduce; }
  resetView() { this.zoom = 1; this.pan = [0, 0]; this.rot = 0; }

  // Fetched once, independent of the population load -- borders are a fixed
  // ~80KB asset (real Natural Earth coastlines at 110m resolution, see
  // BORDER_VS's comment for why they're stored pre-decoded rather than as
  // TopoJSON), not something that scales with -n. A failure here (offline
  // dev server without the asset, say) just means no borders draw; it must
  // never take the population view down with it.
  async loadBorders() {
    let buf;
    try {
      const res = await fetch('/borders.bin');
      if (!res.ok) throw new Error(`borders.bin: ${res.status}`);
      buf = await res.arrayBuffer();
    } catch (e) {
      console.warn('country borders not loaded:', e);
      return;
    }
    const gl = this.gl;
    const dv = new DataView(buf);
    const magic = String.fromCharCode(dv.getUint8(0), dv.getUint8(1), dv.getUint8(2), dv.getUint8(3));
    if (magic !== 'PBD1') { console.warn(`bad borders stream magic: ${magic}`); return; }
    const numRings = dv.getUint32(4, true);

    // One giant position buffer plus one index buffer using WebGL2's
    // fixed primitive-restart index (0xFFFF for UNSIGNED_SHORT) between
    // rings -- every ring in one draw call, the same "one call" discipline
    // the population points already follow.
    let totalVerts = 0;
    { let off = 8; for (let r = 0; r < numRings; r++) { const nv = dv.getUint32(off, true); totalVerts += nv; off += 4 + nv * 8; } }
    if (totalVerts >= 0xFFFF) { console.warn('borders: too many vertices for a 16-bit restart index'); return; }

    const positions = new Float32Array(totalVerts * 2);
    // Every vertex in a ring repeats that ring's own first raw longitude --
    // see BORDER_VS's comment for why the shader needs this to keep a whole
    // ring wrapped consistently under rotation.
    const anchors = new Float32Array(totalVerts);
    const indices = new Uint16Array(totalVerts + numRings); // +1 restart marker per ring
    let voff = 8, pw = 0, iw = 0, vertBase = 0;
    for (let r = 0; r < numRings; r++) {
      const nv = dv.getUint32(voff, true); voff += 4;
      const anchorLon = dv.getFloat32(voff, true);
      for (let k = 0; k < nv; k++) {
        positions[pw++] = dv.getFloat32(voff, true); voff += 4;
        positions[pw++] = dv.getFloat32(voff, true); voff += 4;
        anchors[vertBase + k] = anchorLon;
        indices[iw++] = vertBase + k;
      }
      indices[iw++] = 0xFFFF;
      vertBase += nv;
    }

    this.bBorderPos = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bBorderPos);
    gl.bufferData(gl.ARRAY_BUFFER, positions, gl.STATIC_DRAW);
    this.bBorderAnchor = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bBorderAnchor);
    gl.bufferData(gl.ARRAY_BUFFER, anchors, gl.STATIC_DRAW);
    this.bBorderIdx = gl.createBuffer();
    gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, this.bBorderIdx);
    gl.bufferData(gl.ELEMENT_ARRAY_BUFFER, indices, gl.STATIC_DRAW);

    this.vaoBorder = gl.createVertexArray();
    gl.bindVertexArray(this.vaoBorder);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bBorderPos);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bBorderAnchor);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 1, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, this.bBorderIdx);
    gl.bindVertexArray(null);

    this.borderCount = iw;
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
  }

  // Takes the raw ArrayBuffer from /api/instances. The position attribute is
  // read directly out of the packed record with a stride -- the bytes are
  // never copied into a JS array, so a ten-million-persona load costs one
  // fetch and one bufferData.
  load(buf) {
    const gl = this.gl;
    const dv = new DataView(buf);
    const magic = String.fromCharCode(dv.getUint8(0), dv.getUint8(1), dv.getUint8(2), dv.getUint8(3));
    if (magic !== 'PPL1') throw new Error(`bad instance stream magic: ${magic}`);
    const n = dv.getUint32(4, true);
    const body = buf.slice(8);
    if (body.byteLength !== n * STRIDE) {
      throw new Error(`instance stream is ${body.byteLength} bytes, expected ${n * STRIDE}`);
    }
    this.n = n;

    this.bInst = this.bInst || gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bInst);
    gl.bufferData(gl.ARRAY_BUFFER, body, gl.STATIC_DRAW);   // once, then never

    // The flags buffer is the only thing that changes after load. Kept as its
    // own buffer rather than a field inside the interleaved record so a tick
    // uploads 2 bytes per persona instead of re-uploading 16.
    this.flags = new Uint16Array(n);
    this.bState = this.bState || gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
    gl.bufferData(gl.ARRAY_BUFFER, this.flags, gl.DYNAMIC_DRAW);

    // Positions are kept in JS as well, but only so picking can search them.
    // Read out of the same bytes rather than copied per field.
    this.ll = new Float32Array(n * 2);
    const src = new DataView(body);
    for (let i = 0; i < n; i++) {
      this.ll[i * 2]     = src.getFloat32(i * STRIDE, true);
      this.ll[i * 2 + 1] = src.getFloat32(i * STRIDE + 4, true);
    }

    this.vao = gl.createVertexArray();
    gl.bindVertexArray(this.vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bInst);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, STRIDE, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
    gl.enableVertexAttribArray(1);
    // vertexAttribIPointer, not vertexAttribPointer: the flags word is an
    // integer to be masked, not a number to be interpolated. The float path
    // would silently convert and the bit tests would read garbage.
    gl.vertexAttribIPointer(1, 1, gl.UNSIGNED_SHORT, 0, 0);
    gl.bindVertexArray(null);
  }

  // Applies one state frame from /api/state. Two forms, chosen by the server
  // per frame: a sparse delta while little has changed, a full flags array
  // once more than a third of the population has moved -- at six bytes a delta
  // record against two for a full one, the crossover is real and the wrong
  // choice costs three times the bandwidth.
  applyFrame(buf) {
    const dv = new DataView(buf);
    const magic = String.fromCharCode(dv.getUint8(0), dv.getUint8(1), dv.getUint8(2), dv.getUint8(3));
    const count = dv.getUint32(4, true);
    this.tick = dv.getUint32(8, true);
    if (!this.flags) return 0;

    if (magic === 'PFL1') {
      // Whole array; the view is over the same bytes the server wrote.
      const src = new Uint16Array(buf, 12, Math.min(count, this.flags.length));
      this.flags.set(src);
    } else if (magic === 'PDL1') {
      for (let k = 0; k < count; k++) {
        const o = 12 + k * 6;
        const idx = dv.getUint32(o, true);
        if (idx < this.flags.length) this.flags[idx] = dv.getUint16(o + 4, true);
      }
    } else {
      throw new Error(`bad state frame magic: ${magic}`);
    }
    this.dirty = true;
    return count;
  }

  // Turns a click into a point on the world, or null if it missed.
  //
  // This inverts the vertex shader exactly rather than approximating it, in
  // whichever projection is currently on screen. Keeping it in lockstep with
  // the shader matters: an approximate unprojection would return a person a few
  // degrees from the one under the cursor, which reads as the inspector being
  // wrong about who lives where.
  pick(clientX, clientY) {
    const rect = this.cv.getBoundingClientRect();
    let ndcx = ((clientX - rect.left) / rect.width) * 2 - 1;
    let ndcy = 1 - ((clientY - rect.top) / rect.height) * 2;

    // Undo pan and zoom first; both are applied last in the shader.
    ndcx = (ndcx - this.pan[0]) / this.zoom;
    ndcy = (ndcy - this.pan[1]) / this.zoom;

    let lon, lat;
    if (this.morph > 0.5) {
      const x = ndcx / 0.92;
      const y = ndcy / this.mapY;
      if (Math.abs(x) > 1 || Math.abs(y) > 1) return null;
      lon = x * Math.PI;
      lat = y * Math.PI * 0.5;
    } else {
      const px = ndcx * (this.W * 0.5) / this.R;
      const py = ndcy * (this.H * 0.5) / this.R;
      const r2 = px * px + py * py;
      if (r2 > 1) return null;                  // clicked past the limb
      const pz = Math.sqrt(1 - r2);
      lat = Math.asin(py);
      lon = Math.atan2(px, pz);
    }
    lon -= this.rot;                            // undo the spin
    lon = ((lon + Math.PI) % (2 * Math.PI) + 2 * Math.PI) % (2 * Math.PI) - Math.PI;
    return { lon, lat };
  }

  resize() {
    const gl = this.gl;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.W = Math.max(2, Math.round(this.cv.clientWidth * dpr));
    this.H = Math.max(2, Math.round(this.cv.clientHeight * dpr));
    this.cv.width = this.W;
    this.cv.height = this.H;
    this.R = Math.min(this.W, this.H) * 0.44;
    // Map half-height in NDC that preserves the 2:1 plate carree aspect against
    // whatever shape the window happens to be.
    this.mapY = 0.46 * this.W / this.H;
    gl.viewport(0, 0, this.W, this.H);
  }

  start() {
    const gl = this.gl;
    let last = performance.now();
    const frame = (t) => {
      const dt = Math.min(64, t - last); last = t;

      // Ease the projection rather than snapping. The easing constant is framed
      // against dt so the transition takes the same wall-clock time on a 144Hz
      // display as on a 60Hz one.
      if (this.morph !== this.morphTo) {
        const k = 1 - Math.exp(-dt / 190);
        this.morph += (this.morphTo - this.morph) * k;
        if (Math.abs(this.morphTo - this.morph) < 0.001) this.morph = this.morphTo;
      }
      if (this.spin && this.morph < 0.5) this.rot += dt * 0.00007;

      if (this.dirty) {
        gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
        gl.bufferSubData(gl.ARRAY_BUFFER, 0, this.flags);
        this.dirty = false;
      }

      gl.clearColor(0.055, 0.125, 0.220, 1);   // matches --deep
      gl.clear(gl.COLOR_BUFFER_BIT);

      gl.useProgram(this.pBody);
      gl.bindVertexArray(this.vaoBody);
      gl.uniform1f(this.uBody.u_radius, this.R);
      gl.uniform2f(this.uBody.u_res, this.W, this.H);
      gl.uniform1f(this.uBody.u_morph, this.morph);
      gl.uniform1f(this.uBody.u_zoom, this.zoom);
      gl.uniform1f(this.uBody.u_mapy, this.mapY);
      gl.uniform2f(this.uBody.u_pan, this.pan[0], this.pan[1]);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);

      // Between the ground and the population: context under the data.
      if (this.borderCount) {
        gl.useProgram(this.pBorder);
        gl.bindVertexArray(this.vaoBorder);
        gl.uniform1f(this.uBorder.u_rot, this.rot);
        gl.uniform1f(this.uBorder.u_radius, this.R);
        gl.uniform2f(this.uBorder.u_res, this.W, this.H);
        gl.uniform1f(this.uBorder.u_morph, this.morph);
        gl.uniform1f(this.uBorder.u_zoom, this.zoom);
        gl.uniform1f(this.uBorder.u_mapy, this.mapY);
        gl.uniform2f(this.uBorder.u_pan, this.pan[0], this.pan[1]);
        gl.drawElements(gl.LINE_STRIP, this.borderCount, gl.UNSIGNED_SHORT, 0);
      }

      if (this.n) {
        gl.useProgram(this.pPts);
        gl.bindVertexArray(this.vao);
        gl.uniform1f(this.uPts.u_rot, this.rot);
        gl.uniform1f(this.uPts.u_radius, this.R);
        gl.uniform2f(this.uPts.u_res, this.W, this.H);
        gl.uniform1f(this.uPts.u_morph, this.morph);
        gl.uniform1f(this.uPts.u_zoom, this.zoom);
        gl.uniform1f(this.uPts.u_mapy, this.mapY);
        gl.uniform2f(this.uPts.u_pan, this.pan[0], this.pan[1]);
        // Thin the point size as population rises so a dense globe does not
        // saturate to a solid disc.
        gl.uniform1f(this.uPts.u_size,
          this.n > 4e6 ? 1.0 : this.n > 1e6 ? 1.3 : 1.7);
        gl.drawArrays(gl.POINTS, 0, this.n);   // the whole world, one call
      }
      gl.bindVertexArray(null);

      this.fps = this.fps * 0.92 + (1000 / Math.max(dt, 1)) * 0.08;
      this.onStats({ n: this.n, fps: Math.round(this.fps), tick: this.tick || 0 });
      requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
  }
}
