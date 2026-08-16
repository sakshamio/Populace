// WebGL2 globe. One draw call for the entire population.
//
// The instance buffer arrives from Go as packed binary and goes straight into
// a GPU buffer -- no JSON, no parse, no garbage. Positions are uploaded once
// and never move again; the only buffer that changes per tick is one byte of
// state per persona.
//
// Layout must match internal/world.InstanceBytes exactly:
//   f32 lon | f32 lat | u16 palette | u16 shape | u16 arch | u16 flags   = 16B

const STRIDE = 16;

// The flags word is unpacked on the GPU, not in JS. It arrives from Go in the
// layout internal/sim.packFlags writes:
//   bits 0-1 adoption state, bits 2-7 opinion (64 levels), bit 8 broadcaster.
// Unpacking ten million of these per tick in JavaScript would cost more than
// the simulation tick that produced them; here it is three integer ops in a
// shader that was going to run anyway.
const VS = `#version 300 es
layout(location=0) in vec2 a_ll;      // lon, lat in radians
layout(location=1) in uint a_flags;   // packed state, see internal/sim
uniform float u_rot, u_radius;
uniform vec2  u_res;
uniform float u_size;
out float v_state, v_op, v_media, v_face;
void main(){
  float lon = a_ll.x + u_rot;
  float lat = a_ll.y;
  float cl  = cos(lat);
  vec3  p   = vec3(cl*sin(lon), sin(lat), cl*cos(lon));
  v_face  = p.z;                       // >0 faces the viewer
  v_state = float(a_flags & 3u);
  v_op    = float((a_flags >> 2) & 63u) / 63.0 * 2.0 - 1.0;
  v_media = ((a_flags & 256u) != 0u) ? 1.0 : 0.0;
  gl_Position  = vec4(p.xy * u_radius / (u_res*0.5), 0.0, 1.0);
  gl_PointSize = u_size + v_media * 1.6;
}`;

// Two channels in one dot, which is the whole reason opinion and adoption are
// separate fields rather than one "activated" bit: hue carries which way a
// person leans, brightness carries whether the story has reached them. A
// population can be uniformly aware and completely split, and that is exactly
// the state worth being able to see.
const FS = `#version 300 es
precision highp float;
in float v_state, v_op, v_media, v_face;
out vec4 o;
void main(){
  if (v_face <= 0.02) discard;         // backface cull: one dot product
  vec3 against = vec3(0.298,0.451,0.855);
  vec3 neutral = vec3(0.361,0.388,0.451);
  vec3 favour  = vec3(0.898,0.541,0.243);
  float t = clamp(abs(v_op) * 1.6, 0.0, 1.0);
  vec3 c = mix(neutral, v_op < 0.0 ? against : favour, t);

  // Unaware people stay dim; carriers burn; the fatigued keep the colour they
  // ended up with but stop drawing the eye.
  float lum = 0.30;
  if      (v_state > 1.5) lum = 0.62;  // fatigued: adopted, no longer spreading
  else if (v_state > 0.5) lum = 1.35;  // actively transmitting
  if (v_media > 0.5) lum *= 1.4;

  o = vec4(c * lum * (0.42 + 0.58*v_face), 1.0);
}`;

const BODY_VS = `#version 300 es
layout(location=0) in vec2 a_p;
uniform float u_radius; uniform vec2 u_res;
out vec2 v_p;
void main(){ v_p=a_p; gl_Position=vec4(a_p*u_radius/(u_res*0.5),0.0,1.0); }`;

const BODY_FS = `#version 300 es
precision highp float;
in vec2 v_p; out vec4 o;
void main(){
  float d = length(v_p);
  if (d > 1.0) discard;
  float z = sqrt(max(0.0, 1.0 - d*d));
  float lam = clamp(dot(normalize(vec3(v_p,z)), normalize(vec3(-0.35,0.25,0.9))), 0.0, 1.0);
  o = vec4(mix(vec3(0.055,0.07,0.10), vec3(0.10,0.13,0.19), lam), 1.0);
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

export class Globe {
  constructor(canvas, onStats) {
    this.cv = canvas;
    this.onStats = onStats || (() => {});
    const gl = canvas.getContext('webgl2', { antialias: true, alpha: false });
    if (!gl) throw new Error('WebGL2 is required and is not available here.');
    this.gl = gl;

    this.pPts = link(gl, VS, FS);
    this.pBody = link(gl, BODY_VS, BODY_FS);

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

    this._resize = this.resize.bind(this);
    window.addEventListener('resize', this._resize);
    this.resize();
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

  // Turns a click into a point on the sphere, or null if it missed.
  //
  // This inverts the vertex shader exactly rather than approximating it. The
  // shader maps a unit-sphere point p to NDC as p.xy * R / (res/2), so the
  // inverse is p.xy = ndc * (res/2) / R, and z falls out of the unit-length
  // constraint -- taking the positive root because the far hemisphere is
  // discarded by the same backface test the fragment shader uses.
  //
  // Keeping this in lockstep with the shader matters: an approximate
  // unprojection would return a person a few degrees from the one under the
  // cursor, which reads as the inspector being wrong about who lives where.
  pick(clientX, clientY) {
    const rect = this.cv.getBoundingClientRect();
    const ndcx = ((clientX - rect.left) / rect.width) * 2 - 1;
    const ndcy = 1 - ((clientY - rect.top) / rect.height) * 2;

    const px = ndcx * (this.W * 0.5) / this.R;
    const py = ndcy * (this.H * 0.5) / this.R;
    const r2 = px * px + py * py;
    if (r2 > 1) return null;                  // clicked past the limb
    const pz = Math.sqrt(1 - r2);

    const lat = Math.asin(py);
    let lon = Math.atan2(px, pz) - this.rot;  // undo the spin
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
    gl.viewport(0, 0, this.W, this.H);
  }

  start() {
    const gl = this.gl;
    let last = performance.now();
    const frame = (t) => {
      const dt = Math.min(64, t - last); last = t;
      if (!this.reduce) this.rot += dt * 0.00007;
      if (this.dirty) {
        gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
        gl.bufferSubData(gl.ARRAY_BUFFER, 0, this.flags);
        this.dirty = false;
      }

      gl.clearColor(0.043, 0.055, 0.078, 1);
      gl.clear(gl.COLOR_BUFFER_BIT);

      gl.useProgram(this.pBody);
      gl.bindVertexArray(this.vaoBody);
      gl.uniform1f(gl.getUniformLocation(this.pBody, 'u_radius'), this.R);
      gl.uniform2f(gl.getUniformLocation(this.pBody, 'u_res'), this.W, this.H);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);

      if (this.n) {
        gl.useProgram(this.pPts);
        gl.bindVertexArray(this.vao);
        gl.uniform1f(gl.getUniformLocation(this.pPts, 'u_rot'), this.rot);
        gl.uniform1f(gl.getUniformLocation(this.pPts, 'u_radius'), this.R);
        gl.uniform2f(gl.getUniformLocation(this.pPts, 'u_res'), this.W, this.H);
        // Thin the point size as population rises so a dense globe does not
        // saturate to a solid disc.
        gl.uniform1f(gl.getUniformLocation(this.pPts, 'u_size'),
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
