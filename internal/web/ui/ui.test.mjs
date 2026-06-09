// Dependency-free unit tests for the dashboard's pure helper functions.
// The functions are EXTRACTED from the live index.html <script> (no copy/drift)
// and evaluated with minimal stubs, so these tests fail if the real source
// changes behavior.  Run:  node --test internal/web/ui/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const script = readFileSync(join(here, 'index.html'), 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];

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
};
const NAMES = Object.keys(DEFS);
const defs = NAMES.map(n => extract(DEFS[n])).join('\n');
// DAYN is a plain array const (no braces), which extract() can't lift - pass it in.
const factory = new Function('document', 'TC', 'DAYN', defs + '\nreturn {' + NAMES.join(',') + '};');
const TC = { hm0: '#000000', down: '#ffffff' };
const fakeCtx = { _v: '#000000', set fillStyle(x) { this._v = x; }, get fillStyle() { return this._v; } };
const fakeDoc = { createElement: () => ({ getContext: () => fakeCtx }) };
const F = factory(fakeDoc, TC, ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']);

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

// --- log viewer change-detection signature ---
const LG = new Function(extract('function logSig') + '\nreturn { logSig };')();

test('logSig: stable when unchanged, differs on a new/changed last line', () => {
  assert.equal(LG.logSig([]), '0|0:');
  assert.equal(LG.logSig(null), '0|0:');
  assert.equal(LG.logSig(['a', 'b']), LG.logSig(['a', 'b'])); // same content -> same sig (poll no-ops)
  assert.notEqual(LG.logSig(['a', 'b']), LG.logSig(['a', 'b', 'c'])); // new line appended
  assert.notEqual(LG.logSig(['a', 'b']), LG.logSig(['a', 'x'])); // last line changed
  assert.notEqual(LG.logSig(['a', 'b']), LG.logSig(['a', 'bb'])); // last line length changed
  assert.notEqual(LG.logSig(['a', 'b']), LG.logSig([])); // cleared
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
