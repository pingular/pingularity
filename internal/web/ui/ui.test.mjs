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
// Read a repo file with its line endings normalised to LF. Git checks these out
// with CRLF on Windows (no .gitattributes did that until one was added beside
// this file), and dozens of assertions here match multi-line source with a bare
// "\n" - so on a Windows runner they silently found nothing and reported the
// code had moved. That is how CI failed on windows-latest at a6f5778 while every
// other platform passed. These tests are about what the source SAYS, never about
// how its lines end, so normalising at the door is the fix rather than teaching
// each pattern to spell "\r?\n".
function readSource(...parts) {
	return readFileSync(join(...parts), 'utf8').replace(/\r\n/g, '\n');
}
// The whole page, for the few assertions that are about markup or CSS rather than
// about the script; `script` is what everything else works from.
const html = readSource(here, 'index.html');
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
  spMeasured: 'function spMeasured', pingMeasured: 'function pingMeasured',
  spdAverages: 'function spdAverages',
  speedtestBusy: 'function speedtestBusy',
  speedtestAbortable: 'function speedtestAbortable',
  autoOptionText: 'const autoOptionText', autoScopeText: 'const autoScopeText',
  serverOptionText: 'const serverOptionText',
  spdExportAvgSegments: 'function spdExportAvgSegments',
  bloatBins: 'function bloatBins', bloatCeiling: 'function bloatCeiling',
  bloatDataMax: 'function bloatDataMax', cursorSpan: 'const cursorSpan',
  drawCursor: 'function drawCursor',
  logPostAction: 'function logPostAction', spdAvgNote: 'function spdAvgNote',
  pingAvgText: 'function pingAvgText',
  setSpdAvg: 'function setSpdAvg', upCovNote: 'function upCovNote',
  upCovFoot: 'function upCovFoot', logTruncNote: 'function logTruncNote',
  outageDeletable: 'function outageDeletable',
  isReconcile503: 'function isReconcile503', retryAfterMs: 'function retryAfterMs',
  pkcs1FlagUsable: 'function pkcs1FlagUsable', pkcs1BoxState: 'function pkcs1BoxState',
  qsUseNote: 'function qsUseNote',
  iperfPwOrphan: 'function iperfPwOrphan',
  famLabel: 'function famLabel', famTitle: 'function famTitle',
  udpDirLabel: 'function udpDirLabel',
  udpDirTitle: 'function udpDirTitle',
  congestionContainerHint: 'function congestionContainerHint',
  netOpenNoAuth: 'function netOpenNoAuth',
  netNoAuthWarnText: 'function netNoAuthWarnText',
  qsNoAuthText: 'function qsNoAuthText',
  loopbackHost: 'function loopbackHost',
  latSpanMins: 'function latSpanMins', latPollForMins: 'function latPollForMins',
  latPollDelay: 'function latPollDelay',
  speedPollForMins: 'function speedPollForMins', speedPollDelay: 'function speedPollDelay',
};
const NAMES = Object.keys(DEFS);
const defs = NAMES.map(n => extract(DEFS[n])).join('\n');
// DAYN is a plain array const (no braces), which extract() can't lift - pass it in.
// isNum is the same shape (a one-line arrow const) and spMeasured/spdAverages call
// it, so it is injected the same way rather than copied as a literal here.
// `$` is injected so setSpdAvg can be driven against a fake element: the pill's
// visible text and its accessible name are set on different lines, and whether the
// second one is reached is exactly what is under test.
const factory = new Function('document', 'TC', 'DAYN', 'isNum', '$', 'esc', 'AX_PAD', 'iperfServers',
  'LAT_MAX_WIN_SEC', 'LAT_POLL_RUNGS', 'SPD_POLL_RUNGS', defs + '\nreturn {' + NAMES.join(',') + '};');
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
let fakeIperfServers = [];
// The poll-cadence constants are brace-less consts the extractor cannot lift, and
// they are the numbers under test (the 366-day bound the server applies, and the
// rungs each ladder may return) - so they are evaluated out of the page rather
// than restated here, where they could drift from what ships. MAX_WIN_MINS comes
// along because LAT_MAX_WIN_SEC is now derived from it.
const LAT_CONSTS = new Function(script.match(/const MAX_WIN_MINS = [^;]*;/)[0] + '\n'
  + script.match(/const LAT_MAX_WIN_SEC = [^;]*;/)[0] + '\n'
  + script.match(/const LAT_POLL_RUNGS = [^;]*;/)[0] + '\n'
  + script.match(/const SPD_POLL_RUNGS = [^;]*;/)[0]
  + '\nreturn { MAX_WIN_MINS, LAT_MAX_WIN_SEC, LAT_POLL_RUNGS, SPD_POLL_RUNGS };')();
// esc is a one-line arrow in the page, so it is injected rather than extracted;
// the panel strings below interpolate daemon-supplied place names and addresses
// through it.
const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const F = factory(fakeDoc, TC, ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'], v => typeof v === 'number',
  () => fakePill, esc,
  AX_PAD_VAL, fakeIperfServers, LAT_CONSTS.LAT_MAX_WIN_SEC, LAT_CONSTS.LAT_POLL_RUNGS,
  LAT_CONSTS.SPD_POLL_RUNGS);

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
    { label: 'NAS', addr: '10.0.0.5', orig_addr: '', bind: '', ipver: 'auto', auth: false, username: '', rsa_key: '', pkcs1: false, has_password: false, orig_has_password: false, password: '' });
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

// Blanking an int field falls back to the SHIPPING DEFAULT, not the minimum:
// the FIELDS fallbacks must equal config.go's defaults. iperf_streams (default
// 8, moved from 1 in 0.60.0) and iperf_omit (default 1) drifted - clearing them
// used to save 1 and 0, the worst measurement and no warm-up. Pins the two that
// moved so they cannot silently drift from the Go defaults again.
// Auth-gated downloads must go through the MARKED fetch (downloadVia), not a
// native <a href> GET - a native GET can't send X-Pingularity-UI, so an expired
// session would get the browser's Basic prompt instead of the SPA login.
test('downloads route through the marked fetch, not native href GETs', () => {
  assert.match(script, /async function downloadVia\(/, 'the marked-fetch download helper exists');
  assert.doesNotMatch(html, /id="logDownload"[^>]*href="api\/logs/, 'log download is not a native href GET');
  assert.doesNotMatch(html, /href="api\/speed\/runs\.csv"/, 'CSV download is not a native href GET');
  assert.match(script, /downloadVia\('api\/export/, 'export uses downloadVia');
  assert.match(script, /downloadVia\('api\/logs/, 'logs use downloadVia');
  assert.match(script, /downloadVia\('api\/speed\/runs\.csv/, 'CSV uses downloadVia');
  // The log download MUST carry the mask state, or "Redact PII" is a lie: a
  // masked view downloads raw PII. Pin the masked param on the click handler.
  assert.match(script, /downloadVia\('api\/logs\?download=1&masked='\+\(logMasked\?1:0\)/,
    'log download honors logMasked (else redaction is bypassed on download)');
});

// downloadVia buffers the body (it must, to route through the marked fetch), so
// it caps the buffer and reports 'toobig' instead of letting a giant backup
// exhaust the browser; the export button surfaces the streaming workaround.
// The download path must not hold the body in the JS heap. It used to
// accumulate chunks into an array behind a 100 MiB cap, which fired during
// NORMAL use: a default install at its own 30-day retention (the shipped 3
// probe targets at 5s, DNS on) exports ~150 MiB, measured through the real
// export handler. The backup an operator takes right before doing something
// risky was the thing that broke, so the ceiling is now a declared-size sanity
// check only, and the browser owns the bytes.
test('downloads let the browser own the bytes, and do not cap the happy path', () => {
  assert.match(script, /const downloadCapBytes = /, 'a declared-size ceiling still exists');
  assert.doesNotMatch(script, /chunks\.push\(value\)/,
    'the body must NOT be accumulated in the JS heap - that is what made a routine export fail');
  assert.match(script, /const b=await r\.blob\(\)/,
    'the response goes straight to a Blob, which the browser can spill to disk');
  const cap = /const downloadCapBytes = ([^;]+);/.exec(script);
  assert.ok(cap, 'the ceiling is a literal we can evaluate');
  const bytes = Function('"use strict";return (' + cap[1] + ')')();
  assert.ok(bytes >= 1024 * 1024 * 1024,
    `the ceiling is ${bytes} bytes; a default install exports ~150 MiB, so anything near that refuses ordinary backups`);
  // Still reported (and still explained) when a server DECLARES something absurd.
  assert.match(script, /return 'toobig'/, 'a declared oversize is still refused');
  assert.match(script, /res==='toobig'/, 'the export handler checks for the oversized signal');
  assert.match(script, /too large to download in the browser/, 'and points to the streaming workaround');
});

test('login overlay sits ABOVE Quick Setup (a 401 mid-setup must not paint behind the dim)', () => {
  const login = html.match(/\.login-overlay\{[^}]*z-index:(\d+)/);
  const qsDlg = html.match(/\.qs\{[^}]*z-index:(\d+)/);
  const qsDim = html.match(/\.qs-dim\{[^}]*z-index:(\d+)/);
  assert.ok(login && qsDlg && qsDim, 'all three modal layers declare a z-index');
  const [lz, dz, mz] = [login, qsDlg, qsDim].map(m => Number(m[1]));
  assert.ok(lz > dz && lz > mz, `login z-index ${lz} must exceed Quick Setup ${dz}/${mz} or the login is unreachable behind it`);
});

test('Quick Setup segment pills are keyboard-operable radios', () => {
  // Both groups are radiogroups whose pills carry role=radio + aria-checked + tabindex.
  assert.match(html, /id="qsSched"[^>]*role="radiogroup"/, 'qsSched is a radiogroup');
  assert.match(html, /id="qsAcc"[^>]*role="radiogroup"/, 'qsAcc is a radiogroup');
  assert.match(html, /<b class="on" role="radio" aria-checked="true" tabindex="0">/, 'checked pill is tabbable + announced');
  assert.match(html, /<b role="radio" aria-checked="false" tabindex="-1">/, 'unchecked pills are roving (tabindex -1)');
  // The keyboard handler moves selection with arrows/Home/End and selects on Space/Enter.
  assert.match(script, /function wireSeg\(/, 'the radiogroup wiring helper exists');
  assert.match(script, /ArrowRight'\|\|e\.key==='ArrowDown'/, 'arrow keys move selection');
  assert.match(script, /e\.key===' '\|\|e\.key==='Enter'/, 'Space/Enter select');
  assert.match(script, /setAttribute\('aria-checked'/, 'aria-checked is kept in sync');
});

test('Quick Setup traps focus and inerts the page behind it', () => {
  assert.match(script, /\$\('qsDlg'\)\.addEventListener\('keydown'[\s\S]*?e\.key!=='Tab'/, 'a Tab focus-trap is bound to the dialog');
  assert.match(script, /_qsInerted=\[\.\.\.document\.body\.children\]\.filter/, 'opening inerts the rest of the page');
  assert.match(script, /_qsInerted\.forEach\(el=>el\.inert=false\)/, 'closing restores exactly what it inerted');
  // Esc must not reach the dialog behind an open (required) login.
  assert.match(script, /if\(!loginOverlay\.hidden\) return;.*login is a required blocker/, 'Esc is suppressed while login is up');
});

test('a 401 during Quick Setup shows an INTERACTIVE login, not a dead one', () => {
  // Structural pins for the two-sided fix of the visible-but-inert deadlock.
  assert.match(script, /loginOverlay\.inert=false;/, 'showLogin must clear its own inert when presenting');
  assert.match(script,
    /_qsInerted=\[\.\.\.document\.body\.children\]\.filter\(el=>el!==\$\('qsDlg'\) && el!==\$\('qsDim'\) && el!==\$\('loginOverlay'\)/,
    'showQuickSetup must exclude the login overlay from what it inerts');

  // Behavioral: replay the exact filter/forEach both functions use over a mock
  // body-child list, in the order that broke before (Quick Setup, THEN a 401).
  // The login overlay must end interactive; the dashboard behind must stay inert.
  const el = (id) => ({ id, inert: false, hidden: ['loginOverlay', 'qsDlg', 'qsDim'].includes(id) });
  const wrap = el('wrap'), login = el('loginOverlay'), qsDim = el('qsDim'), qsDlg = el('qsDlg');
  const body = [wrap, login, qsDim, qsDlg];
  // showQuickSetup(): inert everything except qsDlg/qsDim/loginOverlay.
  body.filter(e => e !== qsDlg && e !== qsDim && e !== login && !e.inert).forEach(e => (e.inert = true));
  // ...a 401 fires -> showLogin(): clear own inert, reveal, inert the rest.
  login.inert = false; login.hidden = false;
  body.filter(e => e !== login && !e.inert).forEach(e => (e.inert = true));
  assert.equal(login.inert, false, 'login must be interactive when raised over Quick Setup');
  assert.equal(wrap.inert, true, 'the dashboard behind must stay inert');
});

test('log and CSV downloads surface an oversized result instead of failing silently (#5)', () => {
  assert.match(script, /function flashStatus\(/, 'a status-toast helper exists');
  assert.match(script, /function flashStatus\(msg\)\{[\s\S]{0,140}undoToast/, 'flashStatus writes the role=status toast');
  assert.match(script, /This CSV is too large to download/, 'CSV handler messages on toobig (was silent)');
  assert.match(script, /These logs are too large to download/, 'log handler messages on toobig (was silent)');
  // the no-stream fallback also refuses an over-cap declared size instead of buffering blindly
  assert.match(script, /Content-Length[\s\S]{0,80}downloadCapBytes[\s\S]{0,40}return 'toobig'/, 'blob fallback caps via Content-Length');
});

// role="button" on an <a> announces a button but only gets Enter for free -
// Space activation is native <button> behavior that ARIA does not add, and the
// click handlers listen for clicks only. Native buttons give Space AND Enter
// through the browser's own click synthesis, and with no href there is no
// native navigation left to regress the marked-fetch download paths.
test('download controls are native buttons: Space activates, Enter still works', () => {
  assert.match(html, /<button[^>]*id="logDownload"[^>]*type="button"/, 'log download is a native button');
  assert.match(html, /<button[^>]*id="csvDownload"[^>]*type="button"/, 'CSV download is a native button');
  assert.doesNotMatch(html, /<a[^>]*id="logDownload"/, 'no anchor faking a button for the log download');
  assert.doesNotMatch(html, /<a[^>]*id="csvDownload"/, 'no anchor faking a button for the CSV download');
  assert.doesNotMatch(html, /id="logDownload"[^>]*href=/, 'no href on the log control - nothing to navigate');
  assert.doesNotMatch(html, /id="csvDownload"[^>]*href=/, 'no href on the CSV control - nothing to navigate');
  // The same click handlers still drive the marked fetch, so keyboard
  // activation (both keys) reaches downloadVia exactly like a pointer click.
  assert.match(script, /\$\('logDownload'\)\.addEventListener\('click'/, 'log click handler intact');
  assert.match(script, /\$\('csvDownload'\)\.addEventListener\('click'/, 'CSV click handler intact');
});

test('log download never carries a navigable api/logs href - no Basic-prompt bypass (#6)', () => {
  assert.doesNotMatch(html, /id="logDownload"[^>]*href="api\/logs/, 'initial markup has no native api/logs href');
  assert.doesNotMatch(script, /setAttribute\('href'\s*,\s*'api\/logs/, 'no runtime code sets a native api/logs href');
  assert.doesNotMatch(script, /updateLogDownload/, 'the href-setting helper is gone (middle-click/save-as cannot bypass the marked fetch)');
});

test('Quick Setup update checkbox is named and errors are announced (#8)', () => {
  assert.match(html, /id="qsUpd"[^>]*aria-label="Check for updates"/, 'update checkbox has an accessible name');
  assert.match(html, /id="qsErr"[^>]*role="alert"/, 'the async error element is a live region');
});

test('int-field fallbacks match the shipping defaults (streams=8, omit=1)', () => {
  const streams = html.match(/\['setIperfStreams','iperf_streams','int',(\d+)\]/);
  const omit = html.match(/\['setIperfOmit','iperf_omit','int',(\d+)\]/);
  assert.ok(streams, 'iperf_streams FIELDS entry present');
  assert.equal(streams[1], '8', 'blank Parallel streams must fall back to 8 (config.go IperfStreams), not the minimum 1');
  assert.ok(omit, 'iperf_omit FIELDS entry present');
  assert.equal(omit[1], '1', 'blank warm-up (omit) must fall back to 1 (config.go IperfOmit), not 0');
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
// `tip` stands in for #hmTip, which drawHeatmap now touches; pass one in to read
// back what the rebuild did to it (see the rebuild test below).
function driveHeatmap(rows, todayMs, tip = { hidden: true }) {
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
    rows, id => (id === 'hmTip' ? tip : grid), doc, FrozenDate, { hm0: '#000000', down: '#ffffff' });
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

// The grid rebuilds itself out from under whatever the tooltip is showing: the
// 60s poll repaints it (refreshHeatmap -> drawHeatmap) and clicking an outage
// cell calls drawHeatmap straight from the click handler. The rebuild is an
// innerHTML='' over #heatmap, which deletes the cell the tooltip describes, and
// on the keyboard path the cell that holds focus with it. Nothing else can be counted on
// to take the tooltip down afterwards: mouseleave waits for the pointer to leave
// the grid, focusout waits for focus to leave a cell that is being deleted rather
// than blurred, and the Escape branch fires only while activeElement is still
// inside #heatmap. Left up, it states the old day over a grid that has moved on.
// So the rebuild takes it down itself, which costs nothing when nothing is up.
test('heatmap: a rebuild takes the tooltip down with the cells it describes', () => {
  const tip = { hidden: false }; // as a hover- or focus-raised tooltip leaves it
  driveHeatmap([{ date: '2026-07-15', outages: 1, downtime_s: 60, window_s: 86400, observed_s: 86400 }],
    new Date(2026, 6, 27, 12, 0).getTime(), tip);
  assert.equal(tip.hidden, true,
    'the tooltip outlived the cell it was describing: the next repaint leaves the previous day\'s text over a new grid');
});

// A date the response omits is not a date the response vouches for.
// Store.Clear("downtime") (internal/store/store.go) drops the events and pauses
// tables outright while first_seen_ts survives in `settings` (monitoringSince
// reads it back), and outage retention prunes old events the same way - so after
// "Delete downtime" or a retention roll-off, DowntimeByDay emits nothing for
// those days, which is exactly what it emits for a genuinely clean one. The UI
// cannot tell the two apart from a missing row, so the label may only describe
// the RECORD it was handed, never the day.
test('heatmap: a day with no row claims no more than the response recorded', () => {
  const cells = driveHeatmap(
    [{ date: '2026-07-15', outages: 1, downtime_s: 60, window_s: 86400, observed_s: 86400 }],
    new Date(2026, 6, 27, 12, 0).getTime()); // Monday: no padding, every cell is real
  const tips = cells.map(c => c.dataset.tip).filter(Boolean);
  assert.equal(tips.length, 366, 'every in-window cell is labelled');
  const unbacked = tips.filter(t => !/^2026-07-15:/.test(t));
  assert.equal(unbacked.length, 365, 'one row in, 365 dates the response says nothing about');
  assert.ok(unbacked.every(t => / no outages recorded$/.test(t)),
    'a date with no row is described by what was recorded for it: ' + unbacked.find(t => !/ no outages recorded$/.test(t)));
  assert.ok(!tips.some(t => /: no outages$/.test(t)),
    '"no outages" is a claim about the day, which a missing row cannot support');
  // "no recorded outages" reads as a claim about the outages (the ones nobody
  // wrote down) rather than about the records, so it is not the softer wording.
  assert.ok(!tips.some(t => /no recorded outages/.test(t)),
    'the hedge belongs on the record, not on the outages');
  // The accessible name is built from the same string, so a screen reader hears
  // the same hedge the tooltip shows.
  const ax = cells.map(c => c.attrs['aria-label']).filter(Boolean);
  assert.equal(ax.length, 366);
  assert.ok(ax.some(a => / no outages recorded$/.test(a)) && !ax.some(a => /: no outages$/.test(a)),
    'the accessible name still makes the bare claim');
});

// --- heatmap: the days a multi-day outage spans but did not start on ----------
// The backend prorates an outage's downtime onto every local day it covered but
// counts the outage itself only on the day it BEGAN (store.go DowntimeByDay). So
// the middle of a Mon 23:00 -> Wed 01:00 outage is a shaded, clickable day whose
// outages count is 0 and which holds no event of its own: the locator, which
// matched event dates against the day, found nothing and the click silently did
// nothing. Continuation days now resolve to the outage ENCLOSING them rather than
// being made inert - the alternative cannot be expressed from the heatmap payload
// anyway, since the RECOVERY day is also outages=0 and its click has always worked.
const HM = new Function(extract('function hmDayRange') + '\n' + extract('function hmScanDay')
  + '\nreturn { hmDayRange, hmScanDay };')();
const jul = (d, h, mi = 0) => Math.floor(new Date(2026, 6, d, h, mi).getTime() / 1000);
const scan = (evs, day, off = 0, st = { up: null, upTs: 0 }) => {
  const [from, to] = HM.hmDayRange(day);
  return HM.hmScanDay(evs, off, from, to, st);
};

test('hmDayRange: local midnight to local midnight, taken from the date parts', () => {
  assert.deepEqual(HM.hmDayRange('2026-07-14'), [jul(14, 0), jul(15, 0)]);
  // Month end: day+1 rolls the month over rather than adding 86400 seconds, which
  // is also what keeps a 23h/25h DST day exactly one local day wide.
  assert.deepEqual(HM.hmDayRange('2026-07-31'),
    [jul(31, 0), Math.floor(new Date(2026, 7, 1).getTime() / 1000)]);
  // From the parts, never from the string: "2026-07-14" parses as UTC, which would
  // slide the window off the local days the backend bucketed by (heatmap?tz=...).
  // A UTC test runner cannot tell the two apart, so the construction is pinned.
  assert.match(extract('function hmDayRange'), /new Date\(p\[0\],p\[1\]-1,p\[2\]\)/);
});

test('heatmap locator: a day inside a multi-day outage resolves to that outage', () => {
  // Newest-first, exactly as /api/events returns them.
  const evs = [{ ts: jul(15, 1), type: 'up', has_duration: true, duration_s: 7200 },
    { ts: jul(13, 23), type: 'down' }];
  // Tuesday: prorated downtime, no event of its own -> the recovery that closed it.
  assert.deepEqual(scan(evs, '2026-07-14'), { idx: 0, ts: jul(15, 1), done: true });
  // The days that DO hold an event are untouched - each still pins to its own row.
  assert.deepEqual(scan(evs, '2026-07-13'), { idx: 1, ts: jul(13, 23), done: true });
  assert.deepEqual(scan(evs, '2026-07-15'), { idx: 0, ts: jul(15, 1), done: true });
  // A day AFTER the outage closed encloses nothing - it must not borrow a row.
  assert.deepEqual(scan(evs, '2026-07-16'), { idx: -1, ts: 0, done: true });

  // Still running: no recovery row exists yet, so the day pins to the 'down'.
  assert.deepEqual(scan([{ ts: jul(13, 23), type: 'down' }], '2026-07-14'),
    { idx: 0, ts: jul(13, 23), done: true });

  // A monitor restart re-detects an outage, leaving an extra 'down' inside it, and
  // later outages sit above it. Walking newest-first, the recovery that pairs with
  // a 'down' is the last 'up' SEEN - which is what survives both.
  const messy = [{ ts: jul(20, 8), type: 'up' }, { ts: jul(20, 7), type: 'down' },
    { ts: jul(16, 12), type: 'up' }, { ts: jul(15, 9), type: 'down' },
    { ts: jul(13, 23), type: 'down' }];
  assert.deepEqual(scan(messy, '2026-07-14'), { idx: 2, ts: jul(16, 12), done: true });
});

test('heatmap locator: the walk survives the 1000-event page boundary', () => {
  // The recovery is on one page and the 'down' that opened the outage on the next,
  // so pairing them at all depends on the carried state.
  const st = { up: null, upTs: 0 };
  const first = scan([{ ts: jul(15, 1), type: 'up' }], '2026-07-14', 0, st);
  assert.deepEqual(first, { idx: -1, ts: 0, done: false }, 'a page that settles nothing must keep paging');
  const second = scan([{ ts: jul(13, 23), type: 'down' }], '2026-07-14', 1000, st);
  assert.deepEqual(second, { idx: 0, ts: jul(15, 1), done: true }, 'the recovery index is global, not page-local');
});

// The locator is only worth anything wired up: this drives the REAL click handler
// out of index.html and checks it pages to the row AND that the row highlights.
function driveHeatmapClick(day, events) {
  let handler = null;
  const fetched = [];
  const els = { heatmap: { addEventListener: (_ev, fn) => { handler = fn; } },
    events: { querySelector: () => null } };
  const fget = async url => { fetched.push(url);
    const off = +/offset=(\d+)/.exec(url)[1];
    return { json: async () => ({ events: off ? [] : events }) }; };
  const api = new Function('$', 'fget', 'drawHeatmap', 'setOutagesOpen', 'loadOutages',
    'let outageSelDay=null, outageSelTs=0, outagesPage=1, outagesPerPage=10;\n'
    + script.match(/const fmtLocalDate = [^;]*;/)[0] + '\n'
    + extract('function hmDayRange') + '\n'
    + extract('function hmScanDay') + '\n'
    + extract('function evHighlighted') + '\n'
    + extract("$('heatmap').addEventListener('click'") + ');\n'
    + 'return { evHighlighted, state: () => ({ outageSelDay, outageSelTs, outagesPage }) };')(
    id => els[id], fget, () => {}, () => {}, async () => {});
  const cell = { dataset: { day } };
  return { api, fetched, click: () => handler({ target: { closest: () => cell } }) };
}

test('heatmap click: a continuation day pages to its outage and highlights the row', async () => {
  // 24 newer events sit above the outage, so its recovery is the 25th row - page 3
  // at 10 per page. Nothing here is a same-day match, so only the enclosing-outage
  // path can find it.
  const events = [];
  for (let i = 0; i < 12; i++) {
    events.push({ ts: jul(20, 12) - i * 200, type: 'up', has_duration: true, duration_s: 60 });
    events.push({ ts: jul(20, 12) - i * 200 - 100, type: 'down' });
  }
  const up = { ts: jul(15, 1), type: 'up', has_duration: true, duration_s: 7200 };
  const down = { ts: jul(13, 23), type: 'down' };
  events.push(up, down);

  const { api, fetched, click } = driveHeatmapClick('2026-07-14', events);
  await click();
  const st = api.state();
  assert.equal(st.outageSelDay, '2026-07-14', 'the day is still pinned');
  assert.equal(st.outagesPage, 3,
    'the table stayed on page 1: the click promised events and produced none');
  assert.equal(st.outageSelTs, up.ts, 'the day resolved to no row at all');
  assert.equal(api.evHighlighted(up), true, 'the enclosing outage must be the highlighted row');
  assert.equal(api.evHighlighted(events[0]), false, 'an unrelated outage must not light up');
  assert.equal(api.evHighlighted(down), false,
    "the outage's opening 'down' is dated outside the day and is not its identity row");
  assert.equal(fetched.length, 1, 'one page of events is enough to locate this');

  // Re-clicking the same cell unpins it - and takes the mapped row with it,
  // otherwise the highlight outlives the pin.
  await click();
  assert.equal(api.state().outageSelDay, null);
  assert.equal(api.state().outageSelTs, 0);
  assert.equal(api.evHighlighted(up), false);
});

test('heatmap: a day that only continues an earlier outage says so, and stays clickable', () => {
  const cells = driveHeatmap(
    [{ date: '2026-07-14', outages: 0, downtime_s: 86400, window_s: 86400, observed_s: 86400 }],
    new Date(2026, 7, 2, 12, 0).getTime());
  const c = cells.find(x => (x.dataset.tip || '').startsWith('2026-07-14'));
  assert.ok(c, 'the continuation day drew no cell at all');
  assert.ok(!/outage\(s\)/.test(c.dataset.tip),
    'a day that was offline end to end reads "0 outage(s)": ' + c.dataset.tip);
  assert.match(c.dataset.tip, /down, from an outage that began earlier/);
  // Still interactive: its click now resolves to the outage that encloses it.
  assert.equal(c.dataset.day, '2026-07-14');
  assert.equal(c.attrs.role, 'button');
  assert.match(c.attrs['aria-label'], /Activate to view events\./);
});

// --- heatmap tooltip: the keyboard has to be able to read it too ---------------
// drawHeatmap makes the outage days focusable (tabIndex + role=button) and they
// answer Enter/Space, but the tooltip delegation listened for mouse and touch
// only, so tabbing onto one of those cells showed nothing at all. The observation
// gap ("not monitored", "monitored X of Y") is stated in the label and nowhere
// else - the fill deliberately does not encode it - and drawHeatmap builds the
// cell's aria-label out of that same string, so a screen reader had it on focus
// already. What had no route to it was the SIGHTED keyboard user, left with the
// fill.
//
// The block is an IIFE over $('heatmap')/$('hmTip'), so it is run here against
// stub elements that record their listeners: this drives the REAL handlers rather
// than pattern-matching the source.
function heatmapTip() {
  const start = script.indexOf("(function(){\n  const hm=$('heatmap'), tip=$('hmTip');");
  assert.ok(start > 0, 'the heatmap tooltip delegation block moved');
  const src = script.slice(start, script.indexOf('})();', start) + 5);
  const mk = () => ({ on: {}, hidden: true, textContent: '', style: {},
    addEventListener(t, fn) { (this.on[t] = this.on[t] || []).push(fn); },
    getBoundingClientRect: () => ({ width: 160, height: 22, left: 0, top: 0, right: 160, bottom: 22 }) });
  const hm = mk(), tip = mk();
  new Function('$', 'window', src)(id => (id === 'heatmap' ? hm : tip), { innerWidth: 1200 });
  const fire = (type, ev) => (hm.on[type] || []).forEach(fn => fn(ev));
  return { hm, tip, fire };
}
// A cell as the delegate sees it: dataset.tip plus a rect, reachable via closest.
const hmCell = (tipText, rect) => {
  const c = { dataset: tipText == null ? {} : { tip: tipText },
    getBoundingClientRect: () => rect, closest: s => (s === '.cell' ? c : null) };
  return c;
};

test('heatmap tooltip: focusing a cell shows its label, anchored to the cell', () => {
  const { hm, tip, fire } = heatmapTip();
  // focus does not bubble, so a delegate on the container never hears it. Only
  // focusin reaches a listener registered on the grid.
  assert.ok(hm.on.focusin, 'no focusin listener: a focused cell shows nothing');
  assert.ok(!hm.on.focus, 'focus does not bubble to a delegate - it would never fire');
  assert.ok(hm.on.focusout, 'nothing hides the tooltip when focus leaves the grid');

  // The valuable label: a day that was offline AND only partly watched. The
  // observation half is stated nowhere else on screen.
  const cell = hmCell('2026-07-14: 1 outage(s), 10m down · monitored 3h of 24h',
    { left: 40, top: 60, right: 52, bottom: 72, width: 12, height: 12 });
  fire('focusin', { target: cell });
  assert.equal(tip.hidden, false, 'the tooltip stayed hidden on focus');
  assert.match(tip.textContent, /monitored 3h of 24h/,
    'the observation disclosure never reached the keyboard user');

  // Anchored to the CELL, not to a cursor keyboard navigation never moves: the
  // position has to track the cell's own rect. A second cell elsewhere on the
  // grid must land somewhere else.
  const first = { left: tip.style.left, top: tip.style.top };
  fire('focusin', { target: hmCell('2026-07-20: not monitored',
    { left: 400, top: 300, right: 412, bottom: 312, width: 12, height: 12 }) });
  assert.notEqual(tip.style.left, first.left, 'the tooltip is pinned, not anchored to the focused cell');
  assert.notEqual(tip.style.top, first.top, 'the tooltip is pinned, not anchored to the focused cell');
  assert.ok(parseFloat(first.left) >= 40 && parseFloat(first.left) < 100,
    `the first cell's tooltip landed at ${first.left}, nowhere near its rect`);
  assert.ok(parseFloat(first.top) >= 72 && parseFloat(first.top) < 130,
    `the first cell's tooltip landed at ${first.top}, nowhere near its rect`);

  // Tabbing off hides it.
  fire('focusout', {});
  assert.equal(tip.hidden, true, 'the tooltip outlives the focus that raised it');

  // The guard is two conditions and each gets its own case: a test that only ever
  // fires at a target outside any cell exercises the first half, and the second
  // can then be deleted in one token with the suite still green. Both start from a
  // shown tooltip, so a fall-through shows up as the PREVIOUS day's text left
  // standing under the new focus - the failure that actually reaches a reader.
  const shown = () => fire('focusin', { target: hmCell('2026-07-21: no outages recorded',
    { left: 1, top: 1, right: 13, bottom: 13 }) });
  // No cell at all: focus landed on the grid container itself rather than on one
  // of its children.
  shown();
  assert.equal(tip.hidden, false, 'the setup for the guard cases never showed a tooltip');
  fire('focusin', { target: { closest: () => null } });
  assert.equal(tip.hidden, true, 'focus on no cell at all leaves the previous day\'s text showing');
  // A real cell carrying no label. drawHeatmap mints exactly this for the days
  // older than the fetched window: className 'cell', visibility hidden, empty
  // dataset. Nothing gives those a tabindex today, so this half of the guard is
  // holding a line no other test would notice it dropping.
  shown();
  fire('focusin', { target: hmCell(null, { left: 5, top: 5, right: 17, bottom: 17 }) });
  assert.equal(tip.hidden, true, 'a cell with no label shows the previous day\'s text');
});

// Escape is the standard dismiss, and a focus-raised tooltip has no pointer to
// move away from it - short of tabbing off the cell or the grid repainting under
// it, nothing a keyboard user does takes it down. Three things are pinned here: it
// dismisses WITHOUT moving focus; it leaves alone the Escape the settings drawer
// is waiting for when focus is somewhere else entirely; and it costs the drawer
// exactly ONE press. That last one is the deliberate trade. Focusing a cell raises
// its tooltip, and the drawer is reachable from the grid - it inerts
// nothing behind it and traps no Tab, unlike Quick Setup - so an open drawer does
// lose the first press. The tooltip is the innermost thing showing and focus never
// moves, so the second press reaches the drawer exactly as the first would have;
// the alternative leaves a keyboard user no way to dismiss the tooltip at all
// while the drawer is open.
test('Escape dismisses a focused heatmap tooltip, and only that one', () => {
  const src = extract("document.addEventListener('keydown', e=>{");
  const els = { qsDlg: { hidden: true }, dataPop: { hidden: true }, uptimePop: { hidden: true },
    hmTip: { hidden: false }, families: { querySelector: () => null } };
  let closed = 0;
  const cell = { closest: s => (s === '#heatmap' ? cell : null) };
  const doc = { activeElement: cell, addEventListener(_t, fn) { this.fn = fn; } };
  new Function('document', '$', 'loginOverlay', 'qsDecline', 'hideDataPop', 'hideUptimePop',
    '_drawer', 'requestCloseDrawer', src + ');')(
    doc, id => els[id], { hidden: true }, () => {}, () => {}, () => {},
    { classList: { contains: () => true } }, () => { closed++; });

  doc.fn({ key: 'Escape' });
  assert.equal(els.hmTip.hidden, true, 'Escape left the tooltip sitting over the grid');
  assert.equal(closed, 0, 'one Escape also closed the drawer - two actions for one keypress');

  // The drawer is one more press away and no further: focus has not moved, and the
  // thing that won the first press is down.
  doc.fn({ key: 'Escape' });
  assert.equal(closed, 1, 'with its tooltip already down the grid went on eating the drawer\'s Escape');

  // Focus outside the grid: this is a pointer-raised tooltip, which has a pointer
  // to move away from it, so Escape belongs to the drawer on the FIRST press.
  els.hmTip.hidden = false;
  doc.activeElement = { closest: () => null };
  doc.fn({ key: 'Escape' });
  assert.equal(closed, 2, 'the heatmap tooltip ate the drawer\'s Escape');
  assert.equal(els.hmTip.hidden, false, 'a tooltip nobody focused is not the keyboard\'s to dismiss');
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

  // An empty loadedFor is the third way in, and it has to read as stale too:
  // refreshSpeedChart empties the marker as a FORCED load begins, so this is a
  // status poll landing between "the user deleted these runs" and the refetch
  // returning. spdData is pre-delete, and 'loading range…' is the honest caption.
  assert.equal(C.spdSyncPlan(4, 4, '', 'from=3&to=4'), 'stale');

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
// TILE_GATE is the real .section-hidden check out of the page, injected into
// every driver below so they answer with the shipped gate rather than a stub.
const TILE_GATE = extract('function sectionHidden') + '\n' + extract('function tileIdle');
// A document whose only .panel is `section`, hidden or not, for that gate.
const gateDoc = (section, hidden) => ({
  querySelector: sel => sel === '.panel[data-section="' + section + '"]'
    ? { classList: { contains: c => c === 'section-hidden' && hidden } } : null,
});
function driveNetinfo({ netinfoOff = false, monitoring = true, snapshot = {}, hidden = false, force = false } = {}) {
  const body = extract('async function refreshNetinfo');
  const panel = { innerHTML: '' };
  const warn = { innerHTML: '', classList: { add() {}, remove() {} } };
  const refreshBtn = { classList: { contains: () => false } };
  const els = { netinfo: panel, netinfoWarn: warn, netinfoRefresh: refreshBtn };
  const scheduled = [], fetched = [];
  const $ = id => els[id] || { innerHTML: '', classList: { add() {}, remove() {}, contains: () => false },
    setAttribute() {}, getAttribute() {}, hidden: false };
  // netinfoIdleReason is pulled from the same source, not restated here: the
  // whole point of the fix is that one answer drives both call sites.
  const idle = script.includes('function netinfoIdleReason') ? extract('function netinfoIdleReason') : '';
  const make = new Function(
    '$', 'fget', 'syncNetinfoOffMark', 'netinfoOff', 'monitoring', 'setTimeout', 'clearTimeout',
    'labelInfoBubbles', 'esc', 'document',
    TILE_GATE + '\n' + idle + '\nlet netinfoSeq = 0, netinfoRetry = null;\n' + body
    + '\nreturn Object.assign(refreshNetinfo, { seq: () => netinfoSeq });');
  const fn = make(
    $,
    async (url, _ms, opt) => { fetched.push((opt && opt.method) || 'GET'); return { json: async () => snapshot }; },
    () => {},
    netinfoOff, monitoring,
    (f, ms) => { scheduled.push(ms); return 1; },
    () => {},
    () => {},
    s => String(s),
    gateDoc('connection', hidden),
  );
  return fn(force).then(() => ({ html: panel.innerHTML, polls: scheduled.length, fetched, seq: fn.seq() }));
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
  // A real classList, so the shipped btnBusy runs unmodified and the spinner swap
  // is observed rather than stubbed away.
  const classes = new Set();
  const btn = { disabled: false, addEventListener: (_ev, fn) => { handler = fn; },
    classList: { toggle: (c, on) => { on ? classes.add(c) : classes.delete(c); },
      add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c) },
    setAttribute: (k, v) => { btn._attrs = Object.assign(btn._attrs || {}, { [k]: v }); },
    removeAttribute: k => { if (btn._attrs) delete btn._attrs[k]; },
    _attrs: {}, classes };
  const msg = { textContent: '' };
  const els = { importBtn: btn, importFile: { files: [file] }, importMsg: msg,
    outagesSection: { style: { display: 'none' } } };
  const sent = { body: undefined, headers: null, disabledMidUpload: null };
  const fetchStub = async (_url, opt) => {
    sent.body = opt.body; sent.headers = opt.headers;
    sent.disabledMidUpload = btn.disabled; // a second click here would upload twice
    sent.busyMidUpload = classes.has('busy'); // and the label is a spinner while it runs
    if (fetchFails) throw new Error('network went away');
    return { ok, status: ok ? 200 : 500, json: async () => resp,
      text: async () => JSON.stringify(resp) };
  };
  const noop = async () => {};
  const register = new Function('$', 'getCats', 'fetch', 'confirm', 'loadSettings',
    'loadAccess', 'formSnapshot', 'refreshStatus', 'refreshChart', 'refreshSpeedChart',
    'refreshHeatmap', 'loadOutages',
    'let savedBody = null;\n' + extract('function btnBusy(') + '\n' +
    extract("$('importBtn').addEventListener('click'") + ');');
  register(id => els[id], () => ['pings'], fetchStub, () => true, noop, noop,
    () => '', () => {}, () => {}, () => {}, () => {}, noop);
  sent.busyMidUpload = null;
  const origFetch = fetchStub;
  return handler().then(() => ({ file, btn, msg, sent, classes }));
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

// The spinner is the only feedback a long import gives: the message line says
// "Importing" once and never changes again, so a restore that takes minutes looks
// identical to one that hung. It has to go up for the whole upload and come down
// on every exit, including the ones that threw.
test('import swaps its label for a spinner while the upload runs, and puts it back', async () => {
  const done = await driveImport();
  assert.equal(done.sent.busyMidUpload, true,
    'a multi-minute restore has to show it is working, or the operator cannot tell it from a hang');
  assert.equal(done.classes.has('busy'), false,
    'and the label has to come back on success, or the button reads as busy forever');
  const refused = await driveImport({ ok: false, resp: { error: 'nope' } });
  assert.equal(refused.classes.has('busy'), false, 'a refused import must not leave the spinner up');
  const broken = await driveImport({ fetchFails: true });
  assert.equal(broken.classes.has('busy'), false, 'nor must a network failure mid-upload');
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
  assert.ok(/centred on <b>Newtown, QC<\/b> where your last auto test ran - not where the next one will/.test(t), t);
  // The disclaimer is the point: toggling auto on shows the LAST run's city for
  // browsing, but the note must not read as where the NEXT test will go (it is
  // raced fresh each run, so no city is known until the test runs).
  assert.ok(/chosen fresh when the next test runs/.test(t), t);
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

// A dropdown row survives the fields the daemon cannot promise: the by-ID
// resolve returns an empty country on sparse Ookla records (measured - it
// shipped as "EBOX - Montréal, QC," with a dangling comma), and a 0 distance
// means unknown, not adjacent, so no "(0 km)".
test('a server row renders without a dangling comma or a bogus 0 km', () => {
  assert.equal(F.serverOptionText({sponsor:'EBOX', name:'Montréal, QC', country:'', distance_km:0}),
    'EBOX - Montréal, QC');
  assert.equal(F.serverOptionText({sponsor:'Bell Canada', name:'Scarborough, ON', country:'Canada', distance_km:4.2}),
    'Bell Canada - Scarborough, ON, Canada (4 km)');
  assert.equal(F.serverOptionText({sponsor:'EBOX', name:'Montréal, QC', country:'Canada', distance_km:297}),
    'EBOX - Montréal, QC, Canada (297 km)');
});

// THE SAVED IMAGE CARRIES THE AVERAGES THE SCREEN SHOWS. A PNG leaves the app
// with no pills above it, so the export header repeats them: same reducer
// (spdAverages), same formatters, arrows in the colours of the lines they
// summarize, the house '-' for no-data, and the sampled-mean note when the
// window was thinned.
test('the saved speed image carries the on-screen averages', () => {
  const segs = F.spdExportAvgSegments({down: 486.6, up: 120.7, ping: 13.6}, '');
  const text = segs.map((s) => s[0]).join('');
  assert.ok(/487 Mbps/.test(text), text);
  assert.ok(/121 Mbps/.test(text), text);
  assert.ok(/14 ms/.test(text), text);
  assert.ok(!/avg/.test(text), 'no avg label: the band is values-only');
  assert.equal(segs.find((s) => s[0].includes('\u2193'))[1], 'dl');
  assert.equal(segs.find((s) => s[0].includes('\u2191'))[1], 'ul');
  const wave = segs.find((s) => s[0] === 'icon:ping');
  assert.ok(wave, 'the ping value is introduced by the wave icon, not a text label');
  assert.equal(wave[1], 'ping');
  assert.ok(!/ping /.test(text), text);
  const empty = F.spdExportAvgSegments({down: null, up: null, ping: null}, '').map((s) => s[0]).join('');
  assert.ok(!/NaN|null|0 Mbps/.test(empty), empty);
  assert.ok(/-/.test(empty), empty);
  const noted = F.spdExportAvgSegments({down: 1, up: 1, ping: 1}, ' (mean of the 500 runs charted, of 1200 in range)');
  assert.equal(noted[noted.length - 1][1], 'axis');
  assert.ok(/mean of the 500/.test(noted[noted.length - 1][0]));
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

// The legacy-RSA-padding toggle is gated on what the LOCAL iperf3 can actually
// send, and both edges are real upstream behavior, verified against esnet/iperf:
// the flag arrives in 3.17, and 3.20 marks it server-only so a 3.20+ client
// refuses to start with it ("some option you are trying to set is server only").
// Offering the toggle outside that window turns a well-meant tick into a run that
// never connects, which is exactly the confusion this gate exists to prevent.
test('pkcs1FlagUsable spans only [3.17, 3.20)', () => {
  const { pkcs1FlagUsable } = F;
  assert.equal(pkcs1FlagUsable(316), false, '3.16 has no such flag - PKCS#1 is already its default');
  assert.equal(pkcs1FlagUsable(317), true, '3.17 introduced the client-settable flag');
  assert.equal(pkcs1FlagUsable(318), true);
  assert.equal(pkcs1FlagUsable(319), true, '3.19 is the last client-settable build');
  assert.equal(pkcs1FlagUsable(320), false, '3.20 made it server-only; a client rejects it outright');
  assert.equal(pkcs1FlagUsable(321), false);
  assert.equal(pkcs1FlagUsable(0), true, 'undetectable version must not disable the operator\'s choice');
});

// Disabling the toggle when the flag is unusable stops it being turned ON, but a
// server SAVED with it on (ticked under a 3.17-3.19 build, or before the host's
// iperf3 was upgraded past 3.20) keeps sending the flag - and a disabled checkbox
// cannot be unticked, so the only remaining cure was deleting the server and
// losing its stored password. Locked against turning on, never against turning off.
test('an already-on legacy-padding toggle stays clearable on a build that cannot send it', () => {
  const { pkcs1BoxState } = F;
  for (const [vnum, ver] of [[321, '3.21'], [320, '3.20'], [316, '3.16']]) {
    const off = pkcs1BoxState(vnum, ver, false);
    assert.equal(off.disabled, true, `${ver}: an unusable flag must not be tickable`);
    assert.ok(off.note.length > 0, `${ver}: a locked toggle must say why`);
    const on = pkcs1BoxState(vnum, ver, true);
    assert.equal(on.disabled, false, `${ver}: a saved-on flag must stay clearable`);
    assert.match(on.note, /off/i, `${ver}: the note must tell the operator to turn it off`);
    assert.notEqual(on.note, off.note, `${ver}: on-and-broken is not the same message as unavailable`);
  }
  const ok = pkcs1BoxState(318, '3.18', true);
  assert.equal(ok.disabled, false, '3.18 can send the flag');
  assert.equal(ok.note, '', 'a usable flag needs no note');
  assert.equal(pkcs1BoxState(0, '', false).disabled, false, 'an undetectable build locks nothing');
});

// The coach must never mount behind the Quick Setup dim: its once/capture
// pointerdown would let a click INSIDE the dialog mark the never-seen coach as
// seen forever. And the save path's guards: the byte cap that stops a bcrypt
// rejection landing after the marker persisted, and the access latch that
// stops a retry re-posting credentials the server already took.
test('Quick Setup guard rails: coach deferral, byte cap, access latch', () => {
  const coach = extract('function showCoach');
  assert.match(coach, /if\(!\$\('qsDlg'\)\.hidden\) return;/, 'showCoach must defer behind the dialog');
  const save = extract('function qsSave');
  assert.match(save, /encode\(pass\)\.length>72/, 'the 72-byte bcrypt cap is checked before the round trip');
  const dec = extract('function qsDecline');
  assert.match(dec, /qsGo'\)\.disabled\) return;/, 'Esc must not decline mid-save');
  assert.match(html, /id="qsPass" placeholder="Password" maxlength="72"/, 'the field cap matches the server');
});

// The coachmark anchors to the header's BOTTOM, and on narrow viewports the
// header grows after the card mounts (the toolbar wraps as status pills fill
// in). A window-resize listener never fires for that, which left the card
// overlapping the wrapped pill rows at ~390px - so the anchor itself must be
// observed, and the observer must die with the card.
test('the coachmark tracks header growth, not just window resizes', () => {
  const coach = extract('function showCoach');
  assert.match(coach, /new ResizeObserver\(repositionCoach\)/, 'showCoach must observe the header');
  assert.match(coach, /coachRO\.observe\(hdr\)/, 'the observer must watch the header element');
  const dis = extract('function dismissCoach');
  assert.match(dis, /coachRO\.disconnect\(\)/, 'dismissCoach must disconnect the observer');
});

// The save gate must judge what is IN the editor, not what was last captured:
// a typed replacement password (or a just-cleared field) exists only in the
// DOM until captureIperfEditor folds it in.
// Quick Setup applies as ONE atomic call to /api/quick-setup - not a sequence of
// partial settings/access/marker POSTs (which froze settings, could commit
// opposite choices on retry, and could mark done half-applied). The server marks
// the install answered in the same transaction; the client just posts once.
test('Quick Setup applies via one atomic /api/quick-setup call', () => {
  const save = extract('function qsSave');
  // Exactly one write, to the atomic endpoint - no /api/settings, /api/access,
  // /api/update, or a separate marker POST.
  assert.match(save, /fetch\('api\/quick-setup'/, 'qsSave posts to the atomic endpoint');
  assert.doesNotMatch(save, /fetch\('api\/settings'/, 'no separate settings POST');
  assert.doesNotMatch(save, /fetch\('api\/access'/, 'no separate access POST');
  assert.doesNotMatch(save, /fetch\('api\/update'/, 'no separate update POST');
  assert.doesNotMatch(save, /quick_setup_done/, 'the marker is the server\'s job, not a client write');
  // The body carries the full answer, and "This machine only" maps to local_only.
  assert.match(save, /local_only: !network/, 'machine-only maps to local_only:true');
  assert.match(save, /encode\(user\)\.length>128/, 'username byte cap checked before the round trip');
});

test('the orphan save-gate captures the open editor before judging', () => {
  const save = script.slice(script.indexOf("$('saveSettings').addEventListener"));
  const cap = save.indexOf('captureIperfEditor();');
  const gate = save.indexOf('iperfPwOrphan()');
  assert.ok(cap >= 0 && gate >= 0 && cap < gate, 'captureIperfEditor() must run before the orphan check');
});

// A renamed iperf3 server whose stored password was never re-entered is one
// Save away from losing the credential: the store files passwords strictly by
// address, so the rename orphans it. v0.60.0's render-time message clear made
// that loss SILENT (Done's own click wiped the warning); the orphan predicate
// is what now keeps the warning alive and blocks the save.
test('iperfPwOrphan: exactly the one-save-from-loss state', () => {
  const { iperfPwOrphan } = F;
  const base = { _del: false, orig_has_password: true, orig_addr: 'a.example:5201', addr: 'b.example:5201', password: '', label: '' };
  fakeIperfServers.length = 0;
  fakeIperfServers.push({ ...base });
  assert.ok(iperfPwOrphan(), 'renamed + stored password + nothing typed = orphan');
  fakeIperfServers[0].password = 'newpw';
  assert.equal(iperfPwOrphan(), null, 'a typed replacement un-orphans');
  fakeIperfServers[0].password = ''; fakeIperfServers[0].addr = 'a.example:5201';
  assert.equal(iperfPwOrphan(), null, 'restoring the address un-orphans');
  fakeIperfServers[0].addr = 'b.example:5201'; fakeIperfServers[0]._del = true;
  assert.equal(iperfPwOrphan(), null, 'a deleted row is an explicit choice, not an orphan');
  fakeIperfServers[0]._del = false; fakeIperfServers[0].orig_has_password = false;
  assert.equal(iperfPwOrphan(), null, 'no stored password, nothing to lose');
  fakeIperfServers.length = 0;
});

// The warning outlives the editor exactly while its reason does, and the save
// is blocked while the reason stands. Source-order pins: the orphan branch runs
// before the editor-closed clear, and the guard sits before the body capture.
test('the orphaned-password warning survives Done and blocks Save', () => {
  const render = script.slice(script.indexOf('function renderIperfServers()'));
  const rbody = render.slice(0, render.indexOf('\nfunction '));
  assert.ok(rbody.indexOf('iperfPwOrphan()') >= 0 && rbody.indexOf('iperfPwOrphan()') < rbody.indexOf('iperfEditingId==null'),
    'render must consult the orphan predicate before the editor-closed clear');
  const save = script.slice(script.indexOf("$('saveSettings').addEventListener"));
  assert.ok(save.indexOf('iperfPwOrphan()') >= 0 && save.indexOf('iperfPwOrphan()') < save.indexOf('const body = settingsBody()'),
    'save must refuse an orphaning payload before it is built');
});

// Quick Setup's cost line is arithmetic shown to a stranger deciding whether to
// consent to data spend - the numbers are pinned, not trusted, and pinned as a
// CEILING. A speedtest-go transfer runs a fixed 15s per direction (the library's
// DataManager captureTime, which production never shortens) unless the rate
// converges early, and the earliest that can happen is 10.05s, so on the 1 Gbit
// reference line one ATTEMPT at one direction tops out at 15s x 125 MB/s =
// 1.875 GB.
//
// An attempt is not a run. speedDefaultRetries is 1 (speedtest.go, "2 attempts
// per direction"), the retry re-runs the whole window rather than resuming, and
// both attempts are billed on purpose: ookla.go rebuilds the transfer per
// attempt and sums the bytes into the run's usage, and the iperf engine's
// TestIperfRunFailureSumsRetryBytes pins a retried direction at 2 x 125 MB to
// keep that property. So the figure below is a per-attempt ceiling, and it
// is only honest while the note also says the retry can double it: that
// disclosure is asserted here next to the number so neither can be edited away
// alone.
//
// The old copy priced the window mid-range (1.8 GB down, 1.275 GB up) and so
// promised 30.6 GB up per day for the hourly cadence where the per-attempt
// ceiling is 45.0 GB - 47% more data than the dialog asked consent for, on a
// link that may be metered. The rule enforced here is one-directional: the
// figure may over-state, never under-state.
test('Quick Setup cost note: never under-states the data spend', () => {
  const { qsUseNote } = F;
  const CEILING_GB = 1.875; // 15s cap x 125 MB/s: one attempt at one direction
  const stated = s => {
    const m = s.match(/([\d.]+)\u00a0GB down \+ ([\d.]+)\u00a0GB up per day/);
    assert.ok(m, 'the note must quote a down and an up figure: ' + s);
    return [Number(m[1]), Number(m[2])];
  };
  // Runs per day per branch, priced at the interval the install is ACTUALLY on.
  // The on-reconnect trigger no choice here switches off is spaced by
  // reconnectSpeedGap = max(15 min, the configured interval) (main.go), and
  // reconnectGate.allow enforces that as a minimum SPACING, not a per-day quota:
  // it refuses a run only while now-last is under the gap, so runs land at t,
  // t+gap, t+2gap, ... and a day holds ceil(86400/gap) of them - one at the start
  // plus one per whole gap after it. The two scheduled branches apply their own
  // interval (qsSave posts speed_seconds 3600 / 21600), so 24 and 4 hold whatever
  // the daemon is currently set to. "Manually" applies none (ApplyQuickSetup
  // writes keySpeed only when tests are enabled), so it inherits the daemon's -
  // and at any interval under 15 minutes the floor takes over at 96 runs a day,
  // four times what the hourly default gets. That case is the reason the figure is
  // derived rather than hardcoded, so it is priced here at 1m and 5m alike. 25m and
  // 5h are the intervals that do NOT divide the day, where the run that starts the
  // day is the whole difference between the two roundings.
  //
  // 8h is here for the OTHER way this figure could come in low: not the run count
  // but the printing of it. 3 runs is 5.625 GB, which one decimal cannot express,
  // and rounding to the nearest one printed "5.6" - 25 MB under what the install
  // can spend. It is the smallest of the reachable ties (every runs%4===3 lands on
  // .625 or .125: 3, 7, 11 ... 95, so 24 of the 96 reachable counts), and an
  // 8-hour interval is an ordinary setting rather than a corner. The assertions
  // below compare against the unrounded ceiling, so they catch a rounding that
  // goes the wrong way as readily as a wrong run count.
  for (const [sel, intervalS, runs] of [[0, 3600, 24], [1, 3600, 4], [2, 3600, 24],
    [0, 60, 24], [1, 60, 4], [2, 60, 96], [2, 300, 96], [2, 900, 96], [2, 21600, 4],
    [2, 1500, 58], [2, 18000, 5], [2, 28800, 3], [2, 12343, 7]]) {
    const [down, up] = stated(qsUseNote(sel, intervalS));
    const ceil = runs * CEILING_GB;
    assert.ok(down >= ceil, `sel=${sel} at ${intervalS}s promises ${down} GB down, under the ${ceil} GB ceiling`);
    assert.ok(up >= ceil, `sel=${sel} at ${intervalS}s promises ${up} GB up, under the ${ceil} GB ceiling`);
  }
  // The exact figures, so a re-derivation has to come through this test.
  assert.match(qsUseNote(0, 3600), /up to <b>45\.0\u00a0GB down \+ 45\.0\u00a0GB up per day<\/b>/);
  assert.match(qsUseNote(1, 3600), /up to <b>7\.5\u00a0GB down \+ 7\.5\u00a0GB up per day<\/b>/);
  // "Manually" on a 1-minute install: 15-minute floor, 96 runs, 180.0 GB per
  // direction. A hardcoded 24 quoted 45.0 GB here - a quarter of the truth on
  // the one screen whose job is consent for data on a metered link.
  assert.match(qsUseNote(2, 60), /up to <b>180\.0\u00a0GB down \+ 180\.0\u00a0GB up per day<\/b>/);
  assert.match(qsUseNote(2, 3600), /up to <b>45\.0\u00a0GB down \+ 45\.0\u00a0GB up per day<\/b>/);
  assert.match(qsUseNote(2, 21600), /up to <b>7\.5\u00a0GB down \+ 7\.5\u00a0GB up per day<\/b>/);
  // A gap that does not divide the day. 86400/1500 = 57.6, so 57 whole gaps fit -
  // but the run that opens the day is a run too, and the 58th starts at 85500s,
  // still inside it: 58 x 1.875 = 108.8 GB. floor() quoted 106.9 and left a whole
  // run of consent unasked for.
  assert.match(qsUseNote(2, 1500), /up to <b>108\.8\u00a0GB down \+ 108\.8\u00a0GB up per day<\/b>/);
  // The same one-run miss, and the wider the gap the bigger a share of the day it
  // is: 4 runs where 5 fit is a fifth of the spend.
  assert.match(qsUseNote(2, 18000), /up to <b>9\.4\u00a0GB down \+ 9\.4\u00a0GB up per day<\/b>/);
  // A run count the printed decimal cannot express: 3 x 1.875 = 5.625, rounded UP
  // to 5.7 rather than to the nearer 5.6, which was 25 MB under the spend.
  assert.match(qsUseNote(2, 28800), /up to <b>5\.7\u00a0GB down \+ 5\.7\u00a0GB up per day<\/b>/);
  // The next tie up, to pin the rule rather than the one case: 7 x 1.875 = 13.125.
  assert.match(qsUseNote(2, 12343), /up to <b>13\.2\u00a0GB down \+ 13\.2\u00a0GB up per day<\/b>/);
  // The spacing the copy quotes has to be the spacing the figure came from, or
  // the number is unexplainable on screen.
  assert.match(qsUseNote(2, 60), /once every 15 minutes/);
  assert.match(qsUseNote(2, 3600), /once every hour/);
  assert.match(qsUseNote(2, 21600), /once every 6 hours/);
  assert.match(qsUseNote(2, 1500), /once every 25 minutes/);
  assert.match(qsUseNote(2, 18000), /once every 5 hours/);
  // A payload that omits speed_interval_s falls back to the shipped 1h
  // (config.Default) rather than pricing the screen at zero runs.
  assert.equal(qsUseNote(2, undefined), qsUseNote(2, 3600));
  // "roughly" reads as a two-sided estimate; a ceiling is one-sided.
  assert.doesNotMatch(qsUseNote(0, 3600), /roughly/, 'a ceiling is quoted as "up to", not "roughly"');
  assert.doesNotMatch(qsUseNote(0, 3600), /month/, 'the monthly total was deliberately dropped');
  // The retry is the difference between a per-attempt figure and a cap, so no
  // branch may quote the figure without it - and naming it has to mean asserting
  // it HAPPENS. A bare /retr(y|ies)/ pin passes just as happily on copy that says
  // the opposite ("A failed test NEVER retries"), so each branch's affirmative
  // phrasing is pinned and a negated one is rejected outright.
  const retryClaim = {0: /\bretries once\b/, 1: /\bretries once\b/, 2: /\bfails? and retr(y|ies)\b/};
  for (const sel of [0, 1, 2]) {
    assert.match(qsUseNote(sel, 3600), retryClaim[sel],
      `sel=${sel} quotes a per-attempt figure without saying a failed test retries`);
    assert.doesNotMatch(qsUseNote(sel, 3600), /\b(never|not|no|without)\b(\s+\w+){0,2}\s+retr/i,
      `sel=${sel} denies the retry that its per-attempt figure depends on`);
  }
});

// reconnectGate holds its spacing in a plain time.Time field on an in-process
// struct (main.go), whose zero value that file documents as an OPEN gate - the
// first reconnect of a process is never suppressed. So a daemon that restarts
// starts the day's budget over, and NO per-day ceiling on this screen survives a
// crash loop. The disclosure has to sit on all three branches: the scheduled ones
// carry the same hole, since their extra reconnect run is gated by the same
// in-memory field. Persisting the timestamp instead was rejected - it would
// invert the tested invariant in main.go and still leave the startup run open -
// so this is a disclosure, and the disclosure is what gets pinned.
test('Quick Setup says its daily ceilings assume the daemon stays up', () => {
  const { qsUseNote } = F;
  for (const [sel, intervalS] of [[0, 3600], [1, 3600], [2, 60], [2, 3600], [2, 21600]]) {
    const note = qsUseNote(sel, intervalS);
    assert.match(note, /assume the daemon keeps running/,
      `sel=${sel} at ${intervalS}s quotes a daily figure without saying it assumes an uninterrupted daemon`);
    assert.match(note, /\brestart\b/,
      `sel=${sel} at ${intervalS}s names no restart, so nothing on screen says what breaks the ceiling`);
  }
});

// SpeedtestOnReconnect defaults ON, and main.go's reconnect dispatch consults it
// without ever consulting SpeedtestEnabled - so no choice on this screen turns it
// off. Under "Manually" Quick Setup leaves the interval alone (ApplyQuickSetup
// writes keySpeed, "speed_interval_s", only when tests are enabled;
// speed_seconds is the request field the screen POSTs), so it inherits whatever
// the daemon already runs at and reconnectSpeedGap = max(15 min, that interval)
// lets a flapping line run 24 tests a day on the shipped 1h and 96 on a 1m
// install: exactly the branch whose old copy implied the data cost was near
// zero. Every branch has to disclose it, at every interval.
test('Quick Setup discloses the after-outage tests this screen cannot switch off', () => {
  const { qsUseNote } = F;
  for (const [sel, intervalS] of [[0, 3600], [1, 3600], [2, 60], [2, 3600], [2, 21600]]) {
    assert.match(qsUseNote(sel, intervalS), /outage/, `sel=${sel} at ${intervalS}s must mention the after-outage run`);
    assert.match(qsUseNote(sel, intervalS), /on by default/, `sel=${sel} at ${intervalS}s must say that trigger is on by default`);
  }
  assert.match(qsUseNote(2, 3600), /not switched off here/, '"Manually" must say it does not disable the trigger');
  // Both scheduled cadences get one extra run per interval at worst - the whole
  // figure again - and the default single retry can double each of those, so the
  // worst case they must quote is four times the headline, not two.
  assert.match(qsUseNote(0, 3600), /four times that/);
  assert.match(qsUseNote(1, 3600), /four times that/);
  // "Manually" has no schedule: its headline is already the reconnect runs, so
  // the retry alone is what doubles it - at every interval, not just the default.
  for (const intervalS of [60, 3600, 21600]) assert.match(qsUseNote(2, intervalS), /twice that/);
});

// The gate is strict equality: an older daemon or the demo shim simply lacks
// the field, and absent must mean never - not truthy-by-accident.
test('Quick Setup only shows on quick_setup_pending === true, and defaults access to how it booted', () => {
  assert.match(script, /s\.quick_setup_pending===true\)\{ qsBootLocalOnly = s\.access_local_only!==false; showQuickSetup\(\); \}/,
    'the boot hook gates on strict === true and captures the booted access mode');
  // When the install booted network-reachable (e.g. -access network), "This
  // machine only" would lock the operator out, so the note must warn instead of
  // promising "nothing is reachable".
  const mo = extract('function qsMachineOnlyNote');
  assert.match(mo, /qsBootLocalOnly/, 'the machine-only note branches on how the install booted');
  assert.match(mo, /lock you out/, 'a network-reachable install warns that "This machine only" locks you out');
  // showQuickSetup preselects the access radio from the booted mode.
  assert.match(script, /segChoose\.qsAcc\(qsBootLocalOnly \? 0 : 1, false\)/,
    'the access choice defaults to the booted mode so accepting it never locks the operator out');
});

// qsUseNote is pure so its numbers can be pinned above, which only means anything
// if the page hands it the real interval: both places that paint the note have to
// pass it, and it has to come off the status payload on the POLLED path rather
// than being read once at boot. The dialog is long-lived - it sits there until the
// operator answers it - and the value is the daemon's, not the page's, so one read
// at startup could be stale by the time anything prices itself from it.
//
// Source order is deliberately NOT asserted. The capture does sit above the
// quick_setup_pending branch in refreshStatus, but nothing visible turns on that:
// showQuickSetup runs at most once (qsShown) and paints qsSchedSel, which starts
// at 0 - the Hourly branch, which never looks at intervalS - so the automatic
// paint prices nothing from it. Every later paint is a segment click (wireSeg
// 'qsSched'), with refreshStatus re-capturing on its 3s interval underneath.
// Asserting that one line precedes the other would pin source order, not
// behaviour, and the reason given for it before ("a 1m install priced at the
// shipped 1h for the whole of its life") was not true of any of those paths.
test('Quick Setup prices its note from the interval /api/status reported', () => {
  assert.match(script, /let qsSpeedIntervalS=3600;/,
    'the interval has a module-level default to fall back on');
  const calls = script.match(/qsUseNote\([^)]*\)/g).filter(c => !/^qsUseNote\(sel/.test(c));
  assert.ok(calls.length >= 2, 'both the initial paint and the segment change must be covered');
  for (const c of calls) assert.match(c, /qsSpeedIntervalS/, `a caller quotes the default figure: ${c}`);
  assert.ok(script.indexOf('qsSpeedIntervalS=s.speed_interval_s') > 0,
    'nothing reads speed_interval_s off the status payload');
  assert.ok(extract('async function refreshStatus').includes('qsSpeedIntervalS=s.speed_interval_s'),
    'the capture sits outside refreshStatus, so the interval never refreshes under an open dialog');
  // Guarded on a positive number: a payload without the field must leave the
  // shipped default standing, not price the screen at zero runs a day.
  assert.match(script, /typeof s\.speed_interval_s==='number' && s\.speed_interval_s>0/,
    'an absent or nonsense interval must not reach the arithmetic');
});

// The two exits differ, permanently: dismissal persists the answer and applies
// nothing (the previewed theme reverts); the dialog itself is a real modal.
test('Quick Setup markup and decline semantics', () => {
  assert.match(html, /id="qsDlg" role="dialog" aria-modal="true"/);
  assert.equal((html.match(/class="qs-sw(?: on)?" data-sw=/g) || []).length, 9, 'nine theme swatches');
  assert.match(html, /id="qsX" aria-label="Dismiss and keep the defaults"/);
  const dec = extract('function qsDecline');
  assert.match(dec, /applyTheme\(t\)/, 'decline must un-preview the theme');
  assert.match(dec, /qsMark\(\)/, 'decline must persist the answer');
  const mark = extract('function qsMark');
  assert.match(mark, /api\/quick-setup/, 'decline marks done via the atomic endpoint, not the freezing settings post');
  assert.match(mark, /dismiss:true/, 'decline sends dismiss (marker only, freezes nothing)');
  const save = extract('function qsSave');
  assert.match(save, /local_only: !network/, 'access scope is derived from the chosen option');
  assert.match(save, /loadSettings\(\)/, 'a save re-syncs the drawer from server truth');
});

// Leaving a saved-on toggle enabled is only half the rule: the moment it is
// unticked, the reason to leave it enabled is gone. Without a re-gate the box
// stays clickable on a build that cannot send the flag, and the note still reads
// "turn this off" for a flag that is already off - so it can be ticked straight
// back on and every run to that server fails before it connects. This drives the
// real DOM function through the transition, not just the pure rule.
test('unticking the legacy-padding toggle re-locks it and drops the stale note', () => {
  const els = {
    setIperfPKCS1: { checked: true, disabled: false, closest: () => ({ classList: { toggle() {} } }) },
    iperfPKCS1Note: { textContent: '', classList: { add() {}, remove() {} } },
  };
  const gate = new Function('$', 'iperfVersion',
    extract('function iperfVerNum') + '\n' + extract('function pkcs1FlagUsable') + '\n'
    + extract('function pkcs1BoxState') + '\n' + extract('function applyPKCS1Gate')
    + '\nreturn applyPKCS1Gate;')(id => els[id], '3.21');

  gate();  // a server saved with the flag on, opened on a 3.21 host
  assert.equal(els.setIperfPKCS1.disabled, false, 'a saved-on flag must stay clearable');
  assert.match(els.iperfPKCS1Note.textContent, /^Turn this off/);

  els.setIperfPKCS1.checked = false;  // the operator unticks it; the change listener re-gates
  gate();
  assert.equal(els.setIperfPKCS1.disabled, true, 'once off, it must not be tickable again');
  assert.match(els.iperfPKCS1Note.textContent, /^Unavailable/,
    'the note must stop telling the operator to turn off a flag that is already off');
});

// The transition above only happens if something calls the gate on change.
test('the legacy-padding toggle re-gates on change', () => {
  assert.match(script, /\$\('setIperfPKCS1'\)\.addEventListener\('change', applyPKCS1Gate\)/,
    'no change listener: the gate is computed once and never revisited');
});

// The address/password warning is scoped to the edit that raised it. Save, Done,
// Load defaults and a discarded change all close the editor and re-render, so the
// render is where the message dies - otherwise reopening Settings claims a
// password needs re-entering after it was successfully saved.
test('an edit-scoped iperf message cannot outlive its editor', () => {
  const render = script.slice(script.indexOf('function renderIperfServers()'));
  const body = render.slice(0, render.indexOf('\nfunction '));
  assert.match(body, /if\(iperfEditingId==null\) \$\('iperfMsg'\)\.textContent='';/,
    'renderIperfServers does not clear the edit message when no editor is open');
  assert.ok(body.indexOf("if(iperfEditingId==null)") < body.indexOf("if(need)"),
    'the clear must come BEFORE the list status line, or it wipes it');
});

// opacity on a label multiplies into everything inside it, including the help
// tooltip - which is body-size text that then fails contrast. .srow.dep-off has
// carried an escape for this all along; the server editor's own field layout
// gained the same greying and needs the same escape.
test('a greyed field does not grey the tooltip it contains', () => {
  assert.match(html, /\.ie-field\.dep-off \.ie-lbl:has\(\.info:hover\),\.ie-field\.dep-off \.ie-lbl:has\(\.info:focus\)\{opacity:1;/,
    'no full-opacity escape for an open tooltip inside a dimmed .ie-field');
});

// The note element the gate writes into must exist, or the explanation silently
// goes nowhere and the toggle just looks broken.
test('the PKCS1 gate has a note element and a disabled style', () => {
  assert.match(html, /id="iperfPKCS1Note"/, 'gate note element missing from the markup');
  assert.match(html, /\.ie-field\.dep-off/, 'no greying rule for a disabled .ie-field toggle');
});

// The saved iperf3 password is filed under the SAVED address, so mapIperfServer
// must remember what the record actually holds (orig_has_password) separately
// from the live flag. Without that the address handler could only latch the flag
// off: typing the original address back left the UI insisting on a retype for a
// password that was still perfectly reachable.
test('mapIperfServer remembers the saved-password state independently', () => {
  const m = PS.mapIperfServer({ addr: '10.0.0.5', has_password: true });
  assert.equal(m.has_password, true);
  assert.equal(m.orig_has_password, true, 'saved state must survive edits to the live flag');
  assert.equal(m.orig_addr, '10.0.0.5', 'the address the password is filed under');
  const fresh = PS.mapIperfServer({ addr: '10.0.0.6' });
  assert.equal(fresh.orig_has_password, false);
});

// --- speedtest server picker: HTTP Legacy Fallback labelling ----------------
// fallback_ok is three-state and the third state is the common one: the daemon
// probes concurrently under a short budget, so a cold list legitimately arrives
// partly unverified. Only an explicit false may be labelled; undefined must look
// exactly like a healthy row - UNLESS an earlier response already proved the
// server broken, in which case the serverHealth memory keeps the warning (F9).
// This drives the REAL rememberServerHealth/annotateOption/populateServers
// chain out of index.html through a sequence of /servers responses, returning
// the dropdown rows the LAST response built.
function driveServerPicker(responseSeq) {
  const defs = extract('function rememberServerHealth') + '\n' + extract('function annotateOption') + '\n'
    + extract('const serverOptionText') + '\n' + extract('function populateServers');
  const rows = [];
  const sel = { set innerHTML(v) { rows.length = 0; }, appendChild(o) { rows.push(o); } };
  const doc = { createElement: () => ({ dataset: {}, textContent: '' }) };
  const populate = new Function('$', 'document', 'serverHealth', 'autoText', 'applyPendingServer', 'updateScopeNote',
    defs + '\nreturn populateServers;')(() => sel, doc, new Map(), () => 'Auto', () => {}, () => {});
  for (const list of responseSeq) populate(list);
  return rows; // rows[0] is the Auto option
}

const UNAVAIL = / - unavailable \(no speedtest endpoint\)$/;

test('picker labels only servers proven to have no fallback', () => {
  const rows = driveServerPicker([[
    { id: 1, sponsor: 'Good', name: 'A', fallback_ok: true },
    { id: 2, sponsor: 'Dead', name: 'B', fallback_ok: false },
    { id: 3, sponsor: 'Unknown', name: 'C' },
  ]]);
  assert.equal(rows[1].textContent, 'Good - A');
  assert.match(rows[2].textContent, UNAVAIL);
  assert.equal(rows[2].dataset.unusable, '1');
  assert.equal(rows[3].textContent, 'Unknown - C', 'an undetermined server must not be labelled unavailable');
});

// F9 regression: the daemon's annotate budget can expire (or its verdict cache
// lapse) on a LATER fetch, returning a previously-proven-broken server with no
// fallback_ok at all. The rebuilt row must keep the remembered warning, or the
// user pins a server that fails every run.
test('a proven-broken server keeps its warning when a later listing is undetermined', () => {
  const rows = driveServerPicker([
    [{ id: 5, sponsor: 'Dead', name: 'B', fallback_ok: false }],
    [{ id: 5, sponsor: 'Dead', name: 'B' }],   // the verdict aged out of the response
  ]);
  assert.match(rows[1].textContent, UNAVAIL, 'the remembered false verdict must survive a refetch');
  assert.equal(rows[1].dataset.unusable, '1');
});

// ...and the memory must not win the other way: a fresh verdict beats a stale one.
test('a fresh healthy verdict clears a stale remembered warning', () => {
  const rows = driveServerPicker([
    [{ id: 5, sponsor: 'S', name: 'X', fallback_ok: false }],
    [{ id: 5, sponsor: 'S', name: 'X', fallback_ok: true }],   // repaired since
  ]);
  assert.equal(rows[1].textContent, 'S - X');
  assert.equal(rows[1].dataset.unusable, undefined);
});

// The guard governs automatic selection only. A labelled server stays
// selectable, so a user who wants it can still pin it. The block spans
// annotateOption (the single labelling path) through the end of populateServers.
test('an unavailable server is labelled but never disabled', () => {
  const block = html.slice(html.indexOf('function annotateOption'), html.indexOf('applyPendingServer(); updateScopeNote();'));
  assert.match(block, /unavailable \(no speedtest endpoint\)/, 'the label must be applied');
  assert.match(block, /annotateOption\(o, s\.id\)/, 'every rebuilt row must consult the serverHealth memory');
  assert.doesNotMatch(block, /o\.disabled\s*=\s*true/, 'disabling would block an explicit pin the daemon honours');
});

// FIX-8 regression: a pinned server outside the nearby list gets a synthesized
// "Server <id>" row, and applyPendingServer asks the daemon to probe it by ID so
// the row can gain its "unavailable" annotation. handleSpeedtestServers is
// POST + application/json only (requireJSONCT), so the old bare GET 405/415'd and
// the whole lookup was silently swallowed - the annotation never arrived. This
// drives the REAL applyPendingServer out of index.html and captures the request.
function driveApplyPendingServer({ pendingServer, seen = [] }) {
  const body = extract('function applyPendingServer');
  const options = [];
  const sel = { options, appendChild(o) { options.push(o); }, value: '' };
  const $ = id => (id === 'setServer' ? sel : { value: '' });
  const document = { createElement: () => ({ dataset: {} }) };
  const serverHealth = new Map(seen.map(id => [String(id), false]));
  const calls = [];
  const fetchStub = (url, opts) => { calls.push({ url, opts }); return Promise.resolve({ ok: true, json: async () => ({ servers: [] }) }); };
  const make = new Function('$', 'document', 'pendingServer', 'serverHealth', 'annotateOption', 'rememberServerHealth', 'fetch',
    body + '\nreturn applyPendingServer;');
  make($, document, pendingServer, serverHealth, () => {}, () => {}, fetchStub)();
  return calls;
}

test('applyPendingServer resolves an unseen pin with POST+JSON, not a bare GET', () => {
  const calls = driveApplyPendingServer({ pendingServer: '54321', seen: [] });
  assert.equal(calls.length, 1, 'an unseen pin must trigger exactly one by-ID lookup');
  const { url, opts } = calls[0];
  assert.equal(opts?.method, 'POST', 'the handler is POST-only; a GET 405s and the lookup is swallowed');
  assert.equal(opts?.headers?.['Content-Type'], 'application/json', 'requireJSONCT rejects a missing JSON content type with 415');
  assert.ok(!url.startsWith('/'), 'a relative url so it resolves under a reverse-proxy subpath (got ' + url + ')');
  assert.match(url, /^api\/speedtest\/servers\?id=54321$/);
});

test('applyPendingServer does not re-fetch a pin whose health is already known', () => {
  const calls = driveApplyPendingServer({ pendingServer: '77', seen: ['77'] });
  assert.equal(calls.length, 0, 'a known verdict must not re-issue the by-ID lookup');
});

// --- Ookla list requests: only the newest may write the page's scope ----------
// fetchServers is the single entry point for the city search, the by-ID pin and
// the auto refetch, and every one of them mutates autoLoc/autoLabel/pendingServer
// and repopulates the dropdown when it lands. They overlap for real - ticking Auto
// starts the auto refetch under a running city search - so the real functions are
// driven here against fetches the test resolves by hand, in the order that breaks.
const tick = () => new Promise(r => setTimeout(r, 0));
function driveServers() {
  const fetches = [];
  const fetchStub = () => new Promise((resolve, reject) => {
    fetches.push({ ok: body => resolve({ json: async () => body }), fail: () => reject(new Error('offline')) });
  });
  const els = {
    serverCity: { value: '' },
    setServer: { value: '', disabled: false },
    serverSearchBtn: { disabled: false, innerHTML: 'Search' },
    settingsMsg: { textContent: '' },
  };
  const log = { loading: [], populated: [], errs: [] };
  const api = new Function('$', 'fetch', 'setServerLoading', 'populateServers',
    'applyPendingServer', 'updateScopeNote', 'clearServerFieldError', 'serverFieldError', 'serverTabActive',
    'let serversLoaded=false, serversScope="", serverTabSeen=true, pendingServer="", autoLoc="", '
    + 'autoLabel="", autoDefaultLoc="", autoCentreLastRun=false, serverSearchInFlight=null, serverSearchFailed=false;\n'
    // The generation counter itself is lifted from the page, so deleting it there
    // fails this drive rather than letting the test supply its own.
    + script.match(/let serversGen\s*=\s*0;/)[0] + '\n'
    + script.match(/const shortPlace = [^;]*;/)[0] + '\n'
    + extract('async function fetchServers') + '\n'
    + extract('async function loadServers') + '\n'
    + extract('function chooseAutoServer') + '\n'
    + extract('async function searchServers') + '\n'
    + 'return { fetchServers, chooseAutoServer, searchServers, '
    + 'state: () => ({ autoLoc, autoLabel, pendingServer, searchFailed: serverSearchFailed }) };')(
    id => els[id], fetchStub, on => log.loading.push(on), s => log.populated.push(s),
    () => {}, () => {}, () => {}, m => log.errs.push(m), () => true);
  return { api, els, fetches, log };
}

test('server list: a city search abandoned for Auto cannot restore city scope', async () => {
  const { api, els, fetches, log } = driveServers();
  els.serverCity.value = 'Paris';
  const search = api.searchServers();
  await tick();
  assert.equal(fetches.length, 1, 'the search must be in flight for this race to exist');
  api.chooseAutoServer();              // Auto ticked before the city answer lands
  await tick();
  assert.equal(fetches.length, 2, 'ticking Auto refetches the auto-scoped list');

  // The abandoned city request answers FIRST, while the auto one is still running.
  fetches[0].ok({ lat: 48.8, lon: 2.3, location: 'Paris, FR, Europe', servers: [{ id: 'p' }] });
  await search; await tick();
  assert.deepEqual(log.populated, [],
    'the abandoned city list repopulated the dropdown under a checked Auto box');
  assert.deepEqual(log.loading, [true, true],
    'the abandoned request cleared the spinner the live request is still showing');
  assert.deepEqual(log.errs, []);

  fetches[1].ok({ location: 'Berlin, DE', centre: 'last_run', servers: [{ id: 'a' }] });
  await tick();
  const st = api.state();
  assert.equal(st.autoLoc, '', 'a stale city response restored city scope while Auto is checked');
  assert.equal(st.autoLabel, '');
  assert.deepEqual(log.populated, [[{ id: 'a' }]], 'the auto list is what the dropdown ends up showing');
  assert.deepEqual(log.loading, [true, true, false]);
});

test('server list: of two overlapping searches the newest wins, whichever answers first', async () => {
  const { api, fetches, log } = driveServers();
  api.fetchServers({ city: 'Paris' });
  api.fetchServers({ city: 'Berlin' });
  await tick();
  assert.equal(fetches.length, 2);
  fetches[1].ok({ lat: 52.5, lon: 13.4, location: 'Berlin, DE', servers: [{ id: 'b' }] });
  await tick();
  fetches[0].ok({ lat: 48.8, lon: 2.3, location: 'Paris, FR', servers: [{ id: 'p' }] });
  await tick();
  const st = api.state();
  assert.equal(st.autoLoc, '52.5,13.4', 'the older search overwrote the newer one');
  assert.equal(st.autoLabel, 'Berlin, DE');
  assert.deepEqual(log.populated, [[{ id: 'b' }]], 'and it put the older city list back in the dropdown');
});

test('server list: an abandoned search cannot report a failure the page has moved past', async () => {
  const { api, els, fetches, log } = driveServers();
  els.serverCity.value = 'Nowherecity';
  const search = api.searchServers();
  await tick();
  api.chooseAutoServer();
  await tick();
  fetches[0].fail();                   // the abandoned lookup fails, late
  await search; await tick();
  assert.deepEqual(log.errs, [],
    '"city not found" was painted into the footer under a hidden search box');
  assert.equal(api.state().searchFailed, false,
    'the abandoned failure left the save-blocking flag set');
  assert.equal(els.serverSearchBtn.disabled, false,
    'the Search button must be released even when its search was abandoned');
  assert.equal(els.serverSearchBtn.innerHTML, 'Search', 'and its label restored, not left as a spinner');
});

// --- container-awareness surfaces (I1/I5/I8/I9) ------------------------------
// I5: family "auto" resolves invisibly - a dual-stack native host measures IPv6
// where an IPv6-less bridge silently measures IPv4 - so a recorded family is
// named in the detailed run views, and an unrecorded one shows NOTHING (older
// rows / engines that never captured it must not be guessed at).
test('famLabel: names only a recorded family', () => {
  assert.equal(F.famLabel('4'), 'IPv4');
  assert.equal(F.famLabel('6'), 'IPv6');
  // iperf runs down and up as separate processes, and a dual-stack hostname can
  // land them on DIFFERENT families; the engine stores that as 'mixed' rather
  // than letting one direction speak for the row, and the UI must name it.
  assert.equal(F.famLabel('mixed'), 'IPv4+IPv6');
  assert.equal(F.famLabel(''), '');
  assert.equal(F.famLabel(undefined), '');
  assert.equal(F.famLabel('ipv6'), '', 'only the stored enum, never a lookalike');
});

// 'mixed' alone reads like a single dual-stack transfer, so it - and only it -
// carries an explaining tooltip; the plain families need none.
test('famTitle: explains mixed, and ONLY mixed', () => {
  assert.match(F.famTitle('mixed', 'iperf3'), /different IP famil/i, 'the mixed tooltip says what really happened');
  assert.equal(F.famTitle('4', 'iperf3'), '');
  assert.equal(F.famTitle('6', 'ookla'), '');
  assert.equal(F.famTitle('', 'ookla'), '');
  assert.equal(F.famTitle(undefined, 'ookla'), '');
});

// The two engines earn different sentences. iperf3 runs each direction as its
// own process, so 'mixed' really does mean the directions differed; Ookla
// records one family set across both directions AND every retry, so claiming
// per-direction knowledge there would be a lie the data can't support.
test('famTitle: only iperf3 may claim the directions differed', () => {
  assert.match(F.famTitle('mixed', 'ookla'), /across its directions and any retries/i);
  assert.doesNotMatch(F.famTitle('mixed', 'ookla'), /Download and upload were measured over different/i,
    'an Ookla run cannot claim per-direction families');
  assert.match(F.famTitle('mixed', undefined), /across its directions and any retries/i,
    'unknown engine gets the weaker, always-true wording');
});

// I9: which way the UDP loss/jitter probe sampled. Anything but the closed
// down/up enum (older rows, Ookla, a crafted backup) yields no label at all.
test('udpDirLabel / udpDirTitle: down/up -> path labels, anything else -> nothing', () => {
  assert.equal(F.udpDirLabel('down'), 'download path');
  assert.equal(F.udpDirLabel('up'), 'upload path');
  assert.equal(F.udpDirLabel(''), '');
  assert.equal(F.udpDirLabel(undefined), '');
  assert.equal(F.udpDirTitle({ udp_direction: 'up' }), 'Loss/jitter probe: upload path');
  assert.equal(F.udpDirTitle({}), '', 'no direction -> no tooltip attribute at all');
});

// qualityParts needs lossStr (a brace-less arrow const), so it is compiled with
// its real dependencies rather than through the shared factory.
// pingMeasured rides along for the same reason (it gates the ping readout), and
// isNum - a brace-less arrow const - is injected exactly as the shared factory does.
const qualityParts = new Function('isNum', script.match(/const lossStr = [^;]*;/)[0] + '\n'
  + extract('function udpDirLabel') + '\n' + extract('function pingMeasured') + '\n'
  + extract('function qualityParts')
  + '\nreturn qualityParts;')(v => typeof v === 'number');

test('the tooltip labels loss/jitter with the probed direction ONLY when the sample carries one', () => {
  const parts = qualityParts({ ping_ms: 4.5, jitter_ms: 0.4, packet_loss: 0.5, udp_direction: 'down' });
  assert.ok(parts.includes('probed on the download path'), 'a recorded direction must be spoken: ' + parts);
  assert.ok(qualityParts({ packet_loss: 1.2, udp_direction: 'up' }).includes('probed on the upload path'));
  const bare = qualityParts({ ping_ms: 4.5, jitter_ms: 0.4, packet_loss: 0.5 });
  assert.ok(!bare.some(p => /path/.test(p)), 'no recorded direction -> no label (older rows stay unlabelled)');
  const pingOnly = qualityParts({ ping_ms: 4.5, udp_direction: 'up' });
  assert.ok(!pingOnly.some(p => /path/.test(p)), 'a direction without loss/jitter shown would label nothing the probe measured');
});

// --- ping_ms 0 is "not probed" on EVERY surface, not just the tiles -----------
// A successful iperf3 run with the latency probe off: real bytes both ways, a
// real jitter/loss sample, and ping_ms 0 - the documented sentinel. The tiles,
// the range average and /metrics already drop it; the tooltip, the runs table
// and the quality chart rendered it as a genuine zero-millisecond reading, so
// the same row read "-" in one place and a perfect 0.0 ms an inch away.
const iperfNoPing = { ts: 1770000000, engine: 'iperf3',
  down_mbps: 940.2, up_mbps: 912.7, download_bytes: 5.9e8, upload_bytes: 5.7e8,
  ping_ms: 0, jitter_ms: 0.4, packet_loss: 0, udp_direction: 'down' };

test('a run that moved bytes with no ping probe reads as unmeasured, never 0.0 ms', () => {
  // The run is not empty - both directions really were measured, which is exactly
  // why "the speed is real, so the ping must be too" is the wrong inference.
  assert.equal(F.spMeasured(iperfNoPing, 'down_mbps'), true);
  assert.equal(F.spMeasured(iperfNoPing, 'up_mbps'), true);

  assert.equal(F.pingMeasured(iperfNoPing), false);
  assert.equal(F.pingMeasured({ ping_ms: 0.4 }), true, 'a sub-millisecond LAN ping is a real reading');
  assert.equal(F.pingMeasured({}), false, 'omitempty dropped the field entirely');
  assert.equal(F.pingMeasured({ ping_ms: null }), false);

  // Tooltip: the ping line is omitted, the rest of the Quality line is not.
  const parts = qualityParts(iperfNoPing);
  assert.ok(!parts.some(p => /ping/.test(p)), 'the tooltip claimed a 0.0 ms ping: ' + parts.join(' · '));
  assert.ok(parts.some(p => /jitter/.test(p)) && parts.some(p => /loss/.test(p)),
    'the probe DID measure jitter/loss - those must survive: ' + parts.join(' · '));

  // Tiles and the range average already agreed; assert it so the three cannot drift.
  assert.equal(F.pingText(iperfNoPing.ping_ms), '-');
  assert.equal(F.spdAverages([iperfNoPing]).ping, null);
  assert.equal(F.spdAverages([iperfNoPing]).down, 940.2, 'only the ping is unmeasured');
});

test('the runs table and the quality chart gate ping on the same predicate', () => {
  // Both render from DOM/canvas machinery too heavy to drive here, so assert the
  // shipped source consumes the predicate - the same way the family/direction
  // helpers are pinned above.
  const runs = extract('async function loadRuns');
  assert.match(runs, /pingMeasured\(r\)\?num1\(r\.ping_ms\):'-'/,
    'the runs table prints num1(0) = "0.0" for a run that never probed');
  const q = extract('function drawQualityChart');
  assert.match(q, /pingMeasured\(p\)\?p\.ping_ms/, 'the jitter envelope still anchors on a 0 ms ping');
  assert.match(q, /points\.filter\(pingMeasured\)/, 'the plotted set still includes the sentinel');
  assert.match(q, /spdLine\(x,pp,X,Y,'ping_ms'/,
    "spdLine's own filter is isNum, which keeps 0 - it must be handed the gated set");
  assert.match(q, /if\(pingMeasured\(p\)\) chartDot/, 'a single 0 ms run still draws a dot at the floor');
  assert.ok(!/isNum\(p\.ping_ms\)/.test(q), 'a bare isNum ping gate is left in the chart');
});

// The helpers must actually be consumed where runs are detailed, or the labels
// exist only in tests.
test('the run tooltip and the runs table consume the family/direction helpers', () => {
  const tip = extract('function runTip');
  assert.match(tip, /famLabel\(p\.ip_family\)/, 'the tooltip must name the measured family');
  assert.match(tip, /qualityParts\(p\)/, 'the tooltip quality line must ride the labelled builder');
  const runs = extract('async function loadRuns');
  assert.match(runs, /famLabel\(r\.ip_family\)/, 'the runs table must name the measured family');
  assert.match(runs, /famTitle\(r\.ip_family,\s*engOf\(r\)\)/, 'a mixed row must carry the explaining tooltip, scoped to its engine');
  assert.match(runs, /udpDirTitle\(r\)/, 'the loss/jitter cells must carry the probe-direction tooltip');
  const tiles = extract('function setSpeedTiles');
  assert.match(tiles, /udpDirLabel\(sp\.udp_direction\)/, 'the jitter/loss tiles must carry the probe-direction tooltip');
});

// I8: a bridged container cannot read the host's allowed-congestion list
// (/proc/sys/net/ipv4/tcp_allowed_congestion_control is init-netns-only), so an
// empty suggestions list there gets an explanation. Natively an empty list is a
// real answer, a populated list needs no hint, and an older daemon that never
// sends the containerized flag must not hint either.
test('congestion hint: exactly the empty-list-inside-a-container state', () => {
  assert.equal(F.congestionContainerHint([], true), true);
  assert.equal(F.congestionContainerHint(undefined, true), true);
  assert.equal(F.congestionContainerHint(['cubic', 'bbr'], true), false);
  assert.equal(F.congestionContainerHint([], false), false);
  assert.equal(F.congestionContainerHint([], undefined), false, 'absent flag must mean no hint, not truthy-by-accident');
});

test('the congestion hint has a note element and repaints with the engine UI', () => {
  assert.match(html, /id="congestionContainerNote"/, 'hint element missing from the markup');
  assert.match(extract('function updateEngineUI'), /updateCongestionHint\(\)/, 'engine changes must repaint the hint');
  assert.match(extract('function updateCongestionHint'), /congestionContainerHint\(congestionAlgosList, iperfInContainer\)/,
    'the DOM toggle must ride the tested predicate');
});

// I1: inside a BRIDGED container 127.0.0.1/localhost is the CONTAINER itself,
// so a loopback iperf3 target dials itself and gets "connection refused" with
// no explanation. The host must be judged with the port split off first.
test('loopbackHost: 127/8, ::1 and localhost in every host:port shape', () => {
  for (const a of ['localhost', 'LOCALHOST:5201', '127.0.0.1', '127.0.0.1:5201', '127.255.0.7', '[::1]', '[::1]:5201', '::1'])
    assert.equal(F.loopbackHost(a), true, a + ' must read as loopback');
  for (const a of ['192.168.1.10:5201', 'host.docker.internal', 'iperf.example:5201', '[2001:db8::1]:5201', '2001:db8::1', '127.example.com', 'mylocalhost', ''])
    assert.equal(F.loopbackHost(a), false, a + ' must NOT read as loopback');
});

// iperfLoopbackTrap reads the page globals, so it is compiled with injected ones.
function driveLoopbackTrap(servers, bridged) {
  return new Function('iperfServers', 'iperfBridged',
    extract('function loopbackHost') + '\n' + extract('function iperfLoopbackTrap')
    + '\nreturn iperfLoopbackTrap;')(servers, bridged)();
}

test('the loopback trap fires only in a BRIDGED container, skips deleted rows, and warns without blocking Save', () => {
  const lo = { addr: '127.0.0.1:5201', label: '' };
  assert.ok(driveLoopbackTrap([lo], true), 'loopback + bridged container = trap');
  assert.equal(driveLoopbackTrap([lo], false), null,
    'not bridged (native OR host-network container, where localhost IS the host) - a loopback target is legitimate');
  assert.equal(driveLoopbackTrap([{ ...lo, _del: true }], true), null, 'a row being deleted is not a trap');
  assert.equal(driveLoopbackTrap([{ addr: 'lan.box:5201' }], true), null);
  assert.match(html, /id="iperfLoopbackWarn"/, 'the warn bubble markup must exist');
  assert.match(extract('function renderIperfServers'), /updateIperfLoopbackWarn\(\)/,
    'the live bubble must follow every list change');
  // Save-time: the warning is spoken AFTER the successful POST - the save is
  // never gated on it (a loopback target must still save).
  const save = script.slice(script.indexOf("$('saveSettings').addEventListener"));
  const post = save.indexOf("fetch('api/settings'");
  const trap = save.indexOf('iperfLoopbackTrap()');
  assert.ok(post >= 0 && trap > post, 'the loopback check must sit in the success path, not block the save');
});

// The trap itself asks only "bridged container + a loopback server in the list",
// which is the right question for the drawer, where the bubble lives inside
// #iperfServerRow and is hidden outright whenever the engine is Ookla. The
// post-save TOAST has no such markup around it, so it inherited none of that
// gating: an install running Ookla, with an iperf3 server still sitting in the
// saved list (added while trying iperf3, or kept for later), got told on every
// single save that its iperf3 server would refuse connections - about an engine
// that never runs. Two surfaces, same warning, opposite answers.
test('the post-save loopback toast is gated on iperf3 actually being the engine', () => {
  const save = script.slice(script.indexOf("$('saveSettings').addEventListener"));
  const j = save.indexOf('const lb=');
  assert.ok(j > 0, 'the save path must still compute the loopback trap');
  const guard = save.slice(j, save.indexOf(';', j));
  assert.match(guard, /setSpeedEngine/,
    'the toast fires without consulting the engine, so an Ookla install with a leftover iperf3 server ' +
    'is warned about iperf3 on every save; the drawer bubble is gated by its container markup and stays ' +
    'silent, so the two surfaces disagree');
});

// M5: the trap must key on the settings payload's BRIDGED flag, never on the
// broader containerized one - in a host-network container the containerized
// flag is true but localhost IS the host, so keying on it warned about (and
// after a save, toasted about) a configuration that works. The congestion-empty
// hint is the opposite case and stays on containerized: the sysctl is readable
// under host networking, so its list is populated there and the hint never
// shows wrongly.
test('bridged vs containerized: each hint keys on its own flag', () => {
  const trapSrc = extract('function iperfLoopbackTrap');
  assert.match(trapSrc, /iperfBridged/, 'the trap must read the bridged flag');
  assert.doesNotMatch(trapSrc, /iperfInContainer/, 'the trap must not key on merely-containerized');
  assert.match(extract('function applySettings'), /iperfBridged\s*=\s*s\.bridged === true/,
    'applySettings must wire iperfBridged from the settings payload\'s bridged flag');
  assert.match(extract('function updateCongestionHint'), /iperfInContainer/,
    'the congestion hint stays keyed on containerized');
  // The bubble and the post-save toast both name the failing setup precisely.
  assert.match(html.match(/id="iperfLoopbackWarn"[^]*?<\/div>/)[0], /bridged container/,
    'the warn bubble must say BRIDGED container, not just container');
});

// M4: the old access model let a bridged container bypass local-only, and the
// Access tab carried a "Local-only cannot be enforced in this container" bubble
// for it. Local-only is enforced for everyone now (a container opts into
// network reach with -access network at start), so that bubble is a false
// claim and must be gone.
test('the old cannot-enforce-local-only-in-a-container bubble is gone', () => {
  assert.doesNotMatch(html, /localOnlyContainerNote/, 'dead old-access-model bubble still present');
  assert.doesNotMatch(html, /cannot be enforced in this container/i, 'old-access-model wording still present');
});

// A roving-tabindex radiogroup puts focus on the CHECKED option, and the checked
// Quick Setup segment is filled with --accent. An --accent focus ring on it is
// invisible, so the keyboard indicator disappeared on the one element that had
// to show it. --on-accent is the token picked for contrast against that fill.
test('the checked Quick Setup segment has a focus ring you can actually see', () => {
  const base = /\.qs-seg b:focus-visible\{([^}]*)\}/.exec(html);
  assert.ok(base, 'the segments declare a focus-visible ring');
  const onFill = /\.qs-seg b\.on\{([^}]*)\}/.exec(html);
  assert.ok(onFill, 'the checked segment declares its fill');
  assert.match(onFill[1], /background:var\(--accent\)/, 'the checked segment is filled with --accent');

  const checkedRing = /\.qs-seg b\.on:focus-visible\{([^}]*)\}/.exec(html);
  assert.ok(checkedRing, 'the CHECKED segment needs its own focus-ring colour, or the ring is accent-on-accent');
  assert.match(checkedRing[1], /outline-color:var\(--on-accent\)/,
    'the checked segment\'s ring must use the token chosen to contrast with the accent fill');
});

// --- the marked fetch, and the three downloads that exist because of it ---
//
// Every request the SPA issues THROUGH THE WRAPPER carries `X-Pingularity-UI: 1`,
// and the daemon reads it as "this caller is the dashboard, not curl". Through the
// wrapper is the accurate scope, and the exception is deliberate: the login POST
// goes out via `_fetch`, the raw handle captured before the wrapper is installed,
// so it is NOT marked (index.html says so where it does it). That is harmless
// because /api/auth/login is authExempt and neither writer of `WWW-Authenticate`
// sits on the login path - but it means this file's tests bound the wrapper, not
// the set of call sites. A second `_fetch` caller would be equally unmarked and
// equally invisible here. Verified consumers, both of them
// on the 401 path: the session guard in internal/web/auth.go, and the Quick Setup
// dismiss branch in internal/web/web.go. Each one withholds the
// `WWW-Authenticate: Basic` response header when the request carries the marker.
// Unmarked, the daemon offers Basic to the browser instead, and a same-origin
// fetch answered 401+Basic raises the browser's NATIVE credentials dialog on top
// of the SPA's own login overlay - once per API poll, and the status poll alone
// runs every 3 seconds. Suppressing the challenge never weakens the auth check
// itself; curl/wget/Prometheus never send the marker and still get their Basic
// challenge, which is why the discriminator has to be a REQUEST header.
//
// That last point is the whole reason the log, backup-export and speed-runs-CSV
// downloads were converted from <a href> links to fetch: a navigation cannot send
// a request header, so a plain link is unmarkable by construction - middle-click
// and "save link as" would bypass the marker and draw the browser's Basic prompt
// on an expired session.

// A bare indexOf into an 8000-line file can quietly land on a second occurrence
// and compile the wrong code, which looks exactly like a test that fails to
// discriminate. Every anchor below is pinned to exactly one match first.
// `what` names what the anchor holds, because two of them are not downloads at
// all: they are the global fetch binding itself. Reporting "the download it
// drives has gone somewhere untested" when someone breaks `.bind(window)` points
// the reader at the wrong end of the page - that change takes the WHOLE dashboard
// down, not one button.
function soleAnchor(anchor, what = 'whatever it anchors is no longer under test') {
  const n = script.split(anchor).length - 1;
  assert.equal(n, 1,
    `expected exactly one \`${anchor}\` in the page script, found ${n} - if it is 0 the shipped ` +
    `code no longer has this call site, and ${what}`);
  return anchor;
}

// downloadVia is the one definition here that `extract` cannot lift: its
// Content-Disposition regex literal, /filename="?([^"';]+)"?/i, holds a lone
// apostrophe inside the character class, and the brace-walker reads that as the
// start of a string literal - from there the quote parity is inverted and it
// walks straight past the closing brace into the next function. So this one is
// taken as the slice between two pinned anchors instead.
function sliceBetween(from, to) {
  const i = script.indexOf(soleAnchor(from)), j = script.indexOf(soleAnchor(to));
  assert.ok(i >= 0 && j > i, `\`${from}\` must still sit before \`${to}\` in the page script`);
  return script.slice(i, j);
}

// Compile the REAL wrapper, the REAL downloadVia and the REAL click handlers of
// all three downloads together, behind a raw fetch stub that records what the
// network actually saw. Nothing here re-implements the marking: `_fetch` binds off
// the injected window (so the stub stands in for the browser's own fetch), the
// page then overwrites window.fetch with the wrapper, and the requests recorded by
// the stub are whatever the shipped wrapper chose to send.
function driveDownloads({ status = 200 } = {}) {
  const seen = [];
  const rawFetch = async (url, init) => {
    seen.push({ url: String(url), headers: new Headers((init && init.headers) || undefined) });
    // Sampled on the wire: the spinner has to be up WHILE the export is fetching,
    // which is the only window a stuck-looking button would be observed in.
    if (els.exportBtn) out.busyOnWire = els.exportBtn.classes.has('busy');
    return {
      ok: status >= 200 && status < 300, status,
      headers: new Headers({ 'Content-Disposition': 'attachment; filename="from-server.txt"' }),
      blob: async () => ({ size: 12 }),
    };
  };
  // downloadVia hands the buffered bytes to the browser through a throwaway blob
  // anchor, so record every anchor it makes - that is how we can tell a blob
  // handoff from a navigation to the API url.
  const anchors = [];
  const doc = {
    body: { appendChild() {} },
    createElement: () => { const a = { clicked: false, click() { this.clicked = true; }, remove() {} }; anchors.push(a); return a; },
  };
  const els = {}, handlers = {};
  const out = { busyOnWire: null };
  const $ = id => els[id] || (els[id] = (() => {
    const classes = new Set();
    return {
      textContent: '', classes, disabled: false,
      addEventListener: (ev, fn) => { handlers[id + ':' + ev] = fn; },
      classList: { toggle: (c, on) => { on ? classes.add(c) : classes.delete(c); },
        add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c) },
      setAttribute() {}, removeAttribute() {},
    };
  })());
  Object.assign(out, { seen, anchors, handlers, els, flashed: [], loginShown: 0 });
  const src =
    soleAnchor('const _fetch=window.fetch.bind(window);',
      'the raw pre-wrapper handle the login POST uses is gone, and the page has no unmarked escape hatch') + '\n' +
    extract(soleAnchor('window.fetch=async(...a)=>',
      'nothing is installing the marked wrapper, so no request carries the header')) + '\n' +
    // In the browser that assignment IS the rebinding of the global every bare
    // `fetch(...)` in the page resolves to. A `new Function` body has its own
    // scope, so the two are tied together by hand here - downloadVia below still
    // calls a bare `fetch`, exactly as it ships, and reaches the wrapper only
    // because the wrapper is what window.fetch now holds.
    //
    // MODELLING LIMIT, since a harness that looks this faithful invites more
    // trust than it has earned: these two statements are emitted in a FIXED
    // order, not in the order index.html happens to put them. Swap them in the
    // page - capture `_fetch` after the wrapper is installed - and the real
    // browser recurses on the first request until the stack blows, while this
    // harness reproduces the working order and notices nothing. The order is
    // load-bearing and nothing here pins it.
    'const fetch=window.fetch;\n' +
    script.match(/const downloadCapBytes = [^;]*;/)[0] + '\n' +
    sliceBetween('async function downloadVia(', "$('exportBtn').addEventListener('click'") + '\n' +
    extract('function btnBusy(') + '\n' +
    extract(soleAnchor("$('exportBtn').addEventListener('click'")) + ');\n' +
    extract(soleAnchor("$('logDownload').addEventListener('click'")) + ');\n' +
    extract(soleAnchor("$('csvDownload').addEventListener('click'")) + ');\n' +
    'return window.fetch;';
  // setTimeout is stubbed rather than borrowed: downloadVia schedules the blob
  // revoke a minute out, and a real timer would hold the test process open for
  // that whole minute after the assertions have finished.
  out.markedFetch = new Function('window', '$', 'document', 'URL', 'setTimeout',
    'getCats', 'flashStatus', 'showLogin', 'logMasked', src)(
      { fetch: rawFetch }, $, doc,
      { createObjectURL: () => 'blob:pingularity/stub', revokeObjectURL() {} },
      () => {}, () => ['pings'], m => out.flashed.push(m), () => { out.loginShown++; }, true);
  return out;
}

// The swap is half JS and half stylesheet: the class the handlers toggle does
// nothing on its own. These pin the two rules that carry the meaning, and the
// one that keeps the button from resizing under the operator.
test('the busy class actually hides the label and shows the spinner', () => {
  const rule = sel => {
    const m = html.match(new RegExp(sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\{([^}]*)\\}'));
    assert.ok(m, `${sel} is not in the stylesheet, so the busy state renders as nothing at all`);
    return m[1];
  };
  assert.match(rule('.btn.busy .btn-lbl'), /visibility:hidden/,
    'the label has to stay in the layout while it is hidden - display:none would shrink the button ' +
    'mid-click and shift the row under the pointer');
  assert.match(rule('.btn.busy .btn-busy'), /display:block/,
    'the spinner is display:none by default, so without this the busy button shows nothing at all');
  assert.match(rule('.btn .btn-busy'), /display:none/,
    'an idle button must not show a spinner beside its label');
});

// A default export is the whole database and can take a while to build, during
// which downloadVia shows nothing at all - no progress, no message. Without the
// swap the button looks untouched and invites a second click that starts the
// whole export again.
test('export swaps its label for a spinner while the file is being fetched, and puts it back', async () => {
  const d = driveDownloads();
  await d.handlers['exportBtn:click']({ preventDefault() {} });
  assert.equal(d.busyOnWire, true,
    'the export button looked idle while the server was building the file, so a second click starts a second export');
  assert.equal(d.els.exportBtn.classes.has('busy'), false,
    'and the label has to come back once the file is handed over');
  assert.equal(d.els.exportBtn.disabled, false, 'a finished export must leave a usable button');
});

test('requests through the fetch wrapper are marked, or the browser stacks its own password box on top of the login overlay', async () => {
  const d = driveDownloads();
  await d.markedFetch('api/status');
  assert.equal(d.seen[0].headers.get('X-Pingularity-UI'), '1',
    'a plain GET went out unmarked - the daemon then answers a 401 with a Basic challenge and the ' +
    'browser raises its own credentials dialog over the SPA login overlay, once per poll');
  await d.markedFetch('api/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
  assert.equal(d.seen[1].headers.get('X-Pingularity-UI'), '1',
    'a request that brings its own headers went out unmarked - saving settings would draw the native prompt');
  assert.equal(d.seen[1].headers.get('Content-Type'), 'application/json',
    "the caller's own headers must survive the marking, or every POST body is refused as the wrong type");
  // The other half of the same bargain: the SPA suppresses the browser's prompt
  // because it raises its own. If a 401 stopped doing that, an expired session
  // would look like a dashboard that has simply stopped updating.
  const expired = driveDownloads({ status: 401 });
  await expired.markedFetch('api/status');
  assert.equal(expired.loginShown, 1, 'a 401 must raise the SPA login overlay, since the native prompt is suppressed');
});

test('the log, backup and CSV downloads all go out marked, so an expired session gets the SPA login', async () => {
  for (const [id, url] of [['exportBtn', 'api/export'], ['logDownload', 'api/logs'], ['csvDownload', 'api/speed/runs.csv']]) {
    const d = driveDownloads();
    const click = d.handlers[id + ':click'];
    assert.ok(click, `#${id} has no click handler, so whatever downloads now is not the marked fetch`);
    await click({ preventDefault() {} });
    assert.equal(d.seen.length, 1,
      `#${id} put ${d.seen.length} requests on the wire instead of 1 - if it is 0 the download became a ` +
      'navigation, which cannot carry X-Pingularity-UI and draws the browser password box on an expired session');
    assert.ok(d.seen[0].url.startsWith(url), `#${id} requested ${d.seen[0].url}, expected ${url}`);
    assert.equal(d.seen[0].headers.get('X-Pingularity-UI'), '1',
      `#${id} fetched ${url} unmarked - on an expired session the daemon offers Basic and the browser ` +
      'raises its own credentials dialog instead of the SPA login overlay');
    // The anchor it does build points at a blob, not at the API url: that is what
    // makes it a handoff of already-fetched bytes rather than a fresh, unmarked
    // GET performed by the browser's navigation machinery.
    assert.equal(d.anchors.length, 1, `#${id} must hand the file over through exactly one throwaway anchor`);
    assert.equal(d.anchors[0].clicked, true, `#${id} built an anchor but never clicked it, so nothing is saved`);
    assert.ok(String(d.anchors[0].href).startsWith('blob:'),
      `#${id} pointed its anchor at ${d.anchors[0].href} - an api url there is a second, unmarked GET`);
    assert.equal(d.anchors[0].download, 'from-server.txt',
      `#${id} must take the filename the server sent in Content-Disposition`);
  }
});

// The two log/CSV controls are already pinned as href-less native buttons above.
// The Export control is the third download and had no such pin: driving its click
// handler proves what the button does, but not that the markup has not ALSO
// sprouted a link beside it, so that one claim is made against the source.
test('the backup export is a button, never a link that would download unmarked', () => {
  assert.match(html, /<button[^>]*id="exportBtn"[^>]*type="button"/, 'export is a native button');
  assert.doesNotMatch(html, /<a[^>]*id="exportBtn"/, 'no anchor faking the export button');
  assert.doesNotMatch(html, /href="api\/export/, 'nothing in the page navigates to api/export - a navigation goes out unmarked');
});

// Locked-out recovery. Inside a container "the host" is the wrong machine: the
// CLI lives in the image and the database on the volume, so a host shell either
// has no `pingularity` at all or opens some other (empty) database, reports
// success, and leaves the login that locked the operator out exactly where it
// was. The README's Docker section gives the real recipe - a one-off container
// sharing the volume, then a restart, because the running daemon caches settings
// in memory - and both of the page's instructions have to say that instead.
//
// The two strings below are pulled out of the SHIPPED markup rather than typed
// out here, so rewording either one without teaching the rewrite about it fails
// these tests instead of quietly shipping a container branch that does nothing.
// Pulled at MODULE scope, so a miss here throws before any test registers and the
// whole file reports one unnamed TypeError instead of a failure that names what
// moved. mustExtract turns that into a message, and the assertion below turns it
// into a failing TEST rather than a dead run - a rename in the markup should cost
// one red test, not the other 195.
function mustExtract(re, what) {
  const m = html.match(re);
  if (!m) throw new Error('could not find ' + what + ' in index.html - it was renamed or restructured, so the ' +
    'password-reset tests below are no longer reading the shipped markup');
  return m[1];
}
let pageLoginTip = '', pagePwTip = '', resetMarkupErr = null;
try {
  pageLoginTip = mustExtract(/id="loginResetInfo"[^>]*data-tip="([^"]*reset-auth[^"]*)"/, 'the login-overlay reset tooltip');
  pagePwTip = mustExtract(/data-tip="([^"]*Forgot it\? Run pingularity reset-auth[^"]*)"/, 'the password tooltip');
} catch (e) {
  resetMarkupErr = e;
}

test('the password-reset markup these tests read is still present', () => {
  assert.equal(resetMarkupErr, null, String(resetMarkupErr && resetMarkupErr.message));
});

test('the password-reset instructions send a container operator into the container, not to the host', () => {
  const resetAuthText = new Function(extract('function resetAuthText') + '\nreturn resetAuthText;')();
  for (const [what, s] of [['the login-overlay tooltip', pageLoginTip], ['the password tooltip', pagePwTip]]) {
    assert.match(s, /pingularity reset-auth/, what + ' must still name the recovery command');
    assert.equal(resetAuthText(s, false), s, what + ' must keep its wording word-for-word on a native host');
    assert.equal(resetAuthText(s, undefined), s,
      what + ': an older daemon sends no containerized flag, and a guess must not send a native operator into a container');
    const c = resetAuthText(s, true);
    assert.notEqual(c, s,
      what + ' came back unchanged for a container - the rewrite no longer matches the string the page ships, so the advice is still "the host"');
    assert.doesNotMatch(c, /on the host/, what + ' still points at the host, which in a container is the wrong machine');
    assert.match(c, /one-off container sharing the volume/, what + " must give the README's container recipe");
    assert.match(c, /restart/, what + ' must say to restart, or the reset lands and the running daemon keeps asking for the old password');
  }
});

// updateResetAuthHint reads the page globals and writes to two elements, so it is
// compiled with an injected `$` and containerized flag, and driven against fakes
// carrying the real shipped strings.
function resetHintHarness() {
  const els = {
    pwInfo: { dataset: { tip: pagePwTip }, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } },
    loginResetInfo: { dataset: { tip: pageLoginTip }, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } },
  };
  const src = extract('function resetAuthText') + '\n' + extract('function updateResetAuthHint');
  const repaint = new Function('$',
    'return function(iperfInContainer){ ' + src + '\nreturn updateResetAuthHint(); };')(id => els[id] || null);
  return { els, repaint };
}

test('both password-reset instructions turn container-aware together, screen readers included', () => {
  const native = resetHintHarness();
  native.repaint(false);
  assert.equal(native.els.loginResetInfo.dataset.tip, pageLoginTip, 'a native host must see today’s login tooltip untouched');
  assert.equal(native.els.pwInfo.dataset.tip, pagePwTip, 'a native host must see today’s tooltip untouched');

  const h = resetHintHarness();
  h.repaint(true);
  assert.match(h.els.loginResetInfo.dataset.tip, /one-off container sharing the volume/, 'the login-overlay tooltip stayed host-only in a container - the one copy a locked-out operator can reach');
  assert.match(h.els.pwInfo.dataset.tip, /one-off container sharing the volume/, 'the password tooltip stayed host-only in a container');
  // labelInfoBubbles copies data-tip into aria-label ONCE at startup and skips
  // bubbles that already have one, so without rewriting the label here a screen
  // reader would keep reading the host-only instruction no matter what the
  // sighted tooltip says.
  assert.match(h.els.pwInfo.attrs['aria-label'] || '', /one-off container sharing the volume/,
    'the tooltip’s accessible name still tells a screen-reader user to run it on the host');

  // applySettings runs on every settings GET and after every Save, so each of
  // these elements is repainted over and over. Every repaint has to start from
  // the wording stashed on the first call, never from what the last one left:
  // the rewrite is a replace of the "on the host" clause, which the rewritten
  // text no longer contains, so an element rewritten in place could never be
  // painted back. Paint container, then native, and the shipped sentences must
  // return word-for-word. (Repainting container twice proves nothing here - the
  // replace simply finds nothing the second time, cache or no cache.)
  h.repaint(false);
  assert.equal(h.els.loginResetInfo.dataset.tip, pageLoginTip,
    'the login tooltip would not paint back to the shipped wording - the repaint is rewriting its own output, not the page’s sentence');
  assert.equal(h.els.pwInfo.dataset.tip, pagePwTip,
    'the tooltip would not paint back to the shipped wording - the repaint is rewriting its own output, not the page’s sentence');

  // The flag arrives with the settings payload, so the repaint has to run AFTER
  // applySettings stores it. Above that line it paints with the previous value of
  // the global - false until some later settings load repaints it - so the first
  // paint a container operator gets is the host-only advice. Matching the call
  // anywhere in the function would pass either way, hence the positions.
  const apply = extract('function applySettings');
  const flagAt = apply.search(/iperfInContainer\s*=\s*s\.containerized/);
  const repaintAt = apply.search(/updateResetAuthHint\(\)/);
  assert.ok(flagAt >= 0, 'applySettings no longer stores the containerized flag that the repaint reads');
  assert.ok(repaintAt > flagAt,
    'applySettings does not repaint the advice after the containerized flag lands, so it stays host-only in a container');
  assert.match(html, /id="pwInfo"/, 'the password tooltip has no id, so the repaint cannot find it');
});

// The README's Access-tab bullet answers the same "forgot the password?" question
// as the dashboard, and the two drifted the moment the page learned to branch on
// the container (the README still carried the older combined sentence). The
// fragments below are taken out of the page's own rewrite rather than typed here,
// so a reword of the advice that leaves the README behind fails instead of
// shipping two different answers. The container half also points readers at the
// README's Docker section, so that section has to exist.
test('the README gives the same locked-out recovery advice as the dashboard', () => {
  const readmeRaw = readSource(here, '..', '..', '..', 'README.md');
  const readme = readmeRaw.replace(/\s+/g, ' '); // the bullet is hard-wrapped; compare on one line
  const m = extract('function resetAuthText').match(/replace\('([^']+)',\s*'([^']+)'\)/);
  assert.ok(m, 'resetAuthText no longer swaps one literal clause for another, so the wordings the README must match cannot be read out of it');
  const [, hostClause, containerClause] = m;
  const shared = 'to clear it and disable auth', restart = 'then restart the container';
  assert.ok(hostClause.endsWith(shared) && containerClause.includes(shared) && containerClause.includes(restart),
    'the page reworded the advice, so this test can no longer split it into the fragments the README must carry');
  const where = containerClause.split(shared)[0].trim(); // "from a one-off container sharing the volume"
  for (const frag of [hostClause, where, restart]) {
    assert.ok(readme.includes(frag),
      'the README no longer says "' + frag + '", so it and the dashboard now answer "forgot the password?" differently');
  }
  // The command itself is not in the page: it needs an entrypoint override, the
  // pinned -db path and the operator's own volume and tag. So the advice has to
  // say where it is, and that place has to exist.
  assert.ok(/README/.test(containerClause) && /Docker/.test(containerClause),
    'the container advice says to run the reset from a one-off container but no longer says where the command is, and it is not one an operator can guess');
  assert.match(readmeRaw, /^#+ +Docker\b/m,
    'the container advice sends the operator to the README’s Docker section for the command, and there is no such section');
});

// --- hidden tiles do not poll ------------------------------------------------
//
// A tile dragged off the dashboard is display:none (.section-hidden), and every
// refresher behind one used to keep fetching anyway: its own timer, tick() at
// boot / on tab focus / on `online`, and the stale-recovery burst in
// refreshStatus. The gate lives inside each refresher, which is what covers all
// of those call sites at once.
test('tileIdle: a hidden tile stands down, a visible one polls, a forced refresh always runs', () => {
  const gate = hidden => new Function('document', TILE_GATE + '\nreturn tileIdle;')(gateDoc('latency', hidden));
  assert.equal(gate(true)('latency'), true, 'a hidden tile must not poll - nothing it draws is on screen');
  assert.equal(gate(true)('latency', true), false,
    'a FORCED refresh must run even while hidden: force means the rows underneath changed (import, ' +
    'delete, a window change), and skipping it leaves the tile\'s fetch-once guard pointing at data ' +
    'that no longer exists, with nothing to make it refetch later');
  assert.equal(gate(false)('latency'), false, 'a visible tile must poll');
  assert.equal(gate(true)('speed'), false, 'the gate must key on its OWN panel, not on whether any panel is hidden');
});

// Each of the five refreshers is DRIVEN behind a hidden tile further down (see
// "the gate, driven rather than grepped"), which is what an inert or a misplaced
// gate has to fail. The one thing that cannot be tested that way is a gate that
// must NOT be there: refreshStatus owns the top bar, the coachmark, Quick Setup,
// the power button and the speedtest-running state, none of which live inside a
// panel, so it deliberately polls whatever is hidden. Absence is not something an
// inert version can fake, so a source check is the right shape here.
test('refreshStatus is deliberately ungated', () => {
  assert.doesNotMatch(extract('async function refreshStatus'), /tileIdle/,
    'refreshStatus drives the top bar and the power button, which no tile can hide');
});

test('a hidden connection tile fetches nothing, and a forced refresh still lands', async () => {
  const off = await driveNetinfo({ hidden: true });
  assert.deepEqual(off.fetched, [], 'a hidden Connection tile still asked the daemon for a snapshot');
  assert.equal(off.polls, 0, 'and still armed the 3s catch-up poll behind a panel nobody can see');
  const on = await driveNetinfo({ hidden: false });
  assert.deepEqual(on.fetched, ['GET'], 'a visible tile must still fetch - the gate has to key on hidden, not on nothing');
  const forced = await driveNetinfo({ hidden: true, force: true });
  assert.deepEqual(forced.fetched, ['POST'], 'the manual refresh button must work whatever the tile is doing');
});

// Restoring a tile has to REFETCH: redrawCharts paints from the latPoints /
// spdData / hmData caches, and a tile hidden at PAGE LOAD has never filled them.
function driveRefreshSection(id, { outages = 'block' } = {}) {
  const calls = [];
  new Function('refreshChart', 'refreshSpeedChart', 'refreshNetinfo', 'refreshHeatmap', 'loadOutages', '$',
    extract('function refreshSection') + '\nreturn refreshSection;')(
    f => calls.push(['latency', f]), f => calls.push(['speed', f]), f => calls.push(['connection', f]),
    f => calls.push(['heatmap', f]), f => calls.push(['outages', f]),
    () => ({ style: { display: outages } }))(id);
  return calls;
}

test('restoring a tile refetches it, and the connection tile refetches with the cheap GET', () => {
  assert.deepEqual(driveRefreshSection('latency'), [['latency', undefined]]);
  assert.deepEqual(driveRefreshSection('speed'), [['speed', undefined]]);
  assert.deepEqual(driveRefreshSection('downtime'), [['heatmap', undefined], ['outages', undefined]]);
  assert.deepEqual(driveRefreshSection('downtime', { outages: 'none' }), [['heatmap', undefined]],
    'a collapsed outage table must not be loaded just because its tile came back');
  assert.deepEqual(driveRefreshSection('connection'), [['connection', undefined]],
    'restoring the Connection tile must NOT force: refreshNetinfo(true) POSTs, which handleNetinfo ' +
    'answers with a full re-fetch including the exit traceroute under a 25s context, and the forced ' +
    'path skips the in-flight guard - so a few show/hides would stack concurrent 25s POSTs');
});

test('both restore paths refetch, not just redraw', () => {
  assert.match(extract('function unhideSection'), /refreshSection\(id\)/,
    'un-hiding only repaints from cache, so a tile hidden at page load comes back as an empty chart');
  assert.match(extract("rb.addEventListener('click'"), /refreshSection/,
    'Reset tiles brings hidden tiles back the same way, and they have the same empty caches');
});

// --- a failed forced refetch must not freeze the latency chart ---------------
//
// latLoadedFor / latLoadedLive are written only on success; the catch returns
// without touching them. So a forced refetch that FAILS used to leave the marker
// naming the very query that just failed, and every later unforced poll
// short-circuits on it. A live window self-heals (live skips the guard); a
// pinned span never does.
// A promise a test can hold a fetch on, so a second call can be made while the
// first is still in flight - which is how the ORDER of the gate and the sequence
// bump becomes observable.
const deferred = () => { let go; const p = new Promise(r => { go = r; }); return { p, go }; };
function chartDriver({ hidden = false, live = false } = {}) {
  const state = { fetches: 0, fail: false, r503: false, draws: 0, hold: null };
  const api = new Function('latWindowQuery', 'fget', 'isReconcile503', 'retryAfterMs', 'chartLoadFailed',
    'drawChart', 'syncLatPanel', 'document', 'state',
    TILE_GATE + '\nlet chartSeq = 0, latLoadedFor = "", latLoadedLive = false, latPoints = [], latBackoffMs = 0;\n'
    + extract('async function refreshChart')
    + '\nreturn { refreshChart, seq: () => chartSeq, backoff: () => latBackoffMs };')(
    () => ({ q: 'from=1000&to=2000', live }),
    async () => {
      state.fetches++;
      if (state.hold) await state.hold;
      if (state.fail) throw new Error('the server went away');
      if (state.r503) return { ok: false, status: 503, headers: { get: () => '4' }, text: async () => 'reconciling' };
      return { ok: true, json: async () => [{ t: 1 }] };
    },
    () => state.r503, () => 4000, () => {},
    () => { state.draws++; }, () => {},
    gateDoc('latency', hidden), state);
  return Object.assign(api, { state });
}

test('a failed forced refetch cannot freeze a pinned latency span forever', async () => {
  const c = chartDriver();                  // live:false - a pinned span
  await c.refreshChart(true);               // the picker's first load
  assert.equal(c.state.fetches, 1);
  c.state.fail = true;
  await c.refreshChart(true);               // e.g. "delete all latency data", and it fails
  assert.equal(c.state.fetches, 2);
  c.state.fail = false;
  await c.refreshChart();                   // the next ordinary poll
  assert.equal(c.state.fetches, 3,
    'the forced refetch failed, so the chart is still drawing rows the user just deleted - and the ' +
    'failure left the fetch-once marker naming this exact query, so every later poll returns before ' +
    'requesting anything. Nothing on a pinned span ever clears it again.');
});

test('a pinned latency span that loaded cleanly is still fetched exactly once', async () => {
  const c = chartDriver();
  await c.refreshChart(true);
  await c.refreshChart();
  await c.refreshChart();
  assert.equal(c.state.fetches, 1,
    'the fetch-once guard is the whole reason a pinned span is cheap - invalidating the marker must ' +
    'not turn every poll back into a request');
});

test('a hidden latency tile polls nothing, and forced loads still land', async () => {
  const c = chartDriver({ hidden: true, live: true });
  await c.refreshChart();
  assert.equal(c.state.fetches, 0, 'a live window behind a hidden tile is the most expensive poll in the product');
  await c.refreshChart(true);
  assert.equal(c.state.fetches, 1,
    'an import or a delete must still refresh the cache behind a hidden tile, or restoring it draws rows that are gone');
});

test('a forced refetch that hits a reconcile-503 cannot freeze a pinned latency span either', async () => {
  const c = chartDriver();                  // live:false - a pinned span
  await c.refreshChart(true);
  assert.equal(c.state.fetches, 1);
  c.state.r503 = true;
  await c.refreshChart(true);               // the delete lands mid-reconcile
  assert.equal(c.backoff(), 4000, 'the 503 must still park its Retry-After backoff');
  c.state.r503 = false;
  await c.refreshChart();
  assert.equal(c.state.fetches, 3,
    'the 503 return leaves the chart holding pre-delete rows, and it is not the catch - so clearing ' +
    'the marker in the catch instead of at the start of the forced load would still freeze this span');
});

// --- the same freeze, on the speed chart ------------------------------------
//
// refreshSpeedChart has the identical marker: speedLoadedFor/speedLoadedLive are
// written only on its success line, and both of its non-success exits return
// without touching them. Reachable from the two paths that force it - an import
// and a delete-data - both of which are exactly the case where the rows under a
// pinned span really did change.
function speedDriver({ hidden = false, live = false } = {}) {
  const state = { fetches: 0, fail: false, r503: false, paints: 0, hold: null };
  const api = new Function('speedWindowQuery', 'fget', 'isReconcile503', 'retryAfterMs', 'chartLoadFailed',
    'drawSpeedChart', 'drawQualityChart', 'drawBloatChart', 'paintSpdAvgs', 'syncSpeedPanel', 'document', 'state',
    TILE_GATE + '\nlet speedSeq = 0, speedLoadedFor = "", speedLoadedLive = false, spdData = [], spdSampled = false, '
    + 'spdTotal = 0, speedBackoffMs = 0;\n'
    + extract('async function refreshSpeedChart')
    + '\nreturn { refreshSpeedChart, seq: () => speedSeq, backoff: () => speedBackoffMs };')(
    () => ({ q: 'from=1000&to=2000', live }),
    async () => {
      state.fetches++;
      if (state.hold) await state.hold;
      if (state.fail) throw new Error('the server went away');
      if (state.r503) return { ok: false, status: 503, headers: { get: () => '4' }, text: async () => 'reconciling' };
      return { ok: true, headers: { get: k => (k === 'X-Sampled' ? 'false' : '2') }, json: async () => [{ ts: 1 }] };
    },
    () => state.r503, () => 4000, () => {},
    () => { state.paints++; }, () => {}, () => {}, () => {}, () => {},
    gateDoc('speed', hidden), state);
  return Object.assign(api, { state });
}

test('a failed forced refetch cannot freeze a pinned speed span forever', async () => {
  const s = speedDriver();                  // live:false - a pinned span
  await s.refreshSpeedChart(true);
  assert.equal(s.state.fetches, 1);
  s.state.fail = true;
  await s.refreshSpeedChart(true);          // "delete ALL speed data", and it fails
  assert.equal(s.state.fetches, 2);
  s.state.fail = false;
  await s.refreshSpeedChart();              // the next ordinary poll
  assert.equal(s.state.fetches, 3,
    'the forced refetch failed, so the three speed charts and the stat tiles are still drawing runs ' +
    'the user just deleted - and the failure left the fetch-once marker naming this exact query, so ' +
    'every later poll returns before requesting anything. A pinned span never clears it again.');
});

test('a forced refetch that hits a reconcile-503 cannot freeze a pinned speed span either', async () => {
  const s = speedDriver();
  await s.refreshSpeedChart(true);
  s.state.r503 = true;
  await s.refreshSpeedChart(true);
  assert.equal(s.backoff(), 4000, 'the 503 must still park its Retry-After backoff');
  s.state.r503 = false;
  await s.refreshSpeedChart();
  assert.equal(s.state.fetches, 3,
    'clearing the marker in the catch would miss this exit, which is why it is cleared as the forced load begins');
});

test('a pinned speed span that loaded cleanly is still fetched exactly once', async () => {
  const s = speedDriver();
  await s.refreshSpeedChart(true);
  await s.refreshSpeedChart();
  await s.refreshSpeedChart();
  assert.equal(s.state.fetches, 1,
    'the `force` test on the invalidation is what keeps this at one: clearing the marker on every ' +
    'call would refetch an immutable span on every poll');
});

test('the forced invalidation sits above the fetch-once guard, where its condition still means something', () => {
  // This one is order, not behaviour, and deliberately so: the two orders are
  // behaviourally identical, and moving this line back below the guard passes
  // every other test in this file - which is how it came to sit there. What the
  // order decides is whether `if(force)` is load-bearing. BELOW the guard, an
  // unforced poll of a loaded span returns before ever reaching the line, so
  // dropping the condition would be invisible. ABOVE it, dropping the condition
  // empties the marker on every poll and refetches an immutable span every tick,
  // which the "fetched exactly once" tests above catch. Order is the only thing a
  // test can hold here, so it is held as order.
  for (const [def, invalidate, guard] of [
    ['async function refreshChart', "if(force){ latLoadedFor=''", 'if(!force && !live && latLoadedFor===q'],
    ['async function refreshSpeedChart', "if(force){ speedLoadedFor=''", 'if(!force && !live && speedLoadedFor===q'],
  ]) {
    const body = extract(def);
    const i = body.indexOf(invalidate), j = body.indexOf(guard);
    assert.ok(i > 0 && j > 0, def + ' has lost either the forced invalidation or the fetch-once guard');
    assert.ok(i < j, def + ': the forced invalidation has drifted below the fetch-once guard, so its ' +
      '`force` condition no longer decides anything and the next edit to drop it will pass every test');
  }
});

test('a live speed window is never held back by the fetch-once marker', async () => {
  const s = speedDriver({ live: true });
  await s.refreshSpeedChart();
  await s.refreshSpeedChart();
  assert.equal(s.state.fetches, 2, 'a rolling window gains runs, so it must poll however recently it loaded');
});

// --- the gate, driven rather than grepped ------------------------------------
//
// These five were pinned by asserting the gate's SOURCE TEXT appeared in each
// body, which passes two mutations that both ship a bug: an INERT gate (compute
// the answer, ignore it), which polls behind a hidden tile exactly as before,
// and a MISPLACED one below `const my=++seq`, which stands the poll down but
// bumps the sequence on its way out. That second one matters because ++seq is
// the claim "I am the newest request": a hidden tile ticking every few seconds
// would keep taking that claim from the FORCED load an import or a delete
// started, and the forced load then drops its own result on `my!==seq` - leaving
// the tile's cache holding rows that no longer exist, with nothing to refetch
// them. So the tests below measure requests, sequence and repaints.
function heatmapDriver({ hidden = false } = {}) {
  const state = { fetches: 0, draws: 0, hold: null, rows: [{ date: '2026-05-20', downtime_s: 0 }] };
  const api = new Function('fget', 'drawHeatmap', 'document', 'state',
    TILE_GATE + '\nlet heatmapSeq = 0, hmData = [];\n'
    + extract('async function refreshHeatmap')
    + '\nreturn { refreshHeatmap, seq: () => heatmapSeq, data: () => hmData };')(
    async () => { state.fetches++; if (state.hold) await state.hold; return { json: async () => state.rows }; },
    () => { state.draws++; },
    gateDoc('downtime', hidden), state);
  return Object.assign(api, { state });
}
function outagesDriver({ hidden = false, page = 1, pages = {} } = {}) {
  const state = { fetches: [], hold: null };
  const events = { innerHTML: '' };
  const api = new Function('fget', '$', 'evHighlighted', 'outageDeletable', 'TRASH_SVG', 'fmtTime', 'fmtDur',
    'updateOutagesPager', 'document', 'state',
    TILE_GATE + '\nlet outagesSeq = 0, outagesTotal = 0, outagesPerPage = 10, outagesPage = ' + page + ';\n'
    + extract('async function loadOutages')
    + '\nreturn { loadOutages, seq: () => outagesSeq, page: () => outagesPage };')(
    async url => {
      const off = +(/offset=(\d+)/.exec(url) || [0, 0])[1];
      state.fetches.push(off);
      if (state.hold) await state.hold;
      return { json: async () => ({ events: pages[off] || [], total: 1 }) };
    },
    () => events, () => false, () => false, '', () => 'T', () => '0s',
    () => {},
    gateDoc('downtime', hidden), state);
  return Object.assign(api, { state, events });
}

test('a hidden tile issues no request and leaves its sequence where it found it', async () => {
  const c = chartDriver({ hidden: true, live: true });
  await c.refreshChart();
  assert.deepEqual([c.state.fetches, c.seq()], [0, 0], 'the latency chart polled behind a hidden tile');
  const s = speedDriver({ hidden: true, live: true });
  await s.refreshSpeedChart();
  assert.deepEqual([s.state.fetches, s.seq()], [0, 0], 'the speed chart polled behind a hidden tile');
  const h = heatmapDriver({ hidden: true });
  await h.refreshHeatmap();
  assert.deepEqual([h.state.fetches, h.seq()], [0, 0], 'the heatmap polled behind a hidden tile');
  const o = outagesDriver({ hidden: true });
  await o.loadOutages();
  assert.deepEqual([o.state.fetches.length, o.seq()], [0, 0], 'the outages table polled behind a hidden tile');
  const n = await driveNetinfo({ hidden: true });
  assert.deepEqual([n.fetched.length, n.seq], [0, 0], 'the connection tile polled behind a hidden panel');
});

test('a forced refresh of a hidden tile runs anyway, sequence and all', async () => {
  const c = chartDriver({ hidden: true, live: true });
  await c.refreshChart(true);
  assert.deepEqual([c.state.fetches, c.seq(), c.state.draws], [1, 1, 1], 'a forced latency load must land');
  const s = speedDriver({ hidden: true, live: true });
  await s.refreshSpeedChart(true);
  assert.deepEqual([s.state.fetches, s.seq(), s.state.paints], [1, 1, 1], 'a forced speed load must land');
  const o = outagesDriver({ hidden: true, pages: { 0: [{ ts: 1, type: 'up' }] } });
  await o.loadOutages(true);
  assert.deepEqual([o.state.fetches.length, o.seq()], [1, 1], 'a forced outages load must land');
  assert.match(o.events.innerHTML, /<tr/, 'and must repaint the table it just fetched');
  const n = await driveNetinfo({ hidden: true, force: true });
  assert.deepEqual([n.fetched, n.seq], [['POST'], 1], 'the manual refresh button must work whatever the tile is doing');
});

test('the heatmap has no forced path, so what its gate must let through is the VISIBLE tile', async () => {
  // refreshHeatmap takes no `force`, because no caller has one to pass: the 60s
  // timer, tick(), the stale-recovery burst, an import, a delete-all, an outage
  // delete and refreshSection all call it bare, and it keeps no fetch-once marker
  // for a stood-down poll to leave stale. So there is no forced-while-hidden case
  // to drive here, unlike the four above. What is left to hold is the other
  // direction: a gate stuck on `true` - or one that swallowed the whole fetch -
  // stops the heatmap for good, and "a hidden tile issues no request" above passes
  // that mutation happily.
  const h = heatmapDriver({ hidden: false });
  await h.refreshHeatmap();
  assert.deepEqual([h.state.fetches, h.seq(), h.state.draws], [1, 1, 1],
    'a visible heatmap must fetch, claim the sequence and repaint');
  assert.equal(h.data().length, 1, 'and the fetched days must reach the cache the tile redraws from');
});

test('a tick behind a hidden tile cannot invalidate a forced load already in flight', async () => {
  // Each of these starts a FORCED load (an import, a delete-data), holds it open,
  // and lets the hidden tile's own timer tick once while it is in the air. The
  // forced load has to come back and paint. It does not if the gate stands the
  // tick down only AFTER letting it bump the sequence.
  const c = chartDriver({ hidden: true, live: true });
  let d = deferred();
  c.state.hold = d.p;
  let forced = c.refreshChart(true);
  c.state.hold = null;
  await c.refreshChart();
  d.go(); await forced;
  assert.equal(c.state.fetches, 1, 'the hidden latency tick must not have fetched');
  assert.equal(c.state.draws, 1,
    'the forced latency load found its sequence taken by a hidden tick and dropped its own result, so ' +
    'the tile keeps its pre-import points and nothing ever refetches them');

  const s = speedDriver({ hidden: true, live: true });
  d = deferred();
  s.state.hold = d.p;
  forced = s.refreshSpeedChart(true);
  s.state.hold = null;
  await s.refreshSpeedChart();
  d.go(); await forced;
  assert.deepEqual([s.state.fetches, s.state.paints], [1, 1], 'same for the speed chart');

  const o = outagesDriver({ hidden: true, pages: { 0: [{ ts: 1, type: 'up' }] } });
  d = deferred();
  o.state.hold = d.p;
  forced = o.loadOutages(true);
  o.state.hold = null;
  await o.loadOutages();
  d.go(); await forced;
  assert.equal(o.state.fetches.length, 1, 'the hidden outages tick must not have fetched');
  assert.match(o.events.innerHTML, /<tr/, 'same for the outages table');
});

test('the outages recursion carries force with it', async () => {
  // Nothing forces loadOutages today - every call site passes nothing, and the
  // downtime tile is deliberately not force-refreshed by an import or a delete
  // (see tileIdle). The recursion is the one line that would silently drop a
  // force if a caller ever passed one: an emptied last page steps back a page and
  // calls itself, and unforced that second call is stopped by the tile's own gate
  // - leaving the table showing the page that just emptied.
  const o = outagesDriver({ hidden: true, page: 2, pages: { 0: [{ ts: 1, type: 'up' }], 10: [] } });
  await o.loadOutages(true);
  assert.deepEqual(o.state.fetches, [10, 0], 'the step back to page 1 never happened');
  assert.equal(o.page(), 1);
  assert.match(o.events.innerHTML, /<tr/, 'the emptied page was never replaced with the one behind it');
});

// --- latency poll cadence ----------------------------------------------------
//
// latPollMs used to answer a flat 6s for every range that was not a preset, so a
// live custom range re-scanned its whole width ten times a minute however wide it
// was. Cadence now comes from the span that will actually be SCANNED, priced
// against the output bucket the server will aggregate it into.
const CAD_NOW_MS = Date.UTC(2026, 4, 20, 10, 0, 0);
const CAD_NOW = Math.floor(CAD_NOW_MS / 1000);
const cadHour = 3600;
const cadWin = mins => ({ kind: 'win', mins });
const cadRange = (from, to) => ({ kind: 'range', from, to });

test('poll cadence: the preset ladder', () => {
  assert.equal(F.latPollDelay(cadWin(5), CAD_NOW_MS), 6000);
  assert.equal(F.latPollDelay(cadWin(60), CAD_NOW_MS), 6000);
  assert.equal(F.latPollDelay(cadWin(360), CAD_NOW_MS), 15000);
  assert.equal(F.latPollDelay(cadWin(1440), CAD_NOW_MS), 60000,
    'a day aggregates into 57-second output buckets, so a 15s poll re-scanned the whole day four times ' +
    'per new bucket - three of those four repaint the same trailing point');
  assert.equal(F.latPollDelay(cadWin(10080), CAD_NOW_MS), 60000);
});

test('poll cadence: the ladder boundaries, and nothing off the rungs', () => {
  assert.equal(F.latPollForMins(120), 6000);
  assert.equal(F.latPollForMins(121), 15000);
  assert.equal(F.latPollForMins(1440), 60000);
  assert.equal(F.latPollForMins(1441), 60000);
  assert.equal(F.latPollForMins(400), 30000);
  assert.equal(F.latPollForMins(774), 30000);
  for (const m of [0, 0.5, 1, 5, 119, 200, 400, 700, 1439, 5000, 525600]) {
    assert.ok(LAT_CONSTS.LAT_POLL_RUNGS.includes(F.latPollForMins(m)),
      m + ' minutes polls at ' + F.latPollForMins(m) + 'ms, which is not one of the declared cadences');
  }
});

test('a live absolute range is priced on the span it has SCANNED, not the span typed', () => {
  // 09:00 to 23:00, read at 10:00: fourteen hours selected, one hour drawn.
  const r = cadRange(CAD_NOW - cadHour, CAD_NOW + 13 * cadHour);
  assert.equal(F.latSpanMins(r, CAD_NOW_MS), 60, 'nothing exists past now, so only the elapsed hour will be scanned');
  assert.equal(F.latPollDelay(r, CAD_NOW_MS), F.latPollDelay(cadWin(60), CAD_NOW_MS),
    'an hour of drawn data costs the same however it was selected');
  assert.notEqual(F.latPollForMins(14 * 60), F.latPollDelay(r, CAD_NOW_MS),
    'pricing it by the width typed would poll a one-hour chart once a minute');
});

test('a live year-wide range is no longer polled like a five-minute one', () => {
  const year = cadRange(CAD_NOW - 365 * 24 * cadHour, 0);   // "since a year ago", still live
  assert.equal(F.latPollDelay(year, CAD_NOW_MS), 60000,
    'every non-preset range answered a flat 6s, so the widest and most expensive scan in the product ran ten times a minute');
});

test('open start and open end resolve the way the server resolves them', () => {
  const capMins = LAT_CONSTS.LAT_MAX_WIN_SEC / 60;
  assert.equal(F.latSpanMins(cadRange(0, 0), CAD_NOW_MS), capMins,
    'from=0 is the open-start sentinel: the server floors the scan at its 366-day bound');
  assert.equal(F.latPollDelay(cadRange(0, 0), CAD_NOW_MS), 60000);
  assert.equal(F.latSpanMins(cadRange(CAD_NOW - 800 * 24 * cadHour, 0), CAD_NOW_MS), capMins,
    'a start older than the bound scans the bound, not the 800 days asked for');
  assert.equal(F.latSpanMins(cadRange(CAD_NOW - 3 * cadHour, 0), CAD_NOW_MS), 180, 'to=0 is the open end: it runs to now');
  assert.equal(F.latPollDelay(cadRange(CAD_NOW - 3 * cadHour, 0), CAD_NOW_MS), 15000);
});

test('a range that has not started yet has a span of zero, never a negative one', () => {
  const soon = cadRange(CAD_NOW + 24 * cadHour, CAD_NOW + 48 * cadHour);
  assert.equal(F.latSpanMins(soon, CAD_NOW_MS), 0, 'a window wholly in the future scans nothing yet');
  assert.equal(F.latPollDelay(soon, CAD_NOW_MS), 6000,
    'a negative span must not become a negative or zero delay - that is a busy loop against the daemon');
});

test('a live range ending in two seconds wakes at its end, not a minute later', () => {
  // 20 hours, not 12: with the 30s rung in place a 12-hour span arms 30s, and the
  // point of this test is a span whose width alone arms the LONGEST delay.
  const ending = cadRange(CAD_NOW - 20 * cadHour, CAD_NOW + 2);
  assert.equal(F.latPollForMins(F.latSpanMins(ending, CAD_NOW_MS)), 60000, 'its width alone would arm a full minute');
  const d = F.latPollDelay(ending, CAD_NOW_MS);
  assert.ok(d >= 2000 && d <= 5000,
    'the range stops being live in two seconds, and only the poll after that loads the final tail and ' +
    'raises the Live button - arming a minute here holds both back for most of it. Got ' + d + 'ms');
});

test('a frozen span keeps the shortest tick, however wide it is', () => {
  assert.equal(F.latPollDelay(cadRange(CAD_NOW - 365 * 24 * cadHour, CAD_NOW - 1), CAD_NOW_MS), 6000,
    'its fetch no-ops before any request is made, so the ticks cost nothing - and the loop re-arms only ' +
    'after that await with no cancellable handle, so a 60s delay armed here still has to expire before ' +
    'the first poll of whatever window Live switches to');
});

// --- the bucket floor's own rung ---------------------------------------------
//
// The floor first overtakes the ladder at 400 minutes - a 16s bucket, one second
// past the 15s rung - and stops biting at 1440, where the bucket has grown to
// 57s. With no rung between 15s and 60s, that whole band armed a full minute, so
// 400 minutes polled at 3.75x its own bucket width while 399 polled at 1x. The
// 30s rung is what keeps the step at the floor to one rung.
const cadBucketMs = m => Math.max(1, Math.floor(m * 60 / 1500)) * 1000;
test('the bucket floor never costs more than one rung', () => {
  assert.equal(F.latPollForMins(399), 15000, 'a 15s bucket is still met by the 15s rung');
  assert.equal(F.latPollForMins(400), 30000,
    'the bucket reaches 16s here; without a 30s rung this one extra second quadrupled the poll interval');
  assert.equal(F.latPollForMins(775), 60000, 'a 31s bucket has to round up to the minute');
  let worst = 0, at = 0;
  for (let m = 400; m <= 1440; m++) {
    const ratio = F.latPollForMins(m) / cadBucketMs(m);
    if (ratio > worst) { worst = ratio; at = m; }
  }
  assert.ok(worst <= 2,
    'across the band where the output bucket drives the cadence, the widest gap between a poll and the ' +
    'bucket it waits for is ' + worst.toFixed(2) + ' buckets per poll at ' + at + ' minutes. Over 2 means a ' +
    'rung is missing and the chart is lagging its own resolution by more than one step');
});

test('the extra rung spends no more than the ladder already spent', () => {
  // Window-minutes re-scanned per second of wall clock: the cost the daemon
  // actually pays. The 30s rung is only defensible because it stays under what
  // the ladder has always run at just BELOW the band it covers.
  const rate = m => m / (F.latPollForMins(m) / 1000);
  let below = 0, band = 0;
  for (let m = 1; m < 400; m++) below = Math.max(below, rate(m));
  for (let m = 400; m <= 774; m++) band = Math.max(band, rate(m));
  assert.ok(band <= below,
    'the 400..774 band now re-scans up to ' + band.toFixed(1) + ' window-minutes per second, past the ' +
    below.toFixed(1) + ' the ladder already runs at below 400 minutes - so this rung is costing more than ' +
    'the thing it was justified against');
});

test('none of the presets moved when the rung was added', () => {
  for (const [mins, ms] of [[5, 6000], [60, 6000], [360, 15000], [1440, 60000], [10080, 60000]])
    assert.equal(F.latPollForMins(mins), ms, mins + '-minute latency preset changed cadence');
  for (const [mins, ms] of [[1440, 30000], [10080, 60000], [43200, 60000], [525600, 60000]])
    assert.equal(F.speedPollForMins(mins), ms, mins + '-minute speed preset changed cadence');
});

// --- speed poll cadence ------------------------------------------------------
//
// speedPollMs had the same shape of defect latPollMs did: a flat 30000 for every
// range that was not a rolling window, so "since a year ago" re-scanned a year of
// runs twice a minute while the 1y PRESET, the identical scan, ran once a minute.
test('speed cadence: the preset ladder and its boundary', () => {
  assert.equal(F.speedPollDelay(cadWin(1440), CAD_NOW_MS), 30000);
  assert.equal(F.speedPollDelay(cadWin(10080), CAD_NOW_MS), 60000);
  assert.equal(F.speedPollDelay(cadWin(43200), CAD_NOW_MS), 60000);
  assert.equal(F.speedPollDelay(cadWin(525600), CAD_NOW_MS), 60000);
  assert.equal(F.speedPollForMins(1440), 30000);
  assert.equal(F.speedPollForMins(1441), 60000);
  for (const m of [0, 0.5, 1, 5, 400, 1439, 5000, 525600]) {
    assert.ok(LAT_CONSTS.SPD_POLL_RUNGS.includes(F.speedPollForMins(m)),
      m + ' minutes polls at ' + F.speedPollForMins(m) + 'ms, which is not one of the declared speed cadences');
  }
});

test('a live year-wide speed range is no longer polled like a one-day one', () => {
  const year = cadRange(CAD_NOW - 365 * 24 * cadHour, 0);
  assert.equal(F.speedPollDelay(year, CAD_NOW_MS), 60000,
    'every range that was not a rolling preset answered a flat 30s, so the widest speed scan in the ' +
    'product ran at twice the cadence of the same width picked from the dropdown');
});

test('a speed range is priced on the span it has SCANNED, not the span typed', () => {
  // Two days typed, one day of it elapsed: it costs a day, so it pays a day's rate.
  const r = cadRange(CAD_NOW - 24 * cadHour, CAD_NOW + 24 * cadHour);
  assert.equal(F.latSpanMins(r, CAD_NOW_MS), 1440, 'nothing exists past now');
  assert.equal(F.speedPollDelay(r, CAD_NOW_MS), F.speedPollDelay(cadWin(1440), CAD_NOW_MS),
    'a day of drawn runs costs the same however it was selected');
});

test('a frozen speed span keeps the shortest tick, and one ending soon wakes at its end', () => {
  assert.equal(F.speedPollDelay(cadRange(CAD_NOW - 365 * 24 * cadHour, CAD_NOW - 1), CAD_NOW_MS), 30000,
    'refreshSpeedChart no-ops on a frozen span, so the tick is free - but the delay armed here still has ' +
    'to expire before the first poll of the window Live switches to');
  const ending = cadRange(CAD_NOW - 40 * 24 * cadHour, CAD_NOW + 2);
  assert.equal(F.speedPollForMins(F.latSpanMins(ending, CAD_NOW_MS)), 60000, 'its width alone would arm a full minute');
  const d = F.speedPollDelay(ending, CAD_NOW_MS);
  assert.ok(d >= 2000 && d <= 5000,
    'the range stops being live in two seconds and only the poll after that loads the runs at its end. ' +
    'Got ' + d + 'ms');
});

// Both pollMs functions are driven rather than grepped: the backoff has to win
// exactly one poll and clear itself, and the cadence has to come from the helper
// the tests above pin, applied to the LIVE range at the CURRENT time. A source
// check passes a version that reads the helper and returns something else.
function drivePollMs(kind, { backoff = 0, range = { kind: 'win', mins: 1 }, now = CAD_NOW_MS } = {}) {
  const rangeVar = kind === 'lat' ? 'latencyRange' : 'speedRange';
  const helper = kind + 'PollDelay';
  const seen = [];
  const call = new Function('b', 'r', 'delay', 'nowMs',
    'let ' + kind + 'BackoffMs = b;\nconst ' + rangeVar + ' = r;\nconst ' + helper + ' = delay;\n'
    + 'const Date = { now: () => nowMs };\n'
    + extract('function ' + kind + 'PollMs')
    + '\nreturn () => ({ ms: ' + kind + 'PollMs(), left: ' + kind + 'BackoffMs });')(
    backoff, range, (r, n) => { seen.push([r, n]); return 12345; }, now);
  return Object.assign(call(), { seen });
}

test('both pollMs functions take their cadence from the pure helper', () => {
  for (const kind of ['lat', 'speed']) {
    const range = { kind: 'win', mins: 7 };
    const r = drivePollMs(kind, { range });
    assert.equal(r.ms, 12345, kind + 'PollMs does not return what its delay helper answered');
    assert.deepEqual(r.seen, [[range, CAD_NOW_MS]],
      kind + 'PollMs must price the range that is actually selected, at the current time');
  }
});

test('a reconcile-503 backoff wins exactly one poll, then clears', () => {
  for (const kind of ['lat', 'speed']) {
    const r = drivePollMs(kind, { backoff: 4000 });
    assert.equal(r.ms, 4000, kind + 'PollMs ignored the Retry-After the 503 parked');
    assert.equal(r.left, 0, kind + 'PollMs kept the backoff, so every later poll waits Retry-After forever');
    assert.deepEqual(r.seen, [], 'the helper must not even be consulted while a backoff is pending');
  }
});

// --- the page copy of the server's window bound ------------------------------
//
// MAX_WIN_MINS (and LAT_MAX_WIN_SEC, derived from it) is a copy of maxWinMins in
// web.go, and the comment above it describes what each handler does with it. A
// copy nothing checks is a copy that drifts: mutating it to 30 days used to leave
// every test green, because the cadence tests computed their expectations from
// the page's own constant.
const webGo = readSource(here, '..', 'web.go');
test('the 366-day bound is the server maxWinMins, not a number that happens to match', () => {
  const m = /\bconst maxWinMins = ([0-9 *]+)/.exec(webGo);
  assert.ok(m, 'maxWinMins is gone or renamed in web.go, so the page constant is pinned to nothing');
  const serverMins = new Function('return ' + m[1])();
  assert.equal(LAT_CONSTS.MAX_WIN_MINS, serverMins,
    'the picker clamps every rolling window to MAX_WIN_MINS before sending it. Too high and the handler ' +
    'rejects the mins and silently answers with its own default window; too low and windows the server ' +
    'would happily answer become unpickable');
  assert.equal(LAT_CONSTS.LAT_MAX_WIN_SEC, serverMins * 60,
    'cadence prices the clamped span, so the clamp has to be the one parseRangeParams applies');
});

test('what the comment on MAX_WIN_MINS says about web.go is still what web.go does', () => {
  // The clause that was wrong before this round: ?mins= is NOT clamped. Both
  // handlers only ACCEPT a mins inside the bound and otherwise keep their own
  // default, which is why an over-wide mins would draw the default window under
  // the wrong label rather than a clamped one.
  assert.equal((webGo.match(/n > 0 && n <= maxWinMins/g) || []).length, 2,
    'handleSeries and handleSpeedHistory used to ACCEPT-or-default an out-of-range ?mins=; if that is now ' +
    'a clamp, the comment above MAX_WIN_MINS in index.html is describing something else');
  assert.match(webGo, /mins := 120\b/, 'the /api/series fallback the comment names');
  assert.match(webGo, /mins := 7 \* 24 \* 60\b/, 'the /api/speed fallback the comment names');
  assert.match(webGo, /floor, ceil := now\.Add\(-maxWinMins\*time\.Minute\), now\.Add\(maxWinMins\*time\.Minute\)/,
    'the ?from/?to path IS clamped, each end to its own side of the band - that half of the comment');
  // And the floored bucket width the latPollForMins comment leans on: integer
  // division, so a window holds a handful MORE than maxSeriesPoints buckets.
  assert.match(webGo, /bucket := int\(end\.Sub\(since\)\/time\.Second\) \/ maxSeriesPoints/,
    'seriesBucket no longer floors the width by integer division, so "about 1500 buckets, and a handful ' +
    'more" no longer describes it');
});

test('cadence clamping never reaches the wire', () => {
  assert.doesNotMatch(extract('function latWindowQuery'), /LAT_MAX_WIN_SEC|latSpanMins/,
    'the sent from/to must stay exactly as typed. Clamping them was tried here and reverted: a raised ' +
    'start overtakes the end, which the server reads as a reversed range and quietly answers with the ' +
    'default window - asking for January 2020 silently drew this week');
});

// Advice to copy the database file by hand must always name the stop-first
// precondition. Pingularity runs in WAL mode unconditionally (the pragmaConn DSN
// in internal/store/store.go), and nothing in the tree ever issues a
// wal_checkpoint, so a copy taken while the service runs misses everything
// still sitting in pingularity.db-wal. Measured on a young install: the main
// file was 4 KiB against a 193 KiB sidecar, and the copy opened with "no such
// table: settings" - not a partial backup, an empty one. A clean stop folds the
// sidecar back in and the copy verifies.
//
// This is a guard against a CLASS, not the one line that prompted it. The README
// stated the rule correctly in one place and contradicted it four hundred lines
// later, and the dashboard carried the unsafe version twice - including in the
// export fallback, which is shown to someone who is by definition running the
// instance, since they just clicked Export in its dashboard.
test('advice to copy the database by hand always says to stop the service first', () => {
  const sources = [
    ['README.md', readSource(here, '..', '..', '..', 'README.md')],
    ['index.html', html],
  ];
  // A sentence is any run up to a full stop that does not end a code span, so
  // "-db path." and "pingularity.key." do not split one instruction in half.
  for (const [name, text] of sources) {
    const flat = text.replace(/\s+/g, ' ');
    const sentences = flat.split(/(?<!\b-db)(?<!\.[a-z]{2,4})\.\s/);
    const offenders = sentences.filter((s) =>
      /cop(y|ies|ying) the (SQLite )?database file/i.test(s)
      && !/stop(ping)? the service|service (is )?stopped|stop it first/i.test(s));
    assert.deepEqual(offenders, [],
      `${name} tells a user to copy the database file without saying to stop the service first. `
      + `In WAL mode that copy can be empty - the committed rows are in pingularity.db-wal until a `
      + `clean stop folds them back in.`);
  }
});

// The Access tab governs who can reach this daemon, and the one configuration the
// daemon itself warns about at boot - reachable on the network, no login - had no
// in-tab warning. The boot line goes to stderr once and scrolls out of a detached
// container, and turning the toggle on at runtime never produced it at all, so
// this predicate is the whole warning for anyone who opens access from this tab.
// It is judged on PENDING form state so it clears as the password is typed.
test('the no-login warning fires exactly when network access is open and unprotected', () => {
  const rows = [{ url: 'http://192.168.1.24:9000' }];
  const w = (netOn, r, authOn, hasPw, typed) => F.netNoAuthWarnText(netOn, r, authOn, hasPw, typed);
  assert.match(w(true, rows, false, false, ''), /no login is required/i,
    'open and unprotected is the state the warning exists for');
  assert.equal(w(false, rows, false, false, ''), '',
    'network access off: this machine only, nothing to warn about');
  assert.equal(w(true, [], false, false, ''), '',
    'nothing advertised (a loopback -listen overrules the toggle) must stay silent');
  assert.equal(w(true, rows, true, true, ''), '', 'login on with a stored password is protected');
  assert.equal(w(true, rows, true, false, 'hunter2'), '',
    'the bubble clears as the password is typed, not on the next Save');
  assert.match(w(true, rows, true, false, ''), /no login is required/i,
    'the login toggle alone does not activate auth - the server needs a password too');
  assert.match(w(true, rows, false, true, ''), /no login is required/i,
    'a stored password with the login toggle off protects nothing');
});

test('the no-login warning is recomputed from every input that can change it', () => {
  assert.match(html, /id="netNoAuthWarn"/, 'the bubble must exist in the Access tab markup');
  assert.match(extract('function updateNetNoAuthWarn'), /netNoAuthWarnText\(/);
  assert.match(extract('async function loadAccess'), /updateNetNoAuthWarn\(\)/,
    'the tab must warn when it opens, not only after an edit');
  // One handler body per assertion. A pattern that starts at one addEventListener
  // and scans forward reaches the calls in the handlers AFTER it, so it holds
  // whether or not the handler it names calls anything.
  assert.match(extract("$('setNetAccess').addEventListener"), /updateNetNoAuthWarn\(\)/,
    'flipping network access on must warn immediately');
  assert.match(extract("$('setAuthEnabled').addEventListener"), /updateNetNoAuthWarn\(\)/,
    'the login toggle changes the answer');
  assert.match(script, /\$\('authPass'\)\.addEventListener\('input',\s*updateNetNoAuthWarn\)/,
    'per keystroke: the bubble has to go away as the remedy is typed, inches from it');
});

// A max-height off a named rule, or a named failure when the rule has moved -
// String.match returns null, and indexing that throws a TypeError over whatever
// the assertion was going to say.
function cssPx(re, what) {
  const m = html.match(re);
  assert.ok(m, `${what} is not in the stylesheet any more`);
  return Number(m[1]);
}

// updateNetNoAuthWarn is the show path - nothing else puts this bubble on screen.
const NET_WARN_SRC = [extract('function netOpenNoAuth'), extract('function netNoAuthWarnText'),
  extract('function updateNetNoAuthWarn')].join('\n') + '\nreturn updateNetNoAuthWarn;';
// One fake element per driver, reused for every call: whether a state that hides
// the bubble also CLEARS it is only observable on an element still holding the
// copy the call before it wrote.
function netWarnDriver() {
  const els = {
    setNetAccess: { checked: false }, setAuthEnabled: { checked: false },
    authPass: { value: '' }, netNoAuthText: { innerHTML: '' },
    netNoAuthWarn: { classList: { toggle(cls, on) { els.netNoAuthWarn.toggled = { cls, on }; } } },
  };
  const compile = new Function('$', 'accessLanUrls', 'accessHasPassword', NET_WARN_SRC);
  return ({ netOn = true, rows = [], authOn = false, hasPassword = false, typed = '' } = {}) => {
    els.setNetAccess.checked = netOn; els.setAuthEnabled.checked = authOn; els.authPass.value = typed;
    compile(id => els[id], rows, hasPassword)();
    return { text: els.netNoAuthText.innerHTML, toggled: els.netNoAuthWarn.toggled };
  };
}

test('the bubble is put on screen for the open state and taken off it for every other', () => {
  const rows = [{ url: 'http://192.168.1.24:9000' }];
  const warn = netWarnDriver();
  const open = warn({ rows });
  assert.deepEqual(open.toggled, { cls: 'show', on: true }, 'the class is the whole show path');
  assert.match(open.text, /no login is required/i);
  const shut = warn({ rows, authOn: true, hasPassword: true });
  assert.equal(shut.toggled.on, false,
    'a password closes the gap the warning is about, so the bubble has to come off screen - one that stays up after the fix is one people learn to ignore');
  assert.equal(shut.text, '',
    'a hidden bubble must not keep the copy behind it - this element was showing the warning one call ago');
  // The copy is written with innerHTML for its <b>, so neither what the operator
  // types nor what the daemon lists may reach it.
  const echo = warn({ rows: [{ url: 'http://<img src=x>:9000', iface: '<em>eth0</em>' }], typed: '<script>' });
  assert.doesNotMatch(echo.text, /<img|eth0|<script/);
  // .warn-bubble hides its overflow, and this copy - here and in the wizard - is
  // the longest of these bubbles, so at the shared cap the sentence naming the
  // remedy is the part cut off.
  const cap = cssPx(/\.warn-bubble\.show\{[^}]*max-height:(\d+)px/, 'the shared .warn-bubble.show cap');
  for (const id of ['netNoAuthWarn', 'qsNoAuthWarn']) {
    const own = cssPx(new RegExp('#' + id + '\\.show[^{]*\\{[^}]*max-height:(\\d+)px'),
      `the #${id}.show cap that lifts it off the shared one`);
    assert.ok(own > cap, `#${id}.show is ${own}px, which must exceed the shared ${cap}px cap`);
  }
});

// Quick Setup is the path that produces passwordless installs: pick "Anyone on my
// network", leave both fields blank, and the daemon comes up network-reachable
// with nothing asking for a password. The boot line it prints then is on stderr,
// and the Access tab bubble is in a tab this operator has not opened, so the
// wizard has to say it where the choice is made.
test('Quick Setup warns when its own answer would open the network with no login', () => {
  const rows = [{ url: 'http://192.168.1.24:9000' }];
  const w = (net, r, user, pass) => F.qsNoAuthText(net, r, user, pass);
  assert.match(w(true, rows, '', ''), /no login/, 'both fields blank is the install this exists for');
  assert.equal(w(false, rows, '', ''), '', 'this machine only: nothing to warn about');
  assert.equal(w(true, [], '', ''), '', 'a loopback -listen advertises no address, so the choice opens nothing');
  assert.equal(w(true, rows, 'admin', 'hunter2'), '', 'both fields filled is the auth_enabled qsSave posts');
  assert.match(w(true, rows, 'admin', ''), /no login/, 'a username with no password activates no login');
  assert.match(w(true, rows, '', 'hunter2'), /no login/, 'nor a password with no username');
});

test('the Quick Setup warning is wired to the choice and to both fields, and blocks neither', () => {
  assert.ok(html.indexOf('id="qsNoAuthWarn"') > html.indexOf('id="qsPw"')
    && html.indexOf('id="qsNoAuthWarn"') < html.indexOf('id="qsGo"'),
    'the bubble belongs inside the dialog, under the two fields it names');
  assert.match(extract('function updateQsNoAuthWarn'), /qsNoAuthText\(/);
  assert.match(extract("wireSeg('qsAcc'"), /updateQsNoAuthWarn\(\)/,
    'the choice must warn at the click, not at the POST');
  assert.match(script, /\$\('qsUser'\)\.addEventListener\('input',\s*updateQsNoAuthWarn\)/);
  assert.match(script, /\$\('qsPass'\)\.addEventListener\('input',\s*updateQsNoAuthWarn\)/,
    'per keystroke, so it clears as the fields above it are filled in');
  assert.match(extract('async function loadAccess'), /updateQsNoAuthWarn\(\)/,
    'the address list arrives from that fetch, which can land after the dialog is up');
  assert.doesNotMatch(extract('async function qsSave'), /qsNoAuth/,
    'it warns about the choice; it does not gate Start monitoring');
});

// updateQsNoAuthWarn is the wizard's show path - nothing else puts this bubble on
// screen - so only running it pins that the answer reaches the operator. The
// access choice is read off the pills the way qsSave reads it: the second <b> of
// #qsAcc is "Anyone on my network", so the two fakes carry `on` between them.
const QS_WARN_SRC = [extract('function netOpenNoAuth'), extract('function qsNoAuthText'),
  extract('function updateQsNoAuthWarn')].join('\n') + '\nreturn updateQsNoAuthWarn;';
function qsWarnDriver() {
  const pill = which => ({ classList: { contains: c => c === 'on' && els.qsAcc.network === which } });
  const els = {
    qsAcc: { network: false, querySelectorAll: () => [pill(false), pill(true)] },
    qsUser: { value: '' }, qsPass: { value: '' }, qsNoAuthText: { innerHTML: '' },
    qsNoAuthWarn: { classList: { toggle(cls, on) { els.qsNoAuthWarn.toggled = { cls, on }; } } },
  };
  const compile = new Function('$', 'accessLanUrls', QS_WARN_SRC);
  return ({ network = true, rows = [], user = '', pass = '' } = {}) => {
    els.qsAcc.network = network; els.qsUser.value = user; els.qsPass.value = pass;
    compile(id => els[id], rows)();
    return { text: els.qsNoAuthText.innerHTML, toggled: els.qsNoAuthWarn.toggled };
  };
}

test('the Quick Setup bubble is put on screen for the open answer and taken off it for every other', () => {
  const rows = [{ url: 'http://192.168.1.24:9000' }];
  const warn = qsWarnDriver();
  const open = warn({ rows });
  assert.deepEqual(open.toggled, { cls: 'show', on: true }, 'the class is the whole show path');
  assert.match(open.text, /no login/, 'and the copy has to be written, not only the class flipped');
  // Each of these follows a shown bubble on the SAME element, so the empty string
  // is evidence the copy was cleared rather than the value it started at.
  for (const [why, answer] of [
    ['this machine only reaches no network', { rows, network: false }],
    ['a loopback -listen advertises no address, so the choice opens nothing', { rows: [] }],
    ['both fields filled is the auth_enabled qsSave posts', { rows, user: 'admin', pass: 'hunter2' }],
  ]) {
    warn({ rows });
    const shut = warn(answer);
    assert.equal(shut.toggled.on, false, why);
    assert.equal(shut.text, '', `${why}: the copy goes with the bubble`);
  }
  // The copy goes in through innerHTML, for its <b>. That is only safe while
  // qsNoAuthText returns literals, so hand every input it reads a marker - the
  // two fields, and the address list the daemon supplies - and require none of it
  // back. The marker outlives escaping, so escaping it would not rescue this.
  // (Both fields at once cannot warn: that is a login.)
  for (const [user, pass] of [['MARK<script>', ''], ['', 'MARK<script>']]) {
    const echo = warn({ rows: [{ url: 'http://MARK:9000', iface: 'MARK' }], user, pass });
    assert.match(echo.text, /no login/, 'the hostile answer is still the open one, so there is copy to inspect');
    assert.doesNotMatch(echo.text, /MARK/,
      'qsNoAuthText interpolates nothing it is handed, which is the whole licence for that innerHTML');
  }
});

// At phone width a long IPv6 URL wraps to two lines inside this pill, and .url-a
// hides no overflow, so a fixed height paints the second line below the border,
// over the row beneath. Only IPv6 rows are long enough to reach that.
test('the address pill grows for a long IPv6 URL instead of spilling out of it', () => {
  const rule = html.match(/\n\s*\.url-a\{[^}]*\}/)[0];
  assert.match(rule, /min-height:\s*30px/);
  assert.doesNotMatch(rule, /[^-]height:\s*30px/,
    'a fixed height clips or spills the second line of a wrapped IPv6 URL');
});

// This tip has to be readable in one hover, so it carries facts about the panel
// and nothing else. Address-reservation advice is about running a LAN, and is the
// likeliest line here to be quietly wrong for the reader. The rotation caveat
// stays: the IPv6 row shown may be a temporary address, and nothing here can say
// which.
test('the address-list tooltip states facts about this panel, not advice about running a LAN', () => {
  const tip = html.match(/data-tip="Where the dashboard answers[^"]*"/)[0];
  assert.doesNotMatch(tip, /DHCP/, 'address-reservation advice is not a fact about this panel');
  assert.match(tip, /rotate/, 'the do-not-bookmark caveat has to stay');
  assert.ok(tip.length < 1000, `tooltip is ${tip.length} chars`);
});

// Dead plumbing in both directions: the server populated warnings and the client
// read only r.ok, so the caveat that local-only CANNOT block visitors arriving
// through a declared reverse proxy reached nobody. Pin both ends together, and
// run the client half rather than read it: only the server knows which caveat
// applies, so which sentence lands in the toast is the whole point.
async function drivePostAccess(resp) {
  const toasts = [];
  const msg = { textContent: '' };
  const post = new Function('$', 'fetch', 'flashStatus',
    extract('async function postAccess') + '\nreturn postAccess;')(
    () => msg, async () => ({ ok: true, json: async () => resp, text: async () => '' }),
    t => toasts.push(t));
  return { ok: await post({ access: 'local' }), toasts, msg };
}

test('a caveat the server returns with a successful access save reaches the operator', async () => {
  const said = 'Local-only will not exclude visitors arriving through the reverse proxy you declared.';
  const one = await drivePostAccess({ warnings: [said] });
  assert.equal(one.ok, true, 'a caveat is not a refusal - the save happened');
  assert.deepEqual(one.toasts, [said],
    'the server writes this sentence and only the server knows which caveat applies; '
    + 'a fixed client string would be wrong the day the server adds a second one');
  assert.equal(one.msg.textContent, '',
    'Save blanks #settingsMsg and closes the drawer, so the toast is what outlives it');
  const two = await drivePostAccess({ warnings: ['First.', 'Second.'] });
  assert.deepEqual(two.toasts, ['First. Second.'], 'every caveat is shown, not just the first');
  assert.deepEqual((await drivePostAccess({})).toasts, [], 'and a clean save says nothing');
  assert.match(readSource(here, '..', 'auth.go'), /status\["warnings"\]/,
    'the server half of this must still be there');
});
