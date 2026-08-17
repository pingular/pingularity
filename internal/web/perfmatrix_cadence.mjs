// Extract the SHIPPED latency-chart poll cadence out of an index.html and print
// the delay sequence one open dashboard would use over a horizon, per scenario.
//
// Nothing here restates a number from the page. latPollMs is lifted from the
// <script> with balanced-brace extraction (the same technique ui.test.mjs uses)
// together with whatever it calls, so this reports what the page does and not
// what someone believed it does. That makes it work unchanged on a6f5778, where
// latPollMs is a flat ladder with no helpers, and on the current tree, where it
// delegates to latPollDelay.
//
// Usage: node internal/web/perfmatrix_cadence.mjs <index.html> [horizonMinutes]
// Output: JSON { file, scenarios: { name: { delaysMs, pollsPerHorizon, ... } } }

import { readFileSync } from 'node:fs';

const file = process.argv[2];
const horizonMin = Number(process.argv[3] || 10);
const html = readFileSync(file, 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

// Balanced-brace extraction, skipping strings, template literals and line
// comments so their contents cannot throw off the brace count.
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

// A brace-free `const NAME = ...;` one-liner, which extract cannot lift.
function extractConst(name) {
  const m = script.match(new RegExp('^const ' + name + '\\s*=[^;]*;', 'm'));
  return m ? m[0] : null;
}

const parts = [];
for (const c of ['LAT_MAX_WIN_SEC', 'LAT_POLL_RUNGS']) {
  const s = extractConst(c);
  if (s) parts.push(s);
}
for (const f of ['function latSpanMins', 'function latPollForMins', 'function latPollDelay', 'function latPollMs']) {
  const s = extract(f);
  if (s) parts.push(s);
}
if (!parts.some(p => p.startsWith('function latPollMs'))) throw new Error('latPollMs not found in ' + file);

// latBackoffMs and latencyRange are page globals; pass them in as parameters so
// nothing outside the extracted source is evaluated. Date is shadowed so the
// clock is ours: the current tree's latPollMs calls Date.now().
const call = new Function('latencyRange', 'latBackoffMs', 'Date',
  parts.join('\n') + '\nreturn latPollMs();');

const now0 = Date.now();
// The ranges latencyRange actually holds: a preset is {kind:"win",mins}, and an
// absolute live range is {kind:"range",from,to} with to=0 for an open end (see
// latWindowQuery).
const scenarios = {
  '5m': { kind: 'win', mins: 5 },
  '1h': { kind: 'win', mins: 60 },
  '6h': { kind: 'win', mins: 360 },
  '1d': { kind: 'win', mins: 1440 },
  '7d': { kind: 'win', mins: 10080 },
  '30d': { kind: 'range', from: Math.floor(now0 / 1000) - 30 * 24 * 3600, to: 0 },
};

const out = { file, horizonMin, scenarios: {} };
for (const [name, r] of Object.entries(scenarios)) {
  // The page fires refreshChart once from the boot tick() and only THEN arms
  // loopChart, so t=0 is a poll and the first delay is measured from there.
  const delays = [];
  let t = 0, polls = 1;
  const horizon = horizonMin * 60000;
  while (true) {
    const fakeDate = { now: () => now0 + t };
    const d = call(r, 0, fakeDate);
    delays.push(d);
    t += d;
    if (t > horizon) break;
    polls++;
  }
  // Collapse to the distinct delays actually seen; the Go replay cycles the list.
  const uniq = [...new Set(delays)];
  out.scenarios[name] = {
    delaysMs: uniq.length === 1 ? uniq : delays,
    distinct: uniq,
    pollsPerHorizon: polls,
    pollsPerMin: Number((polls / horizonMin).toFixed(3)),
  };
}
console.log(JSON.stringify(out, null, 2));
