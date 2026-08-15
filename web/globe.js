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

const VS = `#version 300 es
layout(location=0) in vec2 a_ll;      // lon, lat in radians
layout(location=1) in float a_state;  // unpacked from flags
uniform float u_rot, u_radius;
uniform vec2  u_res;
uniform float u_size;
out float v_state, v_face;
void main(){
  float lon = a_ll.x + u_rot;
  float lat = a_ll.y;
  float cl  = cos(lat);
  vec3  p   = vec3(cl*sin(lon), sin(lat), cl*cos(lon));
  v_face  = p.z;                       // >0 faces the viewer
  v_state = a_state;
  gl_Position  = vec4(p.xy * u_radius / (u_res*0.5), 0.0, 1.0);
  gl_PointSize = u_size;
}`;

const FS = `#version 300 es
precision highp float;
in float v_state, v_face;
out vec4 o;
void main(){
  if (v_face <= 0.02) discard;         // backface cull: one dot product
  vec3 c = vec3(0.29,0.33,0.41);
  if      (v_state > 1.5) c = vec3(0.851,0.467,0.235);
  else if (v_state > 0.5) c = vec3(0.231,0.365,0.788);
  o = vec4(c * (0.42 + 0.58*v_face), 1.0);
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
    this.cascade = null;
    this.aware = 0;
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

    this.state = new Uint8Array(n);
    this.bState = this.bState || gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
    gl.bufferData(gl.ARRAY_BUFFER, this.state, gl.DYNAMIC_DRAW);

    // Susceptibility stands in for the archetype's disposition until the
    // dynamics engine exists; derived from the packed arch field so it is
    // stable per persona rather than random per load.
    this.suscept = new Float32Array(n);
    this.lonlat = new Float32Array(n * 2);
    const src = new DataView(body);
    for (let i = 0; i < n; i++) {
      const o = i * STRIDE;
      this.lonlat[i * 2] = src.getFloat32(o, true);
      this.lonlat[i * 2 + 1] = src.getFloat32(o + 4, true);
      this.suscept[i] = 0.25 + (src.getUint16(o + 12, true) % 400) / 400 * 0.7;
    }

    this.vao = gl.createVertexArray();
    gl.bindVertexArray(this.vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bInst);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, STRIDE, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 1, gl.UNSIGNED_BYTE, false, 0, 0);
    gl.bindVertexArray(null);
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

  breakStory() {
    if (!this.n) return;
    const i = (Math.random() * this.n) | 0;
    this.cascade = { lon: this.lonlat[i * 2], lat: this.lonlat[i * 2 + 1], r: 0 };
  }

  // One cascade step. Complex contagion -- requiring several independent
  // exposures rather than one -- belongs here once the dynamics engine lands;
  // this single-exposure front is a placeholder and will overstate reach.
  stepCascade() {
    const c = this.cascade;
    if (!c) return;
    c.r += 0.022;
    const cl = Math.cos(c.lat);
    const ex = cl * Math.sin(c.lon), ey = Math.sin(c.lat), ez = cl * Math.cos(c.lon);
    const cosR = Math.cos(c.r);
    let n = 0;
    for (let i = 0; i < this.n; i++) {
      if (this.state[i] !== 0) { n++; continue; }
      const lo = this.lonlat[i * 2], la = this.lonlat[i * 2 + 1], c2 = Math.cos(la);
      const d = c2 * Math.sin(lo) * ex + Math.sin(la) * ey + c2 * Math.cos(lo) * ez;
      if (d > cosR && Math.random() < this.suscept[i] * 0.55) {
        this.state[i] = this.suscept[i] > 0.62 ? 2 : 1;
        n++;
      }
    }
    this.aware = n;
    this.dirty = true;
    if (c.r > Math.PI) this.cascade = null;
  }

  start() {
    const gl = this.gl;
    let last = performance.now(), acc = 0;
    const frame = (t) => {
      const dt = Math.min(64, t - last); last = t;
      if (!this.reduce) this.rot += dt * 0.00007;
      acc += dt;
      if (acc > 90) { this.stepCascade(); acc = 0; }

      if (this.dirty) {
        gl.bindBuffer(gl.ARRAY_BUFFER, this.bState);
        gl.bufferSubData(gl.ARRAY_BUFFER, 0, this.state);
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
      this.onStats({ n: this.n, fps: Math.round(this.fps), aware: this.aware });
      requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
  }
}
