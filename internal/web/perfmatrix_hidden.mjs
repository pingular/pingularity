// How many /api/series requests one open dashboard issues per minute while the
// LATENCY TILE IS HIDDEN, measured by running the shipped refreshChart.
//
// Same technique as perfmatrix_cadence.mjs: the function under measurement is lifted out of
// the page, so this reports what the page does. It works unchanged on a6f5778,
// where refreshChart has no gate at all and sectionHidden/tileIdle do not exist.
//
// Usage: node internal/web/perfmatrix_hidden.mjs <index.html> <pollsPerMin> [minutes]

import { readFileSync } from 'node:fs';

const file = process.argv[2];
const pollsPerMin = Number(process.argv[3]);
const minutes = Number(process.argv[4] || 10);
const html = readFileSync(file, 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

function extract(startStr) {
  const i = script.indexOf(startStr);
  if (i < 0) return null;
  let k = script.indexOf('{', i), depth = 0;
  const n = script.length;
  const skipQuote = (p, q) => { p++; while (p < n) { if (script[p] === '\\') { p += 2; continue; } if (script[p] === q) return p + 1; p++; } return p; };
  while (k < n) {
    const c = script[k];
    if (c === "'" || c === '"' || c === '`') { k = skipQuote(k, c); continue; }
    if (c === '/' && script[k + 1] === '/') { const e = script.indexOf('\n', k); k = e < 0 ? n : e; continue; }
    if (c === '{') depth++;
    else if (c === '}' && --depth === 0) { k++; break; }
    k++;
  }
  if (script[k] === ';') k++;
  return script.slice(i, k);
}

const gateSrc = [extract('function sectionHidden'), extract('function tileIdle')].filter(Boolean).join('\n');
const chart = extract('async function refreshChart');
if (!chart) throw new Error('refreshChart not found in ' + file);

// A document whose only .panel is the latency tile, hidden or not.
const gateDoc = hidden => ({
  querySelector: sel => sel === '.panel[data-section="latency"]'
    ? { classList: { contains: c => c === 'section-hidden' && hidden } } : null,
});

// Build the shipped refreshChart over stubs, counting fetches through `count`.
function build(hidden, count) {
  const make = new Function('latWindowQuery', 'fget', 'isReconcile503', 'retryAfterMs', 'chartLoadFailed',
    'drawChart', 'syncLatPanel', 'document',
    gateSrc + '\nlet chartSeq=0, latLoadedFor="", latLoadedLive=false, latPoints=[], latBackoffMs=0;\n'
    + chart + '\nreturn refreshChart;');
  return make(
    // A LIVE window, the case the poll loop exists for: a pinned span stops
    // fetching after one load whether or not the tile is on screen.
    () => ({ q: 'mins=1440', live: true }),
    async () => { count.n++; return { ok: true, json: async () => [{ t: 1 }] }; },
    () => false, () => 0, () => {}, () => {}, () => {},
    gateDoc(hidden),
  );
}

const polls = Math.round(pollsPerMin * minutes);
const out = { file, minutes, pollsPerMin, polls, gated: gateSrc !== '' };
for (const hidden of [false, true]) {
  const count = { n: 0 };
  const refreshChart = build(hidden, count);
  for (let i = 0; i < polls; i++) await refreshChart();
  out[hidden ? 'hidden' : 'visible'] = { requests: count.n, requestsPerMin: Number((count.n / minutes).toFixed(3)) };
}
console.log(JSON.stringify(out, null, 2));
