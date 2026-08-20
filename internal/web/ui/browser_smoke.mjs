// Browser smoke: the one check ui.test.mjs structurally cannot make. That
// suite extracts the page's pure functions from source and runs them under
// node - no DOM ever renders, so a page that computes everything correctly
// and still fails to PAINT (a bad selector, a runtime TypeError during
// startup, a CSP violation) passes it clean. This script drives a real
// Chromium at a live daemon and asserts the floor: the dashboard paints its
// panels and the chart, and startup emits zero console errors or uncaught
// page errors. Anchors are deliberately coarse (#chart, .panel, the title) -
// fine-grained selector assertions rot into flakes.
//
// Usage: node browser_smoke.mjs <port>   (daemon already listening on 127.0.0.1:<port>)
import { chromium } from 'playwright';

const port = process.argv[2];
if (!port) {
  console.error('usage: node browser_smoke.mjs <port>');
  process.exit(2);
}

const problems = [];
const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  page.on('console', (msg) => {
    if (msg.type() === 'error') problems.push(`console.error: ${msg.text()}`);
  });
  page.on('pageerror', (err) => problems.push(`uncaught page error: ${err.message}`));
  page.on('requestfailed', (req) => {
    // The page must be self-contained against its own daemon; any failed
    // request during startup is a broken asset or endpoint.
    problems.push(`request failed: ${req.method()} ${req.url()} (${req.failure()?.errorText})`);
  });

  await page.goto(`http://127.0.0.1:${port}/`, { waitUntil: 'load', timeout: 20000 });

  const title = await page.title();
  if (!title.includes('Pingularity')) problems.push(`title is ${JSON.stringify(title)}, expected it to name Pingularity`);

  await page.waitForSelector('#chart', { state: 'visible', timeout: 15000 })
    .catch(() => problems.push('the chart (#chart) never became visible'));

  const panels = await page.locator('.panel').count();
  if (panels < 3) problems.push(`only ${panels} .panel elements rendered, expected at least 3`);

  // Give late-startup scripts a beat to throw before judging the error log.
  await page.waitForTimeout(2000);
} catch (err) {
  problems.push(`navigation failed: ${err.message}`);
} finally {
  await browser.close();
}

if (problems.length) {
  console.error(`browser smoke FAILED (${problems.length} problem(s)):`);
  for (const p of problems) console.error(`  - ${p}`);
  process.exit(1);
}
console.log('browser smoke OK: page painted, panels and chart rendered, console clean');
