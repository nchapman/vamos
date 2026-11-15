import {chromium} from 'playwright-core';
import {build} from 'esbuild';
import {createServer} from 'node:http';
import {readFile, mkdir, writeFile, copyFile, rm} from 'node:fs/promises';
import {fileURLToPath} from 'node:url';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {validateFixtures} from './model.js';
import {withDeadline} from './deadline.js';

const browserDeadline = 30_000;

const source = fileURLToPath(new URL('.', import.meta.url));
const repo = fileURLToPath(new URL('../../', import.meta.url));
const output = join(repo, '.pi', 'visual');
await mkdir(output, {recursive: true});
// Never report a stale fixture/test result as evidence for this invocation.
await rm(join(output, 'evaluation.json'), {force: true});
await rm(join(output, 'fixtures.json'), {force: true});
const generated = spawnSync('go', ['test', './internal/cli', '-run', '^TestPresentationVisualFixtures$', '-count=1', '-timeout=2m'], {
  cwd: repo, env: {...process.env, VAMOS_VISUAL_DIR: output}, stdio: 'inherit', timeout: 300_000,
});
if (generated.error) throw generated.error;
if (generated.status !== 0) throw new Error(`Production fixture test failed (${generated.status ?? generated.signal})`);
// Missing/skipped/misnamed tests must fail, even if go test exits successfully.
const fixtures = validateFixtures(JSON.parse(await readFile(join(output, 'fixtures.json'), 'utf8')));
await build({entryPoints: [join(source, 'gallery.js')], bundle: true, format: 'esm',
  outfile: join(output, 'gallery.bundle.js')});
await copyFile(join(source, 'index.html'), join(output, 'index.html'));
await rm(join(output, 'shots'), {recursive: true, force: true});
await mkdir(join(output, 'shots'));

const allowed = new Map([['/', 'text/html'], ['/index.html', 'text/html'],
  ['/gallery.bundle.js', 'text/javascript'], ['/gallery.bundle.css', 'text/css'], ['/fixtures.json', 'application/json']]);
const server = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname;
  if (!allowed.has(path)) { res.writeHead(404); res.end(); return; }
  try {
    res.setHeader('Content-Type', allowed.get(path));
    res.setHeader('Cache-Control', 'no-store');
    res.end(await readFile(join(output, path === '/' ? 'index.html' : path.slice(1))));
  } catch { res.writeHead(500); res.end(); }
});
await withDeadline(new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolve);
}), browserDeadline, 'HTTP server startup');
let browser;
const results = [];
const errors = [];
let failure;
let browserVersion;
try {
  browser = await chromium.launch(process.env.VAMOS_VISUAL_CHROME ?
    {executablePath: process.env.VAMOS_VISUAL_CHROME, headless: true, timeout: browserDeadline} : {channel: 'chrome', headless: true, timeout: browserDeadline});
  browserVersion = browser.version();
  const page = await withDeadline(browser.newPage({viewport: {width: 1600, height: 1200}, deviceScaleFactor: 1}), browserDeadline, 'Browser page startup');
  page.setDefaultTimeout(browserDeadline);
  page.setDefaultNavigationTimeout(browserDeadline);
  page.on('pageerror', err => errors.push(err.message));
  page.on('console', msg => { if (msg.type() === 'error') errors.push(msg.text()); });
  page.on('requestfailed', req => errors.push(`${req.url()}: ${req.failure()?.errorText}`));
  const origin = `http://127.0.0.1:${server.address().port}`;
  // The gallery has no external assets. Unexpected network requests are failures.
  await page.route('**/*', route => {
    if (new URL(route.request().url()).origin === origin) return route.continue();
    errors.push(`Unexpected network request: ${route.request().url()}`);
    return route.abort();
  });
  await page.goto(origin);
  await page.waitForFunction(() => window.ready === true);
  for (const fixture of fixtures) {
    // Inherited foreground and no-color modes are checked on both backgrounds.
    const backgrounds = ['terminal', 'mono'].includes(fixture.theme) ? ['light', 'dark'] : [fixture.theme];
    for (const background of backgrounds) {
      const identity = `${fixture.case}-${fixture.theme}-${fixture.width}-${background}`;
      try {
        await withDeadline((async () => {
          const result = await page.evaluate(async ({fixture: f, background}) => {
            const size = await window.renderCase(f.case, f.theme, f.width, background);
            return {...size, ...window.evaluateCurrent()};
          }, {fixture, background});
          const bounds = await page.locator('#screen').boundingBox();
          await page.setViewportSize({width: Math.max(1600, Math.ceil(bounds.x + bounds.width + 32)), height: 1200});
          const file = `shots/${identity}.png`;
          await page.locator('#screen').screenshot({path: join(output, file), scale: 'css'});
          results.push({...result, file});
        })(), browserDeadline, `Capture ${identity}`);
      } catch (error) { throw new Error(`${identity}: ${error.message}`, {cause: error}); }
    }
  }
  if (errors.length) throw new Error(errors.join('\n'));
} catch (error) {
  failure = error;
} finally {
  try { if (browser) await withDeadline(browser.close(), browserDeadline, 'Browser cleanup'); }
  catch (error) { failure ??= error; }
  try {
    server.closeAllConnections();
    await withDeadline(new Promise(resolve => server.close(resolve)), browserDeadline, 'HTTP server cleanup');
  } catch (error) { failure ??= error; }
  await writeFile(join(output, 'evaluation.json'), JSON.stringify({
    passed: !failure, renderer: 'Production Go presentation fixtures in xterm.js 6.0.0',
    browserVersion, platform: process.platform, architecture: process.arch,
    fontFamily: 'Menlo, Consolas, "Liberation Mono", monospace', fontSize: 14, lineHeight: 1.25,
    sourceFixtures: fixtures.length, screenshots: results.length,
    normalization: 'SGR removed; soft wraps joined; 8-column ASCII tab expansion; right ASCII space padding ignored. Tabs are not byte-matched.',
    selection: 'Standalone unindented vamos lines only; xterm selection to a nonexecuting textarea, never system clipboard or shell.',
    contrastScope: 'Visible terminal cell colors on controlled backgrounds, not arbitrary native terminal palettes.',
    browserErrors: errors, failure: failure?.message ?? null,
    minContrast: results.length ? Math.min(...results.map(r => r.contrast)) : null, results,
  }, null, 2) + '\n');
}
if (failure) throw failure;
console.log(`${results.length} screenshots from ${fixtures.length} production fixtures: logical text, eligible command selections, contrast, and browser errors checked. See .pi/visual/evaluation.json.`);
