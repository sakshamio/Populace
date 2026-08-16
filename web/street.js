// Canvas2D renderer for the street-level view.
//
// This is not the globe's WebGL path reused at a smaller scale -- it is a
// different kind of problem. The globe draws one dot per persona, up to ten
// million of them, and the only thing that matters is throughput. A street
// roster is a few dozen *named* people with real ties between them; the
// hard part is labels, hover, and lines, none of which the globe's shader
// pipeline does at all. Same reasoning as the group chat being plain DOM
// instead of a third WebGL program: pick the tool for what the view actually
// has to show, not for consistency with the other one.
//
// Positions are each resident's real generated lon/lat, min-max normalised
// to fill the canvas -- the true Gaussian scatter around the place centre,
// not a layout algorithm optimised for readability. Aspect is preserved (one
// scale factor for both axes) so the shape of the neighbourhood is not lying
// to you.

const esc = (s) => String(s).replace(/[<>&]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;' }[c]));

// Same formula as globe.js's fragment shader (against/neutral/favour mix by
// |opinion|, luminance by adoption state, magenta tint for feed-driven
// adopters) so a dot here and a dot on the globe read as the same fact.
function colorFor(res) {
  const against = [0.298, 0.494, 1.000];
  const neutral = [0.400, 0.450, 0.580];
  const favour = [1.000, 0.604, 0.239];
  const t = Math.min(1, Math.abs(res.opinion) * 1.6);
  const base = res.opinion < 0 ? against : favour;
  let c = neutral.map((n, i) => n + (base[i] - n) * t);

  let lum = 0.34;
  if (res.state === 'moved on') lum = 0.66;
  else if (res.state === 'talking about it') lum = 1.45;

  if (res.feed_reached) {
    const feed = [0.761, 0.310, 0.820];
    c = c.map((v, i) => v + (feed[i] - v) * 0.55);
  }
  const [r, g, b] = c.map((v) => Math.max(0, Math.min(255, Math.round(v * lum * 255))));
  return `rgb(${r},${g},${b})`;
}

export class StreetView {
  constructor(canvas, tip) {
    this.cv = canvas;
    this.ctx = canvas.getContext('2d');
    this.tip = tip;
    this.data = null;
    this.pts = [];
    this.hover = -1;
    this.dpr = Math.max(1, window.devicePixelRatio || 1);
    this.onPick = null; // (resident) => void, set by the caller

    new ResizeObserver(() => this.resize()).observe(canvas);
    canvas.addEventListener('mousemove', (e) => this.onMove(e));
    canvas.addEventListener('mouseleave', () => { this.hover = -1; this.tip.hidden = true; this.draw(); });
    canvas.addEventListener('click', (e) => this.onClick(e));
    this.resize();
  }

  resize() {
    const r = this.cv.getBoundingClientRect();
    this.cv.width = Math.max(1, Math.round(r.width * this.dpr));
    this.cv.height = Math.max(1, Math.round(r.height * this.dpr));
    this.layout();
    this.draw();
  }

  setData(data) {
    this.data = data;
    this.hover = -1;
    this.tip.hidden = true;
    this.layout();
    this.draw();
  }

  clear() {
    this.data = null;
    this.pts = [];
    this.hover = -1;
    this.tip.hidden = true;
    this.draw();
  }

  layout() {
    this.pts = [];
    if (!this.data || !this.data.residents.length) return;
    const res = this.data.residents;
    let minLon = Infinity, maxLon = -Infinity, minLat = Infinity, maxLat = -Infinity;
    for (const p of res) {
      minLon = Math.min(minLon, p.lon); maxLon = Math.max(maxLon, p.lon);
      minLat = Math.min(minLat, p.lat); maxLat = Math.max(maxLat, p.lat);
    }
    const spanLon = Math.max(maxLon - minLon, 1e-6);
    const spanLat = Math.max(maxLat - minLat, 1e-6);
    const w = this.cv.width, h = this.cv.height;
    const pad = Math.min(w, h) * 0.14;
    const s = Math.min((w - pad * 2) / spanLon, (h - pad * 2) / spanLat);
    const midLon = (minLon + maxLon) / 2, midLat = (minLat + maxLat) / 2;
    const cx = w / 2, cy = h / 2;
    // Screen y grows downward; latitude grows northward -- flip it.
    this.pts = res.map((p) => ({
      x: cx + (p.lon - midLon) * s,
      y: cy - (p.lat - midLat) * s,
    }));
  }

  draw() {
    const ctx = this.ctx, w = this.cv.width, h = this.cv.height;
    ctx.clearRect(0, 0, w, h);
    if (!this.data || !this.pts.length) return;
    const res = this.data.residents;

    // Real graph ties among roster members, drawn underneath the dots. Ties
    // that leave the roster entirely (res[i].outside) are not drawn as lines
    // shooting off-canvas -- they show as a count in the hover card instead,
    // since most of them are homophily or hub ties that can span continents
    // and would just read as noise here.
    ctx.lineWidth = Math.max(1, this.dpr);
    ctx.strokeStyle = 'rgba(255,255,255,0.16)';
    ctx.beginPath();
    for (let i = 0; i < res.length; i++) {
      for (const j of res[i].ties) {
        if (j > i) {
          ctx.moveTo(this.pts[i].x, this.pts[i].y);
          ctx.lineTo(this.pts[j].x, this.pts[j].y);
        }
      }
    }
    ctx.stroke();

    const r0 = 4.5 * this.dpr;
    for (let i = 0; i < res.length; i++) {
      const p = this.pts[i], hot = i === this.hover;
      ctx.beginPath();
      ctx.arc(p.x, p.y, hot ? r0 * 1.7 : r0, 0, Math.PI * 2);
      ctx.fillStyle = colorFor(res[i]);
      ctx.fill();
      if (hot) {
        ctx.lineWidth = 2 * this.dpr;
        ctx.strokeStyle = '#fff';
        ctx.stroke();
      }
    }
  }

  nearest(clientX, clientY) {
    const r = this.cv.getBoundingClientRect();
    const x = (clientX - r.left) * this.dpr, y = (clientY - r.top) * this.dpr;
    const pick = 16 * this.dpr;
    let best = -1, bestD = pick * pick;
    for (let i = 0; i < this.pts.length; i++) {
      const dx = this.pts[i].x - x, dy = this.pts[i].y - y, d = dx * dx + dy * dy;
      if (d < bestD) { bestD = d; best = i; }
    }
    return best;
  }

  onMove(e) {
    const i = this.nearest(e.clientX, e.clientY);
    if (i === this.hover) return;
    this.hover = i;
    this.draw();
    if (i < 0) { this.tip.hidden = true; return; }
    const res = this.data.residents[i];
    this.tip.hidden = false;
    const bits = [esc(res.state)];
    if (res.ties.length) bits.push(`${res.ties.length} tied here`);
    if (res.local_other) bits.push(`${res.local_other} more nearby, not shown`);
    if (res.outside) bits.push(`${res.outside} elsewhere`);
    this.tip.innerHTML = `<b>${esc(res.name)}</b><span>${esc(res.role)}</span><i>${bits.join(' &middot; ')}</i>`;
    const r = this.cv.getBoundingClientRect();
    const px = r.left + this.pts[i].x / this.dpr, py = r.top + this.pts[i].y / this.dpr;
    const tipW = this.tip.offsetWidth || 160;
    this.tip.style.left = Math.min(px + 14, r.right - tipW - 10) + 'px';
    this.tip.style.top = Math.max(py - 10, r.top + 6) + 'px';
  }

  onClick(e) {
    const i = this.nearest(e.clientX, e.clientY);
    if (i < 0 || !this.onPick) return;
    this.onPick(this.data.residents[i]);
  }
}
