// Dependency-free unit tests for the dashboard's pure helper functions.
// The functions are EXTRACTED from the live index.html <script> (no copy/drift)
// and evaluated with minimal stubs, so these tests fail if the real source
// changes behavior.  Run:  node --test internal/web/ui/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
// The whole page, for the few assertions that are about markup or CSS rather than
// about the script; `script` is what everything else works from.
const html = readFileSync(join(here, 'index.html'), 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

// Extract a `function NAME` / `const NAME =` definition with balanced braces,
// skipping strings, template literals and line comments so their contents can't
// throw off the brace count.
function extract(startStr) {
  const i = script.indexOf(startStr);
  if (i < 0) throw new Error('definition not found: ' + startStr);
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

// Build a sandbox: stub the one browser API toHex needs (a canvas 2d context
// whose fillStyle echoes what's assigned - faithful for the hex/rgb inputs toHex
// actually receives), plus a mutable TC object for hmShade.
const DEFS = {
  withAlpha: 'function withAlpha', _rgb: 'function _rgb', relLum: 'function relLum',
  toHex: 'function toHex', onAccent: 'function onAccent', parseDur: 'function parseDur',
  fmtWin: 'function fmtWin', minToTime: 'function minToTime', timeToMin: 'function timeToMin',
  niceStep: 'function niceStep', hmShade: 'function hmShade',
  fmtDur: 'const fmtDur', bytesStr: 'const bytesStr',
  covGrid: 'function covGrid', covStats: 'function covStats',
  parseRange: 'function parseRange', fmtRangeEcho: 'function fmtRangeEcho',
  rangeLoad: 'function rangeLoad', speedTileMbps: 'function speedTileMbps',
  mbpsText: 'function mbpsText', pingText: 'function pingText',
  spMeasured: 'function spMeasured', spdAverages: 'function spdAverages',
  speedtestBusy: 'function speedtestBusy',
  speedtestAbortable: 'function speedtestAbortable',
  autoOptionText: 'const autoOptionText', autoScopeText: 'const autoScopeText',
  bloatBins: 'function bloatBins', bloatCeiling: 'function bloatCeiling',
  bloatDataMax: 'function bloatDataMax', cursorSpan: 'const cursorSpan',
  drawCursor: 'function drawCursor',
  logPostAction: 'function logPostAction', spdAvgNote: 'function spdAvgNote',
  pingAvgText: 'function pingAvgText',
  setSpdAvg: 'function setSpdAvg', upCovNote: 'function upCovNote',
  upCovFoot: 'function upCovFoot', logTruncNote: 'function logTruncNote',
  outageDeletable: 'function outageDeletable',
  isReconcile503: 'function isReconcile503', retryAfterMs: 'function retryAfterMs',
};
const NAMES = Object.keys(DEFS);
const defs = NAMES.map(n => extract(DEFS[n])).join('\n');
// DAYN is a plain array const (no braces), which extract() can't lift - pass it in.
// isNum is the same shape (a one-line arrow const) and spMeasured/spdAverages call
// it, so it is injected the same way rather than copied as a literal here.
// `$` is injected so setSpdAvg can be driven against a fake element: the pill's
// visible text and its accessible name are set on different lines, and whether the
// second one is reached is exactly what is under test.
const factory = new Function('document', 'TC', 'DAYN', 'isNum', '$', 'esc', 'AX_PAD', defs + '\nreturn {' + NAMES.join(',') + '};');
const TC = { hm0: '#000000', down: '#ffffff' };
const fakeCtx = { _v: '#000000', set fillStyle(x) { this._v = x; }, get fillStyle() { return this._v; } };
const fakeDoc = { createElement: () => ({ getContext: () => fakeCtx }) };
let fakePill = null;
// The pill holds a static SVG icon and the number in separate elements, so the
// only thing setSpdAvg writes is the number - the icon is markup, not text.
// `textContent` therefore reports the value alone.
const fakeEl = () => ({
  val: { textContent: '' },
  attrs: {},
  classList: { toggle() {} },
  querySelector() { return this.val; },
  get textContent() { return this.val.textContent; },
  setAttribute(k, v) { this.attrs[k] = v; },
  getAttribute(k) { return this.attrs[k]; },
});
// AX_PAD is a brace-less const built from another one, so it is evaluated out of
// the page rather than copied here - a literal would silently drift.
const AX_PAD_VAL = new Function(script.match(/const AX_GAP = [^;]*;/)[0] + '\n'
  + script.match(/const AX_PAD = [^;]*;/)[0] + '\nreturn AX_PAD;')();
const F = factory(fakeDoc, TC, ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'], v => typeof v === 'number',
  () => fakePill,
  // esc is a one-line arrow in the page, so it is injected rather than extracted;
  // the panel strings below interpolate daemon-supplied place names through it.
  s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
  AX_PAD_VAL);

// Everything below picks single definitions out of the script and compiles those,
// so a syntax error anywhere in the other ~6800 lines passes every test here AND
// the Go build (index.html is embedded, never parsed) - and then blanks the whole
// dashboard, because one bad token stops the browser loading the entire file.
// So compile the WHOLE script once, without running it: no DOM or window stubs are
// needed since nothing executes. vm.Script rather than new Function because it
// compiles with the browser's parse goal - a classic top-level script - and so
// rejects things a function body would happily accept (a stray top-level return).
test('the whole inline script parses', () => {
  new vm.Script(script, { filename: 'index.html <script>' });
});

test('withAlpha: hex -> rgba at the given alpha', () => {
  assert.equal(F.withAlpha('#5b9dff', 0.16), 'rgba(91,157,255,0.16)');
  assert.equal(F.withAlpha('#000', 0.5), 'rgba(0,0,0,0.5)'); // 3-char expands
  assert.equal(F.withAlpha('ffffff', 1), 'rgba(255,255,255,1)'); // tolerates no '#'
});

test('_rgb: parses hex (3 & 6 char), bad -> 0,0,0', () => {
  assert.deepEqual(F._rgb('#ff8800'), [255, 136, 0]);
  assert.deepEqual(F._rgb('f80'), [255, 136, 0]);
  assert.deepEqual(F._rgb(''), [0, 0, 0]);
  assert.deepEqual(F._rgb(null), [0, 0, 0]);
});

test('toHex: passes hex through, converts rgb/rgba (drops alpha)', () => {
  assert.equal(F.toHex('#abcdef'), '#abcdef');
  assert.equal(F.toHex('rgba(255, 0, 0, 0.5)'), '#ff0000');
  assert.equal(F.toHex('rgb(0, 128, 255)'), '#0080ff');
});

test('relLum: black 0, white 1, monotonic', () => {
  assert.equal(F.relLum(0, 0, 0).toFixed(4), '0.0000');
  assert.equal(F.relLum(255, 255, 255).toFixed(4), '1.0000');
  assert.ok(F.relLum(200, 200, 200) > F.relLum(50, 50, 50));
});

test('onAccent: picks the higher-contrast text for every accent', () => {
  // saturated mid-tones must NOT flip to the unreadable choice
  assert.equal(F.onAccent('#00ff00'), '#06101f'); // green -> dark
  assert.equal(F.onAccent('#ff0000'), '#06101f'); // red -> dark
  assert.equal(F.onAccent('#5b9dff'), '#06101f'); // default blue -> dark
  assert.equal(F.onAccent('#000000'), '#f4f4f5'); // black -> light
  assert.equal(F.onAccent('#0a1450'), '#f4f4f5'); // navy -> light
  assert.equal(F.onAccent('#ffffff'), '#06101f'); // white -> dark
});

test('parseDur: unit math + rejects garbage', () => {
  assert.equal(F.parseDur('45m'), 45);
  assert.equal(F.parseDur('12h'), 720);
  assert.equal(F.parseDur('3d'), 4320);
  assert.equal(F.parseDur('1w'), 10080);
  assert.equal(F.parseDur('2mo'), 86400);
  assert.equal(F.parseDur('1.5h'), 90);
  assert.equal(F.parseDur('garbage'), 0);
  assert.equal(F.parseDur(''), 0);
  assert.equal(F.parseDur('5x'), 0); // unknown unit
});

test('minToTime / timeToMin: round-trip + wrap', () => {
  assert.equal(F.minToTime(0), '00:00');
  assert.equal(F.minToTime(90), '01:30');
  assert.equal(F.minToTime(1440), '00:00'); // wraps a full day
  assert.equal(F.minToTime(-30), '23:30');  // negative wraps
  assert.equal(F.timeToMin('01:30'), 90);
  assert.equal(F.timeToMin(''), 0);
  assert.equal(F.timeToMin('00:00'), 0);
  for (const m of [0, 75, 615, 1439]) assert.equal(F.timeToMin(F.minToTime(m)), m);
});

test('niceStep: rounds up to a 1/2/5 * 10^n step', () => {
  assert.equal(F.niceStep(0), 1);
  assert.equal(F.niceStep(0.8), 1);
  assert.equal(F.niceStep(3), 5);
  assert.equal(F.niceStep(7), 10);
  assert.equal(F.niceStep(45), 50);
  assert.equal(F.niceStep(150), 200);
});

test('hmShade: interpolates hm0 -> down by weight', () => {
  assert.equal(F.hmShade(0), TC.hm0);           // no downtime = base
  assert.equal(F.hmShade(-1), TC.hm0);          // clamp
  assert.equal(F.hmShade(1), 'rgb(255,255,255)'); // full downtime = down
  assert.equal(F.hmShade(0.5), 'rgb(128,128,128)'); // midpoint
  // honors a custom Downtime colour (the new picker mutates TC.down)
  TC.down = '#ff0000';
  assert.equal(F.hmShade(1), 'rgb(255,0,0)');
  TC.down = '#ffffff';
});

test('fmtDur: two largest units, cascading up to years', () => {
  assert.equal(F.fmtDur(0), '0s');
  assert.equal(F.fmtDur(45), '45s');
  assert.equal(F.fmtDur(65), '1m 5s');
  assert.equal(F.fmtDur(3661), '1h 1m');
  assert.equal(F.fmtDur(-5), '0s'); // clamps negative
  assert.equal(F.fmtDur(40 * 3600 + 10 * 60), '1d 16h'); // 40h10m rolls up to days
  assert.equal(F.fmtDur(20 * 86400 + 10 * 3600), '20d 10h');
  assert.equal(F.fmtDur(2 * 2592000 + 5 * 86400), '2mo 5d'); // months are ~30d
  assert.equal(F.fmtDur(31536000 + 3 * 2592000), '1y 3mo'); // years are ~365d
});

test('bytesStr: SI units, rounds B/KB, 1dp from MB, rejects non-finite', () => {
  assert.equal(F.bytesStr(0), '0 B');
  assert.equal(F.bytesStr(500), '500 B');
  assert.equal(F.bytesStr(1500), '2 KB');         // KB rounds
  assert.equal(F.bytesStr(1500000), '1.5 MB');    // MB+ gets 1dp
  assert.equal(F.bytesStr(2500000000), '2.5 GB');
  assert.equal(F.bytesStr(999999999), '1.0 GB');  // rolls over instead of "1000.0 MB"
  assert.equal(F.bytesStr(999950), '1.0 MB');     // "1000 KB" boundary rolls over too
  assert.equal(F.bytesStr(999449), '999 KB');     // below the round-up threshold stays put
  assert.equal(F.bytesStr(Infinity), '-');
  assert.equal(F.bytesStr(NaN), '-');
  assert.equal(F.bytesStr('x'), '-');
});

// --- engOf: backend normalizer (used by the runs table + chart series) ---
// engOf is a one-line arrow (no braces), so grab it by regex.
const engFactory = new Function(script.match(/const engOf = [^;]*;/)[0] + '\nreturn { engOf };');
const E = engFactory();

test('engOf: legacy/empty engine reads as ookla', () => {
  assert.equal(E.engOf({}), 'ookla');
  assert.equal(E.engOf({ engine: '' }), 'ookla');
  assert.equal(E.engOf({ engine: 'ookla' }), 'ookla');
  assert.equal(E.engOf({ engine: 'iperf3' }), 'iperf3');
});

// --- iperf3 saved-server picker helpers ---
const IP = new Function('esc', extract('function iperfAddrValid') + '\n' + extract('function iperfCardName') +
  '\nreturn { iperfAddrValid, iperfCardName };')(s => String(s)); // esc stubbed to identity

test('iperfAddrValid: accepts host[:port], rejects empty/flag/whitespace', () => {
  assert.equal(IP.iperfAddrValid('10.0.0.5'), true);
  assert.equal(IP.iperfAddrValid('nas.local:5201'), true);
  assert.equal(IP.iperfAddrValid('  10.0.0.5  '), true); // trimmed before check
  assert.equal(IP.iperfAddrValid(''), false);
  assert.equal(IP.iperfAddrValid('   '), false);
  assert.equal(IP.iperfAddrValid('-R'), false);                // flag-shaped
  assert.equal(IP.iperfAddrValid('host --logfile x'), false);  // whitespace
  assert.equal(IP.iperfAddrValid(null), false);
});

test('iperfCardName: "label - addr", bare addr, or a new-server placeholder', () => {
  assert.equal(IP.iperfCardName({ label: 'Home NAS', addr: '10.0.0.5' }), 'Home NAS <span class="ca">- 10.0.0.5</span>');
  assert.equal(IP.iperfCardName({ label: '', addr: '10.0.0.5' }), '10.0.0.5');
  assert.match(IP.iperfCardName({ label: '', addr: '' }), /new server/);
});

// --- per-server profile mapping ---
const PS = new Function(extract('function newIperfServer') + '\n' + extract('function mapIperfServer') +
  '\nreturn { newIperfServer, mapIperfServer };')();

test('newIperfServer: fresh server defaults to auto IP, default route, auth off', () => {
  assert.deepEqual(PS.newIperfServer('NAS', '10.0.0.5'),
    { label: 'NAS', addr: '10.0.0.5', orig_addr: '', bind: '', ipver: 'auto', auth: false, username: '', rsa_key: '', pkcs1: false, has_password: false, password: '' });
});

// --- chart line gaps ---
const GS = new Function(extract('function seriesGapSec') + '\nreturn { seriesGapSec };')();

test('seriesGapSec: 3x the median point spacing; Infinity when unknowable', () => {
  const pts = ts => ts.map(t => ({ t }));
  assert.equal(GS.seriesGapSec(pts([0, 5, 10, 15, 20])), 15); // even 5s cadence -> 15s
  assert.equal(GS.seriesGapSec(pts([0, 5, 10, 36010, 36015, 36020])), 15); // one 10h hole doesn't stretch it
  assert.equal(GS.seriesGapSec(pts([7])), Infinity); // a single point has no spacing
  assert.equal(GS.seriesGapSec(pts([])), Infinity);
});

// --- settings form field mapping ---
// Stub $ with a per-id object store so set/getField run without a DOM.
const els = {};
const SF = new Function('$', extract('const secOrig') + '\n' + extract('function setField') + '\n' + extract('function getField') +
  '\nreturn { setField, getField };')(id => els[id] || (els[id] = {}));

test('set/getField: an unedited min/day field round-trips its exact seconds', () => {
  SF.setField('ret', 'day', 21600);               // CLI-set 6h renders as 0 days
  assert.equal(els.ret.value, 0);
  assert.equal(SF.getField('ret', 'day'), 21600); // unedited -> exact value back, not 0 = forever
  els.ret.value = '2';                            // edited -> whole days as typed
  assert.equal(SF.getField('ret', 'day'), 2 * 86400);
  SF.setField('spd', 'min', 90);                  // 90s renders as 2 min
  assert.equal(SF.getField('spd', 'min', 60), 90);
  els.spd.value = '5';
  assert.equal(SF.getField('spd', 'min', 60), 300);
});

test('getField int: a typed 0 is a value, not the fallback', () => {
  els.retries = { value: '0' };
  assert.equal(SF.getField('retries', 'int', 1), 0);
  els.retries.value = '';
  assert.equal(SF.getField('retries', 'int', 1), 1); // only an unparsable input falls back
});

test('mapIperfServer: carries path+auth, never echoes a password, fills has_password', () => {
  const got = PS.mapIperfServer({ label: 'VPS', addr: 'vps:5201', bind: '10.0.0.1', ipver: '6',
    auth: true, username: 'bob', rsa_key: 'KEY', pkcs1: true, has_password: true });
  assert.equal(got.password, '');        // GET never returns the secret
  assert.equal(got.has_password, true);  // only the flag
  assert.equal(got.pkcs1, true);         // legacy-padding flag carried through
  assert.equal(got.orig_addr, 'vps:5201'); // stored key, so a rename can warn about the orphaned password
  assert.equal(got.ipver, '6');
  assert.equal(got.bind, '10.0.0.1');
  assert.equal(got.auth, true);
  // A server with no IP version pinned defaults to auto.
  assert.equal(PS.mapIperfServer({ addr: 'h' }).ipver, 'auto');
});

// --- schedule coverage grid: must mirror settings.windowActive exactly ---

const gridOn = g => g.reduce((n, d) => { for (let m = 0; m < 1440; m++) n += d[m]; return n; }, 0);

test('covGrid: [start,end) day range, exclusive end', () => {
  const g = F.covGrid([{ days: '0100000', start: 480, end: 1080 }]); // Mon 08:00-18:00
  assert.equal(g[1][479], 0);
  assert.equal(g[1][480], 1);
  assert.equal(g[1][1079], 1);
  assert.equal(g[1][1080], 0);
  assert.equal(g[0][480], 0); // other days untouched
  assert.equal(gridOn(g), 600);
});

test('covGrid: equal times cover the whole selected day', () => {
  const g = F.covGrid([{ days: '0000001', start: 300, end: 300 }]); // Sat all day
  assert.equal(gridOn(g), 1440);
  assert.equal(g[6][0], 1);
  assert.equal(g[6][1439], 1);
});

test('covGrid: start>end wraps into the NEXT morning, even an unticked day', () => {
  const g = F.covGrid([{ days: '0000010', start: 1320, end: 360 }]); // Fri 22:00-06:00
  assert.equal(g[5][1319], 0);
  assert.equal(g[5][1320], 1); // Fri tail
  assert.equal(g[5][1439], 1);
  assert.equal(g[6][0], 1);    // Sat morning owned by Friday's window
  assert.equal(g[6][359], 1);
  assert.equal(g[6][360], 0);
  assert.equal(gridOn(g), 480);
});

test('covGrid: Sat wrap lands on Sun (week is circular)', () => {
  const g = F.covGrid([{ days: '0000001', start: 1380, end: 60 }]); // Sat 23:00-01:00
  assert.equal(g[6][1380], 1);
  assert.equal(g[0][59], 1);
  assert.equal(g[0][60], 0);
});

test('covStats: multi-window 24/7 is exact coverage (the old single-row check missed this)', () => {
  const st = F.covStats(F.covGrid([
    { days: '0111110', start: 0, end: 0 },   // weekdays all day
    { days: '1000001', start: 0, end: 0 },   // weekend all day
  ]));
  assert.equal(st.pct, 100);
  assert.equal(st.gap, null);
});

test('covStats: longest gap is measured across the Sat->Sun week boundary', () => {
  const st = F.covStats(F.covGrid([{ days: '0111110', start: 480, end: 1080 }])); // Mon-Fri 08-18
  assert.equal(st.gap.from, 'Fri 18:00');
  assert.equal(st.gap.to, 'Mon 08:00');
  assert.equal(st.gap.mins, 62 * 60); // Fri 18:00 -> Mon 08:00
  assert.equal(st.pct, Math.round(5 * 600 / 10080 * 100));
});

test('covStats: no windows means 0% with a week-long gap', () => {
  const st = F.covStats(F.covGrid([]));
  assert.equal(st.pct, 0);
  assert.equal(st.gap.mins, 10080);
});

test('covStats: exact minute count rides along (rounded pct lies at the edges)', () => {
  // 30 ON minutes/week rounds to 0% - the exact count is what "never on" must key off
  const tiny = F.covStats(F.covGrid([{ days: '0100000', start: 480, end: 510 }]));
  assert.equal(tiny.on, 30);
  assert.equal(tiny.pct, 0);
  assert.notEqual(tiny.gap, null);
  // a 50-min real gap rounds to 100% - the warning gate must key off gap, not pct
  const near = F.covStats(F.covGrid([
    { days: '1111110', start: 0, end: 0 },
    { days: '0000001', start: 50, end: 0 }, // Sat 00:50 -> wraps to Sun 00:00
  ]));
  assert.equal(near.on, 10030);
  assert.equal(near.pct, 100);       // the lie
  assert.equal(near.gap.mins, 50);   // the truth
  assert.equal(near.gap.from, 'Sat 00:00');
  assert.equal(near.gap.to, 'Sat 00:50');
});

// --- log viewer cursor merge rule ---
const LG = new Function(extract('function logMerge') + '\nreturn { logMerge };')();

test('logMerge: a delta appends, an empty delta skips, anything else replaces', () => {
  const E = 'r1';
  // The steady state: cursor sent, nothing new since. No repaint at all.
  assert.equal(LG.logMerge({ epoch: E, lines: [] }, true, E, true), 'skip');
  assert.equal(LG.logMerge({ epoch: E, lines: [], dropped: 0 }, true, E, true), 'skip');
  // New lines, or an eviction to mark, append onto what is already rendered.
  assert.equal(LG.logMerge({ epoch: E, lines: [{ raw: 'a' }] }, true, E, true), 'append');
  assert.equal(LG.logMerge({ epoch: E, lines: [], dropped: 12 }, true, E, true), 'append');
  // No cursor was sent (first load, or a POST): full window.
  assert.equal(LG.logMerge({ epoch: E, lines: [{ raw: 'a' }] }, false, E, true), 'replace');
  // Nothing has been painted yet (first load, or a view emptied by Clear that is
  // showing the placeholder): there is nothing to append to.
  assert.equal(LG.logMerge({ epoch: E, lines: [{ raw: 'a' }] }, true, E, false), 'replace');
  assert.equal(LG.logMerge({ epoch: E, lines: [], dropped: 3 }, true, E, false), 'replace');
  // The daemon restarted: sequences restart at 0, so this response is a tail from
  // a different ring and must NOT be spliced onto the lines already on screen.
  assert.equal(LG.logMerge({ epoch: 'r2', lines: [{ raw: 'a' }] }, true, E, true), 'replace');
  assert.equal(LG.logMerge({ epoch: 'r2', lines: [] }, true, E, true), 'replace');
  // A server with no ring wired up reports no epoch; never append to that either.
  assert.equal(LG.logMerge({ lines: [{ raw: 'a' }] }, true, E, true), 'replace');
});

// --- downtime heatmap: the grid must not outrun the data it fetched ----------
// refreshHeatmap asks for days=366 - the cap web.go enforces - so the oldest
// answerable local date is today-366. The grid starts at the Sunday on or
// before today-365 to keep the week columns aligned, which reaches up to 5
// days further back: cells no response can ever describe. With no row, a cell
// drew as the identical clean fully-observed square - the exact false-clean
// disclosure failure the observation tooltip exists to prevent. This drives
// the REAL drawHeatmap with a frozen clock and stub DOM.
function driveHeatmap(rows, todayMs) {
  const defs = extract('function _rgb') + '\n' + extract('function hmShade') + '\n'
    + extract('const fmtDur') + '\n'
    + script.match(/const fmtLocalDate = [^;]*;/)[0] + '\n'
    + script.match(/const hmLevel = [^;]*;/)[0] + '\n'
    + script.match(/const HM_W=[^;]*;/)[0] + '\n'
    + extract('function drawHeatmap');
  const cells = [];
  const grid = { innerHTML: '', style: {}, appendChild: c => cells.push(c) };
  const doc = { createElement: () => ({ className: '', style: {}, dataset: {}, attrs: {},
    classList: { add() {} }, setAttribute(k, v) { this.attrs[k] = v; } }) };
  class FrozenDate extends Date { constructor(...a) { a.length ? super(...a) : super(todayMs); } }
  new Function('ROWS', '$', 'document', 'Date', 'TC',
    'let hmData = ROWS, outageSelDay = null;\n' + defs + '\ndrawHeatmap();')(
    rows, () => grid, doc, FrozenDate, { hm0: '#000000', down: '#ffffff' });
  return cells;
}

test('heatmap: cells older than the API window are padding, not clean days', () => {
  // 2 Aug 2026 is a Sunday: today-365 lands on a Saturday, so the Sunday
  // alignment reaches 5 days - the worst case - past the oldest fetchable day.
  const cells = driveHeatmap(
    [{ date: '2026-07-15', outages: 1, downtime_s: 60, window_s: 86400, observed_s: 86400 }],
    new Date(2026, 7, 2, 12, 0).getTime());
  assert.equal(cells.length, 372); // Sun 27 Jul 2025 .. Sun 2 Aug 2026
  for (let i = 0; i < 5; i++) {
    assert.equal(cells[i].style.visibility, 'hidden',
      `cell ${i} predates the fetched window: with no row it draws as a clean fully-observed day`);
  }
  // The oldest day the API can answer for, and everything after it, draws.
  assert.notEqual(cells[5].style.visibility, 'hidden');
  // Sanity for the harness itself: a real day still renders its disclosure.
  assert.ok(cells.some(c => /2026-07-15: 1 outage/.test(c.dataset.tip || '')),
    'the outage day lost its tooltip - the drive is not rendering real cells');
});

test('heatmap: a Sunday-aligned year needs no padding at all', () => {
  // 27 Jul 2026 is a Monday: today-365 is a Sunday, the grid starts exactly
  // there, and every cell is inside the fetched window.
  const cells = driveHeatmap([], new Date(2026, 6, 27, 12, 0).getTime());
  assert.equal(cells.length, 366);
  assert.ok(cells.every(c => c.style.visibility !== 'hidden'));
});

// --- single-run graph: spanOne duplicates a lone point so it draws a flat line ---
const SO = new Function(extract('function spanOne') + '\nreturn { spanOne };')();

test('spanOne: a lone point is duplicated 1s earlier; multi passes through', () => {
  const one = SO.spanOne([{ ts: 100, down_mbps: 5 }]);
  assert.equal(one.length, 2);
  assert.equal(one[0].ts, 99);            // the earlier duplicate
  assert.equal(one[1].ts, 100);           // the real reading stays at the right edge
  assert.equal(one[0].down_mbps, 5);      // spread carries every field
  const many = [{ ts: 1 }, { ts: 2 }];
  assert.equal(SO.spanOne(many), many);   // 2+ points untouched (same ref)
  assert.deepEqual(SO.spanOne([]), []);   // empty stays empty
});

test('speedTileMbps: "-" when the direction was not measured, else rounded Mbps', () => {
  assert.equal(F.speedTileMbps(123.6, 5000), '124 Mbps'); // measured: bytes present, rounds
  assert.equal(F.speedTileMbps(0, 0), '0 Mbps');          // 0 bytes is still a measurement of a very slow run
  assert.equal(F.speedTileMbps(0, null), '-');            // no upload phase ran -> absent, not "0 Mbps"
  assert.equal(F.speedTileMbps(500, undefined), '-');     // omitempty drops the field entirely
});

// --- Range averages: the three readout pills on the speed panel's runs bar ------
// They describe spdData, i.e. exactly the rows the charts beside them are drawing.
// A row as /api/speed sends it. The byte counts are the "this direction actually
// ran" evidence spMeasured gates on, so dropping one is how a run says it measured
// only the other direction; ping_ms 0 is the "not probed" sentinel.
const run = (o = {}) => ({ ts: 1, down_mbps: 0, up_mbps: 0, ping_ms: 0,
  download_bytes: null, upload_bytes: null, ...o });

test('spdAverages: ping_ms 0 is "not probed" - excluded from the mean, not averaged in as zero', () => {
  const rows = [run({ ping_ms: 10 }), run({ ping_ms: 0 }), run({ ping_ms: 30 })];
  // The mean of the two runs that actually probed. Counting the sentinel as a
  // sample gives 13.3 ms and advertises a link far faster than it is.
  assert.equal(F.spdAverages(rows).ping, 20);
  assert.equal(F.pingText(F.spdAverages(rows).ping), '20.0 ms');
  // Nothing probed at all -> no sample, which is a dash and never "0.0 ms".
  assert.equal(F.spdAverages([run(), run()]).ping, null);
  assert.equal(F.pingText(F.spdAverages([run(), run()]).ping), '-');
});

test('spdAverages: a row missing one direction is skipped per metric, not dropped whole', () => {
  const rows = [
    run({ down_mbps: 100, download_bytes: 5000 }),                                  // download only
    run({ up_mbps: 20, upload_bytes: 4000 }),                                       // upload only
    run({ down_mbps: 200, download_bytes: 5000, up_mbps: 40, upload_bytes: 4000 }), // both
  ];
  const a = F.spdAverages(rows);
  // Dropping any row that lacks a direction would leave only the third run (200/40);
  // averaging its absent direction in as 0 Mbps would give 100/20. Both are wrong.
  assert.equal(a.down, 150);
  assert.equal(a.up, 30);
  assert.equal(F.mbpsText(a.down), '150 Mbps'); // same units/decimals as the tile above
});

test('spdAverages: an empty range yields no mean at all, which renders as the house dash', () => {
  const a = F.spdAverages([]);
  assert.deepEqual(a, { down: null, up: null, ping: null });
  // The concrete failure guarded here: a sum/count of 0/0 is NaN, and a pill reading
  // "NaN Mbps" beside an empty chart.
  assert.equal(F.mbpsText(a.down), '-');
  assert.equal(F.mbpsText(a.up), '-');
  assert.equal(F.pingText(a.ping), '-');
  assert.deepEqual(F.spdAverages(undefined), { down: null, up: null, ping: null });
});

test('spdAverages: every plotted point counts once - /api/speed decimates, it does not aggregate', () => {
  // A wide window is thinned with `ts IN (SELECT MAX(ts) FROM speed .. GROUP BY ts/bucket)`,
  // which keeps the newest REAL run per bucket and re-reads that row whole. So every
  // row here is one genuine speedtest and none carries a sample count: the mean is
  // unweighted, and must stay unweighted even if a row shows up wearing a
  // count-shaped field, because weighting by one would claim an accuracy the payload
  // does not actually carry.
  const rows = [run({ down_mbps: 100, download_bytes: 1 }), run({ down_mbps: 200, download_bytes: 1 })];
  rows[0].count = 500; rows[0].n = 500; rows[0].samples = 500;
  assert.equal(F.spdAverages(rows).down, 150);
});

// --- bufferbloat helpers: latency added under load ---
const BL = new Function(extract('function bloatDelta') + '\n' + extract('function bloatDirText') +
  '\n' + extract('function bloatCell') + '\nreturn { bloatDelta, bloatDirText, bloatCell };')();

test('bloatDelta: worse directional increase, clamps negatives, nulls propagate', () => {
  assert.deepEqual(BL.bloatDelta(null, 5, 6), { max: null, dd: null, ud: null }); // no idle baseline
  assert.deepEqual(BL.bloatDelta(10, 15, 8), { max: 5, dd: 5, ud: 0 });   // loaded below idle clamps to 0
  assert.deepEqual(BL.bloatDelta(10, null, 70), { max: 60, dd: null, ud: 60 }); // unmeasured direction stays null
  assert.deepEqual(BL.bloatDelta(10, null, null), { max: null, dd: null, ud: null });
});

test('bloatDirText: whole-ms added latency for one tile, "-" when unmeasured', () => {
  assert.equal(BL.bloatDirText(10, 68.4), '58 ms');
  assert.equal(BL.bloatDirText(10, 8), '0 ms'); // negative clamps to zero
  assert.equal(BL.bloatDirText(null, 68), '-');
  assert.equal(BL.bloatDirText(10, null), '-');
});

test('bloatCell: directional arrows, muted dash when nothing measured', () => {
  assert.equal(BL.bloatCell(10, 19, 68), '↓9 ↑58');
  assert.equal(BL.bloatCell(10, null, 68), '↑58');
  assert.equal(BL.bloatCell(10, 19, null), '↓9');
  assert.equal(BL.bloatCell(null, 19, 68), '<span class="muted">-</span>');
});

// --- status-poll window selection (uptime pill + data bubble) ---
// Both read module-level picker state; recreate it in the factory preamble and
// expose setters, the same way the stateful setField/getField pair stubs $.
const winState = 'let dataWindow = "all", dataCustomBytes = 0, uptimeWindow = "7d", lastUptimeCustom = null;\n';
const WV = new Function(winState + extract('function dataBarValue') + '\n' + extract('function uptimeValue') +
  '\nreturn { dataBarValue, uptimeValue, setDataWindow: w => { dataWindow = w; },' +
  ' setUptimeWindow: w => { uptimeWindow = w; }, setLastUptimeCustom: v => { lastUptimeCustom = v; } };')();

test('uptimeValue: window lookup, flat-field fallback, custom carry-over, null when absent', () => {
  WV.setUptimeWindow('7d');
  assert.equal(WV.uptimeValue({ uptime: { '7d': 0.99 } }), 0.99); // present-window lookup
  assert.equal(WV.uptimeValue({ uptime_7d: 0.98 }), 0.98);        // old flat payload shape
  WV.setUptimeWindow('24h');
  assert.equal(WV.uptimeValue({ uptime_24h: 0.97 }), 0.97);
  WV.setUptimeWindow('all');
  assert.equal(WV.uptimeValue({}), null);                         // nothing available
  WV.setUptimeWindow('custom');
  assert.equal(WV.uptimeValue({ uptime_custom: 0.5 }), 0.5);      // fresh custom reading
  WV.setLastUptimeCustom(0.75);
  assert.equal(WV.uptimeValue({}), 0.75);                         // carried-over custom value
});

test('dataBarValue: window lookup, flat fallback, custom carries the last reading', () => {
  WV.setDataWindow('all');
  assert.equal(WV.dataBarValue({ data_usage: { all: 123 } }), 123); // present-window lookup
  assert.equal(WV.dataBarValue({ data_used_bytes: 55 }), 55);       // old flat payload shape
  assert.equal(WV.dataBarValue({}), 0);                             // nothing recorded yet
  WV.setDataWindow('24h');
  assert.equal(WV.dataBarValue({ data_usage: { '24h': 7 } }), 7);
  assert.equal(WV.dataBarValue({ data_usage: {} }), 0);             // absent sub-window reads 0
  WV.setDataWindow('custom');
  assert.equal(WV.dataBarValue({ data_used_custom: 512 }), 512);    // fresh reading is cached
  assert.equal(WV.dataBarValue({}), 512);                           // poll without it keeps the cache
});

// lossStr/msStr are brace-less one-line arrows - regex-grab them like engOf.
const fmtFactory = new Function(
  script.match(/const lossStr = [^;]*;/)[0] + '\n' + script.match(/const msStr = [^;]*;/)[0] + '\nreturn { lossStr, msStr };');
const FM = fmtFactory();

test('msStr / lossStr: tiny real values read "<0.1", never a fake zero', () => {
  assert.equal(FM.msStr(0.0094), '<0.1 ms');   // real iperf3 jitter
  assert.equal(FM.msStr(0), '0.0 ms');         // true zero stays zero
  assert.equal(FM.msStr(0.1), '0.1 ms');       // boundary renders normally
  assert.equal(FM.msStr(12.34), '12.3 ms');
  assert.equal(FM.msStr(null), '-');
  assert.equal(FM.lossStr(0.011), '<0.1%');
  assert.equal(FM.lossStr(0), '0.0%');
  assert.equal(FM.lossStr(2.5), '2.5%');
  assert.equal(FM.lossStr(undefined), '-');
});

// ---- parseRange: window text -> rolling window or absolute span -------------
// Spans are half-open [from, to): a bare end date resolves to the START of the
// next day, so "1 jul to 8 jul" includes all of 8 July. `now` is always passed
// in, so none of these can flake at midnight, a month end, or a DST edge.
const NOW = new Date(2026, 6, 18, 14, 30).getTime();          // Sat 18 Jul 2026, 14:30 local
const sec = (...a) => Math.floor(new Date(...a).getTime() / 1000);
const PR = (s, now = NOW, dayFirst = true) => F.parseRange(s, now, dayFirst);
const span = (s, now = NOW, dayFirst = true) => {
  const r = PR(s, now, dayFirst);
  return r && r.kind === 'range' ? [r.from, r.to] : r;
};

test('parseRange: every input parseDur accepts stays a rolling window, unchanged', () => {
  for (const s of ['45m', '12h', '3d', '1w', '2mo', '1.5h', '90']) {
    assert.deepEqual(PR(s), { kind: 'win', mins: F.parseDur(s) }, s);
  }
  // and the things parseDur rejects are still rejected here
  for (const s of ['garbage', '', '5x']) assert.equal(PR(s), null, s);
});

test('parseRange: spelled-out units and last-N aliases stay rolling', () => {
  assert.deepEqual(PR('3 days'), { kind: 'win', mins: 4320 });
  assert.deepEqual(PR('12 hours'), { kind: 'win', mins: 720 });
  assert.deepEqual(PR('last week'), { kind: 'win', mins: 10080 });
  assert.deepEqual(PR('past month'), { kind: 'win', mins: 43200 });
  assert.deepEqual(PR('last 2 weeks'), { kind: 'win', mins: 20160 });
});

test('parseRange: bare end date includes that whole day (half-open)', () => {
  assert.deepEqual(span('jul 1 to jul 8'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.deepEqual(span('2026-07-01 to 2026-07-08'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.deepEqual(span('1 jul to 8 jul'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.deepEqual(span('july 1, 2026 to july 8, 2026'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  // same day both ends = that one whole day
  assert.deepEqual(span('jul 1 to jul 1'), [sec(2026, 6, 1), sec(2026, 6, 2)]);
});

test('parseRange: a single whole unit is that unit', () => {
  assert.deepEqual(span('today'), [sec(2026, 6, 18), sec(2026, 6, 19)]);
  assert.deepEqual(span('yesterday'), [sec(2026, 6, 17), sec(2026, 6, 18)]);
  assert.deepEqual(span('2026-07-01'), [sec(2026, 6, 1), sec(2026, 6, 2)]);
  assert.deepEqual(span('july'), [sec(2026, 6, 1), sec(2026, 7, 1)]);
  assert.deepEqual(span('june to july'), [sec(2026, 5, 1), sec(2026, 7, 1)]);
  assert.deepEqual(span('2024 to 2025'), [sec(2024, 0, 1), sec(2026, 0, 1)]);
});

test('parseRange: a bare four-digit year is the year, other bare numbers are hours', () => {
  // parseDur reads any bare number as hours and still does everywhere else, but
  // 2026 plainly means the year - it already did on both sides of a span.
  assert.deepEqual(span('2026'), [sec(2026, 0, 1), sec(2027, 0, 1)]);
  assert.deepEqual(span('2025'), [sec(2025, 0, 1), sec(2026, 0, 1)]);
  assert.deepEqual(span('2025 to 2026'), [sec(2025, 0, 1), sec(2027, 0, 1)]);
  // outside the year range, and for shorter numbers, hours are unchanged
  assert.deepEqual(PR('90'), { kind: 'win', mins: 90 * 60 });
  assert.deepEqual(PR('500'), { kind: 'win', mins: 500 * 60 });
  assert.deepEqual(PR('1200'), { kind: 'win', mins: 1200 * 60 });
  assert.deepEqual(PR('2500'), { kind: 'win', mins: 2500 * 60 });
});

test('parseRange: open-ended forms leave the other bound at 0', () => {
  const a = PR('since jul 1');
  assert.deepEqual([a.from, a.to, a.openEnd], [sec(2026, 6, 1), 0, true]);
  assert.deepEqual([PR('from jul 1').from, PR('from jul 1').to], [sec(2026, 6, 1), 0]);
  const b = PR('until yesterday');
  assert.deepEqual([b.from, b.to, b.openStart], [0, sec(2026, 6, 18), true]);
});

test('parseRange: before and after exclude the day named, until and since include it', () => {
  assert.equal(PR('until jul 5').to, sec(2026, 6, 6));   // through the end of the 5th
  assert.equal(PR('before jul 5').to, sec(2026, 6, 5));  // stops where the 5th starts
  assert.equal(PR('since jul 5').from, sec(2026, 6, 5)); // the 5th is in
  assert.equal(PR('after jul 5').from, sec(2026, 6, 6)); // the 5th is out
});

test('parseRange: separator family', () => {
  const want = [sec(2026, 6, 1), sec(2026, 6, 9)];
  for (const s of ['jul 1 to jul 8', 'jul 1 until jul 8', 'jul 1 through jul 8',
                   'jul 1 - jul 8', 'jul 1 .. jul 8', 'jul 1 -> jul 8',
                   'between jul 1 and jul 8']) {
    assert.deepEqual(span(s), want, s);
  }
});

test('parseRange: an unspaced hyphen never splits an ISO date', () => {
  // would otherwise read as the year 2026 and the month-day 07-01
  assert.deepEqual(span('2026-07-01'), [sec(2026, 6, 1), sec(2026, 6, 2)]);
  // the one unspaced form allowed: a pair of slash dates
  assert.deepEqual(span('1/7-8/7'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
});

test('parseRange: slash order follows the locale, unless a field settles it', () => {
  assert.deepEqual(span('1/7 to 8/7', NOW, true), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.deepEqual(span('7/1 to 7/8', NOW, false), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  // 13 cannot be a month, so day-first regardless of what the locale said
  assert.deepEqual(span('13/07/2026', NOW, false), [sec(2026, 6, 13), sec(2026, 6, 14)]);
  assert.equal(PR('1/7 to 8/7', NOW, true).guessed, true);      // locale decided: flag it
  assert.equal(PR('13/07/2026', NOW, false).guessed, false);    // arithmetic decided: no flag
});

test('parseRange: clock times make exact instants, and mix with whole days', () => {
  assert.deepEqual(span('jul 1 09:00 to jul 1 18:30'),
    [sec(2026, 6, 1, 9, 0), sec(2026, 6, 1, 18, 30)]);
  assert.deepEqual(span('jul 1 9am to jul 1 5pm'),
    [sec(2026, 6, 1, 9, 0), sec(2026, 6, 1, 17, 0)]);
  // a timed start with a bare end day still runs to the end of that day
  assert.deepEqual(span('jul 1 09:00 to jul 8'), [sec(2026, 6, 1, 9, 0), sec(2026, 6, 9)]);
  assert.deepEqual(span('jul 1 12am to jul 1 12pm'),
    [sec(2026, 6, 1, 0, 0), sec(2026, 6, 1, 12, 0)]);
});

test('parseRange: bare times mean today, rolling back a day if that is ahead of now', () => {
  assert.deepEqual(span('9am to 5pm'), [sec(2026, 6, 18, 9, 0), sec(2026, 6, 18, 17, 0)]);
  const early = new Date(2026, 6, 18, 8, 0).getTime();  // before 9am: must mean yesterday
  assert.deepEqual(span('9am to 5pm', early), [sec(2026, 6, 17, 9, 0), sec(2026, 6, 17, 17, 0)]);
});

test('parseRange: a year-less date resolves to the most recent one', () => {
  const jan = new Date(2027, 0, 10, 12, 0).getTime();
  // dec 25 typed in January is last December, not eleven months out
  assert.deepEqual(span('dec 25', jan), [sec(2026, 11, 25), sec(2026, 11, 26)]);
  // and each endpoint rolls independently, so a span across new year works
  assert.deepEqual(span('dec 20 to jan 5', jan), [sec(2026, 11, 20), sec(2027, 0, 6)]);
});

test('parseRange: reversed dates swap, and the whole-unit rule is reapplied', () => {
  const r = PR('jul 8 to jul 1');
  assert.deepEqual([r.from, r.to], [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.equal(r.swapped, true);
});

test('parseRange: relative endpoints', () => {
  assert.deepEqual(span('3d ago to now'), [Math.floor(NOW / 1000) - 3 * 86400, Math.floor(NOW / 1000)]);
  assert.deepEqual(span('yesterday to today'), [sec(2026, 6, 17), sec(2026, 6, 19)]);
  assert.equal(PR('2 hours ago to now').from, Math.floor(NOW / 1000) - 7200);
});

test('parseRange: rejects what it cannot read honestly', () => {
  for (const s of [
    'feb 30',                       // impossible date, must not roll to Mar 2
    'jul 32 to jul 40',
    'now',                          // zero width
    '9am',                          // a lone time is never a window
    'jul 1 09:00 utc',              // a typed timezone is refused, not ignored
    'jul 1 09:00+02:00',
    'next tuesday',                 // no data can exist ahead of now
    'sometime last summer',
    'jul 1 to',
    'x'.repeat(65),                 // over the input maxlength
    '25:00 to 26:00',               // impossible clock
  ]) assert.equal(PR(s), null, s);
});

test('parseRange: month-name spellings and punctuation', () => {
  const want = [sec(2026, 6, 1), sec(2026, 6, 2)];
  for (const s of ['jul 1', 'july 1', 'jul. 1', '1 jul', '1 july', 'jul 1 2026', 'jul 1, 2026']) {
    assert.deepEqual(span(s), want, s);
  }
});

test('fmtRangeEcho: says how the text was read, with an inclusive end', () => {
  assert.equal(F.fmtRangeEcho({ kind: 'win', mins: 10080 }), 'Last 7d');
  // whole days print as days, and the end reads as the last day INSIDE the span
  assert.equal(F.fmtRangeEcho(PR('jul 1 to jul 8')), '1 Jul 2026 to 8 Jul 2026');
  assert.equal(F.fmtRangeEcho(PR('jul 1 09:00 to jul 1 18:30')),
    '1 Jul 2026, 09:00 to 1 Jul 2026, 18:30');
  assert.equal(F.fmtRangeEcho(PR('since jul 1')), 'From 1 Jul 2026 onwards');
  assert.equal(F.fmtRangeEcho(PR('until yesterday')), 'Up to 17 Jul 2026');
  assert.match(F.fmtRangeEcho(PR('jul 8 to jul 1')), /dates swapped/);
  assert.match(F.fmtRangeEcho(PR('1/7 to 8/7', NOW, true)), /day\/month order/);
  assert.equal(F.fmtRangeEcho(null), '');
});

test('rangeLoad: rolling stays a bare int, spans are JSON, junk resets', () => {
  const MAX = 366 * 24 * 60;
  // what the current build has always written
  assert.deepEqual(F.rangeLoad('10080', 1440, MAX), { kind: 'win', mins: 10080 });
  assert.deepEqual(F.rangeLoad(null, 1440, MAX), { kind: 'win', mins: 1440 });
  assert.deepEqual(F.rangeLoad('', 1440, MAX), { kind: 'win', mins: 1440 });
  assert.equal(F.rangeLoad('999999999', 1440, MAX).mins, MAX);   // clamped
  const r = F.rangeLoad(JSON.stringify({ kind: 'range', from: 100, to: 200 }), 1440, MAX);
  assert.deepEqual([r.kind, r.from, r.to], ['range', 100, 200]);
  // reversed, zero-width, malformed and non-JSON all fall back to the default
  for (const s of ['{"kind":"range","from":200,"to":100}', '{"kind":"range","from":0,"to":0}',
                   '{"kind":"nope"}', 'not json', '-5', '0']) {
    assert.deepEqual(F.rangeLoad(s, 1440, MAX), { kind: 'win', mins: 1440 }, s);
  }
});

test('rangeLoad: an older build reading a span falls back, not to a nonsense window', () => {
  // the reason spans are stored as JSON: the shipped reader is parseInt()>0
  assert.ok(Number.isNaN(parseInt(JSON.stringify({ kind: 'range', from: 1e9, to: 2e9 }), 10)));
});

// ---- regressions found by reviewing the first cut of parseRange ------------
// All three produced a confident WRONG span rather than a rejection, which is
// the worst failure mode for this control: the echo confirmed the bad reading.

test('parseRange: a span running into the future is not year-rolled into an inverted one', () => {
  // jul 1 to aug 31 typed in July: the end simply has not happened yet. Rolling
  // each endpoint independently used to send the end back a year and then
  // "swap" it, yielding 31 Aug 2025 to 1 Jul 2026.
  assert.deepEqual(span('jul 1 to aug 31'), [sec(2026, 6, 1), sec(2026, 8, 1)]);
  assert.deepEqual(span('jan 1 to dec 31'), [sec(2026, 0, 1), sec(2027, 0, 1)]);
  assert.deepEqual(span('july to august'), [sec(2026, 6, 1), sec(2026, 8, 1)]);
  assert.equal(PR('jul 1 to aug 31').swapped, false);
  // both ends ahead of now still means last year, and the cross-new-year and
  // reversed readings both survive
  const jan = new Date(2027, 0, 10, 12, 0).getTime();
  assert.deepEqual(span('dec 1 to dec 20', jan), [sec(2026, 11, 1), sec(2026, 11, 21)]);
  assert.deepEqual(span('dec 20 to jan 5', jan), [sec(2026, 11, 20), sec(2027, 0, 6)]);
  assert.deepEqual(span('jul 8 to jul 1'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
});

test('parseRange: a year given on one side applies to the other', () => {
  assert.deepEqual(span('1 jan to 31 dec 2025'), [sec(2025, 0, 1), sec(2026, 0, 1)]);
  assert.deepEqual(span('jul 1 to jul 8 2025'), [sec(2025, 6, 1), sec(2025, 6, 9)]);
  assert.deepEqual(span('jul 1 2025 to jul 8'), [sec(2025, 6, 1), sec(2025, 6, 9)]);
  // an explicit year on both sides is untouched
  assert.deepEqual(span('1 jan 2025 to 31 dec 2025'), [sec(2025, 0, 1), sec(2026, 0, 1)]);
});

test('parseRange: only whole month names count, never a three-letter prefix', () => {
  // junk read as June, octopus as October, decade as December
  for (const s of ['junk', 'octopus', 'decade', 'maybe', 'augment', 'marching', 'jul 1 to junk'])
    assert.equal(PR(s), null, s);
  // real spellings still work, including the sept alias
  assert.deepEqual(span('sep'), [sec(2025, 8, 1), sec(2025, 9, 1)]);
  assert.deepEqual(span('sept'), [sec(2025, 8, 1), sec(2025, 9, 1)]);
  assert.deepEqual(span('september'), [sec(2025, 8, 1), sec(2025, 9, 1)]);
  assert.deepEqual(span('mar'), [sec(2026, 2, 1), sec(2026, 3, 1)]);
});

test('parseRange: an overnight span is the night, not the daytime complement', () => {
  // swapping 10pm/2am would answer with 2am to 10pm, a span nobody asked for; the
  // night between them is 10pm to 2am THE NEXT MORNING - a 4-hour span, not the
  // 28-hour two-night span the end-from-now reconstruction used to produce.
  assert.deepEqual(span('10pm to 2am'),
    [sec(2026, 6, 17, 22, 0), sec(2026, 6, 18, 2, 0)]);
  assert.deepEqual(span('22:00 to 02:00'),
    [sec(2026, 6, 17, 22, 0), sec(2026, 6, 18, 2, 0)]);
  // and a lone time is still a rejection, not 24 hours
  assert.equal(PR('9am'), null);
});

test('parseRange: from X to Y is a span, since X stays open-ended', () => {
  assert.deepEqual(span('from jul 1 to jul 8'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.deepEqual(span('since jul 1 to jul 8'), [sec(2026, 6, 1), sec(2026, 6, 9)]);
  assert.equal(PR('since jul 1').to, 0);
});

test('parseRange: nothing predates the epoch, since rangeLoad would drop it on reload', () => {
  for (const s of ['until 1920', 'before 1950-01-01', '1 jan 1900 to 1 jan 1901'])
    assert.equal(PR(s), null, s);
});

// ---------------------------------------------------------------------------
// Chart x-axis + speed-panel poll guard.
//
// drawXAxis only needs a 2D context that can measure and place text, so it runs
// here against a stub that RECORDS every fillText instead of painting it. The
// recorded x is the tick position (the end labels are edge-aligned, the interior
// ones centred, and in both cases fillText is passed the tick x), so a test can
// ask the one question that matters: does the label at this x name the time that
// the chart actually plotted at this x?
// ---------------------------------------------------------------------------
const CHART_DEFS = {
  drawXAxis: 'function drawXAxis', axFmtLadder: 'function axFmtLadder',
  fmtAxisTime: 'function fmtAxisTime', AXF: 'const AXF',
  spdSyncPlan: 'function spdSyncPlan', xMap: 'function xMap',
};
const CNAMES = Object.keys(CHART_DEFS);
const cdefs = CNAMES.map(n => extract(CHART_DEFS[n])).join('\n');
// AXMON and AX_GAP are brace-less consts that extract() can't lift - pass them in,
// same as DAYN above.
const chartFactory = new Function('TC', 'AXMON', 'AX_GAP',
  cdefs + '\nreturn {' + CNAMES.join(',') + '};');
const AXMON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const C = chartFactory({ xLab: true, axis: '#888' }, AXMON, 10);

// 11px ui-monospace: every glyph the same width, which is what the real cull
// measures against. 6.6px is close enough that label widths - and therefore which
// labels the cull drops - match a browser within a pixel or two.
const CHW = 6.6;
function recCtx() {
  const calls = [];
  return {
    calls, font: '', fillStyle: '', textBaseline: '', textAlign: 'left',
    save() {}, restore() {},
    measureText(s) { return { width: s.length * CHW }; },
    fillText(t, x) { calls.push({ text: t, x, align: this.textAlign }); },
  };
}

// A desktop-width latency chart: floor(846/105) = 8, so the tick count is capped
// by the 6 ceiling and 7 labels are asked for - the shape all three defects showed up in.
const CW = 900, PADL = 44, PADR = 10, PLOT = CW - PADL - PADR;
const at = (y, mo, d, h, mi, s = 0) => Math.floor(new Date(y, mo, d, h, mi, s).getTime() / 1000);

// labelToTs reads an axis label back into the instant it names, resolving the
// day from the time the label sits next to (a bare clock label can belong to
// either side of a midnight). Throws on anything it can't parse, so a format
// change can't quietly weaken these tests into passing.
function labelToTs(label, nearTs) {
  const m = label.match(/^(?:(\d{1,2}) ([A-Za-z]{3}) )?(\d{2}):(\d{2})(?::(\d{2}))?$/);
  if (!m) throw new Error('unparsable axis label: ' + JSON.stringify(label));
  const [, dd, mon, hh, mi, ss] = m;
  const near = new Date(nearTs * 1000);
  let best = null;
  for (let off = -1; off <= 1; off++) {
    const d = new Date(near.getFullYear(), near.getMonth(), near.getDate() + off,
      +hh, +mi, ss ? +ss : 0);
    if (dd) { d.setMonth(AXMON.indexOf(mon)); d.setDate(+dd); }
    const ts = Math.round(d.getTime() / 1000);
    if (best === null || Math.abs(ts - nearTs) < Math.abs(best - nearTs)) best = ts;
  }
  return best;
}

// The shape that broke the latency axis: a 1 day window whose middle 18 hours
// have no samples at all (the monitor was off), sampled once a minute either side.
function gappedSeries() {
  const pts = [];
  const push = t => pts.push({ t, ts: t, lat: 20, online: true });
  for (let i = 0; i < 118; i++) push(at(2026, 6, 25, 8, 36) + i * 60);   // 08:36 -> 10:33
  for (let i = 0; i < 242; i++) push(at(2026, 6, 26, 4, 34) + i * 60);   // 04:34 -> 08:35
  return pts;
}

test('drawXAxis: over a hole, every label names the time actually plotted at its x', () => {
  const pts = gappedSeries();
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  // Exactly the mapping drawChart plots with: x linear in TIME.
  const X = t => PADL + (t - t0) / (t1 - t0) * PLOT;
  const x = recCtx();
  C.drawXAxis(x, pts, 't', X, CW, 300, PADL, PADR, 'time');

  assert.ok(x.calls.length >= 5, 'expected a full axis, got ' + x.calls.length + ' labels');
  for (const c of x.calls) {
    // The time this chart really draws at this x, inverted from the same X().
    const want = t0 + (c.x - PADL) / PLOT * (t1 - t0);
    const got = labelToTs(c.text, want);
    // Tolerance is 60s: the label is truncated to the minute, so that is the
    // finest it can be right to. The defect was 8.2 HOURS.
    assert.ok(Math.abs(got - want) <= 60,
      `label ${JSON.stringify(c.text)} at x=${c.x.toFixed(1)} names ` +
      `${new Date(got * 1000).toLocaleString()} but that x plots ` +
      `${new Date(want * 1000).toLocaleString()} - off by ` +
      `${((got - want) / 3600).toFixed(2)}h`);
  }
  const seen = x.calls.map(c => c.text);
  assert.equal(new Set(seen).size, seen.length, 'duplicate labels on the axis: ' + seen.join(' | '));

  // Control: the SAME gapped array through the index-based mapping the speed
  // charts use. There every label must be the time of a real sample, and that
  // sample must plot within half a sample step of the tick.
  const xi = recCtx();
  const XI = C.xMap(pts, PADL, PADR, CW);
  C.drawXAxis(xi, pts, 'ts', XI, CW, 300, PADL, PADR);
  for (const c of xi.calls) {
    // Invert the INDEX mapping (not the time one): the sample that sits at this x.
    const want = pts[Math.round((c.x - PADL) / PLOT * (pts.length - 1))];
    const got = labelToTs(c.text, want.ts);
    assert.ok(Math.abs(got - want.ts) <= 60,
      `index-mode label ${JSON.stringify(c.text)} at x=${c.x.toFixed(1)} names ` +
      `${new Date(got * 1000).toLocaleString()}, but the sample at that x is ` +
      `${new Date(want.ts * 1000).toLocaleString()}`);
  }

  // And the latency chart must actually ask for time mode - the fix is only live
  // if the call site passes it.
  const call = script.match(/drawXAxis\(x, points, 't',[^\n]*/);
  assert.ok(call && /'time'/.test(call[0]),
    'drawChart must pass tMode=time to drawXAxis, got: ' + (call && call[0]));
});

test('drawXAxis: never draws more ticks than the series has distinct times', () => {
  // Four runs, the sparse series the 1y preset produced - it asked for 7 ticks
  // and several resolved to the same sample, so the axis read 21:21 three times.
  const base = at(2026, 6, 25, 21, 21);
  const pts = [0, 7 * 3600, 14 * 3600, 22 * 3600].map(o => ({ ts: base + o }));
  const x = recCtx();
  C.drawXAxis(x, pts, 'ts', C.xMap(pts, PADL, PADR, CW), CW, 300, PADL, PADR);
  const seen = x.calls.map(c => c.text);
  assert.ok(seen.length <= pts.length,
    `4 samples but ${seen.length} ticks: ` + seen.join(' | '));
  assert.equal(new Set(seen).size, seen.length, 'duplicate labels: ' + seen.join(' | '));

  // Two samples is two ticks, not seven.
  const two = [{ ts: base }, { ts: base + 3600 }];
  const x2 = recCtx();
  C.drawXAxis(x2, two, 'ts', C.xMap(two, PADL, PADR, CW), CW, 300, PADL, PADR);
  assert.equal(x2.calls.length, 2, 'two samples: ' + x2.calls.map(c => c.text).join(' | '));

  // A series with one instant repeated has no range to describe and draws nothing.
  const same = [{ ts: base }, { ts: base }, { ts: base }];
  const x3 = recCtx();
  C.drawXAxis(x3, same, 'ts', C.xMap(same, PADL, PADR, CW), CW, 300, PADL, PADR);
  assert.equal(x3.calls.length, 0);
});

test('drawXAxis: the label format is fine enough that neighbouring ticks differ', () => {
  // A 3 day window is past the 36h clock-time cutoff, so labels went date-only -
  // but 7 ticks across 3 days sit 12 hours apart, so the same date printed twice
  // in a row. Dense samples, so the tick cap is not what saves this.
  const start = at(2026, 6, 25, 0, 0);
  const pts = [];
  for (let i = 0; i <= 3 * 24 * 6; i++) pts.push({ ts: start + i * 600 });
  const x = recCtx();
  C.drawXAxis(x, pts, 'ts', C.xMap(pts, PADL, PADR, CW), CW, 300, PADL, PADR);
  const seen = x.calls.map(c => c.text);
  assert.ok(seen.length >= 5, 'expected a full axis, got ' + seen.join(' | '));
  for (let i = 1; i < seen.length; i++)
    assert.notEqual(seen[i], seen[i - 1],
      'neighbouring ticks read the same: ' + seen.join(' | '));

  // The ladder is what makes that possible: each span offers a finer fallback,
  // and every rung is strictly more detailed than the one above it.
  for (const span of [3 * 86400, 400 * 86400]) {
    const ladder = C.axFmtLadder(span);
    assert.ok(ladder.length > 1, 'span ' + span + ' has no finer format to fall back on');
    const d = new Date(2026, 6, 25, 9, 30, 15);
    const rendered = ladder.map(f => f(d));
    assert.equal(new Set(rendered).size, rendered.length, 'ladder rungs are not distinct');
  }
  // The top of each ladder is still what the span picked before - unchanged.
  const d = new Date(2026, 6, 25, 9, 30, 15), s = Math.floor(d.getTime() / 1000);
  assert.equal(C.fmtAxisTime(s, 10 * 60), '09:30:15');
  assert.equal(C.fmtAxisTime(s, 6 * 3600), '09:30');
  assert.equal(C.fmtAxisTime(s, 10 * 86400), '25 Jul');
  assert.equal(C.fmtAxisTime(s, 400 * 86400), 'Jul 26');
});

test('spdSyncPlan: a status poll cannot repaint a span selected after it left', () => {
  // The race: the poll leaves under range A, the user picks range B (which bumps
  // speedSeq and leaves speedLoadedFor still on A), then the poll lands.
  const seqAtSend = 4;
  assert.equal(C.spdSyncPlan(seqAtSend, 5, 'from=1&to=2', 'from=3&to=4'), 'skip');

  // Nothing moved: the poll owns the panel and spdData really is this range.
  assert.equal(C.spdSyncPlan(4, 4, 'from=3&to=4', 'from=3&to=4'), 'sync');

  // The sequence settled but the fetch for B FAILED, so spdData is still A. A
  // sequence check alone would call this fresh and paint A over B.
  assert.equal(C.spdSyncPlan(4, 4, 'from=1&to=2', 'from=3&to=4'), 'stale');

  // The call site must translate that into syncSpeedPanel(fresh) correctly:
  // skip paints nothing, stale paints the loading state, sync paints the data.
  const painted = [];
  const callSite = (seq, curSeq, loadedFor, wantQ) => {
    const plan = C.spdSyncPlan(seq, curSeq, loadedFor, wantQ);
    if (plan !== 'skip') painted.push(plan === 'sync');
  };
  callSite(4, 5, 'from=1&to=2', 'from=3&to=4');
  assert.deepEqual(painted, [], 'a superseded poll must not paint the panel at all');
  callSite(4, 4, 'from=1&to=2', 'from=3&to=4');
  assert.deepEqual(painted, [false], 'stale data must never be painted as fresh');
  callSite(4, 4, 'from=3&to=4', 'from=3&to=4');
  assert.deepEqual(painted, [false, true]);

  // And refreshStatus must actually route through it. The guard is only worth
  // anything at the call site, so assert the poll no longer re-asserts the old
  // window unconditionally and that it compares the sequence it LEFT under.
  assert.equal(script.match(/if\(speedFrozen\(\)\)\s*syncSpeedPanel\(true\);/), null,
    'refreshStatus still calls syncSpeedPanel(true) unconditionally when frozen');
  assert.ok(/const mySpeed=speedSeq;/.test(script),
    'refreshStatus must capture speedSeq before it awaits');
  assert.ok(/spdSyncPlan\(mySpeed, speedSeq, speedLoadedFor, speedWindowQuery\(\)\.q\)/.test(script),
    'refreshStatus must consult spdSyncPlan before painting the pinned panel');
});

// The RUN button offers "click to stop" for ANY running speedtest - the poll
// drives that from the backend's speedtest_running, so a scheduled run, a
// reconnect-triggered run, or one started in another tab all show it. The abort
// path must therefore cover the same set. It used to test only speedtestPending,
// a flag this tab sets when IT posts a run, so clicking the stop button on
// anything else started a second test, got the backend's already-running
// rejection, and left the original running.
test('speedtestAbortable: the stop path covers every run the button offers to stop', () => {
  // This tab's own run, once the poll has delivered its id: abortable.
  assert.equal(F.speedtestAbortable(true, false, 7), true);
  // A backend-originated run - the case that was broken.
  assert.equal(F.speedtestAbortable(false, true, 7), true);
  // Both, mid-handover between the POST and the poll.
  assert.equal(F.speedtestAbortable(true, true, 7), true);
  // Nothing running: a click means START, not stop.
  assert.equal(F.speedtestAbortable(false, false, 0), false);
  // A run whose id is not known yet - the optimistic paint before the poll
  // catches up - must NOT offer a stop: with no id the abort could only be the
  // id-less kind, which stops "whatever is running now" and can kill the NEXT
  // run if the one the operator meant ends while the confirm dialog is open.
  assert.equal(F.speedtestAbortable(true, false, 0), false);
  assert.equal(F.speedtestAbortable(false, true, 0), false);
  assert.equal(F.speedtestAbortable(true, true, 0), false);
});

test('speedtestBusy: any in-flight run is busy, id known or not', () => {
  assert.equal(F.speedtestBusy(true, false), true);
  assert.equal(F.speedtestBusy(false, true), true);
  assert.equal(F.speedtestBusy(false, false), false);
});

// The affordance and the abort read the same predicate, so the sub-poll window
// in which the id is unknown must LOOK different too: a button that says
// "click to stop" while a click can stop nothing (or worse, the wrong run) is
// the same lie the predicate was built to prevent. This drives the REAL
// setSpeedtestRunning against a fake button.
test('the RUN button does not offer to stop a run whose id it does not know', () => {
  const btn = { title: '', attrs: {}, classList: { toggle() {} },
    setAttribute(k, v) { this.attrs[k] = v; } };
  const when = { innerHTML: '', classList: { remove() {} } };
  const els = { runSpeed: btn, speedWhen: when };
  const defs = extract('function speedtestRunText') + '\n'
    + (script.includes('function speedtestBusy') ? extract('function speedtestBusy') + '\n' : '')
    + extract('function speedtestAbortable') + '\n'
    + extract('function setSpeedtestRunning');
  const set = new Function('$', 'esc', 'speedtestPending',
    'let speedtestRunningNow=false, speedtestRunId=0;\n' + defs + '\nreturn setSpeedtestRunning;')(
    id => els[id], s => String(s), false);
  set(true, '', false, '', 0);      // the optimistic paint: running, id unknown
  assert.ok(!/click to stop/.test(btn.title),
    'the button offers a stop it cannot deliver: with no id the click would send an id-less abort');
  assert.match(btn.title, /starting/i, 'the id-less window should read as a starting state');
  set(true, 'srv', false, '', 42);  // the poll delivered the id
  assert.match(btn.title, /click to stop/);
  set(false, '', false, '', 0);
  assert.match(btn.title, /Run a speedtest now/);
});

// The affordance and the action must be driven by the same state, not by two
// variables that happen to agree today.
test('the RUN click handler consults speedtestAbortable, not the local flag alone', () => {
  assert.ok(/if\(speedtestAbortable\(speedtestPending, speedtestRunningNow, speedtestRunId\)\)/.test(script),
    'the click handler must branch on speedtestAbortable(pending, running, runId)');
  assert.equal(/if\(speedtestPending\)\{\s*\n\s*if\(!confirm\('Stop the running speedtest/.test(script), false,
    'the old pending-only abort guard is still there');
  // setSpeedtestRunning is the single place that learns the backend state, so it
  // must be what records it.
  assert.ok(/function setSpeedtestRunning\(running,[^)]*\)\{\s*\n\s*speedtestRunningNow=!!running;/.test(script),
    'setSpeedtestRunning must record the backend running state it was handed');
  // The stop request must name the run the operator decided about, captured
  // BEFORE confirm() blocks the tab - by the time they answer, that run may have
  // finished and another started. Matching on the parameter list rather than this
  // invariant is what made the assertion above break when runId was added.
  assert.ok(/const target=speedtestRunId;\s*\n\s*if\(!confirm\('Stop the running speedtest/.test(script),
    'the run id must be captured before the confirm dialog, not after it');
  // The abort always names its run. The id-less form asked the daemon to stop
  // "whatever is running now" - Abort(0) - which bypasses the identity check the
  // id exists for, so the fallback must be gone, not merely rarely taken.
  assert.ok(/const q='\?run='\+encodeURIComponent\(target\);/.test(script),
    'the abort request must always carry the run id');
  assert.equal(/target\?\('\?run='/.test(script), false,
    'the id-less abort fallback is still there');
  // A busy click with no id yet must do NOTHING: not abort (no id to name), and
  // not fall through to the start path, where the POST would only collect the
  // daemon’s already-running rejection.
  assert.ok(/if\(speedtestBusy\(speedtestPending, speedtestRunningNow\)\) return;/.test(script),
    'a busy-but-id-less click must be swallowed, not turned into a second run');
});

// A POST to /api/logs answers with a fresh 500-line WINDOW, never a delta. The
// pane meanwhile accumulates well past that from the 2.5s delta polls, up to the
// ring's 4000 lines. Repainting from every POST therefore discarded the operator's
// scrollback - and the worst trigger was the Redact-PII switch, a pure display
// flip that had already been applied locally with no round-trip.
test('logPostAction: only a clear repaints the pane, and a clear is what rotates the epoch', () => {
  // Same epoch = the ring is unchanged; a level or redact flip must not repaint.
  assert.equal(F.logPostAction('ep-1', 'ep-1'), 'settings');
  // A clear rotates the epoch, so the pane must be replaced.
  assert.equal(F.logPostAction('ep-2', 'ep-1'), 'replace');
  // First load, nothing held yet.
  assert.equal(F.logPostAction('ep-1', null), 'replace');
});

test('postLogs consults logPostAction instead of repainting on every POST', () => {
  assert.ok(/if\(logPostAction\(d\.epoch, logEpoch\)==='replace'\)/.test(script),
    'postLogs must branch on logPostAction');
  assert.equal(/if\(my===logsSeq\)\{ setLogStall\(false\); renderLogs\(d, false\); \}/.test(script), false,
    'the unconditional repaint is still there, so a Redact flip still wipes scrollback');
});

// The server discloses thinning in X-Sampled / X-Total-Count. The dashboard threw
// those away, so the chart, the stat tiles and the three average pills all
// described a ~1500-run sample while the pills were labelled averages "across the
// range". A mean over a positional stride is not the range's mean - the stride
// keeps every Nth run, so periodic data aliases.
test('spdAvgNote: the pills admit when they describe a sample', () => {
  assert.equal(F.spdAvgNote(false, 900, 900), '', 'a complete window needs no caveat');
  const note = F.spdAvgNote(true, 40000, 1500);
  assert.match(note, /1500/, 'must say how many runs were actually averaged');
  assert.match(note, /40000/, 'must say how many are in the range');
});

test('the speed chart reads the thinning headers it is sent', () => {
  assert.ok(/r\.headers\.get\('X-Sampled'\)/.test(script),
    'refreshSpeedChart must read X-Sampled');
  assert.ok(/r\.headers\.get\('X-Total-Count'\)/.test(script),
    'refreshSpeedChart must read X-Total-Count');
  assert.ok(/spdAvgNote\(spdSampled,\s*spdTotal,\s*spdData\.length\)/.test(script),
    'paintSpdAvgs must pass the sampling state into the pill labels');
});

// The average pill rounds to whole milliseconds. A mean over a window of runs does
// not have tenth-of-a-millisecond resolution, so printing one implies precision
// the figure does not have. pingText - which renders a SINGLE run in the tile
// beside it - deliberately keeps its decimal.
test('pingAvgText: the average pill shows whole milliseconds', () => {
  assert.equal(F.pingAvgText(22.1), '22 ms');
  assert.equal(F.pingAvgText(22.5), '23 ms', 'rounds, not truncates');
  assert.equal(F.pingAvgText(22.4), '22 ms');
  assert.equal(F.pingAvgText(9.6), '10 ms');
  // 0 means "not probed", the same rule pingText follows and /metrics gates on.
  assert.equal(F.pingAvgText(0), '-');
  assert.equal(F.pingAvgText(null), '-');
  assert.equal(F.pingAvgText(undefined), '-');
});

test('the single-run ping tile keeps its decimal', () => {
  assert.equal(F.pingText(22.1), '22.1 ms', 'one measurement does resolve to a tenth');
  assert.ok(/\$\('sp_ping'\)\.textContent = pingText\(/.test(script),
    'the tile must still use pingText');
  // Pinned by which formatter each consumer calls, not by the whole argument list -
  // an argument-shape regex breaks on unrelated refactors and teaches people to
  // "fix" the test rather than read it.
  assert.ok(/setSpdAvg\('spdAvgPing'[^)]*pingAvgText\(/.test(script),
    'the average pill must use pingAvgText');
});

// The sampling caveat rides in the pill's ACCESSIBLE NAME, but setSpdAvg returns
// early when the VISIBLE text is unchanged - and the visible text is a rounded
// number. So when a window becomes sampled without the rounded average moving, the
// caveat is never written at all: the pill silently keeps describing a ~1500-run
// sample as the average across the whole range.
//
// The in-code comment on that early return says the accessible name "is derived
// from the same txt as the content, so it can never be left behind by this guard".
// That stopped being true the moment the name gained a note the text does not
// carry.
test('setSpdAvg: the sampling caveat lands even when the rounded value does not move', () => {
  fakePill = fakeEl();
  // Complete window first.
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps');
  assert.equal(fakePill.textContent, '50 Mbps');
  assert.equal(fakePill.getAttribute('aria-label'), 'average download 50 Mbps');

  // Now the same rounded figure, but the window is a SAMPLE - the caveat is in the
  // name only, so the visible text is byte-identical.
  const note = F.spdAvgNote(true, 40000, 1500);
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps', true, note);
  assert.match(fakePill.getAttribute('aria-label'), /1500/,
    'the caveat never reached the pill: the early return is gated on the rounded ' +
    'visible text, which did not change');
  assert.match(fakePill.getAttribute('aria-label'), /40000/);
});

// ...and leaving the sampled state must clear it again.
test('setSpdAvg: the caveat is removed when the window is complete again', () => {
  fakePill = fakeEl();
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps', true, F.spdAvgNote(true, 40000, 1500));
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps', false, '');
  assert.equal(fakePill.getAttribute('aria-label'), 'average download 50 Mbps',
    'a stale caveat survived the window becoming complete');
});

// The guard still has to do its job: paintSpdAvgs runs once per animation frame
// while the pointer is over a chart, so an unchanged pill must not be rewritten.
test('setSpdAvg: an entirely unchanged pill is still not rewritten', () => {
  // Count writes rather than mutating the node: overwriting textContent would make
  // it genuinely differ, so the guard would be right to write again and the test
  // would be measuring its own tampering.
  let textWrites = 0, attrWrites = 0;
  const counted = () => { let t = ''; return { get textContent() { return t; }, set textContent(v) { textWrites++; t = v; } }; };
  fakePill = {
    val: counted(), attrs: {}, classList: { toggle() {} },
    querySelector() { return this.val; },
    setAttribute(k, v) { attrWrites++; this.attrs[k] = v; },
    getAttribute(k) { return this.attrs[k]; },
  };
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps');
  const t0 = textWrites, a0 = attrWrites;
  F.setSpdAvg('spdAvgDn', 'average download', '50 Mbps');
  assert.equal(textWrites, t0, 'rewrote identical visible text');
  assert.equal(attrWrites, a0, 'rewrote an identical accessible name');
});

// The coverage note is shown when a window was not watched end to end. It treats
// anything under 99.9% as partial but printed the percentage with no decimals, so
// coverage between 99.5% and 99.9% rounded up to "100%" and the sentence
// contradicted itself: "monitored 100% of this window; the rest was not
// monitored". The reader is left unable to tell whether they are looking at a
// rounding artefact or a real gap.
test('upCovNote: a partial window never claims to be complete', () => {
  for (const cv of [0.995, 0.996, 0.998, 0.9989]) {
    const s = F.upCovNote(cv);
    assert.notEqual(s, '', `coverage ${cv} is partial and must say something`);
    assert.ok(!/monitored 100% of this window/.test(s),
      `coverage ${cv} rendered as "${s}" - it says 100% and then says the rest was not monitored`);
  }
});

test('upCovNote: the ordinary cases are unchanged', () => {
  assert.equal(F.upCovNote(null), '');
  assert.equal(F.upCovNote(1), '', 'full coverage says nothing');
  assert.equal(F.upCovNote(0.9995), '', 'above the threshold says nothing');
  assert.match(F.upCovNote(0), /nothing was monitored/);
  assert.match(F.upCovNote(0.5), /monitored 50% of this window/, 'a clear figure keeps its round form');
  assert.match(F.upCovNote(0.62), /monitored 62% of this window/);
  // Flooring must not turn "a little was watched" into "none was", which is the
  // same error the other way round; real zero has its own sentence.
  assert.match(F.upCovNote(0.004), /monitored <1% of this window/);
  assert.match(F.upCovNote(0), /nothing was monitored/);
});

// The whole-percent arm used toFixed(0), which rounds to NEAREST: every x.5-x.9
// coverage displayed one percent higher than measured (98.5% read "monitored
// 99%"), violating the floor guarantee the comment above the code promises. The
// cases above all sit where round==floor, so they never caught it.
test('upCovNote: a half-way figure floors, it never rounds coverage up', () => {
  assert.match(F.upCovNote(0.985), /monitored 98% of this window/);
  assert.match(F.upCovNote(0.989), /monitored 98% of this window/);
  assert.match(F.upCovNote(0.645), /monitored 64% of this window/);
  assert.match(F.upCovNote(0.115), /monitored 11% of this window/);
  assert.match(F.upCovNote(0.015), /monitored 1% of this window/);
  // The bands either side are untouched: >=99 keeps its floored decimal, and
  // below 1% the "<1" wording still owns it.
  assert.match(F.upCovNote(0.9905), /monitored 99\.0% of this window/);
  assert.match(F.upCovNote(0.009), /monitored <1% of this window/);
});

// Until now the coverage caveat reached the eye through a title attribute only.
// Phones do not show those at all and keyboard focus does not reliably surface one
// either, so a window that was half monitored but flawless while watched rendered
// as a bare 100.000% with nothing to say otherwise. (A screen reader did get it,
// out of the pill's accessible name.) The popover is the one place already reachable
// by tap and by keyboard, so the note goes there in visible text.
test('upCovFoot: the popover says out loud that a window is only partly covered', () => {
  const foot = F.upCovFoot(0.5, '7d');
  // The wording is upCovNote's alone - the footer must not paraphrase it, or the pill,
  // its accessible name and the popover start telling slightly different stories.
  assert.ok(foot.includes(F.upCovNote(0.5)), 'the note is not carried verbatim: ' + foot);
  // It names the window, because the popover lists several and the note itself says
  // only "this window".
  assert.ok(foot.includes('7d'), foot);
  assert.match(F.upCovFoot(0, '1d'), /nothing was monitored/);
  // Nothing to disclose, nothing rendered - no empty box at the foot of the popover.
  assert.equal(F.upCovFoot(1, '7d'), '', 'a fully covered window has no caveat');
  assert.equal(F.upCovFoot(null, '7d'), '', 'nor has one the server said nothing about');
  assert.equal(F.upCovFoot(undefined, '7d'), '');
  // And the popover actually renders it - a function nothing calls discloses nothing.
  assert.match(extract('function showUptimePop'), /upCovFoot\(/,
    'the popover does not put the note on screen');
});

// ...but the PILL is deliberately not outlined for it. The dashed marker the
// sampled speed averages carry says "this figure does not describe the whole of
// what it names", which is true of a partly-covered uptime too - except that
// coverage drops below 99.9% on any restart, pause or power-button toggle, so the
// marker would be lit more often than not and would stop reading as a caveat.
// The note itself is not dropped: it still reaches the title, the accessible name
// and the popover, where it can be stated in words instead of hinted at.
//
// Pinned at the source because the pill is a markup string built deep inside
// refreshStatus, which cannot be driven from here.
test('the uptime pill carries its coverage note without being outlined', () => {
  // The interactive pill, not the `upv==null` dash fallback beside it.
  const pill = script.match(/`<span class="pill stat-up"[^`]*role="button"[^`]*`/);
  assert.ok(pill, 'could not find the interactive uptime pill markup');
  assert.ok(!/sampled/.test(pill[0]),
    'the uptime pill is marked sampled again; that outline is on more often than off');
  assert.ok(!/\.stat-up\.sampled/.test(html),
    'the stylesheet still gives the uptime pill a dashed outline');
  // The caveat must survive losing its visual marker, in all three places.
  assert.match(pill[0], /title="\$\{upTitle\}"/, 'the note no longer reaches the title');
  assert.match(script, /upTitle = upNote \|\|/, 'upTitle no longer carries the note');
  assert.match(script, /upAria[\s\S]{0,200}upNote/, 'the accessible name no longer carries the note');
  assert.match(extract('function showUptimePop'), /upCovFoot\(/,
    'the popover no longer states the note');
  // The speed averages keep theirs - they are a different case, sampled only when
  // the range genuinely exceeds what the chart drew.
  assert.match(html, /\.chart-stat\.sampled\{border-style:dashed/,
    'the speed averages lost their marker too');
});

// The server caps a bare log read at 500 lines while retaining up to 4000, and
// reports both in the response. The viewer discarded them, so the pane - and the
// Copy button, which serialises what the pane holds - presented the newest slice as
// though it were the whole retained log.
test('logTruncNote: says so when the pane holds only part of the retained log', () => {
  assert.match(F.logTruncNote(500, 4000), /newest 500 of 4000/);
  assert.match(F.logTruncNote(500, 4000), /Download/, 'must point at the way to get all of it');
  // Nothing to say when the pane already has everything.
  assert.equal(F.logTruncNote(4000, 4000), '');
  assert.equal(F.logTruncNote(120, 120), '');
  // A pane that has grown past the initial tail via delta polls is complete too.
  assert.equal(F.logTruncNote(4000, 3999), '');
});

// logShownCount is the "shown" figure the truncation note discloses: what the
// pane holds once this response is applied. Only an append adds this response's
// lines to what is rendered; a skip leaves the pane exactly as it is, and using
// the response's (empty) line count there is what wrote "the newest 0 of N".
test('logShownCount: skip keeps the rendered count, replace takes the response, append adds', () => {
  const LS = new Function(extract('function logShownCount') + '\nreturn logShownCount;')();
  assert.equal(LS('skip', 500, 0), 500);
  assert.equal(LS('replace', 500, 120), 120);
  assert.equal(LS('append', 500, 2), 502);
});

// The note block's own comment says it is "driven off the entries actually
// rendered rather than off this response" - but the shown count was taken from
// the response's lines for every non-append mode. An idle delta poll (mode
// 'skip', the pane's steady state at the 2.5s cadence) carries zero lines, so
// 2.5s after the About tab opened a correct "newest 500 of 4000" was rewritten
// to "newest 0 of 4000" - and a COMPLETE 300-of-300 pane gained a fabricated
// truncation banner. This drives the REAL renderLogs with stub DOM, so it pins
// the wiring and not just the helper above.
function driveLogPane() {
  const parts = ['function logMerge', 'function logTruncNote'];
  if (script.includes('function logShownCount')) parts.push('function logShownCount');
  parts.push('function renderLogs');
  const defs = parts.map(extract).join('\n');
  const els = {
    logWindow: { contains: () => false },
    logTrunc: { textContent: '', hidden: true },
  };
  const state = 'let logMaskInit = true, logMasked = false, logEpoch = null, logCursor = null;\n'
    + 'let logPainted = false, logEntries = [];\nconst LOG_VIEW_MAX = 4000;\n'
    + 'const paintLogs = () => { logPainted = true; };\n'
    + 'const appendLogs = () => {};\nconst updateLogDownload = () => {};\n';
  const render = new Function('$', 'window', state + defs + '\nreturn renderLogs;')(
    id => els[id], { getSelection: () => null });
  return { render, note: () => ({ text: els.logTrunc.textContent, hidden: els.logTrunc.hidden }) };
}
const logLines = n => Array.from({ length: n }, (_, i) => ({ raw: 'l' + i, masked: 'l' + i }));

test('log truncation note: an idle delta poll does not rewrite the shown count', () => {
  const { render, note } = driveLogPane();
  // First load: the newest 500 of a 4000-line ring.
  render({ epoch: 'r1', lines: logLines(500), next_seq: 4000, dropped: 0, buffered: 4000 }, false);
  assert.match(note().text, /newest 500 of 4000/);
  // The steady state: a quiet 2.5s delta ('skip'). The pane still shows 500 lines.
  render({ epoch: 'r1', lines: [], next_seq: 4000, dropped: 0, buffered: 4000 }, true);
  assert.match(note().text, /newest 500 of 4000/,
    'an empty delta rewrote the note from the response instead of the rendered pane');
  assert.equal(note().hidden, false);
  // New lines still advance the count - the append arm was always right.
  render({ epoch: 'r1', lines: logLines(2), next_seq: 4002, dropped: 0, buffered: 4000 }, true);
  assert.match(note().text, /newest 502 of 4000/);
});

test('log truncation note: a complete pane stays unbannered across idle polls', () => {
  const { render, note } = driveLogPane();
  render({ epoch: 'r1', lines: logLines(300), next_seq: 300, dropped: 0, buffered: 300 }, false);
  assert.equal(note().hidden, true, 'the whole ring is on screen; there is nothing to disclose');
  render({ epoch: 'r1', lines: [], next_seq: 300, dropped: 0, buffered: 300 }, true);
  assert.equal(note().hidden, true, 'an idle poll fabricated a truncation banner for a complete pane');
  assert.equal(note().text, '');
});

// The 2.5s poll must not be gated on the log-level switch. That switch decides
// whether the daemon produces lines; a tab with logging off still has to notice a
// Clear performed in another tab, which is signalled by the epoch changing.
test('the log poll is not gated on the logging switch', () => {
  assert.equal(/setInterval\(\(\)=>\{ if\(!document\.hidden && \$\('setLogOn'\) && \$\('setLogOn'\)\.checked/.test(script), false,
    'the poll still requires logging to be on, so a tab with it off never sees a clear');
  assert.ok(/setInterval\(\(\)=>\{ if\(!document\.hidden\s*\n\s*&& \$\('drawer'\)\.classList\.contains\('open'\)/.test(script),
    'the poll should run whenever the About tab is open and visible');
});

// --- the Connection panel when no lookup is coming -------------------------
//
// Automatic connection lookups are gated on BOTH the connection-info setting and
// the monitoring power button (main.go: EnabledFn = Monitoring() && NetinfoEnabled()).
// With no cached snapshot and neither of those on, nothing will ever fetch one, so
// the panel must say so and stop asking. Saying "gathering…" and re-polling every
// 3s forever promises work that is not happening.
//
// This drives the REAL refreshNetinfo out of index.html with stubs, so it fails if
// the shipped early-return logic changes.
function driveNetinfo({ netinfoOff = false, monitoring = true, snapshot = {} } = {}) {
  const body = extract('async function refreshNetinfo');
  const panel = { innerHTML: '' };
  const warn = { innerHTML: '', classList: { add() {}, remove() {} } };
  const refreshBtn = { classList: { contains: () => false } };
  const els = { netinfo: panel, netinfoWarn: warn, netinfoRefresh: refreshBtn };
  const scheduled = [];
  const $ = id => els[id] || { innerHTML: '', classList: { add() {}, remove() {}, contains: () => false },
    setAttribute() {}, getAttribute() {}, hidden: false };
  // netinfoIdleReason is pulled from the same source, not restated here: the
  // whole point of the fix is that one answer drives both call sites.
  const idle = script.includes('function netinfoIdleReason') ? extract('function netinfoIdleReason') : '';
  const make = new Function(
    '$', 'fget', 'syncNetinfoOffMark', 'netinfoOff', 'monitoring', 'setTimeout', 'clearTimeout',
    'labelInfoBubbles', 'esc',
    idle + '\nlet netinfoSeq = 0, netinfoRetry = null;\n' + body + '\nreturn refreshNetinfo;');
  const fn = make(
    $,
    async () => ({ json: async () => snapshot }),
    () => {},
    netinfoOff, monitoring,
    (f, ms) => { scheduled.push(ms); return 1; },
    () => {},
    () => {},
    s => String(s),
  );
  return fn().then(() => ({ html: panel.innerHTML, polls: scheduled.length }));
}

test('connection panel: nothing cached and monitoring paused -> says so, stops polling', async () => {
  const { html, polls } = await driveNetinfo({ monitoring: false, snapshot: {} });
  assert.ok(!/gathering/.test(html),
    'the panel says "gathering…" while the monitor is paused, so no lookup is coming and none ' +
    'will: automatic lookups need the power button on. It is describing work that is not ' +
    'happening. Got: ' + html);
  assert.equal(polls, 0,
    'it also re-polls every 3s forever waiting for a snapshot nothing will ever fetch');
});

test('connection panel: nothing cached and connection info off -> unchanged behaviour', async () => {
  const { html, polls } = await driveNetinfo({ netinfoOff: true, snapshot: {} });
  assert.match(html, /not looked up yet/);
  assert.equal(polls, 0);
});

test('connection panel: nothing cached but monitoring on -> still gathers and polls', async () => {
  const { html, polls } = await driveNetinfo({ monitoring: true, snapshot: {} });
  assert.match(html, /gathering/, 'a healthy first run must still show progress');
  assert.equal(polls, 1, 'and keep polling until the first snapshot lands');
});

// Stopping the poll when nothing is coming (above) means something has to start it
// again. Turning monitoring back on is that something: the panel is sitting on
// "not looked up yet" with no timer pending, so unless the resume triggers a
// render the panel stays stuck on a message that is no longer true.
//
// setPowerUI runs on EVERY status poll, so it must fire only on the transition -
// a refresh per poll would hammer the endpoint the panel just stopped calling.
function drivePower(sequence) {
  const body = extract('function setPowerUI');
  const btn = { classList: { toggle() {} }, setAttribute() {}, title: '' };
  const calls = [];
  const make = new Function('$', 'syncNetinfoOffMark', 'refreshNetinfo', 'monitoring',
    body + '\nreturn { setPowerUI, state: () => monitoring };');
  const api = make(() => btn, () => {}, () => calls.push(1), sequence.shift());
  for (const on of sequence) api.setPowerUI(on);
  return calls.length;
}

test('turning monitoring back on releases the connection panel, once', () => {
  assert.equal(drivePower([false, true]), 1,
    'resuming must re-render the Connection panel: the poll stopped while paused, so nothing ' +
    'else will clear "not looked up yet"');
  assert.equal(drivePower([true, true, true]), 0,
    'an unchanged power state must not refresh - setPowerUI runs on every status poll');
  assert.equal(drivePower([true, false]), 0,
    'pausing must not trigger a lookup that the pause exists to stop');
});

// --- the Import button's upload ---------------------------------------------
//
// The server streams an import in and imposes no ceiling on the body, and a
// default export of a year's monitoring runs to hundreds of megabytes. Reading
// the file with f.text() first defeats all of that in the browser: the whole
// backup becomes a JS string, and fetch then makes a second copy of it as UTF-8
// bytes. Handing fetch the File instead lets the browser stream it off disk.
//
// This drives the REAL click handler out of index.html against stubs, so it fails
// if the shipped upload changes shape.
function driveImport({ ok = true, resp = {}, fetchFails = false } = {}) {
  let handler = null;
  const file = { name: 'backup.json', size: 290 * 1024 * 1024, reads: 0,
    async text() { this.reads++; return '{}'; } };
  const btn = { disabled: false, addEventListener: (_ev, fn) => { handler = fn; } };
  const msg = { textContent: '' };
  const els = { importBtn: btn, importFile: { files: [file] }, importMsg: msg,
    outagesSection: { style: { display: 'none' } } };
  const sent = { body: undefined, headers: null, disabledMidUpload: null };
  const fetchStub = async (_url, opt) => {
    sent.body = opt.body; sent.headers = opt.headers;
    sent.disabledMidUpload = btn.disabled; // a second click here would upload twice
    if (fetchFails) throw new Error('network went away');
    return { ok, status: ok ? 200 : 500, json: async () => resp,
      text: async () => JSON.stringify(resp) };
  };
  const noop = async () => {};
  const register = new Function('$', 'getCats', 'fetch', 'confirm', 'loadSettings',
    'loadAccess', 'formSnapshot', 'refreshStatus', 'refreshChart', 'refreshSpeedChart',
    'refreshHeatmap', 'loadOutages',
    'let savedBody = null;\n' + extract("$('importBtn').addEventListener('click'") + ');');
  register(id => els[id], () => ['pings'], fetchStub, () => true, noop, noop,
    () => '', () => {}, () => {}, () => {}, () => {}, noop);
  return handler().then(() => ({ file, btn, msg, sent }));
}

test('import hands the file to fetch instead of reading it into memory', async () => {
  const { file, sent } = await driveImport();
  assert.equal(file.reads, 0,
    'the whole backup was read into a JS string first - a 290 MB export becomes ~580 MB ' +
    'of browser memory once fetch encodes it, which is how a phone runs out');
  assert.equal(sent.body, file,
    'fetch must be given the File itself so the browser streams it from disk');
  // Load-bearing, not decoration: a File picked with no MIME type sends no
  // Content-Type at all, and the import handler answers that with a 415.
  assert.equal(sent.headers['Content-Type'], 'application/json',
    'the explicit Content-Type must survive - the File cannot be relied on to carry one');
});

test('import will not take a second click while the first is still uploading', async () => {
  const done = await driveImport();
  assert.equal(done.sent.disabledMidUpload, true,
    'the button stays live during a multi-minute upload, so an impatient second click ' +
    'imports the same file twice');
  assert.equal(done.btn.disabled, false, 'and it must come back after a success');
  const refused = await driveImport({ ok: false, resp: { error: 'nope' } });
  assert.equal(refused.btn.disabled, false, 'a refused import must not leave it stuck');
  const broken = await driveImport({ fetchFails: true });
  assert.equal(broken.btn.disabled, false, 'nor must a connection that dies mid-upload');
});

// B1: the per-outage delete button gates on outageDeletable, which must match the
// server's guard (ts=? AND type='up') - including a repair-nulled 'up' row whose
// duration is gone (has_duration:false). Gating on has_duration hid the button for
// exactly the rows the server still accepts for deletion.
test('outageDeletable: every closing up event is deletable, even a repair-nulled one', () => {
  assert.equal(F.outageDeletable({ type: 'up', has_duration: true, duration_s: 180 }), true);
  assert.equal(F.outageDeletable({ type: 'up', has_duration: false }), true); // repair nulled the duration - server still deletes it
  assert.equal(F.outageDeletable({ type: 'down' }), false);                   // an in-progress outage can't be deleted
  assert.equal(F.outageDeletable(null), false);
});

// A5: the background chart pollers must recognise the restore-reconcile 503 (an
// expected, self-clearing pause) so they suppress the failure toast and back off
// Retry-After instead of treating it as a load failure.
test('isReconcile503: matches the reconcile 503 by Retry-After or body, nothing else', () => {
  const hdr = v => ({ get: k => (k === 'Retry-After' ? v : null) });
  assert.equal(F.isReconcile503({ status: 503, headers: hdr('2') }), true);   // Retry-After signal
  assert.equal(F.isReconcile503({ status: 503, headers: hdr(null) }, 'restoring a backup; try again shortly'), true); // body fallback
  assert.equal(F.isReconcile503({ status: 503, headers: hdr(null) }, ''), false); // a plain 503 with neither signal is a real failure
  assert.equal(F.isReconcile503({ status: 500, headers: hdr('2') }), false);  // wrong status
  assert.equal(F.isReconcile503(null), false);
  assert.equal(F.retryAfterMs('2'), 2000);
  assert.equal(F.retryAfterMs(null), 2000);   // absent -> the server's default
  assert.equal(F.retryAfterMs('99999'), 60000); // clamped so a hostile header can't wedge the loop
});

// ONLY A SEARCHED CITY MAY BE NAMED AS AUTO'S CENTRE. The dropdown used to read
// "Auto - fastest near Miami" from the BROWSING list's centre, which live auto
// does not use - and on the measured link that centre was a Cloudflare PoP a
// country from every city auto races. With no searched city there is no place
// to name at all: the centre is raced fresh on every test.
test('autoOptionText names a searched city, never the browse centre', () => {
  assert.equal(F.autoOptionText('Montreal'), 'Auto - fastest in Montreal');
  assert.equal(F.autoOptionText(''), 'Auto - fastest near you');
  // "near you" describes the candidates - every city the race considers is an
  // attempt to locate US - so it stays true whichever one wins. What must never
  // come back is a NAMED city that auto did not choose: the browse centre.
  assert.ok(!/Miami|Montreal/.test(F.autoOptionText('')));
});

test('a fallback browse centre is never credited to auto', () => {
  const t = F.autoScopeText('the fastest server', '', 'Miami', '');
  assert.ok(/races the cities/.test(t), t);
  assert.ok(/for browsing/.test(t), t);
  // The two claims the old string made, both false: that auto picks near this
  // place, and that the place is the ISP's exit.
  assert.ok(!/picks .* near/.test(t), t);
  assert.ok(!/exit/.test(t), t);
});

test('a searched city wins outright and the browse centre goes unmentioned', () => {
  assert.equal(F.autoScopeText('the 3 fastest servers', 'Montreal', 'Miami', 'Example ISP, Oldtown'),
    'Auto picks the 3 fastest servers in <b>Montreal</b>.');
});

test('with no browse centre the sentence is still true', () => {
  const t = F.autoScopeText('the fastest server', '', '', '');
  assert.ok(/for browsing/.test(t), t);
  assert.ok(!/<b>/.test(t), t);
});

// THE CENTRE MAY BE NAMED AS WHERE AUTO LAST LANDED - past tense, and only
// when the daemon says so (centre 'last_run': the browse list was centred on
// the last auto run's server). A fallback centre keeps the weaker "may test
// from a different city" wording: before any auto run there is nothing to
// remember, and crediting a candidate city to auto is the old defect back.
test('last-run centring is named in the past tense, fallback centring is not', () => {
  const t = F.autoScopeText('the fastest server', '', 'Newtown, QC', 'Example ISP, Newtown', true);
  assert.ok(/centred on <b>Newtown, QC<\/b>, where your last auto test ran/.test(t), t);
  assert.ok(!/different city/.test(t), t);
  const f = F.autoScopeText('the fastest server', '', 'Oldtown', 'Example ISP, Newtown', false);
  assert.ok(/auto may test from a different city/.test(f), f);
  assert.ok(!/last auto test ran/.test(f), f);
  // The flag without a centre has nothing to name; the sentence must not dangle.
  assert.ok(!/last auto test ran/.test(F.autoScopeText('the fastest server', '', '', '', true)));
});

// Reports what the last run MEASURED, in the past tense, so it stays true
// however that server was chosen - and it is the honest answer to "which city
// did auto use?" without the daemon remembering a race.
test('the last measured server is reported, and only when there is one', () => {
  assert.ok(/last test measured <b>Example ISP, Oldtown<\/b>/.test(
    F.autoScopeText('the fastest server', '', 'Oldtown', 'Example ISP, Oldtown')));
  assert.ok(!/last test measured/.test(F.autoScopeText('the fastest server', '', 'Oldtown', '')));
});

test('the panel escapes place names the daemon supplies', () => {
  assert.ok(F.autoScopeText('x', '', '<img src=x>', '').includes('&lt;img'));
  assert.ok(F.autoScopeText('x', '<img src=y>', '', '').includes('&lt;img'));
  // The past-tense branch interpolates a server-derived place of its own.
  assert.ok(F.autoScopeText('x', '', '<img src=z>', '', true).includes('&lt;img'));
});

// A 30-day window returns hundreds of runs into ~900px. Below one bar per ~2px
// the bars touch and the strip shows density instead of level, so columns are
// binned - but only then: a short window must keep every run's own bar.
test('bloatBins leaves a sparse window alone', () => {
  const f = { dDn: p => p.dn, dUp: p => p.up, dDnX: p => p.dnX, dUpX: p => p.upX };
  const pts = [0, 1, 2, 3].map(i => ({ ts: i, dn: 5 + i, up: 6, dnX: 40, upX: 40 }));
  const X = t => 100.4 + t * 200;             // 200px apart: nothing collides
  const out = F.bloatBins(pts, f, X, 1.6);
  assert.equal(out.length, 4);
  assert.equal(out[0].dn, 5);
  assert.equal(out[3].dn, 8);
  // Positions stay EXACT when nothing needed binning. A binned bar sits on a
  // rounded pixel column, but the hover cursor is drawn at the unrounded X, so
  // rounding a window that did not need it would walk the bar off its own cursor.
  assert.equal(out[0].px, 100.4);
  assert.equal(out[3].px, 700.4);
});

test('bloatBins collapses a dense window to one bar per pixel column', () => {
  const f = { dDn: p => p.dn, dUp: p => p.up, dDnX: p => p.dnX, dUpX: p => p.upX };
  // 400 runs across 100px - far past the point where 1.6px bars overlap.
  const pts = Array.from({ length: 400 }, (_, i) => ({ ts: i, dn: 5, up: 6, dnX: 40, upX: 40 }));
  const X = t => 10 + t * (100 / 399);
  const out = F.bloatBins(pts, f, X, 1.6);
  assert.ok(out.length < 120, `binned to ${out.length} bars, want about one per pixel column`);
  assert.ok(out.length > 50, `binned to ${out.length} - collapsed too far`);
});

// The two marks mean different things, so they reduce differently. The solid bar
// is a LEVEL: a column takes the median so the trend sits where the runs sat. The
// pale tail is a SPIKE: a column takes the maximum, because one bad run inside a
// busy column must not be averaged out of existence by its neighbours.
test('bloatBins takes the median level but the peak tail', () => {
  const f = { dDn: p => p.dn, dUp: p => p.up, dDnX: p => p.dnX, dUpX: p => p.upX };
  const pts = [
    { ts: 0, dn: 5, up: 5, dnX: 20, upX: 20 },
    { ts: 1, dn: 7, up: 7, dnX: 900, upX: 900 },   // the spike
    { ts: 2, dn: 9, up: 9, dnX: 30, upX: 30 },
  ];
  const X = t => 50 + t * 0.2;                     // sub-pixel apart: one column
  const out = F.bloatBins(pts, f, X, 1.6);
  assert.equal(out.length, 1);
  assert.equal(out[0].dn, 7, 'level should be the median of 5/7/9');
  assert.equal(out[0].dnX, 900, 'the spike must survive binning');
});

// A threshold far above what the link does must not own the axis. Set a 200ms
// bufferbloat limit on a line that bloats 5-15ms and the old ceiling scaled to
// the limit, flattening every bar into the baseline - the chart went blank to
// make room for a line that was never going to be crossed.
test('bloatCeiling ignores a threshold that is nowhere near the data', () => {
  const far = F.bloatCeiling(15, 200);
  assert.ok(far <= 20, `ceiling ${far} was stretched to a distant 200ms limit`);
  // A limit close to the data still belongs in scale - that is what makes it
  // useful to see.
  assert.ok(F.bloatCeiling(100, 150) >= 150, 'a nearby limit must stay visible');
});

// Rounded to a stable step, so the axis number stops drifting between refreshes
// while the underlying data wanders a few ms.
test('bloatCeiling rounds to a stable step', () => {
  const a = F.bloatCeiling(23.4, 0), b = F.bloatCeiling(24.1, 0);
  assert.equal(a, b, `ceiling moved from ${a} to ${b} on a 0.7ms change in the data`);
  assert.ok([1, 2, 5, 10, 20, 50, 100, 200, 500, 1000].includes(a), `ceiling ${a} is not a round step`);
});

// THE THREE ICONS HAVE TO SHARE A CAP HEIGHT. As characters they could not: the
// sine is U+223F SINE WAVE, a math operator drawn about the math axis, so it
// occupies roughly x-height while the arrows span nearly the full ascender - it
// read as crushed beside them at any shared font-size. All three also fell out
// of the page font's Latin subset to whatever system-ui resolved to, so their
// relative sizes were the OS's choice. Drawn as SVG they share one height and
// one stroke weight by construction.
test('the average pills draw their icons instead of typing them', () => {
  for (const id of ['spdAvgDn', 'spdAvgUp', 'spdAvgPing']) {
    assert.ok(new RegExp(`id="${id}"[^>]*><i class="stat-face"><svg`).test(html),
      `${id} does not draw its icon; a text glyph cannot be matched to the others`);
  }
  // One rule pins HEIGHT for all three and lets width follow each viewBox, which
  // is what makes a wide wave and a narrow arrow the same size to the eye.
  assert.ok(/\.chart-stat \.stat-face svg\{height:[^;]+;width:auto/.test(html),
    'the icons are not height-matched with free width');
  // Decorative: the pill carries role="img" and the accessible name.
  const icons = html.match(/<i class="stat-face"><svg[^>]*>/g) || [];
  assert.equal(icons.length, 3);
  for (const i of icons) assert.ok(/aria-hidden="true"/.test(i), 'an icon is exposed to assistive tech');
  for (const i of icons) assert.ok(/stroke="currentColor"/.test(i), 'an icon does not follow the pill colour');
  // The number still needs separating from the icon now that no space joins them.
  assert.ok(/\.chart-stat\{[^}]*gap:/.test(html),
    'nothing separates the icon from the value');
});

// THE HEATMAP TINTS BY OUTAGE COUNT, NOT BY DOWNTIME. The README described the
// cells as "per-day outage time", "tinted by how much outage time it saw" - the
// one thing the fill does not encode. A day with a single 23-hour outage is
// level 1, exactly like a day with a single one-second blip; three trivial blips
// outrank both.
//
// This pins the semantics the README now states. If the fill is ever changed to
// encode duration (a reasonable thing to want), this test fails and whoever
// changes it is told to go fix the prose too.
test('heatmap level counts outages and ignores how long they lasted', () => {
  // Lifted the same way driveHeatmap lifts it: a one-line arrow const, which
  // extract() cannot brace-match.
  const hmLevel = new Function(
    script.match(/const hmLevel = [^;]*;/)[0] + '\nreturn hmLevel;')();
  assert.equal(hmLevel(0), 0, 'a clean day is empty');
  assert.equal(hmLevel(1), 1);
  assert.equal(hmLevel(2), 2);
  assert.equal(hmLevel(3), 3);
  assert.equal(hmLevel(99), 3, 'the ramp saturates at 3');

  // The claim in prose form: one outage is one outage, whatever it cost.
  const dayWithOneLongOutage = hmLevel(1);   // 23h down
  const dayWithOneBlip = hmLevel(1);         // 1s down
  const dayWithThreeBlips = hmLevel(3);      // 3s down total
  assert.equal(dayWithOneLongOutage, dayWithOneBlip,
    'duration must not affect the level - that is the documented behaviour');
  assert.ok(dayWithThreeBlips > dayWithOneLongOutage,
    'three brief outages outrank one long one; the README must not claim otherwise');
});

// ONE BAD MOMENT MUST NOT OWN THE AXIS. The bufferbloat chart reduced its p95
// tails with a median (robust) but its TYPICAL values with a plain max, so a
// single run whose upload median hit 256ms set the ceiling to 500 while every
// other bar in the window sat around 8ms. The chart went flat to make room for
// one spike the overflow caret was going to mark anyway.
test('bloatDataMax: a single spike does not set the scale', () => {
  // 41 ordinary runs around 8ms and one catastrophic one, both directions.
  const typical = Array(82).fill(8.4).concat([256.1, 30.0]);
  const tails = Array(82).fill(13.5).concat([1014.7, 95.1]);

  const dm = F.bloatDataMax(typical, tails);
  assert.ok(dm < 60, `ceiling input ${dm} is still being dragged up by the outlier`);
  assert.ok(dm >= 13.5, 'the typical tail level must still fit under the ceiling');

  // The whole point, stated as the user sees it: the axis stays within a small
  // multiple of the level the bars actually occupy.
  assert.ok(F.bloatCeiling(dm, 0) <= 8.4 * 12,
    'the axis is more than 12x the typical bar - the chart reads as flat');
});

// The clamp must not become a lie in the other direction: a link that really is
// this bad has to scale up, or the chart would clip everything and say nothing.
test('bloatDataMax: a genuinely bad link still gets a big axis', () => {
  const typical = Array(40).fill(300);
  const tails = Array(40).fill(420);
  const dm = F.bloatDataMax(typical, tails);
  assert.ok(dm >= 300, `sustained 300ms bloat must raise the ceiling, got ${dm}`);
});

test('bloatDataMax: floors a quiet link and survives empty input', () => {
  assert.equal(F.bloatDataMax([], []), 20, 'no data still needs a sane axis');
  assert.equal(F.bloatDataMax([0.2, 0.4, 0.1], [0.5]), 20,
    'a link with no bloat should not magnify sub-millisecond noise');
  // The tail median counts even when the typical values are tiny.
  assert.ok(F.bloatDataMax(Array(20).fill(1), Array(20).fill(75)) >= 75,
    'a sustained tail level must fit under the ceiling');
});

// ONE SELECTION, THREE CHARTS, ONE LINE. The selection cursor marks the same run
// down a stack of three charts, so it has to read as a single continuous line.
// Each chart used to anchor it to its own plot box, and those boxes reserve
// padding for their own data: the speed chart keeps 10px at the top so a line at
// the ceiling is not clipped, the bufferbloat chart keeps 8px at the bottom for
// the caret its downward bars clip into. The result was three different lengths,
// the bufferbloat one visibly stopping short of the floor.
test('cursorSpan: the selection cursor is the same height on every chart', () => {
  const h = 150;
  // Whatever each chart reserves internally, the cursor spans the canvas.
  assert.deepEqual(F.cursorSpan(h, false), { top: 0, bot: h });
  // Three charts, same canvas height, no axis: identical spans.
  const spans = [false, false, false].map(() => F.cursorSpan(h, false));
  assert.deepEqual(spans[0], spans[1]);
  assert.deepEqual(spans[1], spans[2]);
});

test('cursorSpan: only the x-axis label strip is excluded', () => {
  const h = 150;
  const plain = F.cursorSpan(h, false), axis = F.cursorSpan(h, true);
  assert.equal(axis.top, 0, 'the cursor always starts at the canvas top');
  assert.ok(axis.bot < plain.bot,
    'the chart drawing the timestamps must stop above them, not run dashes through');
  assert.ok(axis.bot > h * 0.5, 'but it must still cross the great majority of the chart');
});

// The rule is worth nothing if a chart goes back to using its own padding, and
// that is exactly how the three drifted apart in the first place - each was
// edited for its own reasons and nothing tied them together.
test('every speed chart takes its cursor span from the shared rule', () => {
  const calls = script.split('\n')
    .filter(l => l.includes('drawCursor(') && !l.includes('function drawCursor'));
  assert.ok(calls.length >= 6, `expected the three charts' six cursor draws, found ${calls.length}`);
  for (const l of calls) {
    assert.ok(/cs\.top,\s*cs\.bot/.test(l),
      `a cursor is drawn from something other than cursorSpan: ${l.trim()}`);
    assert.ok(/,\s*sy\)/.test(l),
      `a cursor is drawn without its stack offset, so its dashes restart: ${l.trim()}`);
  }
  // And all three ask for it the same way.
  assert.equal((script.match(/cursorSpan\(h,\s*mine\)/g) || []).length, 3,
    'each of the three speed charts must derive its cursor span from cursorSpan(h, mine)');
});

// A recording 2d context: enough for drawCursor, and it REMEMBERS the state it
// was handed so a test can prove the function does not inherit it.
const penCtx = (pre = {}) => ({
  lineWidth: 2, globalAlpha: 1, strokeStyle: '', lineDashOffset: 0, dash: null,
  ops: [], ...pre,
  save() { this.ops.push('save'); }, restore() { this.ops.push('restore'); },
  setLineDash(d) { this.dash = d; }, beginPath() {}, moveTo() {}, lineTo() {},
  stroke() { this.ops.push({ lw: this.lineWidth, alpha: this.globalAlpha, off: this.lineDashOffset }); },
});

// THE CURSOR MUST NOT INHERIT THE CHART'S PEN. Every stroke property in
// drawCursor was set explicitly except lineWidth, so the one shared selection
// marker came out at two thicknesses: the speed and quality charts set
// lineWidth to TC.lw (appearance slider, default 2) drawing their data lines,
// while the bufferbloat chart draws bars with fillRect and its caret with
// fill(), touching lineWidth not at all and leaving the canvas reset value of 1.
// It also moved with settings - threshLine drops lineWidth to 1, so enabling a
// threshold thinned that chart's cursor.
test('drawCursor: the same width whatever the chart was drawing with', () => {
  // Two contexts left in the states the real charts leave them in.
  const afterLines = penCtx({ lineWidth: 2 });   // speed/quality: TC.lw
  const afterBars = penCtx({ lineWidth: 1 });    // bufferbloat: never set
  const afterThresh = penCtx({ lineWidth: 4 });  // and an arbitrary third

  for (const c of [afterLines, afterBars, afterThresh]) F.drawCursor(c, 10, 0, 100, true, 0);

  const widths = [afterLines, afterBars, afterThresh].map(c => c.ops.find(o => o.lw !== undefined).lw);
  assert.deepEqual(widths, [1, 1, 1],
    `the cursor drew at ${widths.join('/')} px depending on what the chart drew last`);
});

test('drawCursor: still dashed, still phase-shifted, still dimmer when not selected', () => {
  const a = penCtx(), b = penCtx();
  F.drawCursor(a, 10, 0, 100, true, 14);
  F.drawCursor(b, 10, 0, 100, false, 0);
  assert.deepEqual(a.dash, [3, 3], 'the cursor must stay dashed');
  assert.equal(a.ops.find(o => o.off !== undefined).off, 14 % 6,
    'the stack offset must still shift the dash phase');
  const alphaSel = a.ops.find(o => o.alpha !== undefined).alpha;
  const alphaHover = b.ops.find(o => o.alpha !== undefined).alpha;
  assert.ok(alphaSel > alphaHover, 'the pinned selection stays stronger than a hover');
  // and it must not leak its pen back to the caller
  assert.equal(a.ops[0], 'save');
  assert.equal(a.ops[a.ops.length - 1], 'restore');
});

// THE GAPS BETWEEN THE THREE CHARTS CARRY MEANING - AND LIVE INSIDE THE
// CANVASES. Speed and quality are two line charts sharing one x axis and read
// as a pair, so they sit tight together; bufferbloat is a different kind of
// chart - diverging about a centre zero, with its own scale - and the wider
// gap is what says so.
//
// Two failed shapes, both pinned against here. (1) The grouping was once
// flattened to a uniform 2px while chasing cursor continuity - uniformity
// looked like an improvement in the diff and was a regression on screen, so
// the spacing itself must stay (bufferbloat clearly apart, the pair joined).
// (2) The spacing then lived as CSS margin-top on the canvases - but a margin
// is a strip NO canvas can paint, so the shared selection cursor visibly broke
// across the 18px before bufferbloat; dash phase (drawCursor's stackY) aligns
// the pattern across canvases but cannot draw in a gap between them. So the
// spacing lives INSIDE each canvas as its plot pad.t, the canvases sit flush,
// and the full-canvas cursor runs through the spacing unbroken. Grouping and
// continuity, both.
test('the chart stack spaces bufferbloat apart, inside the canvases', () => {
  // No dead strips: the stacked pair below the speed chart must carry no
  // margin, or the cursor breaks there again.
  for (const id of ['qualityChart', 'bloatChart']) {
    const tag = html.match(new RegExp(`<canvas id="${id}"[^>]*>`))[0];
    assert.ok(!/margin/.test(tag),
      `${id} has a CSS margin - a strip the cursor cannot paint: ${tag}`);
  }
  // The grouping spacing, now as in-canvas top padding on the chart plots.
  const padT = fn => {
    const m = script.match(new RegExp(`spdAxisOwner\\(points\\)==='${fn}';\\r?\\n\\s*const pad=\\{[^}]*t:(\\d+)`));
    assert.ok(m, `${fn}: pad literal not found beside its axis-owner line`);
    return Number(m[1]);
  };
  const quality = padT('qualityChart'), bloat = padT('bloatChart');
  assert.ok(quality <= 4,
    `quality reserves ${quality}px under speed; the pair must stay visually joined`);
  assert.ok(bloat >= 12,
    `bufferbloat reserves only ${bloat}px under quality - it reads as part of the pair above it`);
  assert.ok(bloat > quality * 3,
    'the break before bufferbloat must be clearly larger than the one inside the pair');
});

// WCAG AA WANTS 4.5:1 FOR NORMAL-SIZE TEXT, AND THESE TOKENS ARE NORMAL-SIZE
// TEXT. --muted/--up/--down/--warn colour 11-13px headings, log lines, badges
// and status readouts on .panel and .card, whose backgrounds are gradients
// between --panel and --panel2 - so a token has to clear the bar against BOTH
// ends, not just the one that flatters it.
//
// Five of the eight built-in themes shipped below the bar (twelve token/surface
// pairs, worst 2.38:1). Eyeballing colour is exactly the thing people cannot do,
// so this computes it: any theme added or retuned later is measured the same way
// rather than trusted.
const srgb = h => {
  let x = h.replace('#', '');
  if (x.length === 3) x = [...x].map(c => c + c).join('');
  return [0, 2, 4].map(i => parseInt(x.slice(i, i + 2), 16) / 255);
};
const relLuminance = h => {
  const [r, g, b] = srgb(h).map(c => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const contrast = (a, b) => {
  const [hi, lo] = [relLuminance(a), relLuminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};

test('contrast: relLuminance/contrast agree with the WCAG reference points', () => {
  // Anchors from the spec itself, so a broken formula cannot quietly pass the
  // theme sweep below by rating everything as fine.
  assert.equal(Math.round(contrast('#ffffff', '#000000')), 21);
  assert.equal(Math.round(contrast('#ffffff', '#ffffff')), 1);
  assert.ok(Math.abs(contrast('#767676', '#ffffff') - 4.54) < 0.02,
    '#767676 on white is the canonical "just passes AA" grey');
});

test('every built-in theme meets WCAG AA for its normal-size text tokens', () => {
  const readVars = block => Object.fromEntries(
    [...block.matchAll(/--([a-z0-9-]+):\s*(#[0-9a-fA-F]{3,6})/g)].map(m => [m[1], m[2]]));
  const base = readVars(html.match(/:root\{([\s\S]*?)\n {2}\}/)[1]);
  const themes = {};
  for (const m of html.matchAll(/html\[data-theme="([a-z]+)"\]\{([^}]*)\}/g)) {
    const v = readVars(m[2]);
    if (Object.keys(v).length) themes[m[1]] = { ...(themes[m[1]] || {}), ...v };
  }
  assert.ok(Object.keys(themes).length >= 5, 'no themes parsed - the selector shape changed');

  const failures = [];
  for (const [name, over] of Object.entries(themes)) {
    const t = { ...base, ...over };
    if (!t.panel || !t.panel2) continue;
    for (const tok of ['muted', 'up', 'down', 'warn']) {
      if (!t[tok]) continue;
      for (const surface of ['panel', 'panel2']) {
        const r = contrast(t[tok], t[surface]);
        if (r < 4.5) failures.push(`${name} --${tok} on --${surface}: ${r.toFixed(2)}:1`);
      }
    }
  }
  assert.deepEqual(failures, [],
    `theme text below WCAG AA 4.5:1:\n  ${failures.join('\n  ')}`);
});
