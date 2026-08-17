// One-time offline conversion: the public-domain Natural Earth 110m country
// boundaries (distributed as TopoJSON by the world-atlas npm package) into
// web/borders.bin, a compact binary of border rings in the same lon/lat
// radians convention globe.js already uses for persona positions.
//
// Not part of the running app -- run this by hand only when the borders
// dataset itself needs to change (a different resolution, say). Needs
// `npm install topojson-client` in this directory first.
//
//   node tools/borders/decode.js
//
// See the comment on BORDER_VS in web/globe.js for why the file is rings of
// raw lon/lat rather than anything WebGL-specific: the index buffer (with
// primitive-restart sentinels between rings) and the per-vertex ring anchor
// both get built at load time in JS, not baked in here.
const fs = require('fs');
const path = require('path');
const topojson = require('topojson-client');

const SOURCE_URL = 'https://cdn.jsdelivr.net/npm/world-atlas@2/countries-110m.json';
const OUT_PATH = path.join(__dirname, '..', '..', 'web', 'borders.bin');

async function main() {
  const res = await fetch(SOURCE_URL);
  if (!res.ok) throw new Error(`fetching ${SOURCE_URL}: HTTP ${res.status}`);
  const topology = await res.json();
  const fc = topojson.feature(topology, topology.objects.countries);

  const D2R = Math.PI / 180;
  const rings = []; // each a Float32Array of [lon,lat,lon,lat,...] in radians

  function addRing(coords) {
    // coords: array of [lon_deg, lat_deg]. Drop the duplicated closing vertex
    // GeoJSON rings carry (first === last) -- primitive restart replaces
    // LINE_LOOP's implicit close, so keeping it would just draw one
    // zero-length segment at the seam.
    const n = coords.length - 1;
    if (n < 2) return;
    const pts = new Float32Array(n * 2);
    for (let i = 0; i < n; i++) {
      pts[i * 2] = coords[i][0] * D2R;
      pts[i * 2 + 1] = coords[i][1] * D2R;
    }
    rings.push(pts);
  }

  let countries = 0;
  for (const f of fc.features) {
    countries++;
    const g = f.geometry;
    if (!g) continue;
    if (g.type === 'Polygon') {
      for (const ring of g.coordinates) addRing(ring);
    } else if (g.type === 'MultiPolygon') {
      for (const poly of g.coordinates) for (const ring of poly) addRing(ring);
    }
  }
  const totalVerts = rings.reduce((n, r) => n + r.length / 2, 0);
  console.log(`countries: ${countries}, rings: ${rings.length}, vertices: ${totalVerts}`);
  if (totalVerts >= 0xFFFF) {
    // globe.js's index buffer uses a 16-bit primitive-restart sentinel.
    throw new Error(`${totalVerts} vertices exceeds the 16-bit index limit globe.js assumes`);
  }

  // Binary layout: "PBD1" magic, u32 numRings, then per ring: u32 numVerts,
  // numVerts * (f32 lon, f32 lat).
  let byteLen = 8;
  for (const r of rings) byteLen += 4 + r.byteLength;
  const buf = Buffer.alloc(byteLen);
  let off = 0;
  buf.write('PBD1', off, 'ascii'); off += 4;
  buf.writeUInt32LE(rings.length, off); off += 4;
  for (const r of rings) {
    buf.writeUInt32LE(r.length / 2, off); off += 4;
    Buffer.from(r.buffer, r.byteOffset, r.byteLength).copy(buf, off);
    off += r.byteLength;
  }
  fs.writeFileSync(OUT_PATH, buf);
  console.log(`wrote ${OUT_PATH}: ${(buf.length / 1024).toFixed(1)} KB`);
}

main().catch((e) => { console.error(e); process.exit(1); });
