// Browser tests of the interface. They cover these parts:
// 1 the theme selector, 2 the breadcrumb and the toolbar row, the header and the bucket selector, 3 the bulk upload,
// 4 the denied permissions and the answers 401 and 403, 5 one smoke test on each S3 provider (SeaweedFS, MinIO and
// Garage), 6 the copy in parts on each provider, 7 the edit of the metadata on each provider, 8 the cleanup of the
// incomplete multipart uploads on each provider, 9 the editor of the lifecycle rules on each provider, 10 Object Lock
// (retention and legal hold) on the providers with a bucket with Object Lock, 11 encryption (SSE-S3 of the writes, and
// the display of SSE-S3, SSE-KMS, SSE-C and the default encryption of the bucket), 12 the usage and the quota of a
// bucket (the facts of each bucket in the selector, the warning of a full bucket, the tooltips) and the summary strip
// and the columns of the details, 13 the volumes (a folder of the server: listing, links, writes, uploads, previews,
// incomplete uploads, usage).
// run.sh starts Stowr and the three providers, puts the test objects (seed.js) and runs this file in the
// Playwright container. The tests of parts 1 to 4 use SeaweedFS.
// The tests change the answers of the server with page.route where a case needs it (403, 401, no answer, a denied
// permission).
const { chromium } = require('playwright');
const assert = require('assert/strict');
const { PROVIDERS, LOCKED, ENCRYPTED, VOLUMES } = require('./providers');
const { s3, s3Head, readLifecycle, writeLifecycle, restoreLifecycle } = require('./s3');
const BASE = process.env.BASE;
const MAIN = '-/seaweedfs/seaweedfs-main/';
// ONLY: a part of the name of each test to run, several parts separated by |.
const ONLY = (process.env.ONLY || '').split('|').filter(Boolean);
const selected = name => !ONLY.length || ONLY.some(part => name.includes(part));
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
let browser;

// The sections of the details of a file start closed, except "Storage object" and "Tags", and the browser keeps the
// choice of the person. The pages of the tests open them all, as a person who opened them before (the choice in the
// browser), so that each test sees the content of the sections. sections: 'default' gives the first visit.
const OPEN_SECTIONS = JSON.stringify({ storage: true, encryption: true, metadata: true, custom: true, tags: true, retention: true, file: true, count: true });

async function newPage({ sections = 'open', ...options } = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 860 }, colorScheme: 'dark', ...options });
  if (sections === 'open') {
    await context.addInitScript(value => {
      try { if (!window.localStorage.getItem('stowr.details.sections')) window.localStorage.setItem('stowr.details.sections', value); } catch (_) {}
    }, OPEN_SECTIONS);
  }
  const page = await context.newPage();
  page.errors = [];
  page.on('pageerror', error => page.errors.push(error.message));
  return page;
}

async function open(page, path) {
  await page.goto(BASE + path, { waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable, .empty', { timeout: 15000 });
}

function files(count, prefix, size = 1500) {
  return Array.from({ length: count }, (_, i) => ({ name: `${prefix}-${String(i + 1).padStart(4, '0')}.txt`, mimeType: 'text/plain', buffer: Buffer.alloc(size + (i % 97), 65 + (i % 26)) }));
}

async function listKeys(page, instance, prefix) {
  const keys = [];
  let token = '';
  do {
    const url = `${BASE}api/list?instance=${instance}&prefix=${encodeURIComponent(prefix)}&delimiter=%2F${token ? `&continuationToken=${encodeURIComponent(token)}` : ''}`;
    const data = await (await page.request.get(url)).json();
    for (const item of data.items || []) if (item.type !== 'prefix') keys.push(item.key);
    token = data.nextContinuationToken || '';
  } while (token);
  return keys;
}

async function deletePrefix(page, instance, prefix) {
  for (const key of await listKeys(page, instance, prefix)) {
    await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
  }
}

// Counts the PUT requests in flight at the same time (upload concurrency).
async function watchPuts(page, { delay = 0, fail = null } = {}) {
  const stats = { active: 0, max: 0, total: 0 };
  await page.route(/\/s3\?/, async route => {
    if (route.request().method() !== 'PUT') return route.continue();
    stats.total += 1;
    const verdict = fail ? fail(route.request().url(), stats) : null;
    if (verdict === 'abort') return route.abort('connectionrefused');
    if (verdict) return route.fulfill(verdict);
    stats.active += 1;
    stats.max = Math.max(stats.max, stats.active);
    try {
      if (delay) await sleep(delay);
      await route.continue();
    } finally {
      stats.active -= 1;
    }
  });
  return stats;
}

// Polls a function in the page (the page has a strict CSP, so no string evaluation).
async function until(page, fn, arg, timeout = 30000) {
  const end = Date.now() + timeout;
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() > end) throw new Error(`timeout: ${fn.toString().slice(0, 120)}`);
    await sleep(100);
  }
}

// The number of visible elements whose own text contains a string.
const visibleText = (page, text) => page.evaluate(value => Array.from(document.querySelectorAll('body *'))
  .filter(node => Array.from(node.childNodes).some(child => child.nodeType === 3 && child.textContent.includes(value)))
  .filter(node => node.getClientRects().length && getComputedStyle(node).visibility !== 'hidden' && !node.closest('.visually-hidden')).length, text);

// Chooses a theme with the theme selector of the header: the button, then the option of its menu.
async function chooseTheme(page, value, scope = '') {
  await page.click(`${scope} [data-theme-trigger]`.trim());
  await page.click(`${scope} [data-theme-value="${value}"]`.trim());
}

// A refusal of the storage provider, as the server gives it.
const forbidden = () => ({ status: 403, json: { error: { code: 'accessdenied', message: 'The storage provider refused the request.' } } });

// ------------------------------------------------------------------------------------------------------------------
// 1. Theme control: System / Dark / Light, saved in this browser, applied before the first paint
// ------------------------------------------------------------------------------------------------------------------

test('theme: System follows the colour scheme of the OS', async () => {
  for (const [scheme, background] of [['dark', 'rgb(13, 15, 18)'], ['light', 'rgb(246, 247, 249)']]) {
    const page = await newPage({ colorScheme: scheme });
    await open(page, MAIN);
    assert.equal(await page.getAttribute('html', 'data-theme'), null);
    assert.equal(await page.getAttribute('[data-theme-value="system"]', 'aria-selected'), 'true');
    assert.equal(await page.getAttribute('[data-theme-trigger]', 'aria-label'), 'Theme: system');
    assert.equal(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), background);
    await page.context().close();
  }
});

test('theme: a choice is saved in the browser and applies again after a reload', async () => {
  const page = await newPage({ colorScheme: 'dark' });
  await open(page, MAIN);
  // The selector of ClickHouse Dash: the menu opens under the button, and closes after a choice.
  assert.equal(await page.isHidden('#themeMenu'), true);
  await page.click('[data-theme-trigger]');
  assert.equal(await page.isVisible('#themeMenu'), true);
  assert.equal(await page.getAttribute('[data-theme-trigger]', 'aria-expanded'), 'true');
  await page.click('[data-theme-value="light"]');
  assert.equal(await page.isHidden('#themeMenu'), true);
  assert.equal(await page.getAttribute('html', 'data-theme'), 'light');
  assert.equal(await page.evaluate(() => localStorage.getItem('stowr.theme')), 'light');
  assert.equal(await page.getAttribute('[data-theme-value="light"]', 'aria-selected'), 'true');
  assert.equal(await page.getAttribute('[data-theme-trigger]', 'aria-label'), 'Theme: light');
  // The button shows the icon of the current choice.
  assert.equal(await page.isVisible('[data-theme-trigger] .themeIcon--light'), true);
  assert.equal(await page.isHidden('[data-theme-trigger] .themeIcon--system'), true);
  assert.equal(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), 'rgb(246, 247, 249)');
  await page.reload({ waitUntil: 'networkidle' });
  assert.equal(await page.getAttribute('html', 'data-theme'), 'light');
  await chooseTheme(page, 'system');
  assert.equal(await page.evaluate(() => localStorage.getItem('stowr.theme')), null);
  assert.equal(await page.getAttribute('html', 'data-theme'), null);
  await page.context().close();
});

test('theme: the saved theme is on <html> before the body exists (no flash)', async () => {
  for (const [scheme, saved] of [['dark', 'light'], ['light', 'dark']]) {
    const page = await newPage({ colorScheme: scheme });
    await page.addInitScript(value => {
      try { localStorage.setItem('stowr.theme', value); } catch (_) {}
      window.__themeAtBody = 'unset';
      new MutationObserver((records, observer) => {
        if (document.body) {
          window.__themeAtBody = document.documentElement.getAttribute('data-theme');
          window.__stylesAtBody = document.styleSheets.length;
          observer.disconnect();
        }
      }).observe(document, { childList: true, subtree: true });
    }, saved);
    await open(page, MAIN);
    assert.equal(await page.evaluate(() => window.__themeAtBody), saved);
    await page.context().close();
  }
});

test('theme: the preview page and the 404 page share the choice', async () => {
  const page = await newPage({ colorScheme: 'light' });
  await open(page, MAIN);
  await chooseTheme(page, 'dark');
  await page.goto(`${BASE}${MAIN}reports/2026/q3/summary.md`, { waitUntil: 'networkidle' });
  assert.equal(await page.getAttribute('html', 'data-theme'), 'dark');
  assert.equal(await page.getAttribute('.previewBar [data-theme-value="dark"]', 'aria-selected'), 'true');
  await page.goto(`${BASE}no-such-page`, { waitUntil: 'networkidle' });
  assert.equal(await page.getAttribute('html', 'data-theme'), 'dark');
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2. Breadcrumb: the bucket mark and a slash, with no frame, then the folders
// ------------------------------------------------------------------------------------------------------------------

test('breadcrumb: at the root, the bucket mark and a slash, with no frame and no text', async () => {
  const page = await newPage();
  await open(page, MAIN);
  const root = page.locator('.crumb--root');
  assert.equal(await page.locator('.crumbs .crumb').count(), 1);
  assert.equal((await root.innerText()).trim(), '');
  assert.equal(await root.locator('.ic').count(), 1);
  assert.equal((await page.textContent('.crumbs .crumbs__sep')).trim(), '/');
  assert.equal(await root.getAttribute('aria-current'), 'page');
  assert.equal(await root.getAttribute('aria-label'), 'SeaweedFS - primary, bucket root');
  const frame = await root.evaluate(node => { const style = getComputedStyle(node); return [style.borderTopWidth, style.backgroundColor]; });
  assert.deepEqual(frame, ['0px', 'rgba(0, 0, 0, 0)']);
  // No visible folder title: the heading stays for screen readers.
  assert.equal(await page.textContent('#folderTitle'), 'SeaweedFS - primary, bucket root');
  assert.equal(await page.evaluate(() => document.getElementById('folderTitle').classList.contains('visually-hidden')), true);
  assert.equal(await page.locator('.part__title').count(), 0);
  await page.context().close();
});

test('breadcrumb: the pointer changes the colour of a crumb and draws no frame or background', async () => {
  const page = await newPage();
  await open(page, `${MAIN}reports/2026/`);
  const look = selector => page.locator(selector).evaluate(node => {
    const style = getComputedStyle(node);
    return { color: style.color, background: style.backgroundColor, border: style.borderTopWidth, icon: getComputedStyle(node.querySelector('.ic') || node).color };
  });
  const folder = '.crumbs .crumb:not(.crumb--root):not([aria-current])';
  const before = await look(folder);
  await page.hover(folder);
  const after = await look(folder);
  assert.notEqual(after.color, before.color);
  assert.equal(after.background, 'rgba(0, 0, 0, 0)');
  assert.equal(after.border, '0px');
  const rootBefore = await look('.crumb--root');
  await page.hover('.crumb--root');
  const rootAfter = await look('.crumb--root');
  assert.notEqual(rootAfter.icon, rootBefore.icon);
  assert.equal(rootAfter.background, 'rgba(0, 0, 0, 0)');
  await page.context().close();
});

test('breadcrumb: in a folder, the mark leads back to the bucket root', async () => {
  const page = await newPage();
  await open(page, `${MAIN}reports/2026/q3/`);
  const names = await page.locator('.crumbs .crumb').allInnerTexts();
  assert.deepEqual(names.map(name => name.trim()), ['', 'reports', '2026', 'q3']);
  assert.deepEqual(await page.locator('.crumbs .crumbs__sep').allInnerTexts(), ['/', '/', '/']);
  assert.equal(await page.getAttribute('.crumb--root', 'aria-current'), null);
  assert.equal(await page.getAttribute('.crumbs .crumb:last-of-type', 'aria-current'), 'page');
  await page.click('.crumb--root');
  await until(page, () => location.pathname.endsWith('/-/seaweedfs/seaweedfs-main/'));
  await page.waitForSelector('tr[data-name="reports"]');
  assert.equal(await page.getAttribute('.crumb--root', 'aria-current'), 'page');
  assert.equal(await page.locator('.crumbs .crumb').count(), 1);
  await page.goBack();
  await until(page, () => document.querySelector('#folderTitle').textContent.startsWith('q3,'));
  await page.context().close();
});

test('breadcrumb: a deep path keeps the root and puts the middle folders in a menu', async () => {
  const page = await newPage();
  await open(page, `${MAIN}archive/deep/level-3/level-4/level-5/`);
  assert.equal(await page.locator('.crumb--root').count(), 1);
  assert.deepEqual(await page.locator('.crumbs > .crumb:not(.crumb--root)').allInnerTexts(), ['level-4', 'level-5']);
  await page.click('.crumb--more');
  assert.deepEqual(await page.locator('.crumbs .menu .menu__item').allInnerTexts(), ['archive', 'deep', 'level-3']);
  await page.click('.crumbs .menu .menu__item >> text=deep');
  await until(page, () => document.querySelector('#folderTitle').textContent.startsWith('deep,'));
  await page.context().close();
});

test('breadcrumb: the crumbs and the toolbar share one row on a desktop, and the toolbar has its own row on a phone', async () => {
  const rows = async page => page.evaluate(() => {
    const crumbs = document.querySelector('#crumbs').getBoundingClientRect();
    const toolbar = document.querySelector('.browserHead > .toolbar').getBoundingClientRect();
    return { crumbsTop: crumbs.top, crumbsBottom: crumbs.bottom, toolbarTop: toolbar.top, crumbsRight: crumbs.right, toolbarLeft: toolbar.left,
      overflow: (document.querySelector('[data-page-scroll]') || document.scrollingElement).scrollWidth - window.innerWidth };
  });
  const desktop = await newPage({ viewport: { width: 1280, height: 860 } });
  for (const path of [MAIN, `${MAIN}reports/2026/q3/`, `${MAIN}archive/deep/level-3/level-4/level-5/`]) {
    await open(desktop, path);
    const box = await rows(desktop);
    assert.ok(Math.abs(box.toolbarTop - box.crumbsTop) < 12 || box.toolbarTop < box.crumbsBottom, `one row at ${path}: ${JSON.stringify(box)}`);
    assert.ok(box.crumbsRight <= box.toolbarLeft + 1, `the crumbs come first at ${path}`);
    assert.equal(box.overflow, 0);
  }
  await desktop.context().close();
  const phone = await newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  for (const path of [MAIN, `${MAIN}reports/2026/q3/`]) {
    await open(phone, path);
    const box = await rows(phone);
    assert.ok(box.toolbarTop >= box.crumbsBottom - 1, `the toolbar under the crumbs at ${path}: ${JSON.stringify(box)}`);
    assert.ok(box.overflow <= 0, `no horizontal scroll at ${path}`);
  }
  await phone.context().close();
});

test('breadcrumb: another bucket gives its own root crumb, and the preview path starts at the bucket', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.click('.storageSwitcher__trigger strong');
  await page.click('.storageBucket[data-instance="seaweedfs-archive"] strong');
  await until(page, () => document.querySelector('.storageSwitcher__trigger strong')?.textContent === 'SeaweedFS - archive');
  assert.equal(await page.getAttribute('.crumb--root', 'aria-label'), 'SeaweedFS - archive, bucket root');
  await page.goto(`${BASE}${MAIN}reports/2026/q3/summary.md`, { waitUntil: 'networkidle' });
  assert.equal(await page.textContent('#instanceMeta'), 'SeaweedFS - primary');
  assert.equal((await page.textContent('#docPrefix')).trim(), '/ reports / 2026 / q3');
  assert.equal((await page.textContent('#backBtn')).trim(), 'q3');
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2b. JSON preview: totals, tabs joined to the panel, root of the tree
// ------------------------------------------------------------------------------------------------------------------

test('json: a file that fits in one page shows its totals and no count button', async () => {
  const page = await newPage();
  await page.goto(`${BASE}${MAIN}config/ingest.json`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.json-range-label');
  assert.match(await page.textContent('.json-range-label'), /^Lines 1–\d+ of \d+ · page 1 of 1$/);
  assert.equal(await page.locator('.json-count-button').isVisible(), false);
  await page.click('[data-mode="beautify"]');
  await until(page, () => /^Lines 1–\d+ of \d+ · page 1 of 1$/.test(document.querySelector('.json-range-label')?.textContent || ''));
  assert.equal(await page.locator('.json-count-button').isVisible(), false);
  await page.context().close();
});

test('json: a file of several pages keeps the count button until the totals are known', async () => {
  const page = await newPage();
  await page.goto(`${BASE}${MAIN}big/rows.json`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.json-range-label');
  assert.match(await page.textContent('.json-range-label'), /^Lines 1–[\d,]+ · page 1$/);
  assert.equal(await page.locator('.json-count-button').isVisible(), true);
  await page.click('.json-count-button');
  await until(page, () => /of [\d,]+ · page 1 of [\d,]+$/.test(document.querySelector('.json-range-label')?.textContent || ''));
  assert.equal(await page.locator('.json-count-button').isVisible(), false);
  await page.context().close();
});

test('json: the tabs touch the left edge, the active tab joins the panel, and the panel has no open corner', async () => {
  const page = await newPage();
  await page.goto(`${BASE}${MAIN}config/ingest.json`, { waitUntil: 'networkidle' });
  for (const mode of ['raw', 'beautify', 'tree']) {
    await page.click(`[data-mode="${mode}"]`);
    await until(page, () => !document.querySelector('.json-panel:not([hidden]) .is-loading, .json-panel:not([hidden]).is-loading'));
    const box = await page.evaluate(mode => {
      const panel = document.querySelector('.json-panel:not([hidden]) .wrapped-code-viewer, .json-tree-panel:not([hidden])');
      const tab = document.querySelector(`[data-mode="${mode}"]`).getBoundingClientRect();
      const first = document.querySelector('[data-mode="raw"]').getBoundingClientRect();
      const controls = document.querySelector('.json-mode-controls').getBoundingClientRect();
      const style = getComputedStyle(panel);
      const rect = panel.getBoundingClientRect();
      return { firstLeft: first.left, controlsLeft: controls.left, panelLeft: rect.left, panelTop: rect.top,
        tabBottom: tab.bottom, controlsBottom: controls.bottom, radius: style.borderTopLeftRadius, top: style.borderTopWidth };
    }, mode);
    assert.equal(box.firstLeft, box.controlsLeft, `the first tab at the left edge (${mode}): ${JSON.stringify(box)}`);
    assert.equal(box.firstLeft, box.panelLeft, `the first tab and the panel share the left edge (${mode})`);
    assert.equal(box.radius, '0px', `no top corner on the panel (${mode})`);
    assert.equal(box.top, '0px');
    assert.ok(box.tabBottom >= box.controlsBottom, `the active tab reaches the panel (${mode}): ${JSON.stringify(box)}`);
    assert.ok(Math.abs(box.panelTop - box.controlsBottom) < 1.01, `the panel starts at the line of the tabs (${mode}): ${JSON.stringify(box)}`);
  }
  await page.context().close();
});

test('json: the root of the tree stays open and cannot be closed', async () => {
  const page = await newPage();
  await page.goto(`${BASE}${MAIN}config/ingest.json`, { waitUntil: 'networkidle' });
  await page.click('[data-mode="tree"]');
  await page.waitForSelector('.json-tree-node .json-tree-children:not([hidden])');
  const root = page.locator('.json-tree-panel > .json-tree-node > .json-tree-line .json-tree-toggle');
  assert.equal(await root.isDisabled(), true);
  assert.equal(await root.evaluate(node => getComputedStyle(node).visibility), 'hidden');
  await page.click('.json-tree-panel > .json-tree-node > .json-tree-line .json-tree-key', { force: true });
  await page.dblclick('.json-tree-panel > .json-tree-node > .json-tree-line', { force: true });
  assert.equal(await page.locator('.json-tree-panel > .json-tree-node > .json-tree-children').isVisible(), true);
  // A child still opens and closes.
  const child = page.locator('.json-tree-children .json-tree-node:has(.json-tree-type.is-object) > .json-tree-line .json-tree-toggle').first();
  await child.click();
  await page.waitForSelector('.json-tree-children .json-tree-children:not([hidden])');
  await child.click();
  assert.equal(await page.locator('.json-tree-children .json-tree-children:not([hidden])').count(), 0);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2c. Tags: details, lifecycle, column and filter, folder job, tags of the uploads
// ------------------------------------------------------------------------------------------------------------------

async function apiTags(page, key, instance = 'seaweedfs-main') {
  const response = await page.request.get(`${BASE}api/tags?instance=${instance}&key=${encodeURIComponent(key)}`);
  assert.equal(response.status(), 200, await response.text());
  return (await response.json()).tags.map(tag => `${tag.key}=${tag.value}`);
}

async function setApiTags(page, key, tags, instance = 'seaweedfs-main') {
  const response = await page.request.put(`${BASE}api/tags`, { data: { instance, key, tags } });
  assert.equal(response.status(), 200, await response.text());
}

async function openDetails(page, name) {
  await page.click(`tr[data-name="${name}"] .bb-kebab`);
  await page.click(`tr[data-name="${name}"] .bb-menu-item >> text=Details`);
  await page.waitForSelector('.bb-details--file');
}

test('tags: the details of a file show its tags, add, remove and save', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await openDetails(page, 'ingest.json');
  await page.waitForSelector('[data-tags-section] .tagChip');
  assert.deepEqual(await page.locator('[data-tags-section] .tagChip').allInnerTexts().then(items => items.map(item => item.replace(/\s+/g, ' ').trim()).sort()), ['env prod', 'team data']);
  // The tags are read only until the frameless button Edit opens the form.
  assert.equal(await page.isHidden('[data-tags-section] .tagEditor__form'), true);
  assert.equal(await page.locator('[data-tags-section] .tagChip__remove').count(), 0);
  await page.click('[data-tag-edit]');
  assert.equal(await page.isDisabled('[data-tag-save]'), true);
  // A wrong tag is refused with a reason, a good tag joins the list.
  await page.fill('[data-tags-section] input[aria-label="Tag key"]', 'aws:reserved');
  await page.click('[data-tag-add]');
  assert.match(await page.textContent('.tagEditor__message'), /reserved/);
  await page.fill('[data-tags-section] input[aria-label="Tag key"]', 'owner');
  await page.fill('[data-tags-section] input[aria-label="Tag value"]', 'ada');
  await page.click('[data-tag-add]');
  assert.equal(await page.locator('[data-tags-section] .tagChip').count(), 3);
  assert.equal(await page.isDisabled('[data-tag-save]'), false);
  await page.click('[data-tag-save]');
  await until(page, () => document.querySelector('.tagEditor__message')?.textContent === 'The tags are saved.');
  assert.deepEqual((await apiTags(page, 'config/ingest.json')).sort(), ['env=prod', 'owner=ada', 'team=data']);
  // The save closes the form. Remove one and save again; Revert undoes an edit that is not saved.
  assert.equal(await page.isHidden('[data-tags-section] .tagEditor__form'), true);
  await page.click('[data-tag-edit]');
  await page.click('[data-tags-section] .tagChip:has-text("owner") .tagChip__remove');
  await page.click('[data-tag-revert]');
  assert.equal(await page.locator('[data-tags-section] .tagChip').count(), 3);
  await page.click('[data-tags-section] .tagChip:has-text("owner") .tagChip__remove');
  await page.click('[data-tag-save]');
  await until(page, () => document.querySelector('.tagEditor__message')?.textContent === 'The tags are saved.');
  assert.deepEqual((await apiTags(page, 'config/ingest.json')).sort(), ['env=prod', 'team=data']);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('tags: the limit of ten tags and the characters of S3 are checked in the page', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await openDetails(page, 'app.yaml');
  await page.click('[data-tag-edit]');
  await page.waitForSelector('[data-tags-section] .tagEditor__form');
  for (let index = 0; index < 10; index += 1) {
    await page.fill('[data-tags-section] input[aria-label="Tag key"]', `k${index}`);
    await page.click('[data-tag-add]');
  }
  assert.equal(await page.locator('[data-tags-section] .tagChip').count(), 10);
  assert.equal(await page.isDisabled('[data-tag-add]'), true);
  assert.match(await page.textContent('.tagEditor__count'), /10 of 10/);
  await page.click('[data-tag-revert]');
  await page.fill('[data-tags-section] input[aria-label="Tag value"]', 'a&b');
  await page.fill('[data-tags-section] input[aria-label="Tag key"]', 'bad');
  await page.click('[data-tag-add]');
  assert.match(await page.textContent('.tagEditor__message'), /letters, digits/);
  assert.deepEqual(await apiTags(page, 'config/app.yaml'), []);
  await page.context().close();
});

test('tags: the details show what the lifecycle rules do to the object, before the save', async () => {
  const page = await newPage();
  await open(page, `${MAIN}logs/`);
  await openDetails(page, '2026-10-18.log');
  await page.click('[data-tag-edit]');
  await page.waitForSelector('[data-tags-section] .tagEditor__form');
  assert.equal(await page.locator('.tagEditor__notes').isVisible(), false);
  await page.fill('[data-tags-section] input[aria-label="Tag key"]', 'retention');
  await page.fill('[data-tags-section] input[aria-label="Tag value"]', '30d');
  await page.click('[data-tag-add]');
  assert.match(await page.textContent('.tagEditor__notes'), /Rule expire-logs \(prefix logs\/, tag retention=30d\): the object expires 30 days after its creation\./);
  await page.click('[data-tags-section] .tagChip:has-text("retention") .tagChip__remove');
  assert.equal(await page.locator('.tagEditor__notes').isVisible(), false);
  await page.context().close();
});

test('tags: a provider without tags (501) has no tags section and no tag control', async () => {
  const page = await newPage();
  await page.route(/\/api\/tags/, route => route.fulfill({ status: 501, contentType: 'application/json', body: JSON.stringify({ error: { code: 'tagging_unsupported', message: 'no tags' } }) }));
  await open(page, `${MAIN}config/`);
  await openDetails(page, 'ingest.json');
  await sleep(500);
  assert.equal(await page.locator('[data-tags-section]').count(), 0);
  await page.click('.bb-modal-x');
  await page.waitForSelector('.bb-overlay', { state: 'detached' });
  // The tag column is on by default: its first read gets the 501, and the tag controls go.
  await page.click('#settingsButton');
  await until(page, () => document.querySelector('[data-column="tags"]').closest('label').hidden === true);
  assert.equal(await page.locator('#tagFilterToggle').isHidden(), true);
  assert.equal(await page.locator('#tagFilterWrap').isHidden(), true);
  assert.equal(await page.locator('th.col-tags').isVisible(), false);
  await page.context().close();
});

test('tags: the tag column and the tag filter come from the settings of the listing', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  assert.equal(await page.isHidden('#tagFilterWrap'), true);
  // The tag column is on by default. The settings turn it off and on again.
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  assert.equal(await page.locator('th.col-tags').isVisible(), true);
  await page.click('#settingsButton');
  await page.uncheck('[data-column="tags"]');
  assert.equal(await page.locator('th.col-tags').isVisible(), false);
  await page.check('[data-column="tags"]');
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  assert.equal(await page.locator('th.col-tags').isVisible(), true);
  assert.deepEqual(await page.locator('tr[data-name="ingest.json"] .tagChip').allInnerTexts().then(items => items.map(item => item.replace(/\s+/g, ' ').trim()).sort()), ['env prod', 'team data']);
  assert.equal(await page.isHidden('#tagFilterWrap'), true);
  await page.check('#tagFilterToggle');
  assert.equal(await page.isVisible('#tagFilterWrap'), true);
  await page.fill('#tagFilter', 'team=data');
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === true);
  assert.equal(await page.isVisible('tr[data-name="ingest.json"]'), true);
  assert.equal((await page.textContent('#tagStatus')).trim(), '1 of 2');
  await page.fill('#tagFilter', 'env');
  await until(page, () => document.querySelector('tr[data-name="ingest.json"]').hidden === false);
  await page.fill('#tagFilter', 'team=finance');
  await until(page, () => document.querySelector('tr[data-name="ingest.json"]').hidden === true);
  await page.fill('#tagFilter', '');
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === false);
  // The filter works with no tag column: it reads the tags itself.
  await page.uncheck('[data-column="tags"]');
  assert.equal(await page.locator('th.col-tags').isVisible(), false);
  await page.fill('#tagFilter', 'team=data');
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === true);
  // Turn the filter off: the field goes and every row comes back.
  await page.uncheck('#tagFilterToggle');
  assert.equal(await page.isHidden('#tagFilterWrap'), true);
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === false);
  await page.click('#settingsReset');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('tags: the tag column of versioned files shows no pending mark, and the details of an older version show its tags', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await page.waitForSelector('tr[data-name="report.txt"] .tagChip');
  await page.waitForSelector('tr[data-name="notes.txt"] .tagChip');
  const chipsOf = async name => (await page.locator(`tr[data-name="${name}"] .tagChip`).allInnerTexts()).map(item => item.replace(/\s+/g, ' ').trim()).sort();
  assert.deepEqual(await chipsOf('report.txt'), ['status final', 'team data']);
  assert.deepEqual(await chipsOf('notes.txt'), ['team docs']);
  assert.equal(await page.locator('.tagChips__pending').count(), 0);
  assert.equal(await page.locator('td.col-tags', { hasText: 'Reading' }).count(), 0);
  assert.equal((await page.textContent('th.col-tags')).trim(), 'Tags');
  assert.equal(await page.locator('th.col-tags .visually-hidden').count(), 1);
  // The details of an older version show the tags of that version, and the tags are read only there.
  await openDetails(page, 'report.txt');
  await page.waitForSelector('[data-tags-section] .tagChip');
  assert.equal(await page.isVisible('[data-tag-edit]'), true);
  await page.click('.versionPicker__button');
  await page.click('.versionPicker__item[aria-selected="false"] >> nth=1');
  await until(page, () => document.querySelector('[data-tags-section] .tagChip')?.textContent.includes('draft'));
  assert.equal(await page.isHidden('[data-tags-section] .tagEditor__form'), true);
  assert.equal(await page.isHidden('[data-tag-edit]'), true);
  assert.equal(await page.locator('[data-tags-section] .tagChip__remove').count(), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('tags: the tags float at the right, next to the versions, and the column of versions is narrow', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  const boxes = await page.evaluate(() => {
    const row = document.querySelector('tr[data-name="ingest.json"]');
    const chips = [...row.querySelectorAll('.tagChip')].map(chip => chip.getBoundingClientRect());
    const name = row.querySelector('.nameLink').getBoundingClientRect();
    return { chipsRight: Math.max(...chips.map(box => box.right)), chipsLeft: Math.min(...chips.map(box => box.left)), nameRight: name.right,
      versionsLeft: document.querySelector('th.col-versions').getBoundingClientRect().left, versionsWidth: document.querySelector('th.col-versions').getBoundingClientRect().width,
      sizeLeft: document.querySelector('th.col-size').getBoundingClientRect().left };
  });
  assert.ok(boxes.versionsLeft - boxes.chipsRight < 24, `the tags next to the versions: ${JSON.stringify(boxes)}`);
  const tops = await page.locator('tr[data-name="ingest.json"] .tagChip').evaluateAll(chips => chips.map(chip => Math.round(chip.getBoundingClientRect().top)));
  assert.equal(new Set(tops).size, 1, `the tags on one line: ${tops}`);
  assert.ok(await page.locator('tr[data-name="ingest.json"]').evaluate(row => row.getBoundingClientRect().height) < 45);
  assert.ok(boxes.chipsLeft - boxes.nameRight > 100, `the tags far from the name: ${JSON.stringify(boxes)}`);
  assert.ok(boxes.versionsWidth < 70, `the column of versions is narrow: ${JSON.stringify(boxes)}`);
  await open(page, '');
  await page.waitForSelector('tr[data-name="README.md"] .tagChip');
  assert.deepEqual((await page.locator('tr[data-name="README.md"] .tagChip').allInnerTexts()).map(item => item.replace(/\s+/g, ' ').trim()), ['team docs']);
  await page.context().close();
});

test('tags: a row shows three tags and a count for the other ones', async () => {
  const page = await newPage();
  await open(page, `${MAIN}images/`);
  await page.waitForSelector('tr[data-name="banner.svg"] .tagChip');
  assert.equal(await page.locator('tr[data-name="banner.svg"] .tagChip').count(), 3);
  assert.equal((await page.textContent('tr[data-name="banner.svg"] .tagMore')).trim(), '+2');
  const title = await page.getAttribute('tr[data-name="banner.svg"] .tagMore', 'title');
  assert.equal(title.split(', ').length, 2);
  // The details have the five tags.
  await openDetails(page, 'banner.svg');
  await page.waitForSelector('[data-tags-section] .tagChip');
  assert.equal(await page.locator('[data-tags-section] .tagChip').count(), 5);
  await page.context().close();
});

test('tags: a tag is a badge with a swatch whose colour comes from the key, and a click filters by the tag', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  // The swatch and the colour: the same key has the same colour, two keys have two colours (here: team and env).
  const colours = await page.locator('tr[data-name="ingest.json"] .tagChip').evaluateAll(chips => chips.map(chip => [chip.dataset.tag, getComputedStyle(chip.querySelector('.tagSwatch')).backgroundColor]));
  assert.equal(colours.length, 2);
  assert.notEqual(colours[0][1], colours[1][1]);
  assert.equal(await page.locator('tr[data-name="ingest.json"] .tagChip .tagSwatch').count(), 2);
  assert.equal(await page.isHidden('#tagFilterWrap'), true);
  // A click on a tag turns the filter on and fills it. The rows with no such tag go.
  await page.click('tr[data-name="ingest.json"] .tagChip[data-tag="team=data"] .tagChip__pick');
  assert.equal(await page.isVisible('#tagFilterWrap'), true);
  assert.equal(await page.inputValue('#tagFilter'), 'team=data');
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === true);
  assert.equal(await page.locator('tr[data-name="ingest.json"] .tagChip[data-tag="team=data"].is-active').count(), 1);
  assert.equal(await page.locator('tr[data-name="ingest.json"] .tagChip[data-tag="env=prod"].is-active').count(), 0);
  // Another tag replaces the filter. Ctrl adds a word. A second click on the only tag clears the filter.
  await page.click('tr[data-name="ingest.json"] .tagChip[data-tag="env=prod"] .tagChip__pick');
  assert.equal(await page.inputValue('#tagFilter'), 'env=prod');
  await page.click('tr[data-name="ingest.json"] .tagChip[data-tag="team=data"] .tagChip__pick', { modifiers: ['Control'] });
  assert.equal(await page.inputValue('#tagFilter'), 'env=prod team=data');
  assert.equal(await page.locator('tr[data-name="ingest.json"] .tagChip.is-active').count(), 2);
  await page.click('tr[data-name="ingest.json"] .tagChip[data-tag="team=data"] .tagChip__pick', { modifiers: ['Control'] });
  assert.equal(await page.inputValue('#tagFilter'), 'env=prod');
  await page.click('tr[data-name="ingest.json"] .tagChip[data-tag="env=prod"] .tagChip__pick');
  assert.equal(await page.inputValue('#tagFilter'), '');
  await until(page, () => document.querySelector('tr[data-name="app.yaml"]').hidden === false);
  // The choice of the filter stays turned on, as a setting.
  assert.equal(await page.isVisible('#tagFilterWrap'), true);
  await page.click('#settingsButton');
  assert.equal(await page.isChecked('#tagFilterToggle'), true);
  await page.click('#settingsReset');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('tags: a value with spaces keeps its quotes in the filter', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await setApiTags(page, 'config/app.yaml', [{ key: 'owner', value: 'data team' }]);
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('tr[data-name="app.yaml"] .tagChip');
  await page.click('tr[data-name="app.yaml"] .tagChip .tagChip__pick');
  assert.equal(await page.inputValue('#tagFilter'), 'owner="data team"');
  await until(page, () => document.querySelector('tr[data-name="ingest.json"]').hidden === true);
  assert.equal(await page.isVisible('tr[data-name="app.yaml"]'), true);
  await page.click('#settingsButton');
  await page.click('#settingsReset');
  await setApiTags(page, 'config/app.yaml', []);
  await page.context().close();
});

test('tags: a folder job sets and removes tags on every object, and keeps the other tags', async () => {
  const page = await newPage();
  await open(page, `${MAIN}reports/2026/`);
  const keys = ['reports/2026/q1/summary.md', 'reports/2026/q2/summary.md', 'reports/2026/q3/summary.md', 'reports/2026/q3/orders.csv'];
  await open(page, `${MAIN}reports/`);
  await page.click('tr[data-name="2026"] .bb-kebab');
  await page.click('tr[data-name="2026"] .bb-menu-item >> text=Tag this folder');
  await page.waitForSelector('[data-tag-form]');
  await page.fill('[data-tag-form] textarea', 'batch=yes, bad&key');
  await page.click('[data-tag-form] .bb-btn-primary');
  assert.match(await page.textContent('[data-tag-form] .tagEditor__message'), /letters, digits/);
  await page.fill('[data-tag-form] textarea', 'batch=yes, stage=one');
  await page.click('[data-tag-form] .bb-btn-primary');
  await until(page, () => !document.querySelector('[data-tag-form]'));
  await until(page, async () => true);
  for (const key of keys) {
    const deadline = Date.now() + 30000;
    for (;;) {
      const tags = await apiTags(page, key);
      if (tags.includes('batch=yes')) break;
      assert.ok(Date.now() < deadline, `${key} was not tagged: ${tags}`);
      await sleep(300);
    }
    assert.ok((await apiTags(page, key)).includes('stage=one'));
  }
  assert.deepEqual((await apiTags(page, 'reports/2026/q3/summary.md')).sort(), ['batch=yes', 'stage=one', 'team=finance']);
  // Remove the two keys with a second job.
  await page.click('tr[data-name="2026"] .bb-kebab');
  await page.click('tr[data-name="2026"] .bb-menu-item >> text=Tag this folder');
  await page.waitForSelector('[data-tag-form]');
  await page.fill('[data-tag-form] input', 'batch, stage');
  await page.click('[data-tag-form] .bb-btn-primary');
  await until(page, () => !document.querySelector('[data-tag-form]'));
  for (const key of keys) {
    const deadline = Date.now() + 30000;
    for (;;) {
      const tags = await apiTags(page, key);
      if (!tags.includes('batch=yes') && !tags.includes('stage=one')) break;
      assert.ok(Date.now() < deadline, `${key} kept its tags: ${tags}`);
      await sleep(300);
    }
  }
  assert.deepEqual(await apiTags(page, 'reports/2026/q3/summary.md'), ['team=finance']);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('tags: the uploads of the browser carry the tags that the person sets in the settings', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'tagged/');
  await open(page, `${MAIN}tagged/`);
  assert.equal(await page.locator('#uploadTagsItem').count(), 0);
  await page.click('#settingsButton');
  await page.fill('#uploadTagsInput', 'team=up, bad&key');
  await page.press('#uploadTagsInput', 'Enter');
  assert.match(await page.textContent('#uploadTagsMessage'), /letters, digits/);
  assert.equal(await page.getAttribute('#uploadTagsInput', 'aria-invalid'), 'true');
  await page.fill('#uploadTagsInput', 'team=up, source=browser');
  await page.press('#uploadTagsInput', 'Enter');
  await until(page, () => document.querySelector('#uploadTagsMessage').textContent === 'The next uploads carry these tags.');
  assert.equal(await page.inputValue('#uploadTagsInput'), 'team=up, source=browser');
  // The choice stays after a reload.
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable, .empty');
  await page.click('#settingsButton');
  assert.equal(await page.inputValue('#uploadTagsInput'), 'team=up, source=browser');
  await page.setInputFiles('#fileInput', files(2, 'tagged'));
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 60000 });
  for (const key of ['tagged/tagged-0001.txt', 'tagged/tagged-0002.txt']) {
    assert.deepEqual((await apiTags(page, key)).sort(), ['source=browser', 'team=up']);
  }
  // A multipart upload carries them too.
  await page.setInputFiles('#fileInput', [{ name: 'large-tagged.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(33 * 1024 * 1024, 3) }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 120000 });
  assert.deepEqual((await apiTags(page, 'tagged/large-tagged.bin')).sort(), ['source=browser', 'team=up']);
  // No tag: the next uploads have none.
  await page.fill('#uploadTagsInput', '');
  await page.press('#uploadTagsInput', 'Enter');
  await page.setInputFiles('#fileInput', files(1, 'plain'));
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 60000 });
  assert.deepEqual(await apiTags(page, 'tagged/plain-0001.txt'), []);
  await deletePrefix(page, 'seaweedfs-main', 'tagged/');
  await page.context().close();
});

test('tags: an instance that keeps no tags gets no tag at upload, and the tag settings are hidden', async () => {
  const page = await newPage();
  // The server tells that the instance has no tags (as for Garage). The saved tags of the uploads are not sent.
  await page.addInitScript(() => localStorage.setItem('stowr.uploadTags', JSON.stringify([{ key: 'team', value: 'up' }])));
  await page.route('**/api/instances*', async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const instance of body.instances || []) instance.tagsSupported = false;
    await route.fulfill({ response, json: body });
  });
  const sent = [];
  page.on('request', request => {
    if (request.method() === 'PUT' && request.url().includes('/s3?')) sent.push({ kind: 'put', tagging: request.headers()['x-amz-tagging'] || '' });
    if (request.method() === 'POST' && request.url().includes('/api/uploads')) sent.push({ kind: 'create', tags: (JSON.parse(request.postData() || '{}').tags || []).length });
  });
  await open(page, `${MAIN}config/`);
  await page.click('#settingsButton');
  assert.equal(await page.locator('#uploadTagsSetting').isVisible(), false);
  assert.equal(await page.locator('[data-column="tags"]').isVisible(), false);
  await page.keyboard.press('Escape');
  await page.setInputFiles('#fileInput', [{ name: 'notag.txt', mimeType: 'text/plain', buffer: Buffer.from('no tag') }, { name: 'notag-large.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(33 * 1024 * 1024, 5) }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 120000 });
  assert.ok(sent.length >= 2, JSON.stringify(sent));
  for (const item of sent) assert.ok(item.kind === 'put' ? item.tagging === '' : item.tags === 0, JSON.stringify(item));
  assert.deepEqual(await apiTags(page, 'config/notag.txt'), []);
  await deletePrefix(page, 'seaweedfs-main', 'config/notag');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2d. Listing: columns, settings, icon buttons
// ------------------------------------------------------------------------------------------------------------------

test('listing: the columns are Name, Tags, Versions, Size and Modified, with the clock at the left of Modified', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  const heads = await page.locator('thead th').evaluateAll(nodes => nodes.filter(node => node.offsetParent !== null).map(node => node.textContent.replace(/\s+/g, ' ').trim()));
  assert.deepEqual(heads.slice(0, 5), ['Name', 'Tags', 'Versions', 'Size', 'Modified']);
  const boxes = await page.evaluate(() => {
    const left = selector => document.querySelector(selector).getBoundingClientRect().left;
    return { tags: left('th.col-tags'), versions: left('th.col-versions'), size: left('th.col-size'), modified: left('th.col-modified'),
      clockRight: document.querySelector('.timeToggle').getBoundingClientRect().right, sortLeft: document.querySelector('th.col-modified .sortHead').getBoundingClientRect().left };
  });
  assert.ok(boxes.tags < boxes.versions && boxes.versions < boxes.size && boxes.size < boxes.modified, JSON.stringify(boxes));
  assert.ok(boxes.clockRight <= boxes.sortLeft, `the clock at the left of Modified: ${JSON.stringify(boxes)}`);
  // A file with one version has nothing in the column Versions: the icon and the count show for several versions.
  assert.equal(await page.locator('tr[data-name="ingest.json"] .versionCount').count(), 0);
  assert.equal(await page.locator('tr[data-name="ingest.json"] .nameCell .versionCount').count(), 0);
  await page.keyboard.press('Escape');
  await open(page, `${MAIN}versions/`);
  await page.waitForSelector('tr[data-name="report.txt"] .versionCount');
  assert.equal((await page.textContent('tr[data-name="report.txt"] .versionCount')).trim(), '3');
  assert.equal(await page.getAttribute('tr[data-name="report.txt"] .versionCount', 'title'), '3 versions: open the details');
  assert.equal(await page.locator('tr[data-name="report.txt"] .versionCount .ic-git-commit').count(), 1);
  assert.equal((await page.textContent('tr[data-name="notes.txt"] .versionCount')).trim(), '2');
  assert.equal(await page.locator('th.col-versions .visually-hidden').count(), 1);
  // The Details of the file list the three versions.
  await openDetails(page, 'report.txt');
  await page.waitForSelector('.versionPicker__button');
  assert.match(await pickerText(page), /^● v3/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('listing: the settings and the reload are icons with no frame, and their colour changes under the pointer', async () => {
  const page = await newPage();
  await open(page, MAIN);
  const order = await page.evaluate(() => ['#searchForm', '#settingsButton', '#refreshButton', '#actionsButton'].map(selector => document.querySelector(selector).getBoundingClientRect().left));
  assert.deepEqual([...order].sort((a, b) => a - b), order, `search, settings, reload, Actions: ${order}`);
  const gap = await page.evaluate(() => document.querySelector('#refreshButton').getBoundingClientRect().left - document.querySelector('#settingsButton').getBoundingClientRect().right);
  assert.ok(gap <= 8, `the gear and the reload are close: ${gap}px`);
  for (const selector of ['#settingsButton', '#refreshButton']) {
    const look = () => page.locator(selector).evaluate(node => { const style = getComputedStyle(node); return { color: style.color, background: style.backgroundColor, border: style.borderTopWidth }; });
    const rest = await look();
    assert.equal(rest.background, 'rgba(0, 0, 0, 0)', selector);
    assert.equal(rest.border, '0px', selector);
    await page.hover(selector);
    await until(page, (arg) => getComputedStyle(document.querySelector(arg.selector)).color !== arg.color, { selector, color: rest.color });
    const hover = await look();
    assert.equal(hover.background, 'rgba(0, 0, 0, 0)', selector);
    assert.equal(hover.border, '0px', selector);
    await page.mouse.move(5, 400);
  }
  await page.context().close();
});

test('listing: the settings are a menu under the gear, choose the columns, stay in the browser and reset', async () => {
  const page = await newPage();
  await open(page, MAIN);
  assert.equal(await page.isHidden('#settingsMenu'), true);
  assert.equal(await page.getAttribute('#settingsButton', 'aria-expanded'), 'false');
  await page.click('#settingsButton');
  assert.equal(await page.isVisible('#settingsMenu'), true);
  assert.equal(await page.getAttribute('#settingsButton', 'aria-expanded'), 'true');
  const place = await page.evaluate(() => {
    const gear = document.querySelector('#settingsButton').getBoundingClientRect();
    const menu = document.querySelector('#settingsMenu').getBoundingClientRect();
    return { gearBottom: gear.bottom, menuTop: menu.top, gearRight: gear.right, menuRight: menu.right, width: window.innerWidth };
  });
  assert.ok(place.menuTop >= place.gearBottom, `the menu under the gear: ${JSON.stringify(place)}`);
  assert.ok(place.menuRight <= place.width, `the menu in the window: ${JSON.stringify(place)}`);
  assert.deepEqual(await page.locator('#settingsMenu [data-column]').evaluateAll(nodes => nodes.map(node => `${node.dataset.column}:${node.checked}:${node.disabled}`)),
    ['name:true:true', 'tags:true:false', 'versions:true:false', 'size:true:false', 'modified:true:false']);
  // "Encryption and lock" is a column of the same group, on by default.
  assert.equal(await page.isChecked('#encryptionToggle'), true);
  assert.equal((await page.textContent('#encryptionSetting')).trim(), 'Encryption and lock');
  // A click on a column keeps the menu open, like the fields of a form.
  await page.uncheck('[data-column="size"]');
  await page.uncheck('[data-column="modified"]');
  assert.equal(await page.isVisible('#settingsMenu'), true);
  assert.equal(await page.locator('th.col-size').isVisible(), false);
  assert.equal(await page.locator('th.col-modified').isVisible(), false);
  assert.equal(await page.locator('td.col-size').first().isVisible(), false);
  await page.keyboard.press('Escape');
  await page.waitForSelector('#settingsMenu', { state: 'hidden' });
  // The choice stays after a reload.
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable');
  assert.equal(await page.locator('th.col-size').isVisible(), false);
  assert.equal(await page.locator('th.col-modified').isVisible(), false);
  await page.click('#settingsButton');
  assert.equal(await page.isChecked('[data-column="size"]'), false);
  // A click outside closes the menu.
  await page.click('#crumbs', { force: true });
  await page.waitForSelector('#settingsMenu', { state: 'hidden' });
  await page.click('#settingsButton');
  await page.click('#settingsReset');
  await page.waitForSelector('#settingsMenu', { state: 'hidden' });
  assert.equal(await page.locator('th.col-size').isVisible(), true);
  assert.equal(await page.locator('th.col-modified').isVisible(), true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('listing: nothing in the settings menu is wider than the menu at 390 and 1280 px, and the menu stays in the window', async () => {
  for (const viewport of [{ width: 390, height: 844 }, { width: 1280, height: 720 }]) {
    const page = await newPage({ viewport });
    await open(page, MAIN);
    await page.click('#settingsButton');
    // A long message of the tags of the uploads wraps in the menu too.
    await page.fill('#uploadTagsInput', 'team=up, bad&key');
    await page.press('#uploadTagsInput', 'Enter');
    await until(page, () => document.querySelector('#uploadTagsMessage').textContent.length > 20);
    const sizes = await page.evaluate(() => {
      const menu = document.querySelector('#settingsMenu');
      const rect = menu.getBoundingClientRect();
      const parts = Array.from(menu.querySelectorAll('label, .menu__title, .menu__message, .menu__item, .input'))
        .filter(node => node.getClientRects().length)
        .map(node => ({ name: node.id || node.className || node.tagName, scroll: node.scrollWidth, client: node.clientWidth, right: node.getBoundingClientRect().right }));
      return { menu: { scroll: menu.scrollWidth, client: menu.clientWidth, left: rect.left, right: rect.right }, parts, view: window.innerWidth };
    });
    assert.ok(sizes.menu.scroll <= sizes.menu.client, JSON.stringify(sizes.menu));
    assert.ok(sizes.menu.left >= 0 && sizes.menu.right <= sizes.view, JSON.stringify(sizes.menu));
    for (const part of sizes.parts) {
      assert.ok(part.scroll <= part.client, `${viewport.width}: ${JSON.stringify(part)}`);
      assert.ok(part.right <= sizes.menu.right, `${viewport.width}: ${JSON.stringify(part)}`);
    }
    // The labels are short; the reset button says what it resets in its tooltip.
    assert.equal((await page.textContent('#encryptionSetting')).trim(), 'Encryption and lock');
    assert.equal((await page.textContent('#settingsReset')).trim(), 'Reset to defaults');
    assert.equal(await page.getAttribute('#settingsReset', 'title'), 'Reset the columns, the tag filter, and the encryption and lock of each bucket');
    assert.deepEqual(page.errors, []);
    await page.context().close();
  }
});

// The columns that a person sees on a first visit, and the choices that stay.
const columnState = page => page.evaluate(() => {
  const menu = document.querySelector('#settingsMenu');
  const table = document.querySelector('.dataTable');
  const shown = name => Boolean(table.querySelector(`th.col-${name}`)?.getClientRects().length);
  const setting = selector => { const input = menu.querySelector(selector); return input.closest('label').hidden ? 'hidden' : input.checked; };
  return {
    settings: { tags: setting('[data-column="tags"]'), versions: setting('[data-column="versions"]'), size: setting('[data-column="size"]'), modified: setting('[data-column="modified"]'), marks: setting('#encryptionToggle') },
    table: { tags: shown('tags'), versions: shown('versions'), size: shown('size'), modified: shown('modified') }
  };
});

test('listing: every column is on by default, also "Encryption and lock"; an instance without tags or versions hides them', async () => {
  const page = await newPage();
  const heads = await watchHeads(page);
  await open(page, `${MAIN}config/`);
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  await noLockPending(page);
  assert.deepEqual(await columnState(page), {
    settings: { tags: true, versions: true, size: true, modified: true, marks: true },
    table: { tags: true, versions: true, size: true, modified: true }
  });
  // "Encryption and lock" reads one HEAD for each file of the page, and nothing more.
  const files = await page.locator('.dataTable tbody tr[data-type="content"]').count();
  assert.equal(heads.total, files);
  // Garage has no tags and no versions: the two columns and their settings are hidden. The other columns are on.
  const garage = await lockedHostOf(page, 'garage-main');
  await open(page, `-/${garage.host}/garage-main/`);
  await page.waitForSelector('.dataTable');
  assert.deepEqual(await columnState(page), {
    settings: { tags: 'hidden', versions: 'hidden', size: true, modified: true, marks: true },
    table: { tags: false, versions: false, size: true, modified: true }
  });
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('listing: a saved choice of the person wins, a column without a choice has its default', async () => {
  const page = await newPage();
  // The settings that the browser keeps: the size is off, a choice of the person. The encryption of seaweedfs-archive
  // is off. The other columns have no choice.
  await page.addInitScript(() => {
    if (!sessionStorage.getItem('seeded')) {
      sessionStorage.setItem('seeded', '1');
      localStorage.setItem('stowr.listing', JSON.stringify({ version: 1, columns: { size: false }, tagFilterOn: false, encryption: { 'seaweedfs-archive': false } }));
    }
  });
  await open(page, `${MAIN}config/`);
  await page.waitForSelector('tr[data-name="ingest.json"] .tagChip');
  assert.deepEqual((await columnState(page)).table, { tags: true, versions: true, size: false, modified: true });
  assert.equal((await columnState(page)).settings.marks, true);
  // The choice of the encryption stays for its bucket.
  await open(page, '-/seaweedfs/seaweedfs-archive/');
  assert.equal((await columnState(page)).settings.marks, false);
  // A new choice: the tags off. The browser keeps the choices only.
  await open(page, `${MAIN}config/`);
  await page.click('#settingsButton');
  await page.uncheck('[data-column="tags"]');
  const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('stowr.listing')));
  assert.deepEqual(stored, { version: 1, columns: { size: false, tags: false }, tagFilterOn: false, encryption: { 'seaweedfs-archive': false } });
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable');
  assert.deepEqual((await columnState(page)).table, { tags: false, versions: true, size: false, modified: true });
  // The reset gives every default again.
  await page.click('#settingsButton');
  await page.click('#settingsReset');
  assert.deepEqual((await columnState(page)).table, { tags: true, versions: true, size: true, modified: true });
  assert.deepEqual(await page.evaluate(() => JSON.parse(localStorage.getItem('stowr.listing'))), { version: 1, columns: {}, tagFilterOn: false, encryption: {} });
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('listing: the menu Actions has no item of bucket permissions (the icons of the selector have them)', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.click('#actionsButton');
  const items = (await page.locator('#actionsMenu .menu__item').allInnerTexts()).map(item => item.trim());
  assert.ok(items.length >= 2, `items: ${items}`);
  assert.ok(!items.some(item => /permission/i.test(item)), `items: ${items}`);
  assert.equal(await page.locator('#accessItem').count(), 0);
  assert.equal(await page.locator('#tagsItem').count(), 0);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2e. Versions: the picker of the details and the comparison of two versions
// ------------------------------------------------------------------------------------------------------------------

async function openCompareFromDetails(page, name) {
  await openDetails(page, name);
  await page.click('.versionPicker__button');
  await page.click('.versionPicker__compare');
  // The comparison is a page of its own, not a dialog.
  await page.waitForSelector('.versionComparePage .versionDiff');
  assert.equal(await page.locator('.bb-overlay').count(), 0);
  assert.match(page.url(), /[?&]compare=/);
}

// Chooses a version in the picker From or To of the comparison. The index is the row of the menu (0 is the newest).
async function pickVersion(page, side, index) {
  await page.click(`.versionDiff__${side} .versionPicker__button`);
  await page.click(`.versionPicker__menu:not([hidden]) .versionPicker__item >> nth=${index}`);
}

const cleanText = items => items.map(item => item.replace(/\s+/g, ' ').trim());
// The text of a version picker, with "● " in front when the dot of the current version is in it.
const pickerText = (page, selector = '.versionPicker__button') => page.evaluate(target => {
  const node = document.querySelector(target);
  return node ? `${node.querySelector('.versionPicker__dot') ? '● ' : ''}${node.textContent.replace(/\s+/g, ' ').trim()}` : '';
}, selector);

test('versions: the picker of the details lists each version with its date, its size and its change', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openDetails(page, 'report.txt');
  assert.equal(await page.locator('select[data-details-version]').count(), 0);
  assert.match(await pickerText(page), /^● v3.*3/);
  assert.doesNotMatch(await pickerText(page), /Current|Older/);
  // In the details window the picker keeps a margin for the close cross of the window.
  assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.bb-details-head .bb-details-version-control')).marginRight), '34px');
  assert.equal(await page.getAttribute('.versionPicker__button', 'aria-expanded'), 'false');
  await page.click('.versionPicker__button');
  assert.equal(await page.isVisible('.versionPicker__menu'), true);
  assert.equal(await page.getAttribute('.versionPicker__button', 'aria-expanded'), 'true');
  const items = cleanText(await page.locator('.versionPicker__item').allInnerTexts());
  assert.equal(items.length, 3);
  assert.match(items[0], /^v3 /);
  // The dot of the current version is in the first column of the first row only, with its name on hover.
  const dots = await page.evaluate(() => Array.from(document.querySelectorAll('.versionPicker__item')).map(row => {
    const dot = row.querySelector('.versionPicker__dot');
    if (!dot) return null;
    const style = getComputedStyle(dot);
    return { title: dot.title, label: dot.getAttribute('aria-label'), width: style.width, first: row.querySelector('.versionPicker__choose').firstElementChild === dot, radius: style.borderRadius };
  }));
  assert.deepEqual(dots, [{ title: 'Current', label: 'Current', width: '8px', first: true, radius: '50%' }, null, null]);
  assert.ok(items.every(item => !/Current|Older/.test(item)), JSON.stringify(items));
  assert.match(items[1], /^v2 /);
  assert.match(items[2], /^v1 .*first version/);
  assert.match(items[0], /\+\d+ B/);
  assert.equal(await page.getAttribute('.versionPicker__item >> nth=0', 'aria-selected'), 'true');
  assert.match(await page.textContent('.versionPicker__compare'), /Compare v3 with v2/);
  // The columns line up: the number, the date, the size and the id start at the same place in every row.
  const columns = await page.evaluate(() => Array.from(document.querySelectorAll('.versionPicker__item')).map(row => ['.versionPicker__dot, .versionPicker__dotSpace', '.versionPicker__number', '.versionPicker__when', '.versionPicker__size', '.versionPicker__meta'].map(selector => Math.round(row.querySelector(selector).getBoundingClientRect().left * 10) / 10)));
  for (const row of columns.slice(1)) assert.deepEqual(row, columns[0], `columns: ${JSON.stringify(columns)}`);
  // The menu stays in the window, under the button.
  const place = await page.evaluate(() => {
    const menu = document.querySelector('.versionPicker__menu').getBoundingClientRect();
    const button = document.querySelector('.versionPicker__button').getBoundingClientRect();
    return { top: menu.top, bottom: button.bottom, left: menu.left, right: menu.right, width: window.innerWidth };
  });
  assert.ok(place.top >= place.bottom && place.left >= 0 && place.right <= place.width, JSON.stringify(place));
  // Each row is an option with the button of the choice and the action buttons. The current version has no Restore.
  assert.equal(await page.locator('.versionPicker__item[role="option"] > button.versionPicker__choose').count(), 3);
  assert.deepEqual(await page.locator('.versionPicker__item >> nth=0').locator('[data-version-action]').evaluateAll(nodes => nodes.map(node => node.dataset.versionAction)), ['download', 'remove']);
  assert.deepEqual(await page.locator('.versionPicker__item >> nth=1').locator('[data-version-action]').evaluateAll(nodes => nodes.map(node => node.dataset.versionAction)), ['download', 'restore', 'remove']);
  assert.equal(await page.getAttribute('.versionPicker__item >> nth=1 >> [data-version-action="restore"]', 'aria-label'), 'Restore v2');
  assert.equal(await page.locator('.versionPicker__item >> nth=1 >> [data-version-action="remove"]').evaluate(node => node.classList.contains('iconButton')), true);
  // The action buttons show on hover and on keyboard focus.
  await page.mouse.move(0, 0);
  await until(page, () => getComputedStyle(document.querySelectorAll('.versionPicker__actions')[2]).opacity === '0');
  await page.hover('.versionPicker__item >> nth=2');
  await until(page, () => getComputedStyle(document.querySelectorAll('.versionPicker__actions')[2]).opacity === '1');
  await page.mouse.move(0, 0);
  // Keyboard: Up and Down move between the rows, Right and Left between the choice and the actions, Escape closes and
  // gives the focus back.
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.evaluate(() => document.activeElement.closest('.versionPicker__item')?.dataset.version !== undefined && document.activeElement.classList.contains('versionPicker__choose')), true);
  await page.keyboard.press('ArrowRight');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.versionAction), 'download');
  await until(page, () => getComputedStyle(document.activeElement.closest('.versionPicker__actions')).opacity === '1');
  await page.keyboard.press('ArrowRight');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.versionAction), 'restore');
  await page.keyboard.press('Tab');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.versionAction), 'remove');
  await page.keyboard.press('ArrowLeft');
  await page.keyboard.press('ArrowLeft');
  await page.keyboard.press('ArrowLeft');
  assert.equal(await page.evaluate(() => document.activeElement.classList.contains('versionPicker__choose')), true);
  await page.keyboard.press('Escape');
  await page.waitForSelector('.versionPicker__menu', { state: 'hidden' });
  assert.equal(await page.evaluate(() => document.activeElement.className.includes('versionPicker__button')), true);
  assert.equal(await page.isVisible('.bb-details--file'), true);
  // A choice opens the details of that version.
  await page.click('.versionPicker__button');
  await page.click('.versionPicker__item >> nth=2');
  await until(page, () => /v1/.test(document.querySelector('.versionPicker__button')?.textContent || '') && !document.querySelector('.versionPicker__button .versionPicker__dot'));
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the comparison of two text versions, unified and side by side, with the changed words', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'report.txt');
  assert.match(await page.textContent('.versionDiff__from .versionPicker__button'), /^From\s*v2/);
  assert.match(await page.textContent('.versionDiff__to .versionPicker__button'), /^To\s*v3/);
  // The two pickers and the summary of the change are on one line.
  const line = await page.evaluate(() => ['.versionDiff__from', '.versionDiff__to', '.versionDiff__summary'].map(selector => { const box = document.querySelector(selector).getBoundingClientRect(); return Math.round(box.top + box.height / 2); }));
  assert.ok(Math.max(...line) - Math.min(...line) <= 12, `lines ${line}`);
  assert.match(await page.textContent('.versionDiff__summary'), /^\+\d+ B/);
  await page.waitForSelector('.diff__table');
  // The default view has the two versions side by side.
  assert.equal(await page.locator('.diff__table.is-split').count(), 1);
  assert.equal(await page.getAttribute('.segmented__item[data-value="split"]', 'aria-pressed'), 'true');
  await page.click('.segmented__item[data-value="unified"]');
  await page.waitForSelector('.diff__table:not(.is-split)');
  const stats = cleanText(await page.locator('.diffStat').allInnerTexts());
  assert.deepEqual(stats, ['+3', '−2']);
  assert.doesNotMatch(await page.textContent('.versionDiff__summary'), /null/);
  const removed = cleanText(await page.locator('.diff__row--del .diff__text').allInnerTexts()).map(item => item.replace(/^−\s*/, ''));
  const added = cleanText(await page.locator('.diff__row--add .diff__text').allInnerTexts()).map(item => item.replace(/^\+\s*/, ''));
  assert.deepEqual(removed, ['Status: review', 'Orders: 120']);
  assert.deepEqual(added, ['Status: final', 'Orders: 124', '- approved']);
  // The words that changed are marked in a changed line.
  assert.deepEqual(cleanText(await page.locator('.diff__row--add mark').allInnerTexts()).filter(Boolean).slice(0, 2), ['final', '124']);
  // The 12 lines of context between the two edits are folded; a click shows them.
  assert.equal(await page.locator('.diff__fold').count(), 1);
  assert.match(await page.textContent('.diff__fold'), /\d+ unchanged lines/);
  const before = await page.locator('.diff__row').count();
  await page.click('.diff__expand');
  assert.ok(await page.locator('.diff__row').count() > before + 5);
  assert.equal(await page.locator('.diff__fold').count(), 0);
  // Side by side.
  await page.click('.segmented__item[data-value="split"]');
  await page.waitForSelector('.diff__table.is-split');
  assert.equal(await page.locator('.diff__table.is-split thead th').count(), 2);
  assert.ok(await page.locator('.diff__table.is-split .diff__row--mod').count() >= 2);
  // The whole file, with no fold.
  await page.check('[data-diff-whole]');
  assert.equal(await page.locator('.diff__fold').count(), 0);
  // Swap the two versions: the additions become removals.
  await page.click('.versionDiff__swap');
  await page.waitForSelector('.diffStat');
  assert.deepEqual(cleanText(await page.locator('.diffStat').allInnerTexts()).slice(0, 2), ['+2', '−3']);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the comparison of two JSON versions has the modes Raw, Beautify and Tree, as the preview', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'config.json');
  // Beautify is the first mode: the two documents are indented and side by side.
  await page.waitForSelector('.diff__table.is-split');
  assert.deepEqual(cleanText(await page.locator('[aria-label="Format"] .segmented__item').allInnerTexts()), ['Raw', 'Beautify', 'Tree']);
  assert.equal(await page.getAttribute('.segmented__item[data-value="beautify"]', 'aria-pressed'), 'true');
  const nodes = () => page.evaluate(() => Array.from(document.querySelectorAll('.jsonTree__node')).map(node => `${node.className.match(/--(added|removed|changed|same)/)[1]} ${node.querySelector('.jsonTree__label').textContent}`));
  // Tree: v2 -> v3 has one change, replicas 3 to 4. The format and the order of the keys are not changes.
  await page.click('.segmented__item[data-value="tree"]');
  await page.waitForSelector('.jsonTree');
  { const got = await nodes(); assert.equal(JSON.stringify(got), JSON.stringify(['changed $', 'changed replicas']), JSON.stringify(got)); }
  assert.match(cleanText(await page.locator('.jsonTree__node--changed .jsonTree__change').allInnerTexts())[0], /3 to 4/);
  assert.match(cleanText(await page.locator('.jsonTree__same').allInnerTexts())[0], /^\d+ unchanged$/);
  assert.equal(await page.locator('.versionDiff__actions').count(), 0);
  // The unchanged values show with the option.
  await page.check('[data-diff-whole]');
  assert.ok((await nodes()).includes('same service'));
  await page.uncheck('[data-diff-whole]');
  // v1 -> v3. The keys are in alphabetical order.
  await pickVersion(page, 'from', 2);
  await until(page, () => document.querySelectorAll('.jsonTree__node').length >= 7);
  { const got = await nodes(); assert.equal(JSON.stringify(got), JSON.stringify(['changed $', 'removed beta', 'changed limits', 'added memory', 'changed regions', 'added [1]', 'changed replicas']), JSON.stringify(got)); }
  assert.deepEqual(cleanText(await page.locator('.diffStat').allInnerTexts()).slice(0, 1), ['4 paths changed']);
  // A branch closes and opens.
  await page.click('.jsonTree__node--changed:has(> .jsonTree__row .jsonTree__label:text-is("limits")) > .jsonTree__row .jsonTree__toggle');
  assert.equal(await page.locator('.jsonTree__label:text-is("memory")').isVisible(), false);
  // Raw: the text as it is stored. Beautify: indented. Both have the views of the text.
  await page.click('.segmented__item[data-value="raw"]');
  await page.waitForSelector('.diff__table.is-split');
  assert.equal(await page.locator('[data-diff-sort]').count(), 0);
  assert.match(await page.textContent('.diff__table.is-split'), /\{"service":"ingest"/);
  // The text starts at the left of each side: the number column is narrow.
  const columns = await page.evaluate(() => { const row = document.querySelector('.diff__table.is-split tbody tr.diff__row'); const cells = Array.from(row.children).map(cell => cell.getBoundingClientRect()); return { number: cells[0].width, gap: cells[1].left - cells[0].right, text: cells[1].width, side: cells[2].left - cells[0].left }; });
  assert.ok(columns.number <= 80, `number column ${columns.number}px`);
  assert.ok(Math.abs(columns.side - (columns.number + columns.text)) < 2);
  // A changed line with marked words has no text of an object in it.
  assert.doesNotMatch(await page.textContent('.versionDiff__view'), /\[object /);
  assert.ok(await page.locator('.diff__table.is-split mark').count() >= 1);
  await page.click('.segmented__item[data-value="beautify"]');
  await page.waitForSelector('.diff__table.is-split');
  assert.equal(await page.locator('[data-diff-sort]').count(), 1);
  assert.match(await page.textContent('.diff__table.is-split'), /"service": "ingest"/);
  await page.click('.segmented__item[data-value="unified"]');
  await page.waitForSelector('.diff__table:not(.is-split)');
  assert.equal(await page.locator('.diff__row--add').count() > 0, true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the comparison of two CSV versions shows the rows and the cells that changed', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'orders.csv');
  await page.waitForSelector('.diff__table--csvsplit');
  await page.click('.segmented__item[data-value="table"]');
  await page.waitForSelector('.diff__table--csv:not(.diff__table--csvsplit)');
  // v2 -> v3: the row 1 is removed, the quantity of the row 4 changed.
  assert.deepEqual(cleanText(await page.locator('.diffStat').allInnerTexts()), ['0 added', '1 removed', '1 modified']);
  assert.equal(await page.locator('.diff__row--del').count(), 1);
  assert.equal(await page.locator('.diff__row--mod').count(), 1);
  assert.deepEqual(cleanText(await page.locator('.csvCell--changed').allInnerTexts()), ['1 to 2']);
  assert.match(cleanText(await page.locator('.diff__row--del').allInnerTexts())[0], /1 ada 5/);
  // v1 -> v2: a row added, a quantity changed.
  await pickVersion(page, 'from', 2);
  await pickVersion(page, 'to', 1);
  await until(page, () => document.querySelectorAll('.diffStat').length === 3 && document.querySelector('.diffStat').textContent.startsWith('1 added'));
  assert.deepEqual(cleanText(await page.locator('.diffStat').allInnerTexts()), ['1 added', '0 removed', '1 modified']);
  assert.deepEqual(cleanText(await page.locator('.csvCell--changed').allInnerTexts()), ['7 to 8']);
  // The unchanged rows are folded; the option shows them.
  await page.check('[data-diff-whole]');
  assert.equal(await page.locator('.diff__row--eq').count() >= 2, true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: a version that is not text gives a clear message and the comparison of the bytes', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'blob.bin');
  await page.waitForSelector('.diff__empty');
  assert.match(await page.textContent('.versionDiff__view'), /not text/);
  assert.equal(await page.locator('.versionDiff__view .bb-btn >> text=Compare the bytes').count(), 1);
  await page.context().close();
});

test('versions: two versions with the same content say so', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'notes.txt');
  await page.waitForSelector('.diffStat');
  assert.deepEqual(cleanText(await page.locator('.diffStat').allInnerTexts()), ['+1', '−1']);
  await pickVersion(page, 'from', 0);
  await pickVersion(page, 'to', 0);
  await until(page, () => /two different versions/.test(document.querySelector('.versionDiff__view')?.textContent || ''));
  await page.context().close();
});

test('versions: the menu of a row has no separate action of versions, and the badge of the count opens the details', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await page.waitForSelector('tr[data-name="orders.csv"] .versionCount');
  await page.click('tr[data-name="orders.csv"] .bb-kebab');
  const items = cleanText(await page.locator('tr[data-name="orders.csv"] .bb-menu-item').allInnerTexts());
  assert.ok(items.length >= 3 && !items.some(item => /version/i.test(item)), `items: ${items}`);
  await page.keyboard.press('Escape');
  // The badge is a button: it opens the details of the file, with the version picker. The page has no table of the
  // versions.
  await page.click('tr[data-name="report.txt"] .versionCount');
  await page.waitForSelector('.bb-details--file .bb-details-head .versionPicker__button');
  assert.match(await page.textContent('.bb-details-name'), /^report\.txt$/);
  await page.click('.versionPicker__button');
  assert.equal(await page.locator('.versionPicker__item').count(), 3);
  assert.equal(await page.locator('.versionPicker__all, [data-version-select], [data-version-compare-selected]').count(), 0);
  assert.equal(await visibleText(page, 'All versions and actions'), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the preview page has the same picker, with no select and no separate action', async () => {
  const page = await newPage();
  await page.goto(`${BASE}${MAIN}versions/report.txt`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.versionPicker__button');
  assert.equal(await page.locator('#previewVersionSelect').count(), 0);
  assert.match(await pickerText(page), /^● v3/);
  await page.click('#previewKebabBtn');
  assert.equal(await page.locator('#pv-versions').count(), 0);
  const actions = cleanText(await page.locator('#previewMenu .bb-menu-item').allInnerTexts());
  assert.ok(!actions.some(item => /^versions$/i.test(item)), `actions: ${actions}`);
  await page.keyboard.press('Escape');
  // A choice changes the version of the preview and the label of the picker.
  await page.click('.versionPicker__button');
  assert.equal(await page.locator('.versionPicker__item').count(), 3);
  await page.click('.versionPicker__item >> nth=2');
  await until(page, () => /v1/.test(document.querySelector('.versionPicker__button')?.textContent || '') && !document.querySelector('.versionPicker__button .versionPicker__dot'));
  assert.match(page.url(), /version=/);
  // The comparison opens from the picker.
  const headerBefore = await page.evaluate(() => document.querySelector('.previewBar').getBoundingClientRect().height);
  await page.click('.versionPicker__button');
  await page.click('.versionPicker__compare');
  await page.waitForSelector('.versionComparePage .diff__table');
  assert.equal(await page.locator('.bb-overlay').count(), 0);
  assert.match(page.url(), /compare=/);
  assert.equal(await page.locator('.versionDiff__from .versionPicker__button, .versionDiff__to .versionPicker__button').count(), 2);
  assert.equal(await page.locator('select').count(), 0);
  // No back button, no title and no line of the path in the page.
  assert.equal(await page.locator('.versionComparePage__back, .versionComparePage__title, .versionDiff__file').count(), 0);
  // The header has no new row. The picker stays in its place and becomes the version To. The version From is at its left, and
  // the statistics are between the two.
  const place = await page.evaluate(() => {
    const box = selector => document.querySelector(selector).getBoundingClientRect();
    return {
      header: box('.previewBar').height, from: box('#compareFrom'), stats: box('#compareStats'), to: box('#previewVersionControl'),
      toIsPicker: document.querySelector('#previewVersionControl').classList.contains('versionDiff__to') && !document.querySelector('#previewVersionControl').hidden,
      inHeader: Boolean(document.querySelector('.previewBar #compareFrom .versionPicker__button')),
      bar: Boolean(document.querySelector('#compareBar'))
    };
  });
  assert.ok(Math.abs(place.header - headerBefore) < 1.5, `header ${headerBefore}px, then ${place.header}px`);
  assert.equal(place.inHeader, true);
  assert.equal(place.toIsPicker, true);
  assert.equal(place.bar, false);
  assert.ok(place.from.right <= place.stats.left + 1 && place.stats.right <= place.to.left + 1, JSON.stringify(place));
  assert.match(await page.textContent('#compareFrom'), /^From/);
  assert.match(await page.textContent('#previewVersionControl'), /^To/);
  // The picker To has no margin for a close cross of a window: the close button of the comparison follows at the gap of
  // the bar.
  const closeGap = await page.evaluate(() => ({
    margin: getComputedStyle(document.querySelector('#previewVersionControl')).marginRight,
    gap: document.querySelector('#compareClose').getBoundingClientRect().left - document.querySelector('#previewVersionControl').getBoundingClientRect().right
  }));
  assert.equal(closeGap.margin, '0px');
  assert.ok(closeGap.gap <= 9, JSON.stringify(closeGap));
  // The comparison is in the page: the browser back button returns to the preview, and so does the button of the page.
  await page.goBack();
  await until(page, () => !document.querySelector('.versionComparePage') && document.querySelector('#viewer')?.textContent.includes('Status'));
  assert.doesNotMatch(page.url(), /compare=/);
  await page.click('.versionPicker__button');
  await page.click('.versionPicker__compare');
  await page.waitForSelector('.versionComparePage .diff__table');
  await page.click('#compareClose');
  await until(page, () => !document.querySelector('.versionComparePage') && document.querySelector('#viewer')?.textContent.includes('Status'));
  assert.doesNotMatch(page.url(), /compare=/);
  // The picker of the preview has the actions of the versions and no item of a table of the versions. The pickers From
  // and To of a comparison have no action.
  await page.click('.versionPicker__button');
  assert.equal(await page.locator('.versionPicker__menu:not([hidden]) [data-version-action="download"]').count(), 3);
  assert.equal(await page.locator('.versionPicker__menu:not([hidden]) [data-version-action="restore"]').count(), 2);
  assert.equal(await page.locator('.versionPicker__all').count(), 0);
  await page.click('.versionPicker__compare');
  await page.waitForSelector('.versionComparePage .diff__table');
  for (const side of ['from', 'to']) {
    await page.click(`.versionDiff__${side} .versionPicker__button`);
    assert.equal(await page.locator('.versionPicker__menu:not([hidden]) .versionPicker__item').count(), 3);
    assert.equal(await page.locator('.versionPicker__menu:not([hidden]) [data-version-action]').count(), 0);
    await page.keyboard.press('Escape');
  }
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the git patch has the format of git, and can be copied and downloaded', async () => {
  const page = await newPage();
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(BASE).origin });
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'report.txt');
  await page.waitForSelector('.diff__table');
  await page.click('.segmented__item[data-value="git"]');
  await page.waitForSelector('.gitPatch');
  const patch = await page.locator('.gitPatch').evaluate(node => node.textContent);
  const lines = patch.split('\n');
  assert.match(lines[0], /^diff --git a\/versions\/report\.txt b\/versions\/report\.txt$/);
  assert.match(lines[1], /^index [0-9a-f]{1,7}\.\.[0-9a-f]{1,7} 100644$/);
  assert.equal(lines[2], '--- a/versions/report.txt');
  assert.equal(lines[3], '+++ b/versions/report.txt');
  assert.match(lines[4], /^@@ -1,7 \+1,7 @@$/);
  assert.ok(lines.includes('-Status: review') && lines.includes('+Status: final') && lines.includes('-Orders: 120') && lines.includes('+Orders: 124'));
  assert.ok(lines.includes('+- approved'));
  assert.ok(patch.endsWith('\n'));
  assert.equal(await page.locator('.gitPatch__line--hunk').count(), 2);
  assert.equal(await page.locator('.gitPatch__line--add').count(), 3);
  assert.equal(await page.locator('.gitPatch__line--del').count(), 2);
  // Copy and download give the same text as the page.
  await page.click('[data-diff-copy]');
  await until(page, async () => (await navigator.clipboard.readText()).startsWith('diff --git'));
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), patch);
  const [download] = await Promise.all([page.waitForEvent('download'), page.click('[data-diff-download]')]);
  assert.equal(download.suggestedFilename(), 'report.txt.patch');
  const stream = await download.createReadStream();
  let body = '';
  for await (const chunk of stream) body += chunk;
  assert.equal(body, patch);
  // The patch of two other versions follows the choice of the versions.
  await pickVersion(page, 'from', 2);
  await until(page, () => /@@ -1,\d+ \+1,\d+ @@/.test(document.querySelector('.gitPatch')?.textContent || '') && document.querySelector('.gitPatch').textContent.includes('-Status: draft'));
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: the JSON patch follows the option that sorts the keys, and the CSV has two tables side by side', async () => {
  const page = await newPage();
  await open(page, `${MAIN}versions/`);
  await openCompareFromDetails(page, 'config.json');
  await page.waitForSelector('.diff__table.is-split');
  // v2 -> v3: the document has the same data, with other keys order and format. Sorted keys: one change.
  assert.equal(await page.isChecked('[data-diff-sort]'), true);
  await page.click('.segmented__item[data-value="git"]');
  await page.waitForSelector('.gitPatch');
  assert.equal(await page.locator('.gitPatch__line--add').count(), 1);
  await page.uncheck('[data-diff-sort]');
  await until(page, () => document.querySelectorAll('.gitPatch__line--add').length > 1);
  await open(page, `${MAIN}versions/`);
  // The CSV side by side: the rows before at the left, the rows after at the right, with the changed cells.
  await openCompareFromDetails(page, 'orders.csv');
  await page.waitForSelector('.diff__table--csvsplit');
  assert.equal(await page.locator('.diff__table--csvsplit thead th').count(), 2);
  assert.ok(await page.locator('.diff__table--csvsplit .csvCell--cut.is-del').count() >= 1);
  assert.ok(await page.locator('.diff__table--csvsplit .csvCell--cut.is-add').count() >= 1);
  assert.ok(await page.locator('.diff__table--csvsplit .is-empty').count() >= 1);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('versions: an older version of a JSON file opens in the preview, with its ETag, without HTTP 412', async () => {
  const page = await newPage();
  const list = await (await page.request.get(`${BASE}api/versions?instance=seaweedfs-main&key=versions/config.json`)).json();
  const older = list.versions.find(version => !version.isCurrent);
  assert.ok(older, 'config.json needs an older version');
  for (const endpoint of ['raw', 'summary', 'tree']) {
    const response = await page.request.get(`${BASE}api/json/${endpoint}?instance=seaweedfs-main&key=versions/config.json&version=${older.version}&etag=${encodeURIComponent('"' + older.etag + '"')}`);
    assert.equal(response.status(), 200, `${endpoint}: ${await response.text()}`);
  }
  await page.goto(`${BASE}${MAIN}versions/config.json?version=${older.version}`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.json-range-label', { timeout: 8000 }).catch(async () => assert.fail((await page.evaluate(() => document.querySelector('main, body').innerHTML)).slice(0, 1500)));
  assert.match(await page.textContent('.json-range-label'), /^Lines 1–\d+ of \d+/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('windows: the details and the folder insights use one window, with the same classes, width, top edge and height', async () => {
  const page = await newPage({ viewport: { width: 1600, height: 900 } });
  await open(page, `${MAIN}versions/`);
  const box = () => page.evaluate(() => {
    const overlay = document.querySelector('.bb-overlay');
    const modal = document.querySelector('.bb-modal');
    const body = document.querySelector('.bb-modal .bb-details-body');
    const rect = modal.getBoundingClientRect();
    const inner = modal.querySelector('.bb-modal-body');
    return { overlay: overlay.className.replace(/\s*is-open\s*/, ' ').trim(), modal: modal.className.trim(), top: rect.top, width: rect.width, height: rect.height, body: body.getBoundingClientRect().height, transform: getComputedStyle(modal).transform,
      padding: getComputedStyle(inner).padding, gap: rect.right - body.getBoundingClientRect().right, contentGap: parseFloat(getComputedStyle(body).paddingRight) };
  });
  const settled = () => until(page, () => getComputedStyle(document.querySelector('.bb-modal')).transform === 'none');
  const close = async () => { await page.click('.bb-modal-x'); await page.waitForSelector('.bb-overlay', { state: 'detached' }); };
  // The details, with its two tabs.
  await openDetails(page, 'report.txt');
  await settled();
  const details = await box();
  // The body of the window has no padding at the right: the element that scrolls ends at the edge of the window (its
  // border), and its content keeps a gap.
  assert.equal(details.padding, '8px 0px 16px 20px');
  assert.ok(details.gap <= 1.5, `gap ${details.gap}px`);
  assert.ok(details.contentGap >= 16, `content gap ${details.contentGap}px`);
  assert.equal(details.modal, 'bb-modal bb-modal--large');
  assert.equal(details.overlay, 'bb-overlay bb-overlay--large');
  assert.ok(details.width >= 900, `width ${details.width}px`);
  assert.ok(details.top < 200, `top ${details.top}px`);
  // One page, with no tab: the sections and the tools (integrity, inspection) are in the same window.
  assert.equal(await page.locator('[data-details-tab], .bb-details-tabs').count(), 0);
  assert.equal(await page.locator('[data-details-tool-card="integrity"]').count(), 1);
  assert.equal(await page.locator('[data-details-tool-card="inspection"]').count(), 1);
  assert.equal(await page.locator('[data-tags-section]').isVisible(), true);
  // The arrow of the picker has the size of the text, and the close button changes only its colour.
  const look = await page.evaluate(() => {
    const arrow = document.querySelector('.bb-details-head .versionPicker__button .mdi-chevron-down').getBoundingClientRect();
    return { arrow: arrow.width, before: getComputedStyle(document.querySelector('.bb-modal-x')).color };
  });
  assert.ok(look.arrow <= 20, `arrow ${look.arrow}px`);
  await page.hover('.bb-modal-x');
  await until(page, before => getComputedStyle(document.querySelector('.bb-modal-x')).color !== before, look.before);
  const after = await page.evaluate(() => { const style = getComputedStyle(document.querySelector('.bb-modal-x')); return { background: style.backgroundColor, border: style.borderTopColor }; });
  assert.equal(after.background, 'rgba(0, 0, 0, 0)');
  assert.equal(after.border, 'rgba(0, 0, 0, 0)');
  await close();
  // The folder insights.
  await page.click('#actionsButton');
  await page.click('#insightsItem');
  await page.waitForSelector('.bb-details--insights');
  await settled();
  const insights = await box();
  for (const other of [insights]) {
    assert.equal(other.modal, details.modal);
    assert.equal(other.overlay, details.overlay);
    assert.ok(Math.abs(other.width - details.width) < 0.5, `width ${other.width}px, ${details.width}px`);
    assert.ok(Math.abs(other.top - details.top) < 0.5, `top ${other.top}px, ${details.top}px`);
    assert.ok(Math.abs(other.body - details.body) < 0.5, `body ${other.body}px, ${details.body}px`);
    assert.equal(other.padding, details.padding);
    assert.ok(other.gap <= 1.5 && other.contentGap >= 16, JSON.stringify(other));
  }
  await page.context().close();
});

// Opens the insights of the current folder from the menu of the actions, and waits until the window stops to move.
async function openInsights(page, tab = 'overview') {
  await page.click('#actionsButton');
  await page.click('#insightsItem');
  await page.waitForSelector('.bb-details--insights');
  await until(page, () => getComputedStyle(document.querySelector('.bb-modal')).transform === 'none');
  await page.click(`[data-insights-tab="${tab}"]`);
  await page.waitForSelector(`[data-insights-panel="${tab}"]:not([hidden])`);
}

test('insights: every label of the treemap is readable on its tile, also the tile Others', async () => {
  const prefix = 'insights-readable/';
  const page = await newPage();
  // Eight large files of different types (so, different tile colours), and twelve tiny files: each of them is less than
  // 1% of the total, so the server groups them in the tile Others.
  const types = ['txt', 'png', 'pdf', 'csv', 'json', 'zip', 'md', 'bin'];
  try {
    for (const [index, type] of types.entries()) await putWithServer(page, 'seaweedfs-main', `${prefix}large-${index}.${type}`, Buffer.alloc(40000 + index * 500, 65), { 'Content-Type': 'application/octet-stream' });
    for (let index = 0; index < 12; index++) await putWithServer(page, 'seaweedfs-main', `${prefix}tiny-${index}.txt`, Buffer.alloc(100 + index, 66));
    for (const scheme of ['dark', 'light']) {
      const view = await newPage({ colorScheme: scheme });
      await open(view, MAIN + prefix);
      await openInsights(view, 'treemap');
      await view.waitForSelector('.folder-treemap:not(.is-layout-pending) .folder-treemap-node');
      const result = await view.evaluate(() => {
        // The browser resolves any CSS colour (rgb, hex, color-mix, color()) when it draws it on a canvas of one pixel.
        const context = Object.assign(document.createElement('canvas'), { width: 1, height: 1 }).getContext('2d', { willReadFrequently: true });
        const draw = (color, base) => {
          context.fillStyle = '#000000';
          context.fillStyle = base;
          context.fillRect(0, 0, 1, 1);
          context.fillStyle = '#000000';
          context.fillStyle = color;
          context.fillRect(0, 0, 1, 1);
          return Array.from(context.getImageData(0, 0, 1, 1).data.slice(0, 3));
        };
        const luminance = rgb => {
          const [red, green, blue] = rgb.map(value => { const unit = value / 255; return unit <= 0.04045 ? unit / 12.92 : ((unit + 0.055) / 1.055) ** 2.4; });
          return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
        };
        const ratio = (first, second) => { const [high, low] = [luminance(first), luminance(second)].sort((a, b) => b - a); return (high + 0.05) / (low + 0.05); };
        const rgbText = rgb => `rgb(${rgb.join(', ')})`;
        const modalBackground = getComputedStyle(document.querySelector('.bb-modal')).backgroundColor;
        const tiles = [];
        for (const node of document.querySelectorAll('.folder-treemap-node')) {
          const style = getComputedStyle(node);
          // The colours of the tile: its background, then each colour of the stripes over it. The worst one counts.
          const base = draw(style.backgroundColor, modalBackground);
          const grounds = [base];
          for (const stop of style.backgroundImage.match(/rgba?\([^)]*\)|color\([^)]*\)/g) || []) grounds.push(draw(stop, rgbText(base)));
          for (const text of node.querySelectorAll('.folder-treemap-name, .folder-treemap-size, .folder-treemap-meta')) {
            if (!text.getClientRects().length || getComputedStyle(text).visibility === 'hidden') continue;
            const textStyle = getComputedStyle(text);
            const color = draw(textStyle.color, rgbText(base));
            tiles.push({ tile: node.dataset.name, kind: node.dataset.kind, part: text.className, text: text.textContent, color: textStyle.color, ground: rgbText(base), opacity: textStyle.opacity, ratio: Math.min(...grounds.map(ground => ratio(color, ground))) });
          }
        }
        return tiles;
      });
      const others = result.filter(entry => entry.kind === 'other');
      assert.ok(others.length >= 2, `${scheme}: the tile Others shows its name and its size: ${JSON.stringify(others)}`);
      assert.ok(others.some(entry => entry.tile === 'Others' && entry.part === 'folder-treemap-name'), `${scheme}: the name of the tile Others is visible`);
      for (const entry of others) assert.ok(entry.ratio >= 4.5, `${scheme}: Others ${entry.part} ${entry.ratio.toFixed(2)}: ${JSON.stringify(entry)}`);
      assert.ok(result.filter(entry => entry.kind === 'file').length >= 8, `${scheme}: the file tiles have labels`);
      for (const entry of result) {
        assert.ok(entry.ratio >= 4.5, `${scheme}: ${entry.tile} ${entry.part} ${entry.ratio.toFixed(2)}: ${JSON.stringify(entry)}`);
        assert.equal(entry.opacity, '1', `${scheme}: ${entry.tile} ${entry.part} is opaque`);
      }
      // Both label colours are in use: the dark label on the light tiles, and the light label on the dark tiles.
      assert.ok(new Set(result.filter(entry => entry.kind === 'file').map(entry => entry.color)).size >= 2, `${scheme}: two label colours`);
      assert.deepEqual(view.errors, []);
      await view.context().close();
    }
  } finally {
    await deletePrefix(page, 'seaweedfs-main', prefix);
    await page.context().close();
  }
});

test('insights: the overview of the bucket root fits in the window at 1440x900 and 1366x768, with no scrollbar', async () => {
  for (const [width, height] of [[1440, 900], [1366, 768], [1280, 720]]) {
    const page = await newPage({ viewport: { width, height } });
    await open(page, MAIN);
    await openInsights(page, 'overview');
    const state = await page.evaluate(() => {
      const panel = document.querySelector('[data-insights-panel="overview"]');
      const body = document.querySelector('.bb-details--insights .bb-details-body');
      const modal = document.querySelector('.bb-modal');
      const fonts = Array.from(panel.querySelectorAll('.folder-distribution-label, .folder-distribution-value, .folder-file-list-name, .folder-file-list-secondary, .folder-file-list-value')).map(node => parseFloat(getComputedStyle(node).fontSize));
      const lists = Array.from(panel.querySelectorAll('.folder-file-list')).map(list => {
        const rows = list.querySelector('.folder-file-list-rows');
        const box = rows.getBoundingClientRect();
        const all = Array.from(rows.querySelectorAll('.folder-file-list-row'));
        return { all: all.length, visible: all.filter(row => row.getBoundingClientRect().bottom <= box.bottom + 1).length };
      });
      return {
        panel: [panel.scrollHeight, panel.clientHeight], body: [body.scrollHeight, body.clientHeight], modal: modal.getBoundingClientRect().bottom, inner: innerHeight,
        subtitle: document.querySelector('.bb-details-prefix').textContent, footers: document.querySelectorAll('.folder-details-footer').length, minimumFont: Math.min(...fonts), lists
      };
    });
    const where = `${width}x${height}: ${JSON.stringify(state)}`;
    assert.ok(state.panel[0] <= state.panel[1], `the panel scrolls, ${where}`);
    assert.ok(state.body[0] <= state.body[1], `the body of the window scrolls, ${where}`);
    assert.ok(state.modal <= state.inner, `the window is cut at the bottom, ${where}`);
    assert.match(state.subtitle, / · [\d,]+ objects · [\d.,]+ \w+ · computed in [\d,]+ ms$/, where);
    assert.equal(state.footers, 0, where);
    assert.ok(state.minimumFont >= 11.9, `a text is smaller than 12px, ${where}`);
    assert.equal(await page.locator('text=/^Computed in/').count(), 0);
    // The lists share the room that is left: each shows at least four rows (the others scroll inside the list).
    for (const list of state.lists) assert.ok(list.visible >= Math.min(4, list.all), `a list is too small, ${where}`);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  }
});

test('versions: a file with one version has no version picker; the action button of a row has no frame', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  // The button of the row: no frame and no fill, only the colour changes.
  const kebab = page.locator('tr[data-name="ingest.json"] .bb-kebab');
  const before = await kebab.evaluate(node => getComputedStyle(node).color);
  await kebab.hover();
  await until(page, previous => getComputedStyle(document.querySelector('tr[data-name="ingest.json"] .bb-kebab')).color !== previous, before);
  const style = await kebab.evaluate(node => { const css = getComputedStyle(node); return { background: css.backgroundColor, border: css.borderTopWidth }; });
  assert.equal(style.background, 'rgba(0, 0, 0, 0)');
  assert.equal(style.border, '0px');
  // One version: no picker in the details, and none in the preview page.
  await openDetails(page, 'ingest.json');
  assert.equal(await page.locator('.bb-details-head .versionPicker__button, [data-details-versions]').count(), 0);
  await page.goto(`${BASE}${MAIN}config/ingest.json`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.json-range-label');
  assert.equal(await page.locator('#previewVersionControl').isVisible(), false);
  assert.equal(await page.locator('.versionPicker__button').count(), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// Makes a file of seaweedfs-main with three versions (one second between them, so that the dates differ).
async function makeVersionedObject(key) {
  for (let number = 1; number <= 3; number += 1) {
    await s3('seaweedfs-main', 'PUT', `/default/${key}`, '', Buffer.from(`${key} version ${number}\n`), { 'content-type': 'text/plain' });
    if (number < 3) await sleep(1100);
  }
}

// The versions with data of a file, the newest first, as the picker lists them.
const dataVersions = async (page, instance, key) => (await (await page.request.get(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}`)).json()).versions
  .filter(version => !version.deleteMarker).sort((left, right) => new Date(right.lastModified) - new Date(left.lastModified));

test('versions: the picker of the details restores and deletes a version, with a confirmation, and the details and the listing follow', async () => {
  const page = await newPage();
  const key = `actions/restore-${Date.now()}.txt`;
  const name = key.split('/').pop();
  await makeVersionedObject(key);
  const [v3, v2, v1] = await dataVersions(page, 'seaweedfs-main', key);
  await open(page, `${MAIN}actions/`);
  await until(page, row => document.querySelector(`tr[data-name="${row}"] .versionCount`)?.textContent.trim() === '3', name);
  await openDetails(page, name);
  // Restore v1: a confirmation, then a new current version with the content of v1.
  await pickerAction(page, v1.version, 'restore');
  assert.match(await page.textContent(`${confirmWindow} .bb-modal-body`), new RegExp(`Create a new current version of "${key}" from ${v1.version}`));
  await page.click(`${confirmWindow} .bb-btn-primary`);
  await page.waitForSelector('.bb-toast >> text=Version restored.');
  assert.equal((await dataVersions(page, 'seaweedfs-main', key)).length, 4);
  assert.equal((await getObject(page, 'seaweedfs-main', key)).body.toString(), `${key} version 1\n`);
  // The details open again on the current version, and the badge of the listing counts 4 versions.
  await until(page, () => /v4/.test(document.querySelector('.bb-details-head .versionPicker__button')?.textContent || '') && Boolean(document.querySelector('.bb-details-head .versionPicker__button .versionPicker__dot')));
  await until(page, row => document.querySelector(`tr[data-name="${row}"] .versionCount`)?.textContent.trim() === '4', name);
  // Delete v2: a confirmation, then the version is gone.
  await pickerAction(page, v2.version, 'remove');
  assert.match(await page.textContent(`${confirmWindow} .bb-modal-body`), new RegExp(`Permanently delete exact version ${v2.version} of "${key}"`));
  await page.click(`${confirmWindow} .bb-btn-primary`);
  await page.waitForSelector('.bb-toast >> text=Version deleted.');
  assert.deepEqual((await dataVersions(page, 'seaweedfs-main', key)).map(version => version.version).filter(id => [v1.version, v2.version, v3.version].includes(id)), [v3.version, v1.version]);
  await until(page, () => /v3/.test(document.querySelector('.bb-details-head .versionPicker__button')?.textContent || '') && Boolean(document.querySelector('.bb-details-head .versionPicker__button .versionPicker__dot')));
  await until(page, row => document.querySelector(`tr[data-name="${row}"] .versionCount`)?.textContent.trim() === '3', name);
  // A cancel does nothing.
  await pickerAction(page, v1.version, 'remove');
  await page.click(`${confirmWindow} .bb-btn:not(.bb-btn-primary)`);
  await sleep(300);
  assert.equal((await dataVersions(page, 'seaweedfs-main', key)).length, 3);
  // Download gives the version to the download of the page.
  await page.evaluate(() => { BB.downloads.download = async options => { window.downloaded = options; return null; }; });
  await pickerAction(page, v3.version, 'download');
  await until(page, () => Boolean(window.downloaded));
  const downloaded = await page.evaluate(() => window.downloaded);
  assert.equal(downloaded.key, key);
  assert.equal(downloaded.version, v3.version);
  assert.deepEqual(page.errors, []);
  await deletePrefix(page, 'seaweedfs-main', 'actions/');
  await page.context().close();
});

test('versions: the picker of the preview deletes the version that the page shows, and restores a version', async () => {
  const page = await newPage();
  const key = `actions/preview-${Date.now()}.txt`;
  await makeVersionedObject(key);
  const [, v2, v1] = await dataVersions(page, 'seaweedfs-main', key);
  await page.goto(`${BASE}${MAIN}${key}?version=${encodeURIComponent(v2.version)}`, { waitUntil: 'networkidle' });
  await until(page, text => document.querySelector('#viewer')?.textContent.includes(text), `${key} version 2`);
  assert.match(await pickerText(page, '#previewVersionControl .versionPicker__button'), /^v2/);
  // The delete of the version of the page: the page shows the current version.
  await page.click('#previewVersionControl .versionPicker__button');
  await page.hover(`.versionPicker__item[data-version="${v2.version}"]`);
  await page.click(`.versionPicker__item[data-version="${v2.version}"] [data-version-action="remove"]`);
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('.bb-toast >> text=Version deleted.');
  await until(page, text => document.querySelector('#viewer')?.textContent.includes(text), `${key} version 3`);
  assert.doesNotMatch(page.url(), /version=/);
  assert.match(await pickerText(page, '#previewVersionControl .versionPicker__button'), /^● v2/);
  // The restore of v1 makes a new current version: the page shows it.
  await page.click('#previewVersionControl .versionPicker__button');
  await page.hover(`.versionPicker__item[data-version="${v1.version}"]`);
  await page.click(`.versionPicker__item[data-version="${v1.version}"] [data-version-action="restore"]`);
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('.bb-toast >> text=Version restored.');
  await until(page, () => /v3/.test(document.querySelector('#previewVersionControl .versionPicker__button')?.textContent || '') && Boolean(document.querySelector('#previewVersionControl .versionPicker__button .versionPicker__dot')));
  await until(page, text => document.querySelector('#viewer')?.textContent.includes(text), `${key} version 1`);
  assert.deepEqual(page.errors, []);
  await deletePrefix(page, 'seaweedfs-main', 'actions/');
  await page.context().close();
});

test('versions: the picker shows Restore only with the write permission and Delete only with the delete permission', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const instance of body.instances) {
      if (instance.id !== 'seaweedfs-main') continue;
      for (const name of ['write', 'delete']) instance.capabilities[name] = { state: 'denied', allowed: false, verified: true, source: 'configuration', reason: 'The permissions of the block do not give it.' };
      for (const name of ['upload', 'overwrite', 'createFolder', 'delete', 'copy', 'move', 'rename']) instance.operations[name] = { state: 'denied', allowed: false, verified: true, source: 'configuration' };
    }
    await route.fulfill({ response, json: body });
  });
  await open(page, `${MAIN}versions/`);
  await openDetails(page, 'report.txt');
  await page.click('.bb-details-head .versionPicker__button');
  assert.equal(await page.locator('.versionPicker__menu:not([hidden]) .versionPicker__item').count(), 3);
  assert.deepEqual(await page.locator('.versionPicker__menu:not([hidden]) [data-version-action]').evaluateAll(nodes => [...new Set(nodes.map(node => node.dataset.versionAction))]), ['download']);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('listing: Size and Modified use a font with one width for every character, and their width is the longest text', async () => {
  const page = await newPage();
  await open(page, `${MAIN}config/`);
  const look = await page.evaluate(() => {
    const cell = selector => { const node = document.querySelector(selector); const style = getComputedStyle(node); return { font: style.fontFamily, width: node.getBoundingClientRect().width, ch: parseFloat(style.fontSize) * 0.6 }; };
    return { size: cell('td.col-size'), modified: cell('td.col-modified'), nameWidth: document.querySelector('th:first-child').getBoundingClientRect().width, tableWidth: document.querySelector('.dataTable').getBoundingClientRect().width, modifiedLeft: document.querySelector('th.col-modified').getBoundingClientRect().left };
  });
  assert.match(look.size.font, /mono/i);
  assert.match(look.modified.font, /mono/i);
  // The width is the one of the longest text, the exact time with 19 characters, plus the padding of the cell.
  assert.ok(look.modified.width > look.modified.ch * 19, `Modified: ${look.modified.width}px`);
  assert.ok(look.modified.width < look.modified.ch * 19 + 20 + 24, `Modified: ${look.modified.width}px`);
  assert.ok(look.size.width < look.size.ch * 7 + 20 + 24, `Size: ${look.size.width}px`);
  // The values and the titles are at the right of the columns.
  const align = await page.evaluate(() => ['td.col-size', 'td.col-modified', 'th.col-size', 'th.col-modified'].map(selector => getComputedStyle(document.querySelector(selector)).textAlign));
  assert.deepEqual(align, ['right', 'right', 'right', 'right']);
  // The column keeps its width when the person changes between the relative and the exact time.
  await page.click('.timeToggle');
  await until(page, () => document.querySelector('.dataTable').classList.contains('is-exact-time'));
  const exact = await page.evaluate(() => ({ modified: document.querySelector('td.col-modified').getBoundingClientRect().width, left: document.querySelector('th.col-modified').getBoundingClientRect().left, table: document.querySelector('.dataTable').getBoundingClientRect().width }));
  assert.ok(Math.abs(exact.modified - look.modified.width) < 0.5, `exact ${exact.modified}px, relative ${look.modified.width}px`);
  assert.ok(Math.abs(exact.table - look.tableWidth) < 0.5);
  assert.ok(Math.abs(exact.left - look.modifiedLeft) < 0.5, `left ${exact.left}px, before ${look.modifiedLeft}px`);
  await page.click('.timeToggle');
  // The width does not change with the value: two rows have the same width, and two digits have the same width.
  const widths = await page.evaluate(() => {
    const measure = text => { const span = document.createElement('span'); span.style.cssText = 'position:absolute;visibility:hidden;white-space:pre'; span.className = 'modifiedCell'; document.querySelector('td.col-modified').append(span); span.textContent = text; const width = span.getBoundingClientRect().width; span.remove(); return width; };
    return [measure('11'), measure('88'), measure('iiii')];
  });
  assert.ok(Math.abs(widths[0] - widths[1]) < 0.01);
  assert.ok(Math.abs(widths[1] * 2 - widths[2]) < 0.01);
  await page.context().close();
});

test('header: the brand and the version of ClickHouse Dash at the left edge of the window, the selectors at the right edge, and the page in its centred column', async () => {
  const page = await newPage({ viewport: { width: 1700, height: 900 } });
  await open(page, MAIN);
  const build = (await (await page.request.get(`${BASE}api/instances`)).json()).build;
  assert.equal(await page.textContent('.appBrand__name'), 'Stowr');
  assert.equal(await page.title(), 'SeaweedFS - primary - SeaweedFS - primary - Stowr');
  const version = String(build.version || build.display);
  assert.ok((await page.textContent('#versionBadge')).startsWith(version));
  assert.match(await page.getAttribute('#versionBadge', 'title'), /^Server version\n/);
  const box = await page.evaluate(() => {
    const rect = selector => document.querySelector(selector).getBoundingClientRect();
    return { header: rect('.appHeader').width, brand: rect('.appBrand').left, theme: rect('.themeSelect__button').right, main: [rect('#browser').left, rect('#browser').right],
      gutter: parseFloat(getComputedStyle(document.querySelector('.appHeader__inner')).paddingLeft) };
  });
  assert.equal(box.header, 1700);
  // As in ClickHouse Dash, the header has the full width of the window. The part of the browser keeps its centred column
  // of 1400 px, so its edges are not the edges of the header.
  assert.equal(box.gutter, 12, JSON.stringify(box));
  assert.ok(Math.abs(box.brand - box.gutter) <= 1 && Math.abs(box.theme - (1700 - box.gutter)) <= 1, JSON.stringify(box));
  assert.ok(Math.abs(box.main[0] - (1700 - box.main[1])) <= 1 && box.main[0] > box.brand + 100, JSON.stringify(box));
  // The theme selector is at the right end of the header, after the bucket selector.
  const order = await page.evaluate(() => {
    const switcher = document.querySelector('#storageSwitcher').getBoundingClientRect();
    const theme = document.querySelector('.themeSelect').getBoundingClientRect();
    return switcher.right <= theme.left;
  });
  assert.equal(order, true);
  // The page has no "Back to site" link, with or without a path prefix.
  assert.equal(await page.locator('#backLink').count(), 0);
  await page.context().close();
});

// The edges of a header: the left edge of its first element (with its margin) and the right edge of the theme button,
// against the gutter of the header.
function headerEdges(page, inner, first) {
  return page.evaluate(([innerSelector, firstSelector]) => {
    const node = Array.from(document.querySelectorAll(firstSelector)).find(item => item.getClientRects().length);
    const style = getComputedStyle(node);
    return { left: node.getBoundingClientRect().left - parseFloat(style.marginLeft), right: document.querySelector('.themeSelect__button').getBoundingClientRect().right,
      gutter: parseFloat(getComputedStyle(document.querySelector(innerSelector)).paddingLeft), view: window.innerWidth };
  }, [inner, first]);
}

// The gutter of the header of ClickHouse Dash, measured with its stylesheet: 12 px, and 10 px at 820 px and less.
const DASH_GUTTERS = [[1920, 12], [1440, 12], [1000, 12], [820, 10], [390, 10]];

test('header: at 1920, 1440 and 1000 px the brand is 12 px from the left edge and the theme button 12 px from the right edge (10 px on a phone), on the list page and the preview page', async () => {
  const page = await newPage({ viewport: { width: 1920, height: 900 } });
  const cases = [
    { name: 'list', path: MAIN, inner: '.appHeader__inner', first: '.appBrand' },
    { name: 'preview', path: `${MAIN}reports/2026/q3/summary.md`, inner: '.previewBar__inner', first: '.previewBar__back' },
    { name: 'comparison', path: `${MAIN}versions/report.txt`, inner: '.previewBar__inner', first: '.previewBar__back', compare: true }
  ];
  for (const item of cases) {
    await page.setViewportSize({ width: 1920, height: 900 });
    await page.goto(BASE + item.path, { waitUntil: 'networkidle' });
    if (item.compare) {
      await page.waitForSelector('.previewVersion .versionPicker__button');
      await page.click('.previewVersion .versionPicker__button');
      await page.click('.versionPicker__compare');
      await page.waitForSelector('#compareFrom:not([hidden]) .versionPicker__button');
    }
    for (const [width, gutter] of DASH_GUTTERS) {
      await page.setViewportSize({ width, height: 900 });
      const box = await headerEdges(page, item.inner, item.first);
      const where = `${item.name} at ${width}: ${JSON.stringify(box)}`;
      assert.equal(box.gutter, gutter, where);
      assert.ok(Math.abs(box.left - box.gutter) <= 1, where);
      assert.ok(Math.abs(box.right - (box.view - box.gutter)) <= 1, where);
    }
  }
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The height of the header of ClickHouse Dash, measured in a browser with its own stylesheet and the markup of its
// header: 45 px with its border line (controls of 28 px), and 57 px on a coarse pointer (controls of 40 px).
const DASH_HEADER = 45;
const DASH_HEADER_COARSE = 57;

// The widths of the tests of the header: a phone, a docked DevTools (700 to 1000 px), a laptop and a wide screen.
const HEADER_WIDTHS = [320, 360, 390, 480, 600, 700, 760, 800, 900, 1000, 1100, 1280, 1440, 1920];

// The geometry of the header: its height, the vertical centre of each control (the mark, the trigger and
// the elements over it, the theme button), the order of the controls from left to right, and the scroll of the page.
function headerRow(page) {
  return page.evaluate(() => {
    const shown = node => Boolean(node) && node.getClientRects().length > 0 && node.getBoundingClientRect().width > 0;
    const rect = node => node.getBoundingClientRect();
    const centre = node => Math.round(rect(node).top + rect(node).height / 2);
    const pick = selector => document.querySelector(selector);
    const controls = ['.appBrand__logo', '.storageSwitcher__trigger', '.storageSwitcher__trigger strong', '#storageSwitcherInfo',
      '.storageSwitcher__facts', '.storageSwitcher__chevron', '.themeSelect__button'].map(pick).filter(shown);
    const order = ['.appBrand', '#storageSwitcher', '.themeSelect'].map(pick).filter(shown).map(rect);
    return {
      header: rect(pick('.appHeader')).height, trigger: rect(pick('.storageSwitcher__trigger')).height, theme: rect(pick('.themeSelect__button')).height,
      spread: Math.max(...controls.map(centre)) - Math.min(...controls.map(centre)),
      overlap: order.some((box, index) => index > 0 && box.left < order[index - 1].right - 0.5),
      inside: order.every(box => box.left >= 0 && box.right <= window.innerWidth),
      scroll: (document.querySelector('[data-page-scroll]') || document.scrollingElement).scrollWidth - window.innerWidth
    };
  });
}

test('header: one row with the height of the header of ClickHouse Dash at every width, in the two themes, with a fine and a coarse pointer', async () => {
  for (const coarse of [false, true]) {
    const page = await newPage({ viewport: { width: 1440, height: 900 }, ...(coarse ? { isMobile: true, hasTouch: true } : {}) });
    await open(page, MAIN);
    await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
    for (const colorScheme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme });
      for (const width of HEADER_WIDTHS) {
        await page.setViewportSize({ width, height: 900 });
        const box = await headerRow(page);
        const where = `${width}${coarse ? ' coarse' : ''} ${colorScheme}: ${JSON.stringify(box)}`;
        assert.ok(Math.abs(box.header - (coarse ? DASH_HEADER_COARSE : DASH_HEADER)) <= 1, where);
        // The controls of the header: 28 px as the host picker of ClickHouse Dash, 44 px on a coarse pointer.
        assert.equal(box.trigger, coarse ? 44 : 28, where);
        assert.equal(box.theme, coarse ? 44 : 28, where);
        assert.ok(box.spread <= 2, where);
        assert.equal(box.overlap, false, where);
        assert.equal(box.inside, true, where);
        assert.ok(box.scroll <= 0, where);
      }
    }
    assert.deepEqual(page.errors, []);
    await page.context().close();
  }
});

test('header: a narrow window hides the name of the brand, then its version', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  await open(page, MAIN);
  const seen = () => page.evaluate(() => {
    const shown = node => node.getBoundingClientRect().width > 2;
    return { name: shown(document.querySelector('.appBrand__name')), version: document.querySelector('#versionBadge').getClientRects().length > 0,
      mark: shown(document.querySelector('.appBrand__logo')) };
  });
  for (const [width, expected] of [[1440, { name: true, version: true, mark: true }], [601, { name: true, version: true, mark: true }],
    [600, { name: false, version: true, mark: true }], [421, { name: false, version: true, mark: true }],
    [420, { name: false, version: false, mark: true }], [320, { name: false, version: false, mark: true }]]) {
    await page.setViewportSize({ width, height: 900 });
    assert.deepEqual(await seen(), expected, String(width));
  }
  await page.context().close();
});

// The look of an open menu: the alpha of its background colour (rgb(), rgba(), rgb( / ) and color(srgb / ) forms), the
// opacity of the menu and of its parents at the first frame of its animation, its backdrop filter, and the elements
// of the page over it (points inside its rounded corners).
function menuSurface(page, selector) {
  return page.evaluate(target => {
    const menu = Array.from(document.querySelectorAll(target)).find(node => node.getClientRects().length);
    if (!menu) return { missing: target };
    const alpha = value => {
      const match = /\/\s*([\d.]+)(%?)\s*\)$/.exec(value) || /^rgba\([^,]+,[^,]+,[^,]+,\s*([\d.]+)()\s*\)$/.exec(value);
      if (!match) return /^(rgb|color)\(/.test(value) ? 1 : 0;
      return match[2] ? Number(match[1]) / 100 : Number(match[1]);
    };
    const animations = menu.getAnimations();
    for (const animation of animations) {
      animation.pause();
      animation.currentTime = 0;
    }
    let opacity = 1;
    for (let node = menu; node; node = node.parentElement) opacity *= Number(getComputedStyle(node).opacity);
    for (const animation of animations) animation.finish();
    const style = getComputedStyle(menu);
    const rect = menu.getBoundingClientRect();
    const covered = [];
    for (let i = 1; i < 6; i += 1) {
      for (let j = 1; j < 6; j += 1) {
        const x = rect.left + 8 + ((rect.width - 16) * i) / 6;
        const y = rect.top + 8 + ((rect.height - 16) * j) / 6;
        const node = document.elementFromPoint(x, y);
        if (node && !menu.contains(node)) covered.push(node.className || node.tagName);
      }
    }
    return { background: style.backgroundColor, alpha: alpha(style.backgroundColor), opacity, backdrop: style.backdropFilter, covered: [...new Set(covered)] };
  }, selector);
}

async function assertOpaqueMenu(page, selector, where) {
  const surface = await menuSurface(page, selector);
  const text = `${where} ${selector}: ${JSON.stringify(surface)}`;
  assert.equal(surface.missing, undefined, text);
  assert.equal(surface.alpha, 1, text);
  assert.equal(surface.opacity, 1, text);
  assert.ok(surface.backdrop === 'none' || surface.backdrop === '', text);
  assert.deepEqual(surface.covered, [], text);
}

// Closes the open menu: Escape, then a click on the mark of the brand of the header (or the title of the preview).
async function closeMenu(page) {
  await page.keyboard.press('Escape');
  await page.click('.appBrand__logo, .previewTitle .doc-name');
}

test('header: the trigger of the selector is one row while the buckets load', async () => {
  const page = await newPage({ viewport: { width: 900, height: 800 } });
  let release;
  const wait = new Promise(resolve => { release = resolve; });
  await page.route(/\/api\/instances(\?|$)/, async route => { await wait; await route.continue(); });
  await page.goto(BASE + MAIN);
  await page.waitForSelector('.storageSwitcher__skeleton');
  const box = await headerRow(page);
  assert.ok(Math.abs(box.header - DASH_HEADER) <= 1 && box.trigger === 28 && box.spread <= 2, JSON.stringify(box));
  release();
  await page.waitForSelector('.dataTable, .empty');
  await page.context().close();
});

// The geometry of the bar of the preview: its height, the vertical centres of its controls, and the scroll of the page.
function previewRow(page) {
  return page.evaluate(() => {
    const bar = document.querySelector('.previewBar');
    const shown = node => node.getClientRects().length > 0 && node.getBoundingClientRect().width > 0;
    const centre = node => Math.round(node.getBoundingClientRect().top + node.getBoundingClientRect().height / 2);
    const controls = Array.from(bar.querySelectorAll('.previewBar__back, #docName, .previewBar__tools > :not([hidden]), .themeSelect__button')).filter(shown);
    const tools = document.querySelector('.previewBar__tools').getBoundingClientRect();
    return { height: bar.getBoundingClientRect().height, spread: Math.max(...controls.map(centre)) - Math.min(...controls.map(centre)),
      name: Math.round(document.querySelector('#docName').getBoundingClientRect().width), inside: tools.right <= window.innerWidth,
      scroll: (document.querySelector('[data-page-scroll]') || document.scrollingElement).scrollWidth - window.innerWidth };
  });
}

test('preview: the bar is one row with the height of the header of ClickHouse Dash at every width; the comparison keeps one row from 761 px', async () => {
  for (const coarse of [false, true]) {
    const page = await newPage({ viewport: { width: 1440, height: 900 }, ...(coarse ? { isMobile: true, hasTouch: true } : {}) });
    for (const mode of ['file', 'versions', 'compare']) {
      await page.goto(`${BASE}${MAIN}${mode === 'file' ? 'reports/2026/q3/summary.md' : 'versions/report.txt'}`, { waitUntil: 'networkidle' });
      if (mode !== 'file') await page.waitForSelector('.previewVersion .versionPicker__button');
      if (mode === 'compare') {
        await page.click('.previewVersion .versionPicker__button');
        await page.click('.versionPicker__compare');
        await page.waitForSelector('#compareFrom:not([hidden]) .versionPicker__button');
      }
      for (const colorScheme of ['light', 'dark']) {
        await page.emulateMedia({ colorScheme });
        for (const width of HEADER_WIDTHS) {
          await page.setViewportSize({ width, height: 900 });
          const box = await previewRow(page);
          const where = `${mode} at ${width}${coarse ? ' coarse' : ''} ${colorScheme}: ${JSON.stringify(box)}`;
          assert.ok(box.scroll <= 0 && box.inside, where);
          assert.ok(box.name >= 40, where);
          // The comparison below 761 px puts its versions and statistics in a row of their own under the name.
          if (mode === 'compare' && width <= 760) continue;
          assert.ok(Math.abs(box.height - (coarse ? DASH_HEADER_COARSE : DASH_HEADER)) <= 1, where);
          assert.ok(box.spread <= 2, where);
        }
      }
    }
    assert.deepEqual(page.errors, []);
    await page.context().close();
  }
});

test('menus: the theme menu and every other menu are opaque from their first frame, in the light and the dark theme', async () => {
  const instance = 'seaweedfs-main';
  const name = `opaque-${Date.now()}.txt`;
  for (const colorScheme of ['light', 'dark']) {
    for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
      const page = await newPage({ viewport, colorScheme });
      const where = `${colorScheme} ${viewport.width}`;
      await open(page, MAIN);
      // The menus of the header: the theme and the buckets.
      await page.click('[data-theme-trigger]');
      await assertOpaqueMenu(page, '#themeMenu', where);
      await closeMenu(page);
      await page.click('.storageSwitcher__trigger strong');
      await assertOpaqueMenu(page, '#storageSwitcherMenu', where);
      await closeMenu(page);
      if (viewport.width === 390) {
        await page.context().close();
        continue;
      }
      // The menus of the toolbar and the menu of a row.
      await page.click('#actionsButton');
      await assertOpaqueMenu(page, '#actionsMenu', where);
      await closeMenu(page);
      await page.click('#settingsButton');
      await assertOpaqueMenu(page, '#settingsMenu', where);
      await closeMenu(page);
      await page.click('tr[data-name] .bb-kebab >> nth=0');
      await assertOpaqueMenu(page, '.bb-menu.is-open .bb-menu-popover', where);
      await closeMenu(page);
      // The popover of the suggestions of the metadata editor.
      if (colorScheme === 'light') await putWithServer(page, instance, `meta/${name}`, 'opaque\n', { 'Content-Type': 'application/octet-stream' });
      await open(page, `${MAIN}meta/`);
      await openMetadataForm(page, name);
      await page.focus('[data-metadata-header="contentType"]');
      await page.waitForSelector('.suggest__menu');
      await assertOpaqueMenu(page, '.suggest__menu', where);
      // The preview page: the theme menu, the menu of the actions and the version picker.
      await page.goto(`${BASE}${MAIN}versions/report.txt`, { waitUntil: 'networkidle' });
      await page.waitForSelector('.versionPicker__button');
      await page.click('[data-theme-trigger]');
      await assertOpaqueMenu(page, '#themeMenu', `${where} preview`);
      await closeMenu(page);
      await page.click('#previewKebabBtn');
      await assertOpaqueMenu(page, '#previewMenu .bb-menu-popover', `${where} preview`);
      await closeMenu(page);
      await page.click('.versionPicker__button');
      await assertOpaqueMenu(page, '.versionPicker__menu:not([hidden])', `${where} preview`);
      assert.deepEqual(page.errors, []);
      await page.context().close();
    }
  }
  const page = await newPage();
  await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(`meta/${name}`)}`);
  await page.context().close();
});

// The seven state icons of a bucket, in their order; a volume has the three icons of its rights.
const STATE_ICONS = ['read', 'write', 'delete', 'encryption', 'object-lock', 'versioning', 'lifecycle'];
const stateKeys = (page, selector) => page.locator(`${selector} [data-state-icon]`).evaluateAll(nodes => nodes.map(node => node.dataset.stateIcon));

test('selector: the bucket selector, by host, with the state icons', async () => {
  const page = await newPage();
  await open(page, MAIN);
  const trigger = page.locator('.storageSwitcher__trigger');
  assert.equal(await trigger.locator('strong').innerText(), 'SeaweedFS - primary');
  assert.equal(await trigger.locator('small').count(), 0);
  // On a wide window, one line: the name, the seven state icons, the usage, the chevron.
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  const edge = await page.evaluate(() => {
    const box = selector => document.querySelector(selector).getBoundingClientRect();
    return [box('.storageSwitcher__trigger strong'), box('.storageSwitcher__states'), box('.storageSwitcher__facts'), box('.storageSwitcher__chevron')]
      .map(rect => [Math.round(rect.left), Math.round(rect.right)]);
  });
  assert.ok(edge.every((item, index) => index === 0 || item[0] >= edge[index - 1][1]), `name, state icons, usage, chevron: ${JSON.stringify(edge)}`);
  const details = await trigger.getAttribute('title');
  assert.match(details, /id: seaweedfs-main/);
  assert.match(details, /host: seaweedfs/);
  assert.match(details, /bucket: default/);
  // The state icons are a link of their own over the trigger, not a part of it, always the seven in their order.
  assert.equal(await trigger.locator('.stateIcon').count(), 0);
  assert.deepEqual(await stateKeys(page, '.storageSwitcher__states'), STATE_ICONS);
  assert.equal(await page.locator('.permDot, .permDots, .versionMark, #storageSwitcherVersioning, #storageSwitcherStates, #storageSwitcherFacts').count(), 0);
  assert.equal(await page.isHidden('#storageSwitcherMenu'), true);
  await trigger.locator('strong').click();
  assert.equal(await trigger.getAttribute('aria-expanded'), 'true');
  // One row for each host with more than one bucket; the host of the current bucket is open. The volume is the only
  // entry of its host: it has no row, and its entry is always shown.
  assert.deepEqual(await page.locator('.storageHost__row').evaluateAll(rows => rows.map(row => row.dataset.host)), ['seaweedfs', 'minio', 'garage']);
  assert.equal(await page.locator('.storageHost[data-host="volume"] .storageHost__row').count(), 0);
  assert.equal(await page.isVisible('.storageHost[data-host="volume"] .storageBucket[data-instance="files"]'), true);
  assert.match(await page.textContent('.storageHost__row'), /seaweedfs\s*S3 · 4 buckets/);
  assert.equal(await page.getAttribute('.storageHost__row', 'aria-expanded'), 'true');
  assert.deepEqual(await page.locator('.storageHost[data-host="seaweedfs"] .storageBucket strong').allInnerTexts(), ['SeaweedFS - primary', 'SeaweedFS - archive', 'SeaweedFS - locked', 'SeaweedFS - encrypted']);
  assert.equal(await page.getAttribute('.storageBucket[data-instance="seaweedfs-main"]', 'aria-current'), 'true');
  assert.equal(await page.getAttribute('.storageBucket[data-instance="seaweedfs-archive"]', 'aria-current'), null);
  // Each entry of a bucket has the seven icons, and a volume its three.
  assert.deepEqual(await stateKeys(page, '.storageBucket__row[data-instance="seaweedfs-archive"] .storageBucket__states'), STATE_ICONS);
  assert.deepEqual(await stateKeys(page, '.storageBucket__row[data-instance="files"] .storageBucket__states'), ['read', 'write', 'delete']);
  // No mark in front of a bucket (no check, no folder icon): the highlight of the row tells the current bucket.
  assert.equal(await page.locator('.storageBucket .ic-folder, .storageBucket .storageBucket__check, .storageBucket__mark').count(), 0);
  const columns = await page.evaluate(() => {
    const rows = Array.from(document.querySelectorAll('.storageHost__buckets:not([hidden]) .storageBucket__row'));
    const current = document.querySelector('.storageBucket[data-instance="seaweedfs-main"]');
    const other = document.querySelector('.storageBucket[data-instance="seaweedfs-archive"]');
    return {
      names: rows.filter(row => row.closest('[data-host="seaweedfs"]')).map(row => Math.round(row.querySelector('strong').getBoundingClientRect().left)),
      icons: rows.map(row => Math.round(row.querySelector('.storageBucket__states').getBoundingClientRect().left)),
      highlight: getComputedStyle(current).backgroundColor !== getComputedStyle(other).backgroundColor
    };
  });
  assert.equal(columns.highlight, true, 'the current bucket is highlighted');
  assert.equal(new Set(columns.names).size, 1, `the names in one column: ${JSON.stringify(columns)}`);
  assert.equal(new Set(columns.icons).size, 1, `the state icons in one column: ${JSON.stringify(columns)}`);
  // A click on the host closes its list; Escape closes the menu.
  await page.click('.storageHost__row');
  assert.equal(await page.isHidden('.storageHost[data-host="seaweedfs"] .storageHost__buckets'), true);
  await page.click('.storageHost__row');
  await page.keyboard.press('Escape');
  assert.equal(await page.isHidden('#storageSwitcherMenu'), true);
  // The keyboard: ArrowDown opens the menu on the current bucket.
  await trigger.focus();
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.instance), 'seaweedfs-main');
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.instance), 'seaweedfs-archive');
  await page.keyboard.press('Enter');
  await until(page, () => location.pathname.endsWith('/-/seaweedfs/seaweedfs-archive/') && !location.search);
  assert.match(await page.locator('.storageSwitcher__trigger').innerText(), /SeaweedFS - archive/);
  // A click outside closes the menu. The wide menu covers the centre of the listing: the click is at its left.
  await page.click('.storageSwitcher__trigger strong');
  await page.click('#listing', { position: { x: 10, y: 10 } });
  assert.equal(await page.isHidden('#storageSwitcherMenu'), true);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 3. Bulk upload of hundreds of files
// ------------------------------------------------------------------------------------------------------------------

test('selector: the state icons and the usage are one link to the information page, with the mouse and the keyboard, and no tooltip', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  const link = page.locator('#storageSwitcherInfo');
  assert.equal(await link.count(), 1);
  assert.equal(await link.evaluate(node => node.tagName), 'A');
  assert.match(await link.getAttribute('href'), /\/-\/seaweedfs\/seaweedfs-main\/\?info$/);
  assert.equal(await link.getAttribute('title'), null);
  assert.equal(await link.locator('[title]').count(), 0);
  assert.match(await link.getAttribute('aria-label'), /^Information and permissions of this bucket\. Read: [a-z ]+, Write: [a-z ]+, Delete: [a-z ]+, Encryption: [^,]+, Object Lock: [^,]+, Versioning: [^,]+, Lifecycle rules: [^.]+\. /);
  // The usage is in the link: the icons and the usage are one element.
  assert.equal(await link.locator('.storageSwitcher__states').count(), 1);
  assert.equal(await link.locator('.storageSwitcher__facts [data-fact="usage"]').count(), 1);
  // The link is in the trigger, between the name and the chevron, and not in the button of the trigger.
  const geometry = await page.evaluate(() => {
    const box = selector => document.querySelector(selector).getBoundingClientRect();
    const link = box('#storageSwitcherInfo');
    const trigger = box('.storageSwitcher__trigger');
    return { inside: link.left > trigger.left && link.right < box('.storageSwitcher__chevron').left && link.top >= trigger.top && link.bottom <= trigger.bottom,
      afterName: link.left >= box('.storageSwitcher__trigger strong').right, nested: Boolean(document.querySelector('.storageSwitcher__trigger #storageSwitcherInfo')) };
  });
  assert.deepEqual(geometry, { inside: true, afterName: true, nested: false });
  // A click on the icons opens the information page of the bucket, not the menu.
  await link.locator('[data-state-icon="read"]').click();
  await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-main\/\?info$/);
  await page.waitForSelector('[data-column="identity"]');
  // A click on the usage of the trigger, and on the usage of an entry of the menu, opens the information page too, and
  // does not select the bucket.
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  await page.click(`${FACTS} [data-fact="usage"]`);
  await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-main\/\?info$/);
  await open(page, MAIN);
  await page.click('.storageSwitcher__trigger strong');
  const entry = '.storageBucket__row[data-instance="seaweedfs-archive"] .storageBucket__info';
  await page.waitForSelector(`${entry} [data-fact="usage"]`);
  assert.match(await page.getAttribute(entry, 'aria-label'), /^Information and permissions of this bucket: SeaweedFS - archive\. /);
  assert.equal(await page.getAttribute(entry, 'title'), null);
  await page.click(`${entry} [data-fact="usage"]`);
  await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-archive\/\?info$/);
  // The keyboard: Tab goes from the trigger to the link; Enter and Space open the page.
  for (const key of ['Enter', 'Space']) {
    await open(page, MAIN);
    await page.focus('.storageSwitcher__trigger');
    await page.keyboard.press('Tab');
    assert.equal(await page.evaluate(() => document.activeElement.id), 'storageSwitcherInfo');
    await page.keyboard.press(key);
    await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-main\/\?info$/);
  }
  // In the menu: the arrow Right goes from an entry to its link, the arrow Left goes back.
  await open(page, MAIN);
  await page.focus('.storageSwitcher__trigger');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowRight');
  assert.equal(await page.evaluate(() => document.activeElement.classList.contains('storageBucket__info') && document.activeElement.dataset.info), 'seaweedfs-main');
  await page.keyboard.press('ArrowLeft');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.instance), 'seaweedfs-main');
  await page.keyboard.press('Escape');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('selector: the selector of the information page is the selector of the listing; one bucket gives a static trigger on the two pages', async () => {
  // The structure of the selector (tags and classes) and its widths, after the reads of the usage.
  const shape = page => page.evaluate(() => {
    const walk = node => `${node.tagName.toLowerCase()}.${(node.getAttribute('class') || '').split(/\s+/).filter(Boolean).sort().join('.')}(${Array.from(node.children).map(walk).join(',')})`;
    const root = document.querySelector('#storageSwitcher');
    const width = selector => Math.round(root.querySelector(selector)?.getBoundingClientRect().width || 0);
    return { tree: walk(root.querySelector('.storageSwitcher__trigger')) + walk(root.querySelector('#storageSwitcherInfo .storageSwitcher__states')),
      trigger: width('.storageSwitcher__trigger'), states: width('.storageSwitcher__states') };
  });
  const ready = async page => {
    await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`, { state: 'attached' });
    await until(page, () => !document.querySelector('#storageSwitcher').classList.contains('is-loading'));
  };
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await open(page, MAIN);
    await ready(page);
    const listing = await shape(page);
    await page.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
    await ready(page);
    const info = await shape(page);
    // The same markup at each width. The header of the information page also has the link Browse: on a narrow window
    // its selector is narrower; on a wide window the two selectors have their full width of 460 px.
    assert.equal(info.tree, listing.tree, `at ${width}`);
    if (width === 1440) assert.deepEqual([info.trigger, info.states], [listing.trigger, listing.states], `at ${width}`);
    else assert.ok(info.trigger <= listing.trigger, `at ${width}: ${JSON.stringify([info.trigger, listing.trigger])}`);
    // On the information page, an entry of the menu opens the information page of that bucket.
    if (width === 1440) {
      await page.click('.storageSwitcher__trigger strong');
      await page.click('.storageBucket[data-instance="seaweedfs-archive"] strong');
      await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-archive\/\?info$/);
      await page.waitForSelector('[data-column="identity"]');
      await page.click('.storageSwitcher__trigger strong');
      assert.equal(await page.isVisible('#storageSwitcherMenu'), true);
      await page.keyboard.press('Escape');
      assert.equal(await page.isHidden('#storageSwitcherMenu'), true);
    }
  }
  // One visible bucket: the trigger is static on the two pages, with the link of the state icons.
  await page.route(/\/api\/instances(\?|$)/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    body.instances = body.instances.filter(item => item.id === 'seaweedfs-main');
    await route.fulfill({ response, json: body });
  });
  for (const path of [MAIN, `${MAIN}?info`]) {
    await page.goto(BASE + path, { waitUntil: 'networkidle' });
    await page.waitForSelector('.storageSwitcher__trigger.is-static strong');
    assert.equal(await page.locator('.storageSwitcher__chevron, #storageSwitcherMenu').count(), 0, path);
    assert.equal(await page.locator('#storageSwitcherInfo').count(), 1, path);
  }
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('selector: on a phone the state icons stay in the selector, and the page has no horizontal scroll', async () => {
  const page = await newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await open(page, MAIN);
  const box = await page.evaluate(() => {
    const link = document.querySelector('#storageSwitcherInfo').getBoundingClientRect();
    const trigger = document.querySelector('.storageSwitcher__trigger').getBoundingClientRect();
    const scroller = document.querySelector('[data-page-scroll]');
    return { inside: link.left > trigger.left && link.right < trigger.right && link.top >= trigger.top && link.bottom <= trigger.bottom,
      overflow: scroller.scrollWidth - scroller.clientWidth };
  });
  assert.equal(box.inside, true, JSON.stringify(box));
  assert.ok(box.overflow <= 0);
  await page.tap('#storageSwitcherInfo [data-state-icon="read"]');
  await page.waitForURL(/\?info$/);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 2c. The information page of a bucket (?info) and the scroll of the pages
// ------------------------------------------------------------------------------------------------------------------

test('info: one page with no tabs and no head: three columns, the permissions on one line, the link Browse, and no endpoint, token or path', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  await page.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="storage"]');
  assert.equal(await page.title(), 'SeaweedFS - primary - Information - Stowr');
  assert.equal(await page.textContent('.appBrand__name'), 'Stowr');
  // No tabs, no head, no tiles: the page starts with its columns. A hidden heading names the page.
  assert.equal(await page.locator('.infoNav, .infoNav__tab, .infoHead, .infoTiles, .infoTile, #info-summary, #infoStates, #infoLede').count(), 0);
  assert.equal(await page.textContent('#infoTitle'), 'Information of the bucket SeaweedFS - primary');
  assert.equal(await page.evaluate(() => document.getElementById('infoTitle').classList.contains('visually-hidden')), true);
  assert.equal(await page.isHidden('#infoFailure'), true);
  // Three columns at 1440 px: the identity, the permissions and the functions, the storage; one column on a phone.
  const columns = () => page.locator('.infoColumn').evaluateAll(nodes => nodes.map(node => ({ key: node.dataset.column, left: Math.round(node.getBoundingClientRect().left), top: Math.round(node.getBoundingClientRect().top) })));
  const wide = await columns();
  assert.deepEqual(wide.map(item => item.key), ['identity', 'functions', 'storage']);
  assert.ok(wide[0].left < wide[1].left && wide[1].left < wide[2].left && new Set(wide.map(item => item.top)).size === 1, JSON.stringify(wide));
  // The three permissions are one line of chips, with no red: a denied right would be grey.
  const chips = await page.locator('[data-row="permissions"] [data-permission]').evaluateAll(nodes => nodes.map(node => ({ key: node.dataset.permission, top: Math.round(node.getBoundingClientRect().top), tone: [...node.classList].find(name => name.startsWith('infoChip--')) })));
  assert.deepEqual(chips.map(item => item.key), ['read', 'write', 'delete']);
  assert.equal(new Set(chips.map(item => item.top)).size, 1, JSON.stringify(chips));
  assert.ok(chips.every(item => item.tone === 'infoChip--on'), JSON.stringify(chips));
  // The identity: the name, the kind, the host, the bucket and the region; no endpoint, no admin address, no token.
  assert.deepEqual(await page.locator('[data-card="identity"] [data-row]').evaluateAll(nodes => nodes.map(node => node.dataset.row)), ['name', 'kind', 'host', 'bucket', 'region']);
  assert.equal((await page.textContent('[data-card="identity"] [data-row="kind"] .infoKv__main')).trim(), 'SeaweedFS');
  const text = await page.textContent('#main');
  assert.doesNotMatch(text, /https?:\/\/|:8333|:9333|:8888|:3903|token|secret/i);
  // Functions: pairs of a term and a value, and the lifecycle rules as one chip that links to their part.
  assert.equal(await page.locator('[data-card="protection"] .infoPair, [data-card="operations"] .infoPair').count() >= 8, true);
  assert.equal(await page.getAttribute('[data-row="lifecycle"] [data-info-lifecycle]', 'href'), '#info-lifecycle');
  assert.match(await page.textContent('[data-row="lifecycle"] [data-info-lifecycle]'), /^on \d+$/);
  assert.equal(await page.locator('#info-lifecycle .lifecycle').count(), 1);
  // The way back to the files: the link Browse in the header, after the brand.
  const browse = page.locator('#infoBack');
  assert.equal((await browse.textContent()).trim(), 'Browse');
  assert.match(await browse.getAttribute('href'), /\/-\/seaweedfs\/seaweedfs-main\/$/);
  assert.equal(await page.evaluate(() => document.querySelector('#infoBack').closest('.appHeader') !== null
    && document.querySelector('#infoBack').getBoundingClientRect().left >= document.querySelector('.appBrand').getBoundingClientRect().right), true);
  // One column on a phone, with no horizontal scroll.
  await page.setViewportSize({ width: 390, height: 844 });
  const narrow = await columns();
  assert.ok(narrow.every(item => item.left === narrow[0].left) && narrow[0].top < narrow[1].top && narrow[1].top < narrow[2].top, JSON.stringify(narrow));
  assert.ok(await page.evaluate(() => { const scroller = document.querySelector('[data-page-scroll]'); return scroller.scrollWidth - scroller.clientWidth; }) <= 0);
  await browse.click();
  await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-main\/$/);
  await page.waitForSelector('.dataTable, .empty');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('info: a volume has the rows of a folder and no part of the lifecycle rules; a bucket that does not exist gives a 404', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  await page.goto(`${BASE}-/volume/files/?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="storage"]');
  assert.equal((await page.textContent('[data-card="identity"] [data-row="kind"] .infoKv__main')).trim(), 'Mounted folder of the server');
  assert.equal(await page.locator('[data-card="identity"] [data-row="bucket"]').count(), 0);
  assert.equal(await page.locator('#info-lifecycle, [data-row="lifecycle"], [data-card="protection"]').count(), 0);
  assert.equal(await page.locator('[data-card="volume"]').count(), 1);
  assert.doesNotMatch(await page.textContent('#main'), /\/data\/volume/);
  const missing = await page.goto(`${BASE}-/seaweedfs/no-such-bucket/?info`, { waitUntil: 'networkidle' });
  assert.equal(missing.status(), 404);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('scroll: the header never scrolls: only the element under it scrolls, from the header to the right edge, on each page', async () => {
  const page = await newPage({ viewport: { width: 1280, height: 600 } });
  for (const path of [`${MAIN}reports/2026/q3/`, `${MAIN}?info`, `${MAIN}logs/2026-10-18.log`, 'no-such-page']) {
    await page.goto(BASE + path, { waitUntil: 'networkidle' });
    await page.waitForSelector('[data-page-scroll]');
    const box = await page.evaluate(async () => {
      const header = document.querySelector('body > header');
      const scroller = document.querySelector('[data-page-scroll]');
      // Content enough to scroll, at the end of the element that scrolls.
      const filler = document.createElement('div');
      filler.style.height = '3000px';
      scroller.append(filler);
      scroller.scrollTop = 2000;
      await new Promise(resolve => requestAnimationFrame(resolve));
      const rect = scroller.getBoundingClientRect();
      return { headerTop: header.getBoundingClientRect().top, headerBottom: Math.round(header.getBoundingClientRect().bottom), scrollerTop: Math.round(rect.top),
        scrollerRight: Math.round(rect.right), width: window.innerWidth, scrolled: scroller.scrollTop, document: document.scrollingElement.scrollTop,
        overflow: getComputedStyle(document.documentElement).overflowY };
    });
    const where = `${path}: ${JSON.stringify(box)}`;
    assert.equal(box.headerTop, 0, where);
    assert.ok(box.scrolled > 0, where);
    assert.equal(box.document, 0, where);
    assert.equal(box.scrollerTop, box.headerBottom, where);
    assert.equal(box.scrollerRight, box.width, where);
  }
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('selector: the open menu has the left and right edges of the selector, and the trigger covers its top line', async () => {
  const page = await newPage();
  await open(page, MAIN);
  for (const width of [1440, 1280, 1100, 900, 600, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await page.click('.storageSwitcher__trigger strong');
    const box = await page.evaluate(() => {
      const trigger = document.querySelector('.storageSwitcher__trigger');
      const menu = document.querySelector('#storageSwitcherMenu');
      for (const animation of menu.getAnimations()) animation.finish();
      const t = trigger.getBoundingClientRect();
      const m = menu.getBoundingClientRect();
      const style = getComputedStyle(menu);
      const triggerStyle = getComputedStyle(trigger);
      return { left: m.left - t.left, right: m.right - t.right, width: m.width - t.width, top: style.borderTopWidth,
        radius: [style.borderTopLeftRadius, style.borderTopRightRadius], triggerBottom: [triggerStyle.borderBottomLeftRadius, triggerStyle.borderBottomRightRadius],
        touch: m.top - t.bottom, triggerZ: Number(triggerStyle.zIndex), menuZ: Number(style.zIndex) };
    });
    const where = `${width}: ${JSON.stringify(box)}`;
    assert.ok(Math.abs(box.left) <= 1 && Math.abs(box.right) <= 1 && Math.abs(box.width) <= 1, where);
    // The menu has no line at its top and no round corner there: the open trigger is its top, and the two make one shape.
    assert.equal(box.top, '0px', where);
    assert.deepEqual(box.radius, ['0px', '0px'], where);
    assert.deepEqual(box.triggerBottom, ['0px', '0px'], where);
    assert.ok(box.touch <= 0 && box.touch >= -1.5, where);
    assert.ok(box.triggerZ > box.menuZ, where);
    await page.keyboard.press('Escape');
  }
  await page.context().close();
});

test('upload: 300 files, bounded concurrency and rendering, progress, summary', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'bulk/');
  await open(page, `${MAIN}bulk/`);
  const puts = await watchPuts(page, { delay: 60 });
  await page.setInputFiles('#fileInput', files(300, 'bulk'));
  await page.waitForSelector('.uploadDock[data-state="running"]');
  // During the upload: overall figures, at most 4 rows of files in progress.
  await until(page, () => /^\d+ \/ 300$/.test(document.querySelector('[data-stat="files"]')?.textContent || ''));
  let maxRows = 0;
  for (let i = 0; i < 20; i += 1) {
    maxRows = Math.max(maxRows, await page.locator('.uploadDock .fileList:not(.fileList--failed) > li').count());
    await sleep(100);
  }
  assert.ok(maxRows >= 1 && maxRows <= 4, `rows in progress: ${maxRows}`);
  assert.match(await page.textContent('[data-stat="rate"]'), /\/s$|measuring/);
  assert.match(await page.textContent('[data-stat="bytes"]'), /KB \/ [\d.]+ KB/);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 120000 });
  assert.ok(puts.max <= 4, `PUT requests at the same time: ${puts.max}`);
  assert.equal(puts.total, 300);
  assert.match(await page.textContent('.uploadDock .summary'), /^300 files uploaded \([\d.]+ KB\), 300 checked with SHA-256\.$/);
  assert.equal(await page.textContent('[data-stat="files"]'), '300 / 300');
  assert.ok((await page.locator('.uploadDock li').count()) <= 4);
  assert.equal((await listKeys(page, 'seaweedfs-main', 'bulk/')).length, 300);
  // The listing refreshes at the end of the batch.
  await until(page, () => / of 300$/.test(document.querySelector('.tableFoot__range')?.textContent || ''));
  await page.click('.uploadDock .button >> text=Close');
  assert.equal(await page.isHidden('#uploadDock'), true);
  assert.deepEqual(page.errors, []);
  await deletePrefix(page, 'seaweedfs-main', 'bulk/');
  await page.context().close();
});

test('upload: a page without crypto.subtle uploads with no checksum', async () => {
  const page = await newPage();
  await page.addInitScript(() => { Object.defineProperty(window, 'crypto', { value: { getRandomValues: crypto.getRandomValues.bind(crypto), randomUUID: crypto.randomUUID?.bind(crypto) } }); });
  await deletePrefix(page, 'seaweedfs-main', 'nosum/');
  await open(page, `${MAIN}nosum/`);
  await page.setInputFiles('#fileInput', files(3, 'nosum'));
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 60000 });
  assert.match(await page.textContent('.uploadDock .summary'), /^3 files uploaded \([\d.]+ KB\)\.$/);
  assert.equal((await listKeys(page, 'seaweedfs-main', 'nosum/')).length, 3);
  await deletePrefix(page, 'seaweedfs-main', 'nosum/');
  await page.context().close();
});

test('upload: a large file goes in parts, each part carries its SHA-256, and the provider confirms them', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'sum/');
  await open(page, `${MAIN}sum/`);
  const requests = [];
  page.on('request', request => { if (request.method() === 'PUT' && request.url().includes('/api/uploads')) requests.push(request.headers()['x-checksum-sha256'] || ''); });
  await page.setInputFiles('#fileInput', [{ name: 'large.bin', mimeType: 'application/octet-stream', buffer: Buffer.alloc(33 * 1024 * 1024, 7) }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 120000 });
  assert.match(await page.textContent('.uploadDock .summary'), /^1 file uploaded \([\d.]+ MB\), 1 checked with SHA-256\.$/);
  assert.ok(requests.length >= 2, `chunks: ${requests.length}`);
  assert.ok(requests.every(value => /^[A-Za-z0-9+/]{43}=$/.test(value)), `every chunk has a digest: ${requests}`);
  assert.equal((await listKeys(page, 'seaweedfs-main', 'sum/')).length, 1);
  await deletePrefix(page, 'seaweedfs-main', 'sum/');
  await page.context().close();
});

test('upload: failed files with their reasons, bounded list, then "Retry failed"', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'retry/');
  await open(page, `${MAIN}retry/`);
  let refuse = true;
  const puts = await watchPuts(page, {
    fail: url => {
      if (!refuse) return null;
      if (/key=retry%2Fdenied-/.test(url)) return forbidden();
      if (/key=retry%2Fbroken-/.test(url)) return { status: 500, json: { error: { code: 'upstream', message: 'The provider refused the object.' } } };
      return null;
    }
  });
  await page.setInputFiles('#fileInput', [...files(55, 'denied'), ...files(5, 'broken'), ...files(40, 'fine')]);
  await page.waitForSelector('.uploadDock[data-state="failed"]', { timeout: 60000 });
  assert.equal(await page.textContent('.uploadDock__title'), '60 of 100 files failed');
  assert.equal(await page.locator('.fileList--failed > li:not(.fileList__more)').count(), 50);
  assert.equal(await page.textContent('.fileList--failed .fileList__more'), 'and 10 other files');
  assert.equal(await page.textContent('.fileList--failed li:first-child .fileList__reason'), 'Not allowed (403).');
  const reasons = await page.locator('.fileList--failed .fileList__reason').allInnerTexts();
  assert.ok(reasons.every(reason => reason.length > 0));
  assert.match(await page.textContent('.uploadDock .summary'), /^40 files uploaded \([\d.]+ KB\), 40 checked with SHA-256, 60 failed\.$/);
  refuse = false;
  await page.click('.uploadDock .button >> text=Retry failed');
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 60000 });
  assert.equal(await page.isHidden('.fileList--failed'), true);
  assert.equal((await listKeys(page, 'seaweedfs-main', 'retry/')).length, 100);
  assert.equal(puts.total, 160);
  await deletePrefix(page, 'seaweedfs-main', 'retry/');
  await page.context().close();
});

test('upload: cancel stops the queue and the summary counts the canceled files', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'cancel/');
  await open(page, `${MAIN}cancel/`);
  const puts = await watchPuts(page, { delay: 400 });
  await page.setInputFiles('#fileInput', files(200, 'cancel'));
  await until(page, () => Number((document.querySelector('[data-stat="files"]')?.textContent || '0').split(' ')[0]) >= 8);
  await page.click('.uploadDock .button--danger');
  await page.waitForSelector('.uploadDock[data-state="canceled"], .uploadDock[data-state="done"]', { timeout: 30000 });
  const sent = puts.total;
  await sleep(1200);
  assert.equal(puts.total, sent, 'no request after the cancel');
  assert.ok(sent < 200);
  assert.match(await page.textContent('.uploadDock .summary'), /files? uploaded .*, \d+ canceled\.$/);
  assert.equal(await page.textContent('.uploadDock__title'), 'Upload canceled');
  await deletePrefix(page, 'seaweedfs-main', 'cancel/');
  await page.context().close();
});

test('upload: an unreachable server gives one message, the queue waits, then the upload continues', async () => {
  const page = await newPage();
  await deletePrefix(page, 'seaweedfs-main', 'offline/');
  await open(page, `${MAIN}offline/`);
  let down = false;
  let tripped = false;
  const puts = await watchPuts(page, {
    delay: 30,
    fail: (url, stats) => {
      if (!tripped && stats.total > 30) {
        tripped = true;
        down = true;
      }
      return down ? 'abort' : null;
    }
  });
  await page.route(/healthz/, route => (down ? route.abort('connectionrefused') : route.continue()));
  await page.setInputFiles('#fileInput', files(150, 'offline'));
  await page.waitForSelector('.uploadDock[data-state="offline"]', { timeout: 60000 });
  await sleep(2500);
  // One message: the dock owns it; the page shows no second notice and no file is marked as failed.
  assert.equal(await visibleText(page, 'The server does not answer'), 1);
  assert.equal(await page.locator('#notices > *').count(), 0);
  assert.equal(await page.isHidden('.fileList--failed'), true);
  assert.match(await page.textContent('.uploadDock .callout'), /files? wait in the queue and nothing is lost/);
  assert.equal(await page.textContent('[data-stat="rate"]'), 'paused');
  down = false;
  await page.click('.uploadDock .callout .button >> text=Try now');
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 60000 });
  assert.match(await page.textContent('.uploadDock .summary'), /^150 files uploaded/);
  assert.equal((await listKeys(page, 'seaweedfs-main', 'offline/')).length, 150);
  assert.ok(puts.total > 150);
  await deletePrefix(page, 'seaweedfs-main', 'offline/');
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 4. Denied permissions, 401 and 403
// ------------------------------------------------------------------------------------------------------------------

test('selector: one bucket makes the selector static; no bucket gives one message', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    body.instances = body.instances.filter(instance => instance.id === 'seaweedfs-archive');
    body.default = 'seaweedfs-archive';
    await route.fulfill({ response, json: body });
  });
  await open(page, '');
  // One visible bucket: the selector is static (no menu).
  assert.equal(await page.locator('button.storageSwitcher__trigger').count(), 0);
  assert.match(await page.textContent('.storageSwitcher__trigger'), /SeaweedFS - archive/);
  assert.equal(await page.locator('#storageSwitcherMenu').count(), 0);
  assert.equal(await page.getAttribute('.crumb--root', 'aria-label'), 'SeaweedFS - archive, bucket root');
  await page.unroute(/\/api\/instances/);
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({ response, json: { ...body, instances: [], default: '' } });
  });
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('.empty');
  assert.match(await page.textContent('.empty'), /No bucket is available/);
  assert.match(await page.textContent('.storageSwitcher__trigger'), /No bucket/);
  assert.equal(await page.locator('.storageBucket').count(), 0);
  await page.context().close();
});

test('permissions: a bucket that refuses write and delete shows it in the selector, and no upload', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const instance of body.instances) {
      if (instance.id !== 'seaweedfs-archive') continue;
      for (const name of ['write', 'delete']) instance.capabilities[name] = { state: 'denied', allowed: false, verified: true, source: 'configuration', reason: 'The permissions of the block do not give it.' };
      for (const name of ['upload', 'overwrite', 'createFolder', 'delete', 'copy', 'move', 'rename']) instance.operations[name] = { state: 'denied', allowed: false, verified: true, source: 'configuration' };
    }
    await route.fulfill({ response, json: body });
  });
  await open(page, '-/seaweedfs/seaweedfs-archive/');
  // A denied right is a grey icon (no red, no strike), with its word for a screen reader.
  const states = await page.locator('#storageSwitcherInfo [data-state-icon]').evaluateAll(nodes => nodes.slice(0, 3).map(node => `${node.dataset.stateIcon}:${node.dataset.tone}`));
  assert.deepEqual(states, ['read:on', 'write:off', 'delete:off']);
  assert.equal(await page.textContent('#storageSwitcherInfo [data-state-icon="write"] .visually-hidden'), 'Write: denied. ');
  await page.click('.storageSwitcher__trigger strong');
  const main = await page.locator('.storageBucket__row[data-instance="seaweedfs-main"] [data-state-icon]').evaluateAll(nodes => nodes.slice(0, 3).map(node => node.dataset.tone));
  assert.deepEqual(main, ['on', 'on', 'on']);
  // The information page gives the reason in the title of the chip.
  await page.goto(`${BASE}-/seaweedfs/seaweedfs-archive/?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-permission="write"]');
  assert.equal(await page.getAttribute('[data-permission="write"]', 'data-state'), 'denied');
  assert.match(await page.getAttribute('[data-permission="write"]', 'title'), /The permissions of the block in the configuration do not give it\./);
  assert.match(await page.textContent('[data-permission="write"]'), /^Write: Denied$/);
  await page.goto(`${BASE}-/seaweedfs/seaweedfs-archive/`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable, .empty');
  assert.equal(await page.isHidden('#uploadWrap'), true);
  assert.match(await page.textContent('#accessBadge'), /Read only/);
  await page.context().close();
});

test('errors: 403 on a folder gives a clear state and a way back to the root', async () => {
  const page = await newPage();
  await page.route(/\/api\/list\?/, route => (/prefix=reports/.test(route.request().url()) ? route.fulfill(forbidden()) : route.continue()));
  await open(page, `${MAIN}reports/`);
  assert.match(await page.textContent('.empty'), /You cannot open this folder/);
  assert.equal(await page.isHidden('#uploadWrap'), true);
  await page.click('.empty .button >> text=Go to the bucket root');
  await page.waitForSelector('tr[data-name="reports"]');
  await page.context().close();
});

test('errors: 403 on an action shows the refusal of the provider', async () => {
  const page = await newPage();
  await open(page, `${MAIN}reports/2026/q3/`);
  await page.route(/\/s3\?/, route => (route.request().method() === 'DELETE' ? route.fulfill(forbidden()) : route.continue()));
  await page.click('tr[data-name="orders.csv"] .bb-kebab');
  await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('text=The storage provider refused the request.', { timeout: 10000 });
  assert.ok((await listKeys(page, 'seaweedfs-main', 'reports/2026/q3/')).includes('reports/2026/q3/orders.csv'));
  await page.context().close();
});

test('session: a 401 of the reverse proxy gives one notice that asks for a new sign-in', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.route(/\/api\//, route => route.fulfill({ status: 401, json: { error: { code: 'unauthenticated', message: 'Sign in again.' } } }));
  await page.click('tr[data-name="reports"] .nameLink');
  await page.waitForSelector('#notices .callout--danger');
  assert.equal(await page.locator('#notices > .callout').count(), 1);
  assert.match(await page.textContent('#notices'), /Your sign-in has ended\. Reload the page to sign in again/);
  assert.equal(await page.isVisible('#notices .button >> text=Reload'), true);
  assert.equal(await page.isHidden('#uploadWrap'), true);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 5. Providers: the same smoke test on each provider of providers.js
// ------------------------------------------------------------------------------------------------------------------

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`providers: ${provider.name} lists, uploads, downloads and deletes; versions and tags show only where it has them`, async () => {
    const page = await newPage();
    // The resumable download asks for a file with showSaveFilePicker, and a headless browser cannot answer. Without
    // the picker, the page uses the plain download of the browser.
    await page.addInitScript(() => Object.defineProperty(window, 'showSaveFilePicker', { value: undefined }));
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    assert.equal(info.versioningSupported, provider.versioning);
    await open(page, `-/${info.host}/${instance}/smoke/`);
    assert.deepEqual((await page.locator('tbody tr[data-name]').evaluateAll(rows => rows.map(row => row.dataset.name))).sort(), ['data.json', 'hello.txt', 'versioned.txt']);
    // Versions: the badge of the count where the provider keeps versions, else no column and no setting.
    if (provider.versioning) {
      await page.waitForSelector('tr[data-name="versioned.txt"] .versionCount');
      assert.match(await page.textContent('tr[data-name="versioned.txt"] .versionCount'), /2/);
    } else {
      assert.equal(await page.locator('th.col-versions').isVisible(), false);
      assert.equal(await page.locator('.versionCount').count(), 0);
    }
    // Tags: the chips in the details where the provider has tags, else no section, and the settings hide the column.
    await openDetails(page, 'hello.txt');
    if (provider.tags) {
      await page.waitForSelector('[data-tags-section] .tagChip');
      assert.deepEqual((await page.locator('[data-tags-section] .tagChip').allInnerTexts()).map(item => item.replace(/\s+/g, ' ').trim()).sort(), ['env test', 'team qa']);
    } else {
      await sleep(500);
      assert.equal(await page.locator('[data-tags-section]').count(), 0);
    }
    await page.click('.bb-modal-x');
    await page.waitForSelector('.bb-overlay', { state: 'detached' });
    await page.click('#settingsButton');
    assert.equal(await page.locator('#versionsSetting').isHidden(), !provider.versioning);
    if (!provider.tags) {
      // The server probed the tag requests at start: the controls are hidden from the first load, with no tag read.
      assert.equal(await page.locator('[data-column="tags"]').isHidden(), true);
      assert.equal(await page.locator('#uploadTagsSetting').isHidden(), true);
      assert.equal(await page.locator('th.col-tags').isVisible(), false);
    }
    await page.keyboard.press('Escape');
    // Upload, download and delete of one file.
    const body = `smoke upload on ${provider.name}\n`;
    await page.setInputFiles('#fileInput', [{ name: 'upload.txt', mimeType: 'text/plain', buffer: Buffer.from(body) }]);
    await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 30000 });
    // The three providers check the SHA-256 checksum of the upload and give it back.
    assert.match(await page.textContent('.uploadDock .summary'), /^1 file uploaded \([\d.]+ B\), 1 checked with SHA-256\.$/);
    await page.click('.uploadDock .button >> text=Close');
    await page.waitForSelector('tr[data-name="upload.txt"]');
    await page.click('tr[data-name="upload.txt"] .bb-kebab');
    const [download] = await Promise.all([page.waitForEvent('download'), page.click('.bb-menu.is-open .bb-menu-item >> text=Download')]);
    const chunks = [];
    for await (const chunk of await download.createReadStream()) chunks.push(chunk);
    assert.equal(Buffer.concat(chunks).toString(), body);
    await page.click('tr[data-name="upload.txt"] .bb-kebab');
    await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
    await page.click('.bb-overlay .bb-btn-primary');
    await page.waitForSelector('tr[data-name="upload.txt"]', { state: 'detached' });
    assert.ok(!(await listKeys(page, instance, 'smoke/')).includes('smoke/upload.txt'));
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

// ------------------------------------------------------------------------------------------------------------------
// 6. Copy in parts (UploadPartCopy): the test image copies in parts from 8 KiB up, in parts of 5 MiB
// ------------------------------------------------------------------------------------------------------------------

const sha256 = buffer => require('crypto').createHash('sha256').update(buffer).digest('hex');

async function getObject(page, instance, key) {
  const response = await page.request.get(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
  assert.equal(response.status(), 200, `${key}: ${response.status()}`);
  return { body: await response.body(), headers: response.headers() };
}

// Opens the menu of a row, chooses an action, and gives a new name in the prompt.
async function rowActionWithName(page, name, action, value) {
  await page.click(`tr[data-name="${name}"] .bb-kebab`);
  await page.click(`.bb-menu.is-open .bb-menu-item >> text=${action}`);
  await page.fill('.bb-overlay .bb-input', value);
  await page.click('.bb-overlay .bb-btn-primary');
}

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`copy in parts: ${provider.name} copies and renames a large file as a job, with its bytes, headers, metadata and tags`, async () => {
    const page = await newPage();
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    const source = await getObject(page, instance, 'copy/source.bin');
    assert.equal(source.body.length, 11 * 1024 * 1024 + 123);
    const jobs = [];
    page.on('response', async response => {
      if (/\/api\/(copy|rename|jobs\/)/.test(response.url())) jobs.push(await response.json().catch(() => ({})));
    });
    await open(page, `-/${info.host}/${instance}/copy/`);
    // The copy: the server answers with a job, and the job counts the copied bytes.
    await rowActionWithName(page, 'source.bin', 'Copy', 'copied.bin');
    await page.waitForSelector('tr[data-name="copied.bin"]', { timeout: 60000 });
    const copyJob = jobs.find(job => job.type === 'copy_object' && job.status === 'completed');
    assert.ok(copyJob, JSON.stringify(jobs));
    assert.equal(copyJob.totalBytes, source.body.length);
    assert.equal(copyJob.copiedBytes, source.body.length);
    const copied = await getObject(page, instance, 'copy/copied.bin');
    assert.equal(sha256(copied.body), sha256(source.body));
    // The copy is a multipart object of three parts.
    assert.match(copied.headers.etag, /-3"?$/);
    for (const name of ['content-type', 'cache-control', 'content-disposition', 'x-amz-meta-origin']) {
      assert.equal(copied.headers[name], source.headers[name], name);
    }
    assert.equal(copied.headers['content-type'], 'application/x-copy-test');
    assert.equal(copied.headers['x-amz-meta-origin'], 'seed');
    if (provider.tags) assert.deepEqual(await apiTags(page, 'copy/copied.bin', instance), ['team=copy']);
    // The rename: a move job, the source goes away.
    await rowActionWithName(page, 'copied.bin', 'Rename', 'moved.bin');
    await page.waitForSelector('tr[data-name="moved.bin"]', { timeout: 60000 });
    await page.waitForSelector('tr[data-name="copied.bin"]', { state: 'detached' });
    assert.ok(jobs.some(job => job.type === 'move_object' && job.status === 'completed'));
    const moved = await getObject(page, instance, 'copy/moved.bin');
    assert.equal(sha256(moved.body), sha256(source.body));
    // The gateway adds "private, no-store" to Cache-Control: the value of the provider follows it.
    assert.equal(moved.headers['cache-control'], source.headers['cache-control']);
    assert.match(moved.headers['cache-control'], /max-age=600$/);
    if (provider.tags) assert.deepEqual(await apiTags(page, 'copy/moved.bin', instance), ['team=copy']);
    assert.ok(!(await listKeys(page, instance, 'copy/')).includes('copy/copied.bin'));
    await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent('copy/moved.bin')}`);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

for (const [instance, provider] of Object.entries(PROVIDERS).filter(([, item]) => item.versioning)) {
  test(`copy in parts: ${provider.name} restores an older version above the threshold from that version`, async () => {
    const page = await newPage();
    const key = 'copy/versioned.bin';
    const list = await (await page.request.get(`${BASE}api/versions?instance=${instance}&key=${key}`)).json();
    const older = list.versions.find(version => !version.isCurrent && !version.deleteMarker);
    assert.ok(older, JSON.stringify(list));
    const response = await page.request.post(`${BASE}api/versions/restore`, { data: { instance, key, version: older.version } });
    assert.equal(response.status(), 200, await response.text());
    const restored = await getObject(page, instance, key);
    assert.equal(sha256(restored.body), sha256(Buffer.alloc(20000, 'a')));
    assert.match(restored.headers.etag, /-1"?$/);
    await page.context().close();
  });
}

// ------------------------------------------------------------------------------------------------------------------
// 7. Metadata: the edit in the details. A copy onto the key (CopyObject with x-amz-metadata-directive: REPLACE), or a
// copy in parts from 8 KiB up. Each test checks what the page says before the save against the result.
// ------------------------------------------------------------------------------------------------------------------

async function apiMetadata(page, instance, key) {
  const response = await page.request.get(`${BASE}api/metadata?instance=${instance}&key=${encodeURIComponent(key)}`);
  assert.equal(response.status(), 200, await response.text());
  return response.json();
}

async function versionCount(page, instance, key) {
  const list = await (await page.request.get(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}`)).json();
  return (list.versions || []).filter(version => !version.deleteMarker).length;
}

// Opens the details of a file and the form of its metadata.
async function openMetadataForm(page, name) {
  await openDetails(page, name);
  await page.waitForSelector('[data-metadata-section] .metaEditor__view');
  assert.equal(await page.isHidden('[data-metadata-section] .metaEditor__form'), true);
  await page.click('[data-metadata-edit]');
  await page.waitForSelector('[data-metadata-section] .metaEditor__form', { state: 'visible' });
}

const metadataNotes = async page => cleanText(await page.locator('[data-metadata-section] .metaEditor__notes li').allInnerTexts());

// Saves the form. The details then open again on the current version: the function waits for the values of the new
// window.
async function saveMetadata(page, text) {
  await page.evaluate(() => { document.querySelector('[data-metadata-section]').dataset.before = 'save'; });
  await page.click('[data-metadata-save]');
  await until(page, value => {
    const sections = Array.from(document.querySelectorAll('[data-metadata-section]'));
    return sections.length === 1 && !sections[0].dataset.before && !!sections[0].querySelector('.metaEditor__view:not([hidden])')?.textContent.includes(value);
  }, text);
}

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`metadata: ${provider.name} edits the type and a user metadata; the date changes, the ETag stays, a new version comes where versioning is on`, async () => {
    const page = await newPage();
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    assert.equal(info.metadataEditSupported, true);
    const key = 'meta/small.txt';
    const before = await apiMetadata(page, instance, key);
    assert.equal(before.copyInParts, false);
    assert.equal(before.multipartETag, false);
    // The gateway puts its own Cache-Control in front of the stored value. The form has the stored value.
    const head = await page.request.head(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
    assert.equal(head.headers()['cache-control'], 'private, no-store, max-age=60');
    const versionsBefore = provider.versioning ? await versionCount(page, instance, key) : 0;
    await open(page, `-/${info.host}/${instance}/meta/`);
    await openMetadataForm(page, 'small.txt');
    assert.equal(await page.inputValue('[data-metadata-header="cacheControl"]'), 'max-age=60');
    assert.equal(await page.inputValue('[data-metadata-header="contentType"]'), 'text/plain');
    assert.equal(await page.inputValue('[data-metadata-value="owner"]'), 'ada');
    // Before the save the page says what changes: the date, and a new version where versioning is on. The ETag of an
    // object in one part stays, so the page says nothing about it.
    const notes = await metadataNotes(page);
    assert.ok(notes.some(note => /modification date changes/.test(note)), notes.join(' | '));
    assert.equal(notes.some(note => /new version/.test(note)), provider.versioning, notes.join(' | '));
    assert.ok(!notes.some(note => /ETag/.test(note)), notes.join(' | '));
    assert.equal(await page.isDisabled('[data-metadata-save]'), true);
    await page.fill('[data-metadata-header="contentType"]', 'application/json');
    // S3 keeps the keys in lowercase: the field does the same.
    await page.fill('[data-metadata-section] input[aria-label="Metadata key"]', 'Reviewer');
    await page.fill('[data-metadata-section] input[aria-label="Metadata value"]', 'grace');
    await page.click('[data-metadata-add]');
    await page.waitForSelector('[data-metadata-value="reviewer"]');
    assert.match(await page.textContent('.metaEditor__count'), /^21 of 2,048 bytes$/);
    await saveMetadata(page, 'grace');
    const after = await apiMetadata(page, instance, key);
    assert.equal(after.headers.contentType, 'application/json');
    // The value of the provider, not the value of the gateway: the edit sent the stored value back.
    assert.equal(after.headers.cacheControl, 'max-age=60');
    assert.deepEqual(after.metadata, [{ key: 'owner', value: 'ada' }, { key: 'reviewer', value: 'grace' }]);
    assert.notEqual(after.lastModified, before.lastModified);
    assert.equal(after.etag, before.etag);
    if (provider.versioning) {
      assert.equal(await versionCount(page, instance, key), versionsBefore + 1);
      assert.notEqual(after.version, before.version);
      await page.waitForSelector('.bb-details--file .versionPicker__button');
      assert.equal(await page.locator('.bb-details--file .versionPicker__button').count(), 1);
    }
    if (provider.tags) assert.deepEqual(await apiTags(page, key, instance), ['team=meta']);
    assert.equal((await getObject(page, instance, key)).body.toString(), 'small metadata file\n');
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });

  test(`metadata: ${provider.name} copies a file above the threshold in parts with the new metadata, and its ETag changes`, async () => {
    const page = await newPage();
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    const key = 'meta/large.bin';
    const before = await apiMetadata(page, instance, key);
    assert.equal(before.copyInParts, true);
    const versionsBefore = provider.versioning ? await versionCount(page, instance, key) : 0;
    const sum = sha256((await getObject(page, instance, key)).body);
    await open(page, `-/${info.host}/${instance}/meta/`);
    await openMetadataForm(page, 'large.bin');
    assert.ok((await metadataNotes(page)).some(note => /The ETag changes: Stowr copies an object of this size in parts\./.test(note)));
    await page.click('[data-metadata-remove="owner"]');
    await page.fill('[data-metadata-header="cacheControl"]', 'no-cache');
    await page.fill('[data-metadata-section] input[aria-label="Metadata key"]', 'stage');
    await page.fill('[data-metadata-section] input[aria-label="Metadata value"]', 'done');
    await page.press('[data-metadata-section] input[aria-label="Metadata value"]', 'Enter');
    await saveMetadata(page, 'done');
    const after = await apiMetadata(page, instance, key);
    assert.deepEqual(after.headers, { contentType: 'application/octet-stream', cacheControl: 'no-cache' });
    assert.deepEqual(after.metadata, [{ key: 'stage', value: 'done' }]);
    assert.notEqual(after.etag, before.etag);
    assert.match(after.etag, /-1$/);
    assert.notEqual(after.lastModified, before.lastModified);
    assert.equal(sha256((await getObject(page, instance, key)).body), sum);
    if (provider.versioning) assert.equal(await versionCount(page, instance, key), versionsBefore + 1);
    if (provider.tags) assert.deepEqual(await apiTags(page, key, instance), ['team=meta']);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

for (const [instance, provider] of Object.entries(PROVIDERS).filter(([, item]) => item.versioning)) {
  test(`metadata: ${provider.name} saves the metadata of an older version as the new current version`, async () => {
    const page = await newPage();
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    const key = 'meta/versioned.txt';
    const versionsBefore = await versionCount(page, instance, key);
    await open(page, `-/${info.host}/${instance}/meta/`);
    await openDetails(page, 'versioned.txt');
    await page.waitForSelector('[data-metadata-section] .metaEditor__view');
    await page.click('.versionPicker__button');
    await page.click('.versionPicker__menu:not([hidden]) .versionPicker__item >> nth=1');
    await until(page, () => document.querySelector('[data-metadata-section] .metaEditor__view')?.textContent.includes('text/plain'));
    await page.click('[data-metadata-edit]');
    assert.equal(await page.inputValue('[data-metadata-value="rev"]'), '1');
    assert.ok((await metadataNotes(page)).some(note => /This version becomes the current version, as a restore does\./.test(note)));
    await page.fill('[data-metadata-value="rev"]', '1-restored');
    await saveMetadata(page, '1-restored');
    const after = await apiMetadata(page, instance, key);
    assert.deepEqual(after.metadata, [{ key: 'rev', value: '1-restored' }]);
    assert.equal(after.headers.contentType, 'text/plain');
    assert.equal((await getObject(page, instance, key)).body.toString(), 'versioned metadata v1\n');
    assert.equal(await versionCount(page, instance, key), versionsBefore + 1);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('metadata: an object that changes while the form is open is not overwritten; the form reads the new values', async () => {
  const page = await newPage();
  const key = 'meta/conflict.txt';
  await open(page, `${MAIN}meta/`);
  await openMetadataForm(page, 'conflict.txt');
  await page.fill('[data-metadata-value="owner"]', 'from-the-form');
  // Another person changes the metadata: the ETag stays, the date changes.
  const read = await apiMetadata(page, 'seaweedfs-main', key);
  const other = await page.request.put(`${BASE}api/metadata`, { data: { instance: 'seaweedfs-main', key, etag: read.etag, lastModified: read.lastModified, headers: read.headers, metadata: [{ key: 'owner', value: 'other-person' }] } });
  assert.equal(other.status(), 200, await other.text());
  await page.click('[data-metadata-save]');
  await until(page, () => /The object changed after the form read it/.test(document.querySelector('.metaEditor__message')?.textContent || ''));
  assert.equal(await page.inputValue('[data-metadata-value="owner"]'), 'other-person');
  assert.equal(await page.isDisabled('[data-metadata-save]'), true);
  assert.deepEqual((await apiMetadata(page, 'seaweedfs-main', key)).metadata, [{ key: 'owner', value: 'other-person' }]);
  // The values are checked in the page before the save, and the server checks them again.
  await page.fill('[data-metadata-header="expires"]', 'tomorrow');
  await page.click('[data-metadata-save]');
  assert.match(await page.textContent('.metaEditor__message'), /Expires is an HTTP date/);
  const refused = await page.request.put(`${BASE}api/metadata`, { data: { instance: 'seaweedfs-main', key, etag: read.etag, headers: { expires: 'tomorrow' }, metadata: [] } });
  assert.equal(refused.status(), 400);
  assert.equal((await refused.json()).error.code, 'invalid_metadata');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('metadata: without the edit, the details keep the custom metadata and show no Edit button', async () => {
  const page = await newPage();
  // An instance that the server marked without the edit (a provider that refused it, or GCS).
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const item of body.instances) item.metadataEditSupported = false;
    await route.fulfill({ response, json: body });
  });
  await open(page, `${MAIN}meta/`);
  await openDetails(page, 'small.txt');
  await page.waitForSelector('[data-metadata-fallback]:not([hidden])');
  assert.equal(await page.locator('[data-metadata-section]').count(), 0);
  assert.equal(await page.locator('[data-metadata-edit]').count(), 0);
  await page.context().close();
  // A provider that answers 501 to the read of the metadata: the section goes and the custom metadata stays.
  const second = await newPage();
  await second.route(/\/api\/metadata/, route => route.fulfill({ status: 501, contentType: 'application/json', body: JSON.stringify({ error: { code: 'metadata_unsupported', message: 'no edit' } }) }));
  await open(second, `${MAIN}meta/`);
  await openDetails(second, 'small.txt');
  await second.waitForSelector('[data-metadata-fallback]:not([hidden])');
  assert.equal(await second.locator('[data-metadata-section]').count(), 0);
  assert.match(await second.textContent('[data-metadata-fallback]'), /owner/);
  await second.context().close();
});

test('metadata: the fields suggest values in a popover; the mouse, the keyboard and free text work', async () => {
  const page = await newPage();
  const instance = 'seaweedfs-main';
  const name = `suggest-${Date.now()}.txt`;
  const key = `meta/${name}`;
  await putWithServer(page, instance, key, 'suggest\n', { 'Content-Type': 'application/octet-stream' });
  await open(page, `${MAIN}meta/`);
  await openMetadataForm(page, name);
  // No native list: the page has the popover only.
  assert.equal(await page.evaluate(() => document.querySelector('datalist')), null);
  const menu = '.suggest__menu';
  const header = field => `[data-metadata-header="${field}"]`;
  assert.equal(await page.getAttribute(header('contentType'), 'role'), 'combobox');
  assert.equal(await page.getAttribute(header('contentType'), 'aria-expanded'), 'false');

  // The focus opens the popover. The guessed type of the name is the first row, with a mark.
  await page.focus(header('contentType'));
  await page.waitForSelector(menu);
  assert.equal(await page.getAttribute(header('contentType'), 'aria-expanded'), 'true');
  assert.equal(await page.getAttribute(header('contentType'), 'aria-controls'), await page.getAttribute(menu, 'id'));
  assert.equal(await page.getAttribute(menu, 'role'), 'listbox');
  assert.equal(await page.getAttribute(`${menu} [role="option"]:first-child`, 'data-value'), 'text/plain');
  assert.equal((await page.textContent(`${menu} [role="option"]:first-child .suggest__tag`)).trim(), 'Guess');
  assert.ok(await page.locator(`${menu} [role="option"]`).count() > 5);
  // The popover is in the large window, so that the scroll area does not clip it. It has the look of the other menus.
  assert.equal(await page.evaluate(selector => document.querySelector(selector).parentElement.classList.contains('bb-overlay'), menu), true);
  const looks = await page.evaluate(selector => {
    const pick = node => { const style = getComputedStyle(node); return { radius: style.borderTopLeftRadius, shadow: style.boxShadow, background: style.backgroundColor, border: style.borderTopColor }; };
    const probes = ['versionPicker__menu', 'bb-menu-popover'].map(className => {
      const probe = document.createElement('div');
      probe.className = className;
      document.body.append(probe);
      const result = pick(probe);
      probe.remove();
      return result;
    });
    return { popover: pick(document.querySelector(selector)), picker: probes[0], menu: probes[1] };
  }, menu);
  assert.equal(looks.popover.radius, looks.picker.radius);
  assert.equal(looks.popover.radius, looks.menu.radius);
  assert.equal(looks.popover.shadow, looks.picker.shadow);
  assert.equal(looks.popover.shadow, looks.menu.shadow);
  assert.equal(looks.popover.background, looks.picker.background);
  assert.equal(looks.popover.border, looks.picker.border);
  assert.notEqual(looks.popover.shadow, 'none');

  // The mouse chooses a row. The popover closes and the focus stays in the field.
  await page.click(`${menu} [role="option"][data-value="application/json"]`);
  assert.equal(await page.inputValue(header('contentType')), 'application/json');
  assert.equal(await page.locator(menu).count(), 0);
  assert.equal(await page.getAttribute(header('contentType'), 'aria-expanded'), 'false');
  assert.equal(await page.isDisabled('[data-metadata-save]'), false);

  // The keyboard: Down moves the highlight, Enter chooses it.
  await page.focus(header('cacheControl'));
  await page.waitForSelector(menu);
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.getAttribute(`${menu} [role="option"][aria-selected="true"]`, 'data-value'), 'no-cache');
  assert.equal(await page.getAttribute(header('cacheControl'), 'aria-activedescendant'), await page.getAttribute(`${menu} [role="option"][aria-selected="true"]`, 'id'));
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('Enter');
  assert.equal(await page.inputValue(header('cacheControl')), 'no-cache');
  assert.equal(await page.locator(menu).count(), 0);
  // Down opens a closed popover. Tab leaves the field and closes it; the next field opens its own popover.
  await page.keyboard.press('ArrowDown');
  await page.waitForSelector(menu);
  await page.keyboard.press('Tab');
  assert.equal(await page.getAttribute(header('cacheControl'), 'aria-expanded'), 'false');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.metadataHeader), 'contentDisposition');
  assert.equal(await page.locator(menu).count(), 1);

  // Escape closes the popover only: the large window stays open.
  await page.focus(header('contentEncoding'));
  await page.waitForSelector(menu);
  await page.keyboard.press('Escape');
  assert.equal(await page.locator(menu).count(), 0);
  assert.equal(await page.isVisible('.bb-details--file'), true);
  assert.equal(await page.locator('.bb-overlay--large').count(), 1);

  // A free text stays in the field. The popover shows the values that contain the text, and hides when none does.
  await page.fill(header('contentEncoding'), 'g');
  await page.waitForSelector(menu);
  assert.deepEqual(await page.locator(`${menu} [role="option"]`).evaluateAll(rows => rows.map(row => row.dataset.value)), ['gzip']);
  await page.fill(header('contentLanguage'), 'sv-SE');
  assert.equal(await page.locator(menu).count(), 0);
  assert.equal(await page.inputValue(header('contentLanguage')), 'sv-SE');
  assert.equal(await page.inputValue(header('contentEncoding')), 'g');
  await page.fill(header('contentEncoding'), '');
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('.bb-overlay--large').count(), 1);

  await saveMetadata(page, 'sv-SE');
  const after = await apiMetadata(page, instance, key);
  assert.equal(after.headers.contentType, 'application/json');
  assert.equal(after.headers.cacheControl, 'no-cache');
  assert.equal(after.headers.contentLanguage, 'sv-SE');
  assert.equal(after.headers.contentEncoding, undefined);
  assert.equal(await page.evaluate(() => document.querySelector('datalist')), null);
  assert.deepEqual(page.errors, []);
  await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 8. Incomplete uploads: the list of the incomplete multipart uploads of a folder, and the abort. seed.js puts four
// uploads in mpu/ on each provider: one-part.bin (one part of 1 KiB), no-part.bin, and twice.bin two times.
// ------------------------------------------------------------------------------------------------------------------

async function apiMultiparts(page, instance, prefix) {
  const response = await page.request.get(`${BASE}api/multiparts?instance=${instance}&prefix=${encodeURIComponent(prefix)}`);
  assert.equal(response.status(), 200, await response.text());
  return response.json();
}

// Opens the window of the incomplete uploads from the Actions menu, and waits for the table or the empty state.
async function openMultiparts(page) {
  await page.click('#actionsButton');
  await page.click('#multipartsItem');
  await until(page, () => Array.from(document.querySelectorAll('[data-multiparts] .multiparts__table, [data-multiparts] .multiparts__empty')).some(node => !node.hidden), null, 15000);
}

const multipartRows = page => page.locator('[data-multiparts] tr[data-upload-id]').evaluateAll(rows => rows.map(row => row.dataset.key).sort());
// The confirmation opens over the large window: its button is in the small window.
const confirmAbort = page => page.click('.bb-overlay:not(.bb-overlay--large) .bb-btn-primary');

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`incomplete uploads: ${provider.name} lists the uploads of a folder and aborts one of a key with two uploads, then one with a part`, async () => {
    const page = await newPage();
    const info = (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);
    assert.equal(info.multipartUploadsSupported, true);
    const before = await apiMultiparts(page, instance, 'mpu/');
    assert.deepEqual(before.uploads.map(upload => upload.key).sort(), ['mpu/no-part.bin', 'mpu/one-part.bin', 'mpu/twice.bin', 'mpu/twice.bin']);
    const onePart = before.uploads.find(upload => upload.key === 'mpu/one-part.bin');
    assert.equal(onePart.parts, 1);
    assert.equal(onePart.bytes, 1024);
    // SeaweedFS gives no start date. MinIO and Garage give one.
    assert.equal(before.uploads.every(upload => Boolean(upload.initiated)), provider.name !== 'SeaweedFS', JSON.stringify(before.uploads));
    // The incomplete uploads make no object: the folder lists its one file only.
    assert.deepEqual(await listKeys(page, instance, 'mpu/'), ['mpu/readme.txt']);
    await open(page, `-/${info.host}/${instance}/mpu/`);
    await openMultiparts(page);
    assert.deepEqual(await multipartRows(page), ['mpu/no-part.bin', 'mpu/one-part.bin', 'mpu/twice.bin', 'mpu/twice.bin']);
    const row = `[data-multiparts] tr[data-upload-id="${onePart.uploadId}"]`;
    assert.equal((await page.textContent(`${row} .multiparts__parts`)).trim(), '1');
    if (provider.name === 'SeaweedFS') {
      assert.equal((await page.textContent(`${row} .multiparts__age`)).trim(), 'unknown');
      assert.match(await page.textContent('[data-multipart-undated-note]'), /gives no start date for 4 uploads/);
    } else {
      assert.notEqual((await page.textContent(`${row} .multiparts__age`)).trim(), 'unknown');
      assert.equal(await page.isHidden('[data-multipart-undated-note]'), true);
    }
    assert.match(await page.textContent('[data-multiparts] .multiparts__note'), /lifecycle rule/);
    // Abort everything older than 1 day: the uploads are a few seconds old (or without a date), so all stay.
    assert.equal(await page.inputValue('[data-multipart-older-days]'), '1');
    await page.click('[data-multipart-abort-older]');
    await confirmAbort(page);
    await until(page, () => document.querySelector('[data-multipart-message]')?.dataset.tone === 'ok');
    const olderMessage = await page.textContent('[data-multipart-message]');
    if (provider.name === 'SeaweedFS') assert.match(olderMessage, /4 uploads without a start date stayed/);
    else assert.match(olderMessage, /No upload was older than 1 day/);
    assert.equal((await apiMultiparts(page, instance, 'mpu/')).uploads.length, 4);
    // Abort selected: one of the two uploads of twice.bin. The other upload of the key stays.
    const [first, second] = before.uploads.filter(upload => upload.key === 'mpu/twice.bin');
    await page.check(`[data-multiparts] tr[data-upload-id="${first.uploadId}"] [data-multipart-select]`);
    await page.click('[data-multipart-abort-selected]');
    await confirmAbort(page);
    await page.waitForSelector(`[data-multiparts] tr[data-upload-id="${first.uploadId}"]`, { state: 'detached' });
    await page.waitForSelector(`[data-multiparts] tr[data-upload-id="${second.uploadId}"]`);
    assert.match(await page.textContent('[data-multipart-message]'), /1 upload aborted/);
    // The abort button of a row: the upload with a part.
    await page.click(`${row} [data-multipart-abort-one]`);
    await confirmAbort(page);
    await page.waitForSelector(row, { state: 'detached' });
    assert.deepEqual((await apiMultiparts(page, instance, 'mpu/')).uploads.map(upload => upload.uploadId).sort(),
      [second.uploadId, before.uploads.find(upload => upload.key === 'mpu/no-part.bin').uploadId].sort());
    // The last two uploads go through the API; the window then shows its empty state.
    const rest = (await apiMultiparts(page, instance, 'mpu/')).uploads.map(({ key, uploadId }) => ({ key, uploadId }));
    const aborted = await page.request.post(`${BASE}api/multiparts/abort`, { data: { instance, uploads: rest } });
    assert.equal(aborted.status(), 200, await aborted.text());
    assert.equal((await aborted.json()).aborted.length, 2);
    await page.click('.bb-modal--large .bb-modal-x');
    await page.waitForSelector('.bb-overlay', { state: 'detached' });
    await openMultiparts(page);
    assert.equal((await page.textContent('[data-multiparts] .multiparts__empty')).trim(), 'No incomplete upload');
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('incomplete uploads: an upload that this Stowr process sends now cannot be aborted', async () => {
  const page = await newPage();
  // A resumable upload of the page: the server creates the multipart upload and keeps the session.
  const created = await page.request.post(`${BASE}api/uploads`, { data: { instance: 'seaweedfs-main', key: 'mpu/active.bin', size: 6 * 1024 * 1024, contentType: 'application/octet-stream' } });
  assert.equal(created.status(), 201, await created.text());
  const session = await created.json();
  try {
    const listed = (await apiMultiparts(page, 'seaweedfs-main', 'mpu/')).uploads.find(upload => upload.key === 'mpu/active.bin');
    assert.equal(listed.active, true);
    await open(page, `${MAIN}mpu/`);
    await openMultiparts(page);
    const row = `[data-multiparts] tr[data-upload-id="${listed.uploadId}"]`;
    assert.match(await page.textContent(row), /Uploading now/);
    assert.equal(await page.locator(`${row} [data-multipart-select]`).count(), 0);
    assert.equal(await page.locator(`${row} [data-multipart-abort-one]`).count(), 0);
    // The server refuses it too.
    const refused = await (await page.request.post(`${BASE}api/multiparts/abort`, { data: { instance: 'seaweedfs-main', uploads: [{ key: 'mpu/active.bin', uploadId: listed.uploadId }] } })).json();
    assert.equal(refused.aborted.length, 0);
    assert.equal(refused.skipped.length, 1);
  } finally {
    await page.request.delete(`${BASE}api/uploads`, { headers: { 'X-Stowr-Resume-Token': session.resumeToken } });
  }
  assert.equal((await apiMultiparts(page, 'seaweedfs-main', 'mpu/')).uploads.some(upload => upload.key === 'mpu/active.bin'), false);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('incomplete uploads: the item is hidden for an instance without the list', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const item of body.instances) item.multipartUploadsSupported = false;
    await route.fulfill({ response, json: body });
  });
  await open(page, `${MAIN}mpu/`);
  await page.click('#actionsButton');
  assert.equal(await page.isHidden('#multipartsItem'), true);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 9. Lifecycle rules: the editor of the part "Lifecycle rules" of the information page (?info#info-lifecycle). It has
// one form at the left and the rules at the right, and no window. Each test writes the rules of the bucket through the
// S3 API of the provider first, and restores the rules of the seed at the end: the tests of the tags read the rule of
// SeaweedFS.
// ------------------------------------------------------------------------------------------------------------------

// A rule with an expiration on a date: the editor keeps it as is, and the three providers keep it.
const KEPT_RULE = '<Rule><ID>keep-date</ID><Filter><Prefix>archive/</Prefix></Filter><Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule>';
// The fields that each provider keeps (test/README.md). The buckets of SeaweedFS and MinIO have versioning.
const LIFECYCLE_FIELDS = {
  'seaweedfs-main': { tagFilter: true, noncurrentExpiration: true, abortMultipart: true, expiredDeleteMarker: true },
  'minio-main': { tagFilter: true, noncurrentExpiration: true, abortMultipart: false, expiredDeleteMarker: true },
  'garage-main': { tagFilter: false, noncurrentExpiration: false, abortMultipart: true, expiredDeleteMarker: false },
};

const infoOf = async (page, instance) => (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);

// Opens the part "Lifecycle rules" of the information page of an instance, and waits for the rules.
async function openLifecycle(page, instance = 'seaweedfs-main', query = '') {
  const info = await infoOf(page, instance);
  const target = `${BASE}-/${info.host}/${instance}/?info${query}#info-lifecycle`;
  // The same address with its anchor is no new navigation: the page reloads.
  if (page.url() === target) await page.reload({ waitUntil: 'networkidle' });
  else await page.goto(target, { waitUntil: 'networkidle' });
  await until(page, () => {
    const state = document.querySelector('[data-lifecycle] .lifecycle__state');
    return Boolean(state) && (state.hidden || state.textContent !== 'Loading…');
  }, null, 15000);
}

const confirmLifecycle = page => page.click('[data-lifecycle-confirm-save]');
const lifecycleMessage = page => page.textContent('[data-lifecycle-message]');
const savedLifecycle = page => until(page, () => document.querySelector('[data-lifecycle-message]')?.textContent === 'Saved.');
const LC_FORM = '[data-lifecycle-form]';
// The raw XML of one rule in a configuration, as the provider sends it.
const ruleXML = (xml, id) => (xml.match(new RegExp(`<Rule>(?:(?!</Rule>).)*<ID>${id}</ID>(?:(?!</Rule>).)*</Rule>`, 's')) || [''])[0];
// The count of the rules that are on, in the hourglass of the selector and in the chip of the card Operations.
const lifecycleCounts = page => page.evaluate(() => ({
  icon: document.querySelector('#storageSwitcherInfo [data-state-icon="lifecycle"] .stateIcon__count')?.textContent || '',
  chip: document.querySelector('[data-info-lifecycle]')?.textContent || ''
}));

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`lifecycle: ${provider.name} adds a rule with the form after a confirmation of the deletion, keeps the other rule, and offers only the fields that it keeps`, async () => {
    const page = await newPage({ viewport: { width: 1440, height: 1000 } });
    const info = await infoOf(page, instance);
    assert.equal(info.lifecycleSupported, true);
    await writeLifecycle(instance, `<LifecycleConfiguration>${KEPT_RULE}</LifecycleConfiguration>`);
    const keptBefore = ruleXML(await readLifecycle(instance), 'keep-date');
    assert.ok(keptBefore, 'the provider keeps the rule with a date');
    try {
      await openLifecycle(page, instance);
      // The rule with a date is a read-only row: the badge "Kept as is" has the reason in its title.
      const kept = '[data-lifecycle-kept="keep-date"]';
      assert.match(await page.textContent(kept), /Kept as is/);
      assert.match(await page.getAttribute(`${kept} [data-lifecycle-kept-note]`, 'title'), /It expires the objects on a date\. Stowr writes it back unchanged\./);
      assert.equal(await page.locator('[data-lifecycle-rule]').count(), 0);
      // The form of a new rule: the fields of the provider, and one line "Not offered" with the other fields.
      assert.equal(await page.textContent('.lifecycleForm__title'), 'New rule');
      const fields = LIFECYCLE_FIELDS[instance];
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-tag]`).count(), fields.tagFilter ? 1 : 0);
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="noncurrent"]`).count(), fields.noncurrentExpiration ? 1 : 0);
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="abort"]`).count(), fields.abortMultipart ? 1 : 0);
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="deleteMarker"]`).count(), fields.expiredDeleteMarker ? 1 : 0);
      const missing = Object.keys(fields).filter(field => !fields[field]).sort();
      const hidden = await page.locator('[data-lifecycle-hidden] li').evaluateAll(items => items.flatMap(item => item.dataset.field.split(' ')).sort());
      assert.deepEqual(hidden, missing);
      if (instance === 'minio-main') assert.match(await page.getAttribute('[data-lifecycle-hidden] li', 'title'), /MinIO does not keep the abort/);
      if (instance === 'garage-main') assert.match(await page.getAttribute('[data-lifecycle-hidden] li', 'title'), /Garage drops the tag filter/);
      // A new rule starts at the root of the bucket. Add is grey until the rule has an ID and one valid action.
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-prefix]`).inputValue(), '');
      assert.equal(await page.isDisabled('[data-lifecycle-add]'), true);
      assert.match(await page.getAttribute('[data-lifecycle-add]', 'title'), /has no action: choose at least one/);
      await page.fill(`${LC_FORM} [data-lifecycle-id]`, 'lc-test');
      await page.fill(`${LC_FORM} [data-lifecycle-prefix]`, 'smoke/');
      await page.check(`${LC_FORM} [data-lifecycle-action="expiration"]`);
      await page.fill(`${LC_FORM} [data-lifecycle-days="expiration"]`, '30');
      assert.equal(await page.isEnabled('[data-lifecycle-add]'), true);
      await page.fill(`${LC_FORM} [data-lifecycle-id]`, '');
      assert.equal(await page.isDisabled('[data-lifecycle-add]'), true);
      await page.fill(`${LC_FORM} [data-lifecycle-id]`, 'lc-test');
      await page.click('[data-lifecycle-add]');
      const row = page.locator('[data-lifecycle-rule="lc-test"]');
      assert.equal((await row.locator('.lifecycleRule__summary').textContent()).trim(), 'smoke/ · delete 30 d');
      assert.equal(await lifecycleMessage(page), 'Not saved yet.');
      assert.equal(await page.textContent('.lifecycleForm__title'), 'New rule');
      // Save asks in the page, with no window.
      await page.click('[data-lifecycle-save]');
      await page.waitForSelector('[data-lifecycle-confirm]:not([hidden])');
      assert.equal(await page.locator('.bb-overlay').count(), 0);
      assert.deepEqual(cleanText(await page.locator('[data-lifecycle-confirm] li').allInnerTexts()), ['Objects under smoke/ older than 30 days will be deleted.']);
      await confirmLifecycle(page);
      await savedLifecycle(page);
      assert.equal(await page.locator('[data-lifecycle-warnings] li').count(), 0);
      // The provider has the new rule, and the other rule with its bytes.
      const after = await readLifecycle(instance);
      assert.match(ruleXML(after, 'lc-test'), /<Prefix>smoke\/<\/Prefix>/);
      assert.match(ruleXML(after, 'lc-test'), /<Days>30<\/Days>/);
      assert.equal(ruleXML(after, 'keep-date'), keptBefore);
      // The hourglass of the selector and the chip of the card Operations count the two rules that are on.
      await until(page, () => document.querySelector('[data-info-lifecycle]')?.textContent === 'on 2');
      assert.deepEqual(await lifecycleCounts(page), { icon: '2', chip: 'on 2' });
      // After a reload, the list has the rule. Edit loads it in the form; Cancel goes back to a new rule.
      await openLifecycle(page, instance);
      await page.click('[data-lifecycle-rule="lc-test"] [data-lifecycle-edit]');
      assert.equal(await page.textContent('.lifecycleForm__title'), 'Edit lc-test');
      assert.equal(await page.getAttribute(LC_FORM, 'data-lifecycle-form'), 'edit');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-prefix]`).inputValue(), 'smoke/');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="expiration"]`).isChecked(), true);
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-days="expiration"]`).inputValue(), '30');
      await page.click('[data-lifecycle-cancel]');
      assert.equal(await page.textContent('.lifecycleForm__title'), 'New rule');
      assert.equal(await page.isDisabled('[data-lifecycle-save]'), true);
      // A save without a change of a deletion asks nothing: the rule is disabled.
      await page.locator('[data-lifecycle-rule="lc-test"] [data-lifecycle-enabled]').uncheck();
      await page.click('[data-lifecycle-save]');
      await savedLifecycle(page);
      assert.match(ruleXML(await readLifecycle(instance), 'lc-test'), /<Status>Disabled<\/Status>/);
      await until(page, () => document.querySelector('[data-info-lifecycle]')?.textContent === 'on 1');
      assert.equal((await lifecycleCounts(page)).icon, '1');
      assert.deepEqual(page.errors, []);
    } finally {
      await restoreLifecycle(instance);
      await page.context().close();
    }
  });
}

test('lifecycle: Edit and Update change a rule of the list, Delete asks in the row, and Revert puts the rules of the server back', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 1000 } });
  try {
    await openLifecycle(page);
    // The rule of the seed has a tag filter, which SeaweedFS keeps: the editor edits it.
    const seed = page.locator('[data-lifecycle-rule="expire-logs"]');
    assert.equal(await seed.count(), 1);
    assert.equal((await seed.locator('.lifecycleRule__summary').textContent()).trim(), 'logs/ · delete 30 d · tag retention=30d');
    await seed.locator('[data-lifecycle-edit]').click();
    assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-tag]`).inputValue(), 'retention=30d');
    await page.fill(`${LC_FORM} [data-lifecycle-days="expiration"]`, '45');
    // A rule with no action cannot replace the rule: Update is grey.
    await page.uncheck(`${LC_FORM} [data-lifecycle-action="expiration"]`);
    assert.equal(await page.isDisabled('[data-lifecycle-update]'), true);
    await page.check(`${LC_FORM} [data-lifecycle-action="expiration"]`);
    await page.click('[data-lifecycle-update]');
    assert.equal((await seed.locator('.lifecycleRule__summary').textContent()).trim(), 'logs/ · delete 45 d · tag retention=30d');
    assert.equal(await page.isEnabled('[data-lifecycle-save]'), true);
    // Delete asks in the row: Cancel keeps the rule, Delete takes it out of the list (until the save).
    await seed.locator('[data-lifecycle-remove]').click();
    assert.equal((await seed.locator('.lifecycleRule__ask').textContent()).trim(), 'Delete?');
    assert.equal(await page.locator('.bb-overlay').count(), 0);
    await seed.locator('[data-lifecycle-remove-cancel]').click();
    assert.equal(await seed.locator('[data-lifecycle-edit]').count(), 1);
    await seed.locator('[data-lifecycle-remove]').click();
    await seed.locator('[data-lifecycle-remove-confirm]').click();
    assert.equal(await page.locator('[data-lifecycle-rule="expire-logs"]').count(), 0);
    assert.equal(await lifecycleMessage(page), 'Not saved yet.');
    // Revert puts back the rule of the server, with its 30 days.
    await page.click('[data-lifecycle-revert]');
    assert.equal((await page.locator('[data-lifecycle-rule="expire-logs"] .lifecycleRule__summary').textContent()).trim(), 'logs/ · delete 30 d · tag retention=30d');
    assert.equal(await page.isDisabled('[data-lifecycle-save]'), true);
    // At 1000 px and less, the list goes under the form.
    for (const [width, side] of [[1440, true], [1000, false], [390, false]]) {
      await page.setViewportSize({ width, height: 1000 });
      const box = await page.evaluate(() => {
        const formBox = document.querySelector('.lifecycle__formBox').getBoundingClientRect();
        const list = document.querySelector('.lifecycle__listBox').getBoundingClientRect();
        const scroller = document.querySelector('[data-page-scroll]');
        return { side: list.left >= formBox.right - 1 && Math.abs(list.top - formBox.top) < 2, under: list.top >= formBox.bottom - 1, overflow: scroller.scrollWidth - scroller.clientWidth };
      });
      assert.equal(box.side, side, `${width}: ${JSON.stringify(box)}`);
      if (!side) assert.equal(box.under, true, `${width}: ${JSON.stringify(box)}`);
      assert.ok(box.overflow <= 0, `${width}: ${JSON.stringify(box)}`);
    }
    assert.deepEqual(page.errors, []);
  } finally {
    await restoreLifecycle('seaweedfs-main');
    await page.context().close();
  }
});

test('lifecycle: a change of the rules after the page read them stops the save, and Reload reads them again', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 1000 } });
  try {
    await openLifecycle(page);
    await writeLifecycle('seaweedfs-main', `<LifecycleConfiguration>${ruleXML(await readLifecycle('seaweedfs-main'), 'expire-logs')}${KEPT_RULE}</LifecycleConfiguration>`);
    await page.click('[data-lifecycle-rule="expire-logs"] [data-lifecycle-edit]');
    await page.fill(`${LC_FORM} [data-lifecycle-days="expiration"]`, '45');
    await page.click('[data-lifecycle-update]');
    await page.click('[data-lifecycle-save]');
    await page.waitForSelector('[data-lifecycle-confirm]:not([hidden])');
    assert.deepEqual(cleanText(await page.locator('[data-lifecycle-confirm] li').allInnerTexts()), ['Objects under logs/ with the tag retention=30d older than 45 days will be deleted.']);
    await confirmLifecycle(page);
    await until(page, () => document.querySelector('[data-lifecycle-message]')?.dataset.tone === 'error');
    assert.match(await lifecycleMessage(page), /changed after this page read them/);
    assert.match(ruleXML(await readLifecycle('seaweedfs-main'), 'expire-logs'), /<Days>30<\/Days>/);
    await page.click('[data-lifecycle-reload]');
    await page.waitForSelector('[data-lifecycle-kept="keep-date"]');
    assert.equal((await page.locator('[data-lifecycle-rule="expire-logs"] .lifecycleRule__summary').textContent()).trim(), 'logs/ · delete 30 d · tag retention=30d');
    assert.deepEqual(page.errors, []);
  } finally {
    await restoreLifecycle('seaweedfs-main');
    await page.context().close();
  }
});

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`lifecycle: ${provider.name}: the window of the incomplete uploads links to the editor${LIFECYCLE_FIELDS[instance].abortMultipart ? ' with a rule that aborts them in the form' : ' and says that the provider does not keep such a rule'}`, async () => {
    const page = await newPage({ viewport: { width: 1440, height: 1000 } });
    const info = await infoOf(page, instance);
    const offered = LIFECYCLE_FIELDS[instance].abortMultipart;
    try {
      await open(page, `-/${info.host}/${instance}/mpu/`);
      await openMultiparts(page);
      await page.waitForSelector('[data-multipart-lifecycle]');
      const note = await page.textContent('[data-multipart-lifecycle-note]');
      const link = page.locator('[data-multipart-lifecycle]');
      assert.equal(await link.evaluate(node => node.tagName), 'A');
      if (offered) {
        assert.equal((await link.textContent()).trim(), 'Add a lifecycle rule');
        assert.doesNotMatch(note, /does not keep/);
        assert.match(await link.getAttribute('href'), new RegExp(`/-/${info.host}/${instance}/\\?info&lifecycle-abort=mpu%2F#info-lifecycle$`));
      } else {
        assert.equal((await link.textContent()).trim(), 'Lifecycle rules');
        assert.match(note, /a number of days after their start\. MinIO does not keep the abort of incomplete multipart uploads/);
        assert.match(await link.getAttribute('href'), new RegExp(`/-/${info.host}/${instance}/\\?info#info-lifecycle$`));
      }
      await link.click();
      await page.waitForURL(/\?info/);
      await until(page, () => {
        const state = document.querySelector('[data-lifecycle] .lifecycle__state');
        return Boolean(state) && (state.hidden || state.textContent !== 'Loading…');
      }, null, 15000);
      assert.equal(await page.locator('.bb-overlay').count(), 0);
      if (!offered) {
        assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-id]`).inputValue(), 'rule-1');
        return;
      }
      // The form has the rule, ready to add; the address loses the parameter.
      assert.doesNotMatch(page.url(), /lifecycle-abort/);
      assert.equal(await page.textContent('[data-lifecycle-form-message]'), 'Abort rule ready: Add it, then Save.');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-id]`).inputValue(), 'abort-incomplete-uploads');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-prefix]`).inputValue(), 'mpu/');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="abort"]`).isChecked(), true);
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-days="abort"]`).inputValue(), '7');
      assert.equal(await page.locator(`${LC_FORM} [data-lifecycle-action="expiration"]`).isChecked(), false);
      await page.click('[data-lifecycle-add]');
      await page.click('[data-lifecycle-save]');
      await page.waitForSelector('[data-lifecycle-confirm]:not([hidden])');
      assert.deepEqual(cleanText(await page.locator('[data-lifecycle-confirm] li').allInnerTexts()), ['Incomplete multipart uploads under mpu/ older than 7 days will be aborted, and their parts deleted.']);
      await confirmLifecycle(page);
      await savedLifecycle(page);
      assert.match(ruleXML(await readLifecycle(instance), 'abort-incomplete-uploads'), /<DaysAfterInitiation>7<\/DaysAfterInitiation>/);
      assert.deepEqual(page.errors, []);
    } finally {
      await restoreLifecycle(instance);
      await page.context().close();
    }
  });
}

test('lifecycle: the editor shows what the provider changed, and a read-only editor has no form and no Save', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 1000 } });
  let readOnly = false;
  await page.route(/\/api\/lifecycle/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    if (route.request().method() === 'PUT') body.warnings = ['The storage provider dropped the tag filter retention=30d of the rule "expire-logs".'];
    if (readOnly && body.editor) Object.assign(body.editor, { writable: false, readOnlyReason: 'The storage provider refused the change (HTTP 403 AccessDenied).' });
    await route.fulfill({ response, json: body });
  });
  try {
    await openLifecycle(page);
    await page.locator('[data-lifecycle-rule="expire-logs"] [data-lifecycle-enabled]').uncheck();
    await page.click('[data-lifecycle-save]');
    await until(page, () => document.querySelectorAll('[data-lifecycle-warnings] li').length === 1);
    assert.equal(await lifecycleMessage(page), 'Saved, but the storage provider changed them:');
    assert.match(await page.textContent('[data-lifecycle-warnings]'), /dropped the tag filter retention=30d/);
    readOnly = true;
    await openLifecycle(page);
    assert.equal(await page.textContent('[data-lifecycle-readonly]'), 'Read only. The storage provider refused the change (HTTP 403 AccessDenied).');
    assert.equal(await page.locator('[data-lifecycle-rule]').count(), 0);
    assert.equal(await page.locator('[data-lifecycle-view="expire-logs"]').count(), 1);
    assert.equal(await page.locator(LC_FORM).count(), 0);
    for (const control of ['[data-lifecycle-save]', '[data-lifecycle-revert]']) assert.equal(await page.isHidden(control), true);
    assert.deepEqual(page.errors, []);
  } finally {
    await restoreLifecycle('seaweedfs-main');
    await page.context().close();
  }
});

test('actions: the tag item names the folder or the bucket; the listing has no item and no window of the lifecycle rules', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.click('#actionsButton');
  assert.equal(await page.locator('#lifecycleItem').count(), 0);
  assert.equal((await page.textContent('#folderTagsItem')).trim(), 'Tag this bucket');
  await page.click('#folderTagsItem');
  await page.waitForSelector('.bb-overlay .bb-modal-title');
  assert.equal((await page.textContent('.bb-overlay .bb-modal-title')).trim(), 'Tag this bucket');
  assert.match(await page.textContent('.bb-overlay .tagForm__intro'), /every object in the bucket/);
  await page.click('.bb-modal-x');
  await page.waitForSelector('.bb-overlay', { state: 'detached' });
  await open(page, `${MAIN}config/`);
  await page.click('#actionsButton');
  assert.equal((await page.textContent('#folderTagsItem')).trim(), 'Tag this folder');
  await page.click('#folderTagsItem');
  await page.waitForSelector('.bb-overlay .bb-modal-title');
  assert.equal((await page.textContent('.bb-overlay .bb-modal-title')).trim(), 'Tag this folder');
  assert.match(await page.textContent('.bb-overlay .tagForm__intro'), /under config\//);
  await page.click('.bb-modal-x');
  await page.waitForSelector('.bb-overlay', { state: 'detached' });
  // The old address ?lifecycle opens no window: the listing removes it.
  await page.goto(`${BASE}${MAIN}?lifecycle`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable, .empty');
  assert.equal(await page.locator('.bb-overlay, [data-lifecycle]').count(), 0);
  assert.doesNotMatch(page.url(), /lifecycle/);
  // A bucket without rule: the chip "off" of the card Operations is a link to the part of the rules.
  await openLifecycle(page, 'minio-main');
  const chip = page.locator('[data-info-lifecycle]');
  assert.equal((await chip.textContent()).trim(), 'off');
  assert.equal(await chip.getAttribute('href'), '#info-lifecycle');
  assert.equal(await chip.getAttribute('data-tone'), 'off');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('lifecycle: an instance without lifecycle rules has no part and no row of the rules, and the Actions menu has no item for them', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const item of body.instances) item.lifecycleSupported = false;
    await route.fulfill({ response, json: body });
  });
  await open(page, MAIN);
  await page.click('#actionsButton');
  assert.equal(await page.locator('#lifecycleItem').count(), 0);
  assert.equal(await page.isVisible('#multipartsItem'), true);
  assert.match(await page.getAttribute('#storageSwitcherInfo [data-state-icon="lifecycle"]', 'data-tone'), /^off$/);
  await page.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-column="functions"] [data-card="operations"]');
  assert.equal(await page.locator('#info-lifecycle, [data-row="lifecycle"]').count(), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 10. Object Lock: the section "Retention" of the details (the legal hold and the retention of one version), and the
// refusal of the delete of a locked version. The buckets "locked" of SeaweedFS and MinIO have Object Lock. Each test
// makes its own objects with a unique key, and deletes them at the end: the retention dates are a few seconds ahead.
// The pages open with the time zone UTC, so that the fields of a date have the UTC time.
// ------------------------------------------------------------------------------------------------------------------

// Makes an object of the locked bucket with one or two versions (the versions need one second between them).
async function makeLockedObject(instance, key, versions = 1) {
  for (let number = 1; number <= versions; number += 1) {
    await s3(instance, 'PUT', `/locked/${key}`, '', Buffer.from(`${key} version ${number}\n`), { 'content-type': 'text/plain' });
    if (number < versions) await sleep(1100);
  }
}

const lockUrl = (instance, key, version = '') => `${BASE}api/object-lock?instance=${instance}&key=${encodeURIComponent(key)}&version=${encodeURIComponent(version)}`;

async function apiLock(page, instance, key, version = '') {
  const response = await page.request.get(lockUrl(instance, key, version));
  assert.equal(response.status(), 200, await response.text());
  return response.json();
}

async function putLock(page, instance, key, version, change) {
  return page.request.put(`${BASE}api/object-lock`, { data: { instance, key, version, ...change } });
}

async function apiVersions(page, instance, key) {
  return (await (await page.request.get(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}`)).json()).versions || [];
}

// A date for the field "Retain until": the UTC time, some seconds from now.
// Chromium gives the value of a "datetime-local" input without ":00" for the second 0, and page.fill then fails: the
// date never has the second 0.
const lockField = seconds => {
  const date = new Date(Date.now() + seconds * 1000);
  if (date.getUTCSeconds() === 0) date.setUTCSeconds(1);
  return date.toISOString().slice(0, 19);
};

const lockText = (page, field) => page.textContent(`[data-lock-section] [data-lock-field="${field}"]`).then(text => text.trim());

async function closeWindow(page) {
  await page.click('.bb-modal-x');
  await page.waitForSelector('.bb-overlay', { state: 'detached' });
}

// Opens the details of the file and waits for the section Retention.
async function openLockDetails(page, name) {
  await openDetails(page, name);
  await page.waitForSelector('[data-lock-section] [data-lock-field="legalHold"]');
}

// Clicks Save and waits for the message of the section. The form closes.
async function saveLock(page) {
  await page.click('[data-lock-save]');
  await until(page, () => document.querySelector('[data-lock-message]')?.textContent === 'The retention is saved.');
  assert.equal(await page.isHidden('[data-lock-section] .lockEditor__form'), true);
}

// The small window of a confirmation, over the large window.
const confirmWindow = '.bb-overlay:not(.bb-overlay--large)';

// Opens the version picker of the open details and clicks an action button (download, restore or remove) of a version.
async function pickerAction(page, version, action) {
  await page.click('.bb-details-head .versionPicker__button');
  await page.hover(`.versionPicker__item[data-version="${version}"]`);
  await page.click(`.versionPicker__item[data-version="${version}"] [data-version-action="${action}"]`);
}

const lockedHostOf = async (page, instance) => (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === instance);

for (const [instance, provider] of Object.entries(LOCKED)) {
  test(`object lock: ${provider.name} sets a legal hold, refuses the delete of the version with a clear message, and deletes it after the hold goes`, async () => {
    const page = await newPage({ timezoneId: 'UTC' });
    const info = await lockedHostOf(page, instance);
    assert.equal(info.objectLockSupported, true);
    const key = `lock/hold-${Date.now()}.txt`;
    const name = key.split('/').pop();
    await makeLockedObject(instance, key, 2);
    const held = (await apiVersions(page, instance, key)).find(version => version.isCurrent);
    await open(page, `-/${info.host}/${instance}/lock/`);
    await openLockDetails(page, name);
    assert.equal(await lockText(page, 'mode'), 'No retention');
    assert.equal(await lockText(page, 'legalHold'), 'Off');
    assert.equal(await lockText(page, 'default'), 'None');
    assert.equal(await page.isHidden('[data-lock-section] .lockEditor__form'), true);
    await page.click('[data-lock-edit]');
    await page.waitForSelector('[data-lock-legal-hold]', { state: 'visible' });
    assert.equal(await page.isDisabled('[data-lock-save]'), true);
    await page.check('[data-lock-legal-hold]');
    await saveLock(page);
    assert.equal(await lockText(page, 'legalHold'), 'On');
    const state = await apiLock(page, instance, key);
    assert.equal(state.legalHold, true);
    assert.equal(state.version, held.version);
    assert.equal(state.retention, null);
    // The version picker of the details: the delete of the locked version fails, and the toast says why.
    await pickerAction(page, held.version, 'remove');
    await page.click(`${confirmWindow} .bb-btn-primary`);
    await page.waitForSelector('.bb-toast.is-error');
    const toast = await page.textContent('.bb-toast.is-error');
    assert.match(toast, /Object Lock/);
    assert.match(toast, /legal hold/);
    assert.equal(await versionCount(page, instance, key), 2);
    await page.click('.versionPicker__button');
    assert.equal(await page.locator(`.versionPicker__item[data-version="${held.version}"] [data-version-action="remove"]`).count(), 1);
    await page.keyboard.press('Escape');
    await closeWindow(page);
    // The hold goes off in the details, and then the delete works.
    await openLockDetails(page, name);
    await page.click('[data-lock-edit]');
    await page.uncheck('[data-lock-legal-hold]');
    await saveLock(page);
    assert.equal(await lockText(page, 'legalHold'), 'Off');
    assert.equal((await apiLock(page, instance, key)).legalHold, false);
    // The delete works now. The details open again, and a file with one version has no picker.
    await pickerAction(page, held.version, 'remove');
    await page.click(`${confirmWindow} .bb-btn-primary`);
    await page.waitForSelector('.bb-toast >> text=Version deleted.');
    assert.equal(await versionCount(page, instance, key), 1);
    await until(page, () => !document.querySelector('.bb-details-head .versionPicker__button') && document.querySelector('[data-lock-section] [data-lock-field="legalHold"]'));
    // The other version has no lock: the delete works, and the object is gone.
    const rest = (await apiVersions(page, instance, key)).find(version => !version.deleteMarker);
    const removed = await page.request.delete(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}&version=${encodeURIComponent(rest.version)}`);
    assert.equal(removed.status(), 204);
    assert.equal(await versionCount(page, instance, key), 0);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });

  test(`object lock: ${provider.name} extends a Governance retention, needs the bypass to shorten it, and the bypass works`, async () => {
    const page = await newPage({ timezoneId: 'UTC' });
    const info = await lockedHostOf(page, instance);
    const key = `lock/governance-${Date.now()}.txt`;
    const name = key.split('/').pop();
    await makeLockedObject(instance, key);
    await open(page, `-/${info.host}/${instance}/lock/`);
    await openLockDetails(page, name);
    // A first retention of one minute.
    await page.click('[data-lock-edit]');
    await page.waitForSelector('[data-lock-until]', { state: 'visible' });
    assert.equal(await page.inputValue('[data-lock-until]'), '');
    assert.equal(await page.inputValue('[data-lock-mode]'), 'GOVERNANCE');
    await page.fill('[data-lock-until]', lockField(60));
    await saveLock(page);
    assert.equal(await lockText(page, 'mode'), 'Governance');
    assert.match(await lockText(page, 'until'), /^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC$/);
    assert.match(await page.textContent('[data-lock-note]'), /Users with the bypass right can shorten or remove it\./);
    let state = await apiLock(page, instance, key);
    assert.equal(state.retention.mode, 'GOVERNANCE');
    assert.equal(state.retention.active, true);
    const first = state.retention.retainUntil;
    // A later date: no confirmation.
    await page.click('[data-lock-edit]');
    await page.fill('[data-lock-until]', lockField(120));
    await saveLock(page);
    assert.equal(await page.locator(confirmWindow).count(), 0);
    state = await apiLock(page, instance, key);
    assert.ok(Date.parse(state.retention.retainUntil) > Date.parse(first) + 30000, `${state.retention.retainUntil} after ${first}`);
    const extended = state.retention.retainUntil;
    // An earlier date: the page asks first and names the bypass. Cancel changes nothing.
    await page.click('[data-lock-edit]');
    const shorter = lockField(10);
    await page.fill('[data-lock-until]', shorter);
    await page.click('[data-lock-save]');
    await page.waitForSelector(confirmWindow);
    const dialog = await page.textContent(confirmWindow);
    assert.match(dialog, /Bypass the Governance retention/);
    assert.match(dialog, /x-amz-bypass-governance-retention/);
    assert.match(dialog, /shortens the Governance retention/);
    await page.click(`${confirmWindow} button:has-text("Cancel")`);
    await page.waitForSelector(confirmWindow, { state: 'detached' });
    assert.equal((await apiLock(page, instance, key)).retention.retainUntil, extended);
    assert.equal(await page.isVisible('[data-lock-section] .lockEditor__form'), true);
    // The same change, confirmed: the bypass goes with the request.
    await page.click('[data-lock-save]');
    await page.waitForSelector(confirmWindow);
    await page.click(`${confirmWindow} .bb-btn-primary`);
    await until(page, () => document.querySelector('[data-lock-message]')?.textContent === 'The retention is saved.');
    state = await apiLock(page, instance, key);
    assert.equal(state.retention.retainUntil, `${shorter}Z`);
    assert.equal(state.retention.mode, 'GOVERNANCE');
    assert.equal(await lockText(page, 'until'), `${shorter.replace('T', ' ')} UTC`);
    // The retention ends, and then the version goes.
    const end = Date.now() + 25000;
    while ((await apiLock(page, instance, key)).retention?.active && Date.now() < end) await sleep(500);
    assert.equal((await apiLock(page, instance, key)).retention.active, false);
    const removed = await page.request.delete(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}&version=${encodeURIComponent(state.version)}`);
    assert.equal(removed.status(), 204, await removed.text());
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });

  test(`object lock: ${provider.name} never shortens a Compliance retention`, async () => {
    const page = await newPage({ timezoneId: 'UTC' });
    const info = await lockedHostOf(page, instance);
    const key = `lock/compliance-${Date.now()}.txt`;
    const name = key.split('/').pop();
    await makeLockedObject(instance, key);
    await open(page, `-/${info.host}/${instance}/lock/`);
    await openLockDetails(page, name);
    await page.click('[data-lock-edit]');
    await page.selectOption('[data-lock-mode]', 'COMPLIANCE');
    await page.fill('[data-lock-until]', lockField(25));
    await saveLock(page);
    assert.equal(await lockText(page, 'mode'), 'Compliance');
    assert.match(await page.textContent('[data-lock-note]'), /Nobody can shorten or remove it before this date, not even the owner of the bucket\./);
    const state = await apiLock(page, instance, key);
    assert.equal(state.retention.mode, 'COMPLIANCE');
    // An earlier date: the page refuses it and sends no request.
    const puts = [];
    page.on('request', request => { if (request.method() === 'PUT' && request.url().includes('/api/object-lock')) puts.push(request.url()); });
    await page.click('[data-lock-edit]');
    assert.equal(await page.isDisabled('[data-lock-mode]'), true);
    await page.fill('[data-lock-until]', lockField(10));
    await page.click('[data-lock-save]');
    const refusal = await page.textContent('[data-lock-message]');
    assert.match(refusal, /A Compliance retention cannot be shortened/);
    assert.match(refusal, /nobody can shorten or remove it before \d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC, not even the owner of the bucket\./);
    assert.equal(await page.getAttribute('[data-lock-message]', 'data-tone'), 'error');
    assert.equal(await page.locator(confirmWindow).count(), 0);
    await sleep(300);
    assert.deepEqual(puts, []);
    assert.equal((await apiLock(page, instance, key)).retention.retainUntil, state.retention.retainUntil);
    // The server has the same rule.
    const earlier = await putLock(page, instance, key, state.version, { retention: { mode: 'COMPLIANCE', retainUntil: `${lockField(10)}Z` } });
    assert.equal(earlier.status(), 409);
    assert.equal((await earlier.json()).error.code, 'compliance_retention');
    const governance = await putLock(page, instance, key, state.version, { retention: { mode: 'GOVERNANCE', retainUntil: `${lockField(40)}Z` }, bypassGovernance: true });
    assert.equal(governance.status(), 409);
    assert.equal((await governance.json()).error.code, 'compliance_retention');
    // A later date works.
    const later = await putLock(page, instance, key, state.version, { retention: { mode: 'COMPLIANCE', retainUntil: `${lockField(35)}Z` } });
    assert.equal(later.status(), 200, await later.text());
    assert.ok(Date.parse((await later.json()).retention.retainUntil) > Date.parse(state.retention.retainUntil));
    // The retention ends, and then the version goes.
    const end = Date.now() + 45000;
    while ((await apiLock(page, instance, key)).retention?.active && Date.now() < end) await sleep(500);
    assert.equal((await apiLock(page, instance, key)).retention.active, false);
    const removed = await page.request.delete(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}&version=${encodeURIComponent(state.version)}`);
    assert.equal(removed.status(), 204, await removed.text());
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('object lock: the Retention section is hidden on Garage and on a bucket without Object Lock', async () => {
  const page = await newPage();
  const instances = (await (await page.request.get(`${BASE}api/instances`)).json()).instances;
  const flags = Object.fromEntries(instances.map(item => [item.id, item.objectLockSupported]));
  for (const instance of ['garage-main', 'seaweedfs-main', 'minio-main']) assert.equal(flags[instance], false, instance);
  for (const instance of Object.keys(LOCKED)) assert.equal(flags[instance], true, instance);
  for (const instance of ['garage-main', 'seaweedfs-main']) {
    const info = instances.find(item => item.id === instance);
    await open(page, `-/${info.host}/${instance}/smoke/`);
    await openDetails(page, 'hello.txt');
    await page.waitForSelector('[data-details-tool-card="integrity"]');
    await page.waitForSelector('[data-metadata-section] .metaEditor__view');
    assert.equal(await page.locator('[data-lock-section]').count(), 0, instance);
    assert.equal(await visibleText(page, 'Retention'), 0, instance);
    const response = await page.request.get(lockUrl(instance, 'smoke/hello.txt'));
    assert.equal(response.status(), 501, instance);
    assert.equal((await response.json()).error.code, 'object_lock_unsupported');
  }
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

for (const [instance, provider] of Object.entries(LOCKED)) {
  test(`object lock: ${provider.name}: the delete of a file and of a folder says that a version with a lock stays, before and after`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = Date.now();
    const key = `lockdel/file-${stamp}.txt`;
    const folder = `lockdel/folder-${stamp}/`;
    await makeLockedObject(instance, key);
    await makeLockedObject(instance, `${folder}inner.txt`);
    await open(page, `-/${info.host}/${instance}/lockdel/`);
    // The file: the confirmation has the sentence of Object Lock, and a warning follows the delete.
    await page.click(`tr[data-name="file-${stamp}.txt"] .bb-kebab`);
    await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
    await page.waitForSelector('.bb-overlay [data-lock-delete-note]');
    assert.equal((await page.textContent('[data-lock-delete-note]')).trim(), 'This bucket has Object Lock. The delete adds a delete marker. A version that a lock protects stays in the bucket.');
    assert.doesNotMatch(await page.textContent('.bb-overlay .bb-modal-body'), /Permanently/);
    await page.click('.bb-overlay .bb-btn-primary');
    await page.waitForSelector('.bb-toast.is-warning');
    assert.match(await page.textContent('.bb-toast.is-warning'), /lock protects stay in the bucket.*delete marker/s);
    await page.waitForSelector(`tr[data-name="file-${stamp}.txt"]`, { state: 'detached' });
    // The version stays, behind a delete marker.
    const versions = await apiVersions(page, instance, key);
    assert.equal(versions.filter(version => !version.deleteMarker).length, 1);
    assert.equal(versions.filter(version => version.deleteMarker).length, 1);
    // The folder: the same sentence and the same warning.
    await page.evaluate(() => document.querySelectorAll('.bb-toast').forEach(node => node.remove()));
    await page.click(`tr[data-name="folder-${stamp}"] .bb-kebab`);
    await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
    await page.waitForSelector('.bb-overlay [data-lock-delete-note]');
    await page.click('.bb-overlay .bb-btn-primary');
    await page.waitForSelector('.bb-toast.is-warning', { timeout: 30000 });
    assert.match(await page.textContent('.bb-toast.is-warning'), new RegExp(`${folder}: the delete added a delete marker`));
    assert.equal((await apiVersions(page, instance, `${folder}inner.txt`)).filter(version => !version.deleteMarker).length, 1);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

// A bucket with versioning: a delete adds a delete marker. MinIO still lists the folder of such an object with a delimiter,
// and the folder is empty. The page must not show it, after the delete of the last file, and after the delete of a folder.
const GHOST_INSTANCES = [
  { id: 'minio-main', bucket: 'default' }, { id: 'seaweedfs-main', bucket: 'default' },
  { id: 'minio-locked', bucket: 'locked' }, { id: 'seaweedfs-locked', bucket: 'locked' },
];

async function purgeVersions(page, instance, key) {
  for (const version of await apiVersions(page, instance, key)) {
    await page.request.delete(`${BASE}api/versions?instance=${instance}&key=${encodeURIComponent(key)}&version=${encodeURIComponent(version.version)}`);
  }
}

const listedNames = async (page, instance, prefix) => ((await (await page.request.get(`${BASE}api/list?instance=${instance}&prefix=${encodeURIComponent(prefix)}`)).json()).items || []).map(item => item.name);

for (const { id: instance, bucket } of GHOST_INSTANCES) {
  test(`listing: ${instance}: a folder with no live object is not listed, after the delete of its last file and after the delete of the folder`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    assert.equal(info.versioningSupported, true);
    const stamp = Date.now();
    const parent = 'ghost-tests/';
    const single = `${parent}one-${stamp}/a.txt`;
    const many = [`${parent}many-${stamp}/a.txt`, `${parent}many-${stamp}/b.txt`];
    const keep = `${parent}keep-${stamp}/a.txt`;
    for (const key of [single, ...many, keep]) await s3(instance, 'PUT', `/${bucket}/${key}`, '', Buffer.from(`${key}\n`), { 'content-type': 'text/plain' });
    try {
      assert.deepEqual((await listedNames(page, instance, parent)).sort(), [`keep-${stamp}/`, `many-${stamp}/`, `one-${stamp}/`]);
      // The last file of a folder: after the delete, the folder is not in the parent.
      await open(page, `-/${info.host}/${instance}/${parent}one-${stamp}/`);
      await page.click('tr[data-name="a.txt"] .bb-kebab');
      await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
      await page.click('.bb-overlay .bb-btn-primary');
      await page.waitForSelector('tr[data-name="a.txt"]', { state: 'detached' });
      await open(page, `-/${info.host}/${instance}/${parent}`);
      assert.equal(await page.locator(`tr[data-name="one-${stamp}"]`).count(), 0);
      assert.deepEqual((await listedNames(page, instance, parent)).sort(), [`keep-${stamp}/`, `many-${stamp}/`]);
      // A folder with two files: the delete of the folder, then a reload.
      await page.click(`tr[data-name="many-${stamp}"] .bb-kebab`);
      await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
      await page.click('.bb-overlay .bb-btn-primary');
      await page.waitForSelector(`tr[data-name="many-${stamp}"]`, { state: 'detached', timeout: 30000 });
      await page.reload({ waitUntil: 'networkidle' });
      await page.waitForSelector('.dataTable, .empty');
      assert.equal(await page.locator(`tr[data-name="many-${stamp}"]`).count(), 0);
      assert.deepEqual(await listedNames(page, instance, parent), [`keep-${stamp}/`]);
      // The folder with a live file stays.
      assert.equal(await page.locator(`tr[data-name="keep-${stamp}"]`).count(), 1);
      assert.deepEqual(page.errors, []);
    } finally {
      for (const key of [single, ...many, keep]) await purgeVersions(page, instance, key);
      await page.context().close();
    }
  });
}

test('object lock: a bucket without Object Lock has no sentence of Object Lock in the confirmation of a delete', async () => {
  const page = await newPage();
  await open(page, `${MAIN}smoke/`);
  await page.click('tr[data-name="hello.txt"] .bb-kebab');
  await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
  await page.waitForSelector('.bb-overlay .bb-modal-text');
  assert.match(await page.textContent('.bb-overlay .bb-modal-body'), /^Permanently delete "smoke\/hello\.txt"\?$/);
  assert.equal(await page.locator('[data-lock-delete-note]').count(), 0);
  await page.click('.bb-overlay .bb-btn:not(.bb-btn-primary):not(.bb-modal-x)');
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 11. Encryption: the key encryption = "sse-s3" of a bucket, and the section "Encryption" of the details. The buckets
// "encrypted" of SeaweedFS and MinIO really encrypt (a test key in the stack). The instance garage-encrypted has the
// bucket "default" of Garage: Garage accepts the header and does not encrypt. The tests of the writes make their
// source objects with s3(), without the header, and so they show that the server adds it. Each test makes its own
// objects with a unique key in the folder enc/ and deletes them.
// ------------------------------------------------------------------------------------------------------------------

const ENCRYPTION_XML = '<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>';
const encryptionText = (page, field) => page.textContent(`[data-encryption-section] [data-encryption-field="${field}"]`).then(text => text.trim());
const encryptionStamp = () => `${Date.now()}-${Math.floor(Math.random() * 1000)}`;

// The path of an object for s3(), with its bucket, and the HEAD headers on the provider.
const encryptedPath = (instance, key) => `/${ENCRYPTED[instance].bucket}/${key}`;
const headEncrypted = (instance, key) => s3Head(instance, encryptedPath(instance, key));

// Waits until the HEAD of an object gives the header of SSE-S3 (a job of the page ends a short time before the listing).
async function assertSSE(instance, key, algorithm = 'AES256') {
  const headers = await headEncrypted(instance, key);
  assert.equal(headers['x-amz-server-side-encryption'], algorithm, `${key}: ${JSON.stringify(headers)}`);
  return headers;
}

// Waits for the section Encryption of the details with its row "object".
async function openEncryptionDetails(page, name) {
  await openDetails(page, name);
  await page.waitForSelector('[data-encryption-section] [data-encryption-field="object"]');
}

// Waits for the details of a file whose provider reports no encryption: the page removes the section.
async function openDetailsWithoutEncryption(page, name) {
  await openDetails(page, name);
  await page.waitForSelector('[data-details-tool-card="integrity"]');
  await until(page, () => !document.querySelector('[data-encryption-section]'));
}

// An upload through the page: the input, then the end of the queue and the new row.
async function uploadWithPage(page, name, buffer, timeout = 60000) {
  await page.setInputFiles('#fileInput', [{ name, mimeType: 'application/octet-stream', buffer }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout });
  await page.click('.uploadDock .button >> text=Close');
  await page.waitForSelector(`tr[data-name="${name}"]`);
}

// A write through the server (PUT /s3): it adds the header of the encryption of the instance.
async function putWithServer(page, instance, key, body, headers = { 'Content-Type': 'text/plain' }) {
  const response = await page.request.put(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`, { data: Buffer.from(body), headers });
  assert.equal(response.ok(), true, `${response.status()} ${await response.text()}`);
}

// A write on the provider with no encryption header: the source of the tests of the copy, the edit and the restore.
async function putPlain(instance, key, body, extra = {}) {
  await s3(instance, 'PUT', encryptedPath(instance, key), '', Buffer.from(body), { 'content-type': 'text/plain', ...extra });
  assert.equal((await headEncrypted(instance, key))['x-amz-server-side-encryption'], undefined, `${key} has no encryption`);
}

async function deleteEncrypted(page, instance, ...keys) {
  for (const key of keys) await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
}

// Waits until the object exists on the provider (the job of a move has no row to wait for in the folder of the page).
async function waitForHead(instance, key, timeout = 30000) {
  const end = Date.now() + timeout;
  for (;;) {
    try {
      return await headEncrypted(instance, key);
    } catch (error) {
      if (Date.now() > end) throw error;
      await sleep(300);
    }
  }
}

for (const [instance, provider] of Object.entries(ENCRYPTED)) {
  test(`encryption: ${provider.name} encrypts an upload of the page, and the details say SSE-S3; a plain object has no section`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    assert.equal(info.bucketEncryptionSupported, true);
    const answer = await (await page.request.get(`${BASE}api/encryption?instance=${instance}`)).json();
    assert.equal(answer.uploads, 'sse-s3');
    assert.equal(answer.bucketDefault, null);
    const stamp = encryptionStamp();
    const key = `enc/upload-${stamp}.txt`;
    const plain = `enc/plain-${stamp}.txt`;
    await putPlain(instance, plain, 'plain on the provider\n');
    await open(page, `-/${info.host}/${instance}/enc/`);
    await uploadWithPage(page, `upload-${stamp}.txt`, Buffer.from('uploaded by the page\n'));
    const head = await assertSSE(instance, key);
    assert.equal(head['x-amz-server-side-encryption-aws-kms-key-id'], undefined);
    await openEncryptionDetails(page, `upload-${stamp}.txt`);
    assert.equal(await page.textContent('[data-encryption-section] .bb-details-section-title'), ' Encryption');
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    // The bucket has no default encryption: the page shows no row for it, and no row of a key.
    assert.equal(await page.locator('[data-encryption-field="default"], [data-encryption-field="kmsKey"], [data-encryption-note]').count(), 0);
    assert.equal(await page.locator('.bb-details--file [data-details-tool-card="integrity"]').count(), 1);
    await closeWindow(page);
    // The provider does not encrypt this object: the section is removed.
    await openDetailsWithoutEncryption(page, `plain-${stamp}.txt`);
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, key, plain);
    await page.context().close();
  });

  test(`encryption: ${provider.name} encrypts the copy, the rename and the move of an object that has no encryption`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const key = `enc/source-${stamp}.txt`;
    const names = { copy: `copy-${stamp}.txt`, rename: `renamed-${stamp}.txt` };
    const moved = `enc/moved/moved-${stamp}.txt`;
    await putPlain(instance, key, 'a small object\n');
    await open(page, `-/${info.host}/${instance}/enc/`);
    await rowActionWithName(page, `source-${stamp}.txt`, 'Copy', names.copy);
    await page.waitForSelector(`tr[data-name="${names.copy}"]`, { timeout: 60000 });
    await assertSSE(instance, `enc/${names.copy}`);
    // The source stays as it was.
    assert.equal((await headEncrypted(instance, key))['x-amz-server-side-encryption'], undefined);
    await rowActionWithName(page, names.copy, 'Rename', names.rename);
    await page.waitForSelector(`tr[data-name="${names.rename}"]`, { timeout: 60000 });
    await page.waitForSelector(`tr[data-name="${names.copy}"]`, { state: 'detached' });
    await assertSSE(instance, `enc/${names.rename}`);
    await openEncryptionDetails(page, names.rename);
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    await closeWindow(page);
    // A move into another folder: the same request as the rename of a path.
    const response = await page.request.post(`${BASE}api/rename`, { data: { instance, src: key, dst: moved, isPrefix: false } });
    assert.equal(response.status() < 300, true, await response.text());
    await waitForHead(instance, moved);
    await assertSSE(instance, moved);
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, names.rename, moved);
    await page.context().close();
  });

  test(`encryption: ${provider.name} encrypts the copy in parts and the rename of a large object`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const key = `enc/large-${stamp}.bin`;
    const copy = `copy-large-${stamp}.bin`;
    const renamed = `renamed-large-${stamp}.bin`;
    // 20000 bytes are above the threshold of the test image (8192): the copy goes in parts (CreateMultipartUpload).
    await putPlain(instance, key, 'L'.repeat(20000));
    await open(page, `-/${info.host}/${instance}/enc/`);
    await rowActionWithName(page, `large-${stamp}.bin`, 'Copy', copy);
    await page.waitForSelector(`tr[data-name="${copy}"]`, { timeout: 60000 });
    const head = await assertSSE(instance, `enc/${copy}`);
    // An object that has the ETag of a multipart object came from a copy in parts.
    assert.match(head.etag, /-1"?$/);
    assert.equal((await getObject(page, instance, `enc/${copy}`)).body.toString(), 'L'.repeat(20000));
    await rowActionWithName(page, copy, 'Rename', renamed);
    await page.waitForSelector(`tr[data-name="${renamed}"]`, { timeout: 60000 });
    await page.waitForSelector(`tr[data-name="${copy}"]`, { state: 'detached' });
    assert.match((await assertSSE(instance, `enc/${renamed}`)).etag, /-1"?$/);
    await openEncryptionDetails(page, renamed);
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, key, `enc/${renamed}`);
    await page.context().close();
  });

  test(`encryption: ${provider.name} encrypts the edit of the metadata, of a small and of a large object`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const keys = [`enc/meta-small-${stamp}.txt`, `enc/meta-large-${stamp}.bin`];
    await putPlain(instance, keys[0], 'a small object\n', { 'x-amz-meta-owner': 'ada' });
    await putPlain(instance, keys[1], 'M'.repeat(20000), { 'x-amz-meta-owner': 'ada' });
    await open(page, `-/${info.host}/${instance}/enc/`);
    for (const key of keys) {
      const name = key.split('/').pop();
      await openMetadataForm(page, name);
      await page.fill('[data-metadata-value="owner"]', 'grace');
      await saveMetadata(page, 'grace');
      await assertSSE(instance, key);
      assert.equal((await apiMetadata(page, instance, key)).metadata[0].value, 'grace');
      // The details open again on the new version: they say SSE-S3.
      await page.waitForSelector('[data-encryption-section] [data-encryption-field="object"]');
      assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
      await closeWindow(page);
    }
    // The large object went in parts.
    assert.match((await headEncrypted(instance, keys[1])).etag, /-1"?$/);
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, ...keys);
    await page.context().close();
  });

  test(`encryption: ${provider.name} encrypts the restore of an older version that has no encryption`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const key = `enc/restore-${stamp}.txt`;
    await putPlain(instance, key, 'version 1, no encryption\n');
    await sleep(1100);
    await putWithServer(page, instance, key, 'version 2, from the page\n');
    await assertSSE(instance, key);
    const versions = await apiVersions(page, instance, key);
    const older = versions.find(version => !version.isCurrent && !version.deleteMarker);
    assert.ok(older, JSON.stringify(versions));
    const response = await page.request.post(`${BASE}api/versions/restore`, { data: { instance, key, version: older.version } });
    assert.equal(response.status(), 200, await response.text());
    await assertSSE(instance, key);
    assert.equal((await getObject(page, instance, key)).body.toString(), 'version 1, no encryption\n');
    assert.equal((await apiVersions(page, instance, key)).filter(version => !version.deleteMarker).length, 3);
    await open(page, `-/${info.host}/${instance}/enc/`);
    await openEncryptionDetails(page, key.split('/').pop());
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, key);
    await page.context().close();
  });

  test(`encryption: ${provider.name} encrypts a multipart upload of the page`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const name = `parts-${stamp}.bin`;
    await open(page, `-/${info.host}/${instance}/enc/`);
    const requests = [];
    page.on('request', request => { if (request.method() === 'PUT' && request.url().includes('/api/uploads')) requests.push(request.url()); });
    // 33 MiB are above the threshold of the resumable upload of the page (32 MiB).
    await uploadWithPage(page, name, Buffer.alloc(33 * 1024 * 1024, 5), 120000);
    assert.ok(requests.length >= 2, `chunks: ${requests.length}`);
    const head = await assertSSE(instance, `enc/${name}`);
    assert.match(head.etag, /-\d+"?$/);
    await openEncryptionDetails(page, name);
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, `enc/${name}`);
    await page.context().close();
  });
}

test('encryption: MinIO keeps the SSE-KMS of the source in the edit of the metadata and in the restore', async () => {
  const page = await newPage();
  const instance = 'minio-encrypted';
  const info = await lockedHostOf(page, instance);
  const key = `enc/kms-${encryptionStamp()}.txt`;
  const kms = { 'x-amz-server-side-encryption': 'aws:kms' };
  await s3(instance, 'PUT', encryptedPath(instance, key), '', Buffer.from('kms version 1\n'), { 'content-type': 'text/plain', 'x-amz-meta-rev': '1', ...kms });
  await sleep(1100);
  await s3(instance, 'PUT', encryptedPath(instance, key), '', Buffer.from('kms version 2\n'), { 'content-type': 'text/plain', 'x-amz-meta-rev': '2', ...kms });
  assert.equal((await headEncrypted(instance, key))['x-amz-server-side-encryption'], 'aws:kms');
  await open(page, `-/${info.host}/${instance}/enc/`);
  const name = key.split('/').pop();
  await openMetadataForm(page, name);
  await page.fill('[data-metadata-value="rev"]', '2-edited');
  await saveMetadata(page, '2-edited');
  // The source says aws:kms and the instance asks for AES256: the edit keeps the encryption of the source.
  const edited = await assertSSE(instance, key, 'aws:kms');
  assert.match(edited['x-amz-server-side-encryption-aws-kms-key-id'], /stowr-test/);
  await page.waitForSelector('[data-encryption-section] [data-encryption-field="kmsKey"]');
  assert.equal(await encryptionText(page, 'object'), 'SSE-KMS (aws:kms)');
  await closeWindow(page);
  const older = (await apiVersions(page, instance, key)).find(version => !version.isCurrent && !version.deleteMarker);
  const response = await page.request.post(`${BASE}api/versions/restore`, { data: { instance, key, version: older.version } });
  assert.equal(response.status(), 200, await response.text());
  assert.equal((await assertSSE(instance, key, 'aws:kms'))['x-amz-server-side-encryption-aws-kms-key-id'], edited['x-amz-server-side-encryption-aws-kms-key-id']);
  assert.deepEqual(page.errors, []);
  await deleteEncrypted(page, instance, key);
  await page.context().close();
});

test('encryption: an upload to an instance without encryption gives no encryption section', async () => {
  const page = await newPage();
  const info = await lockedHostOf(page, 'seaweedfs-main');
  const name = `plain-${encryptionStamp()}.txt`;
  await open(page, `-/${info.host}/seaweedfs-main/enc/`);
  await uploadWithPage(page, name, Buffer.from('uploaded to the bucket without encryption\n'));
  assert.equal((await s3Head('seaweedfs-main', `/default/enc/${name}`))['x-amz-server-side-encryption'], undefined);
  await openDetailsWithoutEncryption(page, name);
  assert.deepEqual(page.errors, []);
  await deleteEncrypted(page, 'seaweedfs-main', `enc/${name}`);
  await page.context().close();
});

for (const instance of ['seaweedfs-main', 'minio-main']) {
  test(`encryption: ${PROVIDERS[instance].name} shows the default encryption of the bucket, and no row for an object that has none`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    assert.equal(info.bucketEncryptionSupported, true);
    const before = await (await page.request.get(`${BASE}api/encryption?instance=${instance}`)).json();
    assert.equal(before.uploads, '');
    assert.equal(before.bucketDefault, null);
    await s3(instance, 'PUT', '/default', 'encryption=', Buffer.from(ENCRYPTION_XML));
    try {
      const answer = await (await page.request.get(`${BASE}api/encryption?instance=${instance}`)).json();
      assert.equal(answer.bucketDefault.algorithm, 'AES256');
      await open(page, `-/${info.host}/${instance}/smoke/`);
      await openDetails(page, 'hello.txt');
      await page.waitForSelector('[data-encryption-section] [data-encryption-field="default"]');
      assert.equal(await encryptionText(page, 'default'), 'SSE-S3 (AES256)');
      // The object was stored before the default: the provider reports no encryption for it.
      assert.equal(await page.locator('[data-encryption-field="object"]').count(), 0);
      assert.equal(await page.locator('[data-encryption-note]').count(), 0);
      assert.deepEqual(page.errors, []);
    } finally {
      await s3(instance, 'DELETE', '/default', 'encryption=');
    }
    // The bucket ends as it started: no default, and so no section.
    assert.equal((await (await page.request.get(`${BASE}api/encryption?instance=${instance}`)).json()).bucketDefault, null);
    await page.reload({ waitUntil: 'networkidle' });
    await open(page, `-/${info.host}/${instance}/smoke/`);
    await openDetailsWithoutEncryption(page, 'hello.txt');
    await page.context().close();
  });
}

test('encryption: MinIO shows the SSE-KMS of an object with its KMS key', async () => {
  const page = await newPage();
  const info = await lockedHostOf(page, 'minio-main');
  await open(page, `-/${info.host}/minio-main/enc/`);
  await openEncryptionDetails(page, 'kms.txt');
  assert.equal(await encryptionText(page, 'object'), 'SSE-KMS (aws:kms)');
  assert.match(await encryptionText(page, 'kmsKey'), /^arn:aws:kms:stowr-test/);
  assert.equal(await page.locator('[data-encryption-note]').count(), 0);
  // The object has a normal HEAD answer, so the metadata section stays.
  await page.waitForSelector('[data-metadata-section] .metaEditor__view');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

for (const instance of ['seaweedfs-main', 'garage-main']) {
  test(`encryption: ${PROVIDERS[instance].name} opens the details of an object with a key of the client (SSE-C), and never asks for the key`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    // The provider refuses the HEAD with 400: the server answers 200 and says that the key is the key of the client.
    const response = await page.request.get(`${BASE}api/media-info?instance=${instance}&key=${encodeURIComponent('enc/customer-key.txt')}`);
    assert.equal(response.status(), 200, await response.text());
    const media = await response.json();
    assert.equal(media.customerKey, true);
    assert.equal(media.size, 'An object with a key of the client.\n'.length);
    assert.ok(!JSON.stringify(media).includes('customer-key-md5'), 'the answer has no hash of the key');
    await open(page, `-/${info.host}/${instance}/enc/`);
    await openDetails(page, 'customer-key.txt');
    await page.waitForSelector('[data-encryption-section] [data-encryption-field="object"]');
    assert.match(await encryptionText(page, 'object'), /^Key of the client/);
    assert.match(await page.textContent('[data-encryption-note]'), /key of the client/);
    // The size comes from the listing: the section "Storage object" has it.
    const storage = await page.locator('.bb-details-section', { hasText: 'Storage object' }).innerText();
    assert.match(storage.replace(/\s+/g, ' '), /Size 36 B/);
    assert.equal(await page.locator('[data-metadata-section]').count(), 0);
    // The page has no field for a key.
    assert.equal(await page.locator('.bb-details--file input[type="password"], [data-encryption-section] input, [data-encryption-section] textarea').count(), 0);
    assert.equal(await visibleText(page, 'customer-key-md5'), 0);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('encryption: Garage accepts an upload with encryption = "sse-s3" and shows no encryption; garage-main has no default row', async () => {
  const page = await newPage();
  const instances = (await (await page.request.get(`${BASE}api/instances`)).json()).instances;
  const flags = Object.fromEntries(instances.map(item => [item.id, item.bucketEncryptionSupported]));
  assert.deepEqual(flags, {
    'seaweedfs-main': true, 'seaweedfs-archive': true, 'seaweedfs-locked': true, 'seaweedfs-encrypted': true,
    'minio-main': true, 'minio-locked': true, 'minio-encrypted': true,
    'garage-main': false, 'garage-encrypted': false, files: false,
  });
  for (const instance of ['garage-main', 'garage-encrypted']) {
    const response = await page.request.get(`${BASE}api/encryption?instance=${instance}`);
    assert.equal(response.status(), 501, instance);
    assert.equal((await response.json()).error.code, 'bucket_encryption_unsupported');
  }
  const info = instances.find(item => item.id === 'garage-encrypted');
  const name = `garage-${encryptionStamp()}.txt`;
  await open(page, `-/${info.host}/garage-encrypted/enc/`);
  await uploadWithPage(page, name, Buffer.from('uploaded to Garage\n'));
  assert.equal((await getObject(page, 'garage-encrypted', `enc/${name}`)).body.toString(), 'uploaded to Garage\n');
  // Garage does not encrypt: the HEAD says nothing, so the details have no section (and no default row).
  assert.equal((await s3Head('garage-main', `/default/enc/${name}`))['x-amz-server-side-encryption'], undefined);
  await openDetailsWithoutEncryption(page, name);
  await closeWindow(page);
  await deleteEncrypted(page, 'garage-encrypted', `enc/${name}`);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The preview page of an object with a key of the client: the name and the path in the bar, and the message. The
// link to the original and the download give the message too.
const CUSTOMER_KEY_TEXT = 'This object is encrypted with a key of the client (SSE-C). The browser cannot read it without that key.';

for (const instance of ['seaweedfs-main', 'garage-main']) {
  test(`encryption: ${PROVIDERS[instance].name}: the preview, the link to the original and the download of an SSE-C object say why they cannot read it`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    await page.goto(`${BASE}-/${info.host}/${instance}/enc/customer-key.txt`, { waitUntil: 'networkidle' });
    await page.waitForSelector('.preview-error');
    assert.equal(await page.textContent('#docName'), 'customer-key.txt');
    assert.equal(await page.textContent('#docPrefix'), ' / enc');
    assert.equal(await page.textContent('.preview-error strong'), 'Encrypted with a key of the client');
    assert.equal(await page.textContent('.preview-error p'), CUSTOMER_KEY_TEXT);
    assert.doesNotMatch(await page.textContent('#viewer'), /HTTP 400/);
    assert.match(await page.title(), /^customer-key\.txt - /);
    // The error and the bar show the key of the encryption, with the reason in its tooltip.
    assert.equal(await page.locator('.preview-error .mdi-key').count(), 1);
    assert.deepEqual(await previewMarks(page), { encryption: 'Encrypted with a key of the client (SSE-C)' });
    // The link to the original opens no page: it gives the message.
    let pages = 0;
    page.context().on('page', () => { pages += 1; });
    await page.click('#openRawBtn');
    await page.waitForSelector('.bb-toast.is-error');
    assert.match(await page.textContent('.bb-toast.is-error'), /key of the client \(SSE-C\)\. The browser cannot read it/);
    assert.equal(pages, 0);
    await page.evaluate(() => document.querySelectorAll('.bb-toast').forEach(node => node.remove()));
    // The download of the page.
    await page.click('#previewKebabBtn');
    await page.click('#pv-download');
    await page.waitForSelector('.bb-toast.is-error');
    assert.match(await page.textContent('.bb-toast.is-error'), /key of the client \(SSE-C\)/);
    assert.equal(await page.locator('.bb-toast >> text=Resumable download was unavailable').count(), 0);
    // The download of the listing.
    await open(page, `-/${info.host}/${instance}/enc/`);
    await page.click('tr[data-name="customer-key.txt"] .bb-kebab');
    await page.click('.bb-menu.is-open .bb-menu-item >> text=Download');
    await page.waitForSelector('.bb-toast.is-error');
    assert.match(await page.textContent('.bb-toast.is-error'), /key of the client \(SSE-C\)/);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('encryption: an other answer 400 of the provider is not an SSE-C object: the preview names the error', async () => {
  const page = await newPage();
  await page.route(/\/s3\?/, route => (route.request().method() === 'HEAD' && route.request().url().includes('hello.txt') ? route.fulfill({ status: 400, body: '' }) : route.continue()));
  await page.goto(`${BASE}${MAIN}smoke/hello.txt`, { waitUntil: 'networkidle' });
  await page.waitForSelector('.preview-error');
  assert.equal(await page.textContent('#docName'), 'hello.txt');
  assert.equal(await page.textContent('.preview-error strong'), 'Preview unavailable');
  assert.match(await page.textContent('.preview-error p'), /HTTP 400/);
  assert.equal(await page.locator('.preview-error .mdi-key').count(), 0);
  assert.deepEqual(await previewMarks(page), {});
  await page.context().close();
});

// The key of a bucket in the bucket selector: the state icon "encryption" of the trigger or of an entry of the menu.
const bucketLockOf = (page, scope) => page.evaluate(selector => {
  const node = document.querySelector(`${selector} [data-state-icon="encryption"]`);
  return node ? { icon: node.querySelector('.ic').className, text: node.querySelector('.visually-hidden').textContent, tone: node.dataset.tone } : null;
}, scope);

test('encryption: the bucket selector shows a key for a bucket that encrypts, and a key in the colour of a warning for Garage, which does not', async () => {
  const page = await newPage();
  const instances = (await (await page.request.get(`${BASE}api/instances`)).json()).instances;
  const facts = Object.fromEntries(instances.map(item => [item.id, [item.encryption, item.defaultEncryption, item.encryptionApplied || '']]));
  assert.deepEqual(facts['seaweedfs-encrypted'], ['sse-s3', '', 'yes']);
  assert.deepEqual(facts['minio-encrypted'], ['sse-s3', '', 'yes']);
  assert.deepEqual(facts['garage-encrypted'], ['sse-s3', '', 'no']);
  assert.deepEqual(facts['seaweedfs-main'], ['', '', '']);
  await open(page, MAIN);
  for (const instance of ['seaweedfs-encrypted', 'minio-encrypted']) {
    assert.deepEqual(await bucketLockOf(page, `.storageBucket__row[data-instance="${instance}"]`), { icon: 'ic ic-key', text: 'Encryption: on (SSE-S3). ', tone: 'on' }, instance);
  }
  for (const instance of ['seaweedfs-main', 'minio-main', 'garage-main', 'seaweedfs-locked']) {
    assert.deepEqual(await bucketLockOf(page, `.storageBucket__row[data-instance="${instance}"]`), { icon: 'ic ic-key', text: 'Encryption: off. ', tone: 'off' }, instance);
  }
  assert.deepEqual(await bucketLockOf(page, '.storageBucket__row[data-instance="garage-encrypted"]'), { icon: 'ic ic-key', text: 'Encryption: requested but not applied by this provider. ', tone: 'warn' });
  assert.equal((await bucketLockOf(page, '#storageSwitcherInfo')).tone, 'off');
  // The trigger of the selector shows the key of the open bucket; the information page says that Garage does not apply it.
  const info = instances.find(item => item.id === 'seaweedfs-encrypted');
  await open(page, `-/${info.host}/seaweedfs-encrypted/`);
  assert.equal((await bucketLockOf(page, '#storageSwitcherInfo')).tone, 'on');
  await page.goto(`${BASE}-/garage/garage-encrypted/?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-row="encryption-applied"]');
  assert.equal(await page.getAttribute('[data-row="encryption-applied"] .infoChip', 'data-tone'), 'warn');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// Counts the HEAD requests of the page to the objects, and the most of them at the same time.
async function watchHeads(page, delay = 0) {
  const stats = { total: 0, active: 0, max: 0, keys: [] };
  await page.route(/\/s3\?/, async route => {
    if (route.request().method() !== 'HEAD') return route.continue();
    stats.total += 1;
    stats.active += 1;
    stats.max = Math.max(stats.max, stats.active);
    stats.keys.push(new URL(route.request().url()).searchParams.get('key'));
    try {
      if (delay) await sleep(delay);
      await route.continue();
    } finally {
      stats.active -= 1;
    }
  });
  return stats;
}

const noLockPending = page => until(page, () => !document.querySelector('.nameLock__pending'));
const fileLock = (page, name) => page.evaluate(row => document.querySelector(`tr[data-name="${row}"] .nameLock__icon[data-mark="encryption"]`)?.title || '', name);

for (const [instance, provider] of Object.entries(ENCRYPTED)) {
  test(`encryption: ${provider.name}: the listing shows a lock next to the name of an encrypted file, by default, and no new column`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const stamp = encryptionStamp();
    const plain = `enc/plain-${stamp}.txt`;
    await putPlain(instance, plain, 'plain on the provider\n');
    await open(page, `-/${info.host}/${instance}/enc/`);
    const columns = await page.locator('.dataTable thead th').count();
    await uploadWithPage(page, `locked-${stamp}.txt`, Buffer.from('uploaded by the page\n'));
    await noLockPending(page);
    assert.equal(await fileLock(page, `locked-${stamp}.txt`), 'Encrypted: SSE-S3');
    assert.equal(await fileLock(page, `plain-${stamp}.txt`), '');
    // The lock is in the cell of the name, at the right of the name.
    const place = await page.evaluate(row => {
      const name = document.querySelector(`tr[data-name="${row}"] .nameLink`).getBoundingClientRect();
      const lock = document.querySelector(`tr[data-name="${row}"] .nameLock__icon`).getBoundingClientRect();
      return { name: name.right, lock: lock.left, cell: document.querySelector(`tr[data-name="${row}"] .nameLock`).closest('td') === document.querySelector(`tr[data-name="${row}"] td`) };
    }, `locked-${stamp}.txt`);
    assert.ok(place.cell && place.lock >= place.name, JSON.stringify(place));
    assert.equal(await page.locator('.dataTable thead th').count(), columns);
    await page.click('#settingsButton');
    assert.equal(await page.isChecked('#encryptionToggle'), true);
    assert.deepEqual(page.errors, []);
    await deleteEncrypted(page, instance, plain, `enc/locked-${stamp}.txt`);
    await page.context().close();
  });
}

test('encryption: the setting is on by default for seaweedfs-main, goes off for one bucket, stays in the browser, and shows SSE-C and SSE-KMS', async () => {
  const page = await newPage();
  const heads = await watchHeads(page);
  await open(page, `${MAIN}enc/`);
  // An instance that does not encrypt reads the files too: the setting is on for each bucket by default.
  await noLockPending(page);
  // The HEAD of an SSE-C object fails with 400: the details confirm it.
  assert.equal(await fileLock(page, 'customer-key.txt'), 'Encrypted with a key of the client (SSE-C)');
  const files = await page.locator('.dataTable tbody tr[data-type="content"]').count();
  assert.ok(heads.total >= 1 && heads.total <= files, `${heads.total} HEAD for ${files} files`);
  await page.click('#settingsButton');
  assert.equal(await page.isChecked('#encryptionToggle'), true);
  await page.uncheck('#encryptionToggle');
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('.nameLock__icon, .nameLock__pending').count(), 0);
  // The choice stays after a reload, for this bucket only: no new request.
  const before = heads.total;
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('.dataTable');
  await sleep(500);
  assert.equal(heads.total, before);
  assert.equal(await page.locator('.nameLock__icon, .nameLock__pending').count(), 0);
  // MinIO gives the key of an SSE-KMS object. Its bucket keeps the default.
  const minio = await lockedHostOf(page, 'minio-main');
  await open(page, `-/${minio.host}/minio-main/enc/`);
  await noLockPending(page);
  assert.match(await fileLock(page, 'kms.txt'), /^Encrypted: SSE-KMS, key arn:aws:kms:stowr-test/);
  // The reset gives the default again.
  await open(page, `${MAIN}enc/`);
  await page.click('#settingsButton');
  await page.click('#settingsReset');
  await noLockPending(page);
  assert.equal(await fileLock(page, 'customer-key.txt'), 'Encrypted with a key of the client (SSE-C)');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('encryption: the locks of the listing read at most 100 files, 4 at the same time, and read a file one time', async () => {
  const page = await newPage();
  const prefix = `encmany-${Date.now()}/`;
  for (let start = 0; start < 120; start += 10) {
    await Promise.all(Array.from({ length: 10 }, (_, index) => s3('seaweedfs-encrypted', 'PUT', `/encrypted/${prefix}file-${String(start + index).padStart(3, '0')}.txt`, '', Buffer.from('x'), { 'content-type': 'text/plain' })));
  }
  const info = await lockedHostOf(page, 'seaweedfs-encrypted');
  const heads = await watchHeads(page, 60);
  await open(page, `-/${info.host}/seaweedfs-encrypted/${prefix}`);
  await noLockPending(page);
  // One page of 50 rows: 50 requests, 4 at most at the same time.
  assert.equal(heads.total, 50);
  assert.ok(heads.max <= 4, `max ${heads.max}`);
  assert.equal(new Set(heads.keys).size, 50);
  // A new drawing of the page (the exact times) and a new listing read nothing again.
  await page.click('.timeToggle');
  await page.click('#refreshButton');
  await page.waitForSelector('.tableBox:not(.is-busy)');
  await sleep(800);
  assert.equal(heads.total, 50);
  // 500 rows on a page: the page reads the first 100 rows only, and the 50 rows that it read stay.
  await page.selectOption('.tableFoot select', '500');
  await noLockPending(page);
  assert.equal(heads.total, 100);
  assert.equal(new Set(heads.keys).size, 100);
  assert.ok(heads.max <= 4, `max ${heads.max}`);
  assert.equal(await page.locator('tbody tr:nth-child(n+101) .nameLock__pending, tbody tr:nth-child(n+101) .nameLock__icon').count(), 0);
  assert.deepEqual(page.errors, []);
  await deletePrefix(page, 'seaweedfs-encrypted', prefix);
  await page.context().close();
});

test('encryption: Garage: the setting is on for garage-encrypted, and shows no lock, because Garage does not encrypt', async () => {
  const page = await newPage();
  const info = await lockedHostOf(page, 'garage-encrypted');
  const heads = await watchHeads(page);
  await open(page, `-/${info.host}/garage-encrypted/enc/`);
  await page.click('#settingsButton');
  assert.equal(await page.isChecked('#encryptionToggle'), true);
  await page.keyboard.press('Escape');
  const name = `garage-lock-${encryptionStamp()}.txt`;
  await uploadWithPage(page, name, Buffer.from('uploaded to Garage\n'));
  await noLockPending(page);
  assert.ok(heads.total >= 1);
  assert.equal(await fileLock(page, name), '');
  // The SSE-C object of Garage has its lock.
  assert.equal(await fileLock(page, 'customer-key.txt'), 'Encrypted with a key of the client (SSE-C)');
  assert.deepEqual(page.errors, []);
  await deleteEncrypted(page, 'garage-encrypted', `enc/${name}`);
  await page.context().close();
});

test('encryption: a default encryption of the bucket gives the lock of the bucket and starts the locks of the files', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const instance of body.instances) if (instance.id === 'seaweedfs-main') instance.defaultEncryption = 'AES256';
    await route.fulfill({ response, json: body });
  });
  const heads = await watchHeads(page);
  await open(page, `${MAIN}enc/`);
  assert.deepEqual(await bucketLockOf(page, '#storageSwitcherInfo'), { icon: 'ic ic-key', text: 'Encryption: on (SSE-S3). ', tone: 'on' });
  await noLockPending(page);
  assert.ok(heads.total >= 1);
  assert.equal(await fileLock(page, 'customer-key.txt'), 'Encrypted with a key of the client (SSE-C)');
  await page.click('#settingsButton');
  assert.equal(await page.isChecked('#encryptionToggle'), true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The marks of a file in the listing: the key of the encryption and the padlock of Object Lock, with their tooltips.
const fileMarks = (page, name) => page.evaluate(row => Object.fromEntries(Array.from(document.querySelectorAll(`tr[data-name="${row}"] .nameLock__icon`))
  .map(mark => [mark.dataset.mark, mark.title])), name);

// The marks of the version in the bar of the preview, with their tooltips. The bar is hidden without a mark.
const previewMarks = page => page.evaluate(() => {
  const host = document.querySelector('#docMarks');
  return host.hidden ? {} : Object.fromEntries(Array.from(host.querySelectorAll('[data-mark]')).map(mark => [mark.dataset.mark, mark.title]));
});

// Waits for the end of the load of the preview: the bar then has the marks of the version.
const previewReady = page => until(page, () => Boolean(document.querySelector('#viewer')) && !document.querySelector('#viewer').classList.contains('preview-loading'));

// The tooltip of a lock in the listing: the mode, and the retain-until date in UTC to the minute.
const lockTooltip = (mode, until) => `Object Lock: ${mode} until ${new Date(until).toISOString().slice(0, 16).replace('T', ' ')} UTC`;

for (const [instance, provider] of Object.entries(LOCKED)) {
  test(`object lock: ${provider.name}: the listing and the preview show the lock of each file next to its name, from the HEAD of the encryption, and the bucket has its padlock`, async () => {
    const page = await newPage({ timezoneId: 'UTC' });
    const info = await lockedHostOf(page, instance);
    const prefix = `marks-${encryptionStamp()}/`;
    const names = { hold: 'hold.txt', governance: 'governance.txt', compliance: 'compliance.txt', plain: 'plain.txt', both: 'both.txt', ended: 'ended.txt' };
    for (const name of Object.values(names)) {
      const extra = name === names.both ? { 'content-type': 'text/plain', 'x-amz-server-side-encryption': 'AES256' } : { 'content-type': 'text/plain' };
      await s3(instance, 'PUT', `/locked/${prefix}${name}`, '', Buffer.from(`${name}\n`), extra);
    }
    const versionOf = async name => (await apiVersions(page, instance, `${prefix}${name}`)).find(version => version.isCurrent).version;
    const lock = async (name, change) => {
      const answer = await putLock(page, instance, `${prefix}${name}`, await versionOf(name), change);
      assert.equal(answer.status(), 200, await answer.text());
    };
    const governanceUntil = `${lockField(90)}Z`;
    const complianceUntil = `${lockField(45)}Z`;
    await lock(names.hold, { legalHold: true });
    await lock(names.governance, { retention: { mode: 'GOVERNANCE', retainUntil: governanceUntil } });
    await lock(names.compliance, { retention: { mode: 'COMPLIANCE', retainUntil: complianceUntil } });
    await lock(names.both, { legalHold: true, retention: { mode: 'GOVERNANCE', retainUntil: governanceUntil } });
    const endedUntil = `${lockField(3)}Z`;
    await lock(names.ended, { retention: { mode: 'GOVERNANCE', retainUntil: endedUntil } });
    // The provider encrypts the object of the two marks.
    assert.equal((await s3Head(instance, `/locked/${prefix}${names.both}`))['x-amz-server-side-encryption'], 'AES256');
    while (Date.now() <= Date.parse(endedUntil) + 500) await sleep(250);
    // The bucket has Object Lock and no default encryption: the marks show by default.
    const heads = await watchHeads(page);
    let lockRequests = 0;
    page.on('request', request => { if (request.url().includes('/api/object-lock')) lockRequests += 1; });
    await open(page, `-/${info.host}/${instance}/${prefix}`);
    await noLockPending(page);
    assert.deepEqual(await fileMarks(page, names.hold), { 'object-lock': 'Object Lock: legal hold' });
    assert.deepEqual(await fileMarks(page, names.governance), { 'object-lock': lockTooltip('Governance', governanceUntil) });
    assert.deepEqual(await fileMarks(page, names.compliance), { 'object-lock': lockTooltip('Compliance', complianceUntil) });
    assert.deepEqual(await fileMarks(page, names.plain), {});
    assert.deepEqual(await fileMarks(page, names.ended), {}, 'a retention whose date is past');
    assert.deepEqual(await fileMarks(page, names.both), { encryption: 'Encrypted: SSE-S3', 'object-lock': `${lockTooltip('Governance', governanceUntil)}, legal hold` });
    // The key of the encryption and the padlock of the lock, at the right of the name, in one cell.
    const look = await page.evaluate(row => {
      const cell = document.querySelector(`tr[data-name="${row}"] .nameCell`);
      const [key, padlock] = cell.querySelectorAll('.nameLock__icon');
      return { key: key.querySelector('.ic').className, padlock: padlock.querySelector('.ic').className, order: cell.querySelector('.nameLink').getBoundingClientRect().right <= key.getBoundingClientRect().left && key.getBoundingClientRect().right <= padlock.getBoundingClientRect().left };
    }, names.both);
    assert.deepEqual(look, { key: 'ic ic-key', padlock: 'ic ic-lock', order: true });
    // One HEAD for each file, and no other request for the lock.
    assert.equal(heads.total, Object.keys(names).length);
    assert.equal(new Set(heads.keys).size, Object.keys(names).length);
    assert.equal(lockRequests, 0);
    // The bucket of the selector has its padlock. The bucket "default" of the same provider has none.
    assert.equal(await page.getAttribute('#storageSwitcherInfo [data-state-icon="object-lock"]', 'data-tone'), 'on');
    assert.equal(await page.locator('#storageSwitcherInfo [data-state-icon="object-lock"] .ic-lock').count(), 1);
    assert.equal(await page.getAttribute('#storageSwitcherInfo [data-state-icon="encryption"]', 'data-tone'), 'off');
    // The preview shows the same marks next to the size, from its own HEAD, with no request for the lock.
    for (const [name, marks] of [[names.both, { encryption: 'Encrypted: SSE-S3', 'object-lock': `${lockTooltip('Governance', governanceUntil)}, legal hold` }],
      [names.compliance, { 'object-lock': lockTooltip('Compliance', complianceUntil) }], [names.plain, {}], [names.ended, {}]]) {
      const preview = await newPage({ timezoneId: 'UTC' });
      preview.on('request', request => { if (request.url().includes('/api/object-lock')) lockRequests += 1; });
      await preview.goto(`${BASE}-/${info.host}/${instance}/${prefix}${name}`, { waitUntil: 'networkidle' });
      await previewReady(preview);
      assert.deepEqual(await previewMarks(preview), marks, name);
      assert.deepEqual(preview.errors, []);
      await preview.context().close();
    }
    assert.equal(lockRequests, 0);
    // A new load of the listing reads each file again, one time: the answers stay only in the page.
    await open(page, `-/${info.host}/${instance}/${prefix}`);
    await noLockPending(page);
    const files = Object.keys(names).length;
    assert.equal(heads.total, 2 * files);
    // The setting stops the marks and the requests, and starts them again from the answers that the page has.
    await page.click('#settingsButton');
    assert.equal((await page.textContent('#encryptionSetting')).trim(), 'Encryption and lock');
    assert.equal((await page.textContent('#settingsReset')).trim(), 'Reset to defaults');
    assert.match(await page.getAttribute('#settingsReset', 'title'), /columns.*tag filter.*encryption and lock/);
    assert.equal(await page.isChecked('#encryptionToggle'), true);
    await page.uncheck('#encryptionToggle');
    await page.keyboard.press('Escape');
    await until(page, () => !document.querySelector('.nameLock__icon'));
    await page.click('#refreshButton');
    await page.waitForSelector('.tableBox:not(.is-busy)');
    await sleep(500);
    assert.equal(await page.locator('.nameLock__icon, .nameLock__pending').count(), 0);
    assert.equal(heads.total, 2 * files);
    await page.click('#settingsButton');
    await page.check('#encryptionToggle');
    await page.keyboard.press('Escape');
    await noLockPending(page);
    assert.deepEqual(await fileMarks(page, names.hold), { 'object-lock': 'Object Lock: legal hold' });
    assert.equal(heads.total, 2 * files);
    // A change of the lock in the details: the listing reads the file again.
    await openLockDetails(page, names.hold);
    await page.click('[data-lock-edit]');
    await page.uncheck('[data-lock-legal-hold]');
    await saveLock(page);
    await closeWindow(page);
    await until(page, name => !document.querySelector(`tr[data-name="${name}"] .nameLock__icon`) && !document.querySelector('.nameLock__pending'), names.hold);
    assert.equal(heads.total, 2 * files + 1);
    // The bucket "default" of the provider has no Object Lock: no padlock.
    const main = await lockedHostOf(page, provider.provider);
    await open(page, `-/${main.host}/${provider.provider}/`);
    assert.equal(await page.getAttribute('#storageSwitcherInfo [data-state-icon="object-lock"]', 'data-tone'), 'off');
    assert.deepEqual(page.errors, []);
    // The end: the hold goes, the Governance retention goes with the bypass, the Compliance retention ends.
    await lock(names.both, { legalHold: false });
    for (const name of [names.governance, names.both]) {
      await lock(name, { retention: { mode: 'GOVERNANCE', retainUntil: `${lockField(2)}Z` }, bypassGovernance: true });
    }
    const end = Math.max(Date.parse(complianceUntil), Date.now() + 2500) + 500;
    while (Date.now() <= end) await sleep(500);
    for (const name of Object.values(names)) await purgeVersions(page, instance, `${prefix}${name}`);
    for (const name of Object.values(names)) assert.equal(await versionCount(page, instance, `${prefix}${name}`), 0, name);
    await page.context().close();
  });
}

test('object lock: the padlock of a bucket gives its default retention, and a bucket without Object Lock has none', async () => {
  const page = await newPage();
  await page.route(/\/api\/instances/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const instance of body.instances) if (instance.id === 'seaweedfs-locked') instance.objectLockDefault = { mode: 'GOVERNANCE', days: 30 };
    await route.fulfill({ response, json: body });
  });
  const info = await lockedHostOf(page, 'seaweedfs-locked');
  await open(page, `-/${info.host}/seaweedfs-locked/`);
  assert.equal(await page.getAttribute('#storageSwitcherInfo [data-state-icon="object-lock"]', 'data-tone'), 'on');
  assert.equal(await page.textContent('#storageSwitcherInfo [data-state-icon="object-lock"] .visually-hidden'), 'Object Lock: on, default retention Governance, 30 days. ');
  await page.goto(`${BASE}-/${info.host}/seaweedfs-locked/?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-row="object-lock"]');
  assert.match(await page.textContent('[data-row="object-lock"]'), /On.*Governance, 30 days/s);
  await open(page, MAIN);
  assert.equal(await page.getAttribute('#storageSwitcherInfo [data-state-icon="object-lock"]', 'data-tone'), 'off');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

for (const [instance, provider] of Object.entries(ENCRYPTED)) {
  test(`encryption: ${provider.name}: the preview and the details show the encryption of the version on the page, and a version without encryption has no key`, async () => {
    const page = await newPage();
    const info = await lockedHostOf(page, instance);
    const key = `enc/versions-${encryptionStamp()}.txt`;
    const name = key.split('/').pop();
    // Version 1 without encryption, version 2 with SSE-S3, both written on the provider.
    await putPlain(instance, key, 'version 1\n');
    await sleep(1100);
    await s3(instance, 'PUT', encryptedPath(instance, key), '', Buffer.from('version 2\n'), { 'content-type': 'text/plain', 'x-amz-server-side-encryption': 'AES256' });
    const versions = await apiVersions(page, instance, key);
    const first = versions.find(version => !version.isCurrent && !version.deleteMarker);
    const second = versions.find(version => version.isCurrent);
    assert.ok(first && second, JSON.stringify(versions));
    const heads = await watchHeads(page);
    for (const [query, marks] of [[`?version=${encodeURIComponent(first.version)}`, {}], [`?version=${encodeURIComponent(second.version)}`, { encryption: 'Encrypted: SSE-S3' }], ['', { encryption: 'Encrypted: SSE-S3' }]]) {
      const before = heads.total;
      await page.goto(`${BASE}-/${info.host}/${instance}/${key}${query}`, { waitUntil: 'networkidle' });
      await previewReady(page);
      assert.deepEqual(await previewMarks(page), marks, query);
      assert.equal(heads.total - before, 1, `one HEAD for ${query || 'the current version'}`);
    }
    // The version picker of the preview changes the version, and the marks follow.
    await page.click('#previewVersionControl .versionPicker__button');
    await page.click(`.versionPicker__item[data-version="${first.version}"] .versionPicker__choose`);
    await until(page, version => new URLSearchParams(location.search).get('version') === version, first.version);
    await previewReady(page);
    assert.deepEqual(await previewMarks(page), {});
    // The details of each version: the section Encryption for version 2, no section for version 1.
    await open(page, `-/${info.host}/${instance}/enc/`);
    await openEncryptionDetails(page, name);
    assert.equal(await encryptionText(page, 'object'), 'SSE-S3 (AES256)');
    await page.click('.bb-details-head .versionPicker__button');
    await page.click(`.versionPicker__item[data-version="${first.version}"] .versionPicker__choose`);
    await until(page, () => /v1/.test(document.querySelector('.bb-details-head .versionPicker__button')?.textContent || ''));
    await page.waitForSelector('[data-details-tool-card="integrity"]');
    await until(page, () => !document.querySelector('[data-encryption-section]'));
    assert.deepEqual(page.errors, []);
    await purgeVersions(page, instance, key);
    await page.context().close();
  });
}

// ------------------------------------------------------------------------------------------------------------------
// 12. Usage and quota of a bucket: the facts of each bucket in the bucket selector (versioning, encryption, Object
// Lock, lifecycle rules and usage), the warning of a full bucket, the usage in the tooltips of the selector, and the
// summary strip of the details with its sections that close. The usage blocks of config.hcl read the admin API of each
// provider. The bucket "default" of each provider has a quota of 20 GiB (Garage also 100000 objects);
// seaweedfs-archive has quota_bytes = 10 GiB; the other buckets have no quota.
// ------------------------------------------------------------------------------------------------------------------

const QUOTA_BYTES = 21474836480;
const FACTS = '.storageSwitcher__facts';
const usageItem = page => page.locator(`${FACTS} [data-fact="usage"]`);
const factNames = (page, scope = FACTS) => page.locator(`${scope} .bucketFact`).evaluateAll(nodes => nodes.map(node => node.dataset.fact));
const apiUsage = async (page, instance, fresh = false) => (await page.request.get(`${BASE}api/usage?instance=${instance}${fresh ? '&fresh=1' : ''}`)).json();
// An entry of the menu: the button of the bucket and, next to it, the link of its state icons and its usage.
const entry = instance => `.storageBucket__row[data-instance="${instance}"]`;
const entryFacts = (page, instance) => factNames(page, `${entry(instance)} .bucketFacts`);
// The state of a state icon: its tone (on, warn, off) and its word.
const stateOf = (page, scope, key) => page.locator(`${scope} [data-state-icon="${key}"]`).evaluate(node => ({ tone: node.dataset.tone, text: node.querySelector('.visually-hidden').textContent }));

// Opens the menu of the selector with every host open, and waits for the usage of each bucket.
async function openAllBuckets(page) {
  await page.click('.storageSwitcher__trigger strong');
  while (await page.locator('.storageHost__row[aria-expanded="false"]').count()) await page.click('.storageHost__row[aria-expanded="false"]');
  await until(page, () => document.querySelectorAll('.storageBucket__info [data-fact="usage"]').length === document.querySelectorAll('.storageBucket').length
    && !document.querySelector('.storageBucket__info [data-level="loading"]'), null, 20000);
}

for (const [instance, provider] of Object.entries(PROVIDERS)) {
  test(`usage: ${provider.name}: the selector shows the amount, the bar and the percent, and the information page gives what the bucket holds, its quota and the storage`, async () => {
    const page = await newPage({ viewport: { width: 1440, height: 900 } });
    const answer = await apiUsage(page, instance);
    assert.equal(answer.supported, true);
    assert.equal(answer.available, true, JSON.stringify(answer));
    assert.equal(answer.limitBytes, QUOTA_BYTES);
    assert.equal(answer.limitSource, 'provider');
    assert.equal(answer.level, 'ok');
    assert.equal(answer.source, instance.split('-')[0]);
    // The answer never names the admin API or its token. A port is the port of an address: a number of the answer (a
    // count of bytes, the nanoseconds of a time) can hold the same digits.
    assert.doesNotMatch(JSON.stringify(answer), /:(3903|9333|8888)\b|admin-token|minio\/admin/);
    await open(page, `-/${instance.split('-')[0]}/${instance}/`);
    await page.waitForSelector(`${FACTS} [data-fact="usage"][data-level="ok"]`);
    // The storage of each provider is a tmpfs of 4 GiB, for all the buckets of the provider. The chip shows the more
    // constraining part: the storage, or the quota of 20 GiB for an equal part (Garage compresses the data that it stores).
    const storageFirst = answer.storage.percent > answer.quotaPercent;
    assert.equal(answer.constraint, storageFirst ? 'storage' : 'quota', JSON.stringify(answer));
    assert.equal(answer.storage.totalBytes, 4 * 1024 ** 3);
    assert.equal(answer.storage.usedBytes + answer.storage.freeBytes <= answer.storage.totalBytes, true);
    const text = (await usageItem(page).locator('.bucketUsage__text').textContent()).trim();
    assert.match(text, storageFirst ? /^Storage: [\d.]+ (B|KB|MB|GB) of 4 GB$/ : /^~?[\d.]+ (B|KB|MB|GB) of 20 GB$/);
    // The amount that the bucket holds comes before the percent; SeaweedFS and MinIO count with a delay: "~".
    assert.match(await usageItem(page).locator('.bucketUsage__amount').textContent(), answer.approximate ? /^~[\d.]+ (B|KB|MB|GB)$/ : /^[\d.]+ (B|KB|MB|GB)$/);
    assert.match(await usageItem(page).locator('.bucketUsage__short').textContent(), /^(<1|~?\d+) %$/);
    assert.equal(await usageItem(page).locator('.usageMeter[role="meter"]').count(), 1);
    // The usage has no tooltip of its own; the tooltip of the trigger has its lines.
    assert.equal(await usageItem(page).getAttribute('title'), null);
    assert.match(await page.getAttribute('.storageSwitcher__trigger', 'title'), /\nusage: ~?[\d.]+ (B|KB|MB|GB) of 20 GB \([\d.]+ %\)\nstorage: [\d.]+ (B|KB|MB|GB) of 4 GB \([\d.]+ %\), [\d.]+ (MB|GB) left$/);
    // The usage is in the trigger of the selector, on the line of the name (a wide window): no chip in the header.
    const box = await page.evaluate(selector => {
      const rect = node => node.getBoundingClientRect();
      const facts = rect(document.querySelector(selector));
      const trigger = rect(document.querySelector('.storageSwitcher__trigger'));
      const name = rect(document.querySelector('.storageSwitcher__trigger strong'));
      return { after: facts.left >= name.right, inside: facts.left > trigger.left && facts.right < trigger.right && facts.top > trigger.top && facts.bottom < trigger.bottom,
        line: Math.abs((facts.top + facts.bottom) / 2 - (name.top + name.bottom) / 2) <= 3, old: document.querySelectorAll('#bucketChips, #bucketInfo, #bucketInfoButton, .bucketChip, #usagePopover').length };
    }, FACTS);
    assert.deepEqual(box, { after: true, inside: true, line: true, old: 0 });
    // The facts of the trigger: the usage only. The versioning is a state icon: on where the bucket has it.
    assert.deepEqual(await factNames(page), ['usage']);
    assert.deepEqual(await stateOf(page, '#storageSwitcherInfo', 'versioning'), provider.versioning
      ? { tone: 'on', text: 'Versioning: on. ' } : { tone: 'off', text: 'Versioning: not available on this provider. ' });
    // The real usage of the test stack is far from the limit: no warning.
    assert.equal(await page.isHidden('#usageBanner'), true);
    assert.equal(await page.locator('.storageSwitcher.is-full, .bucketMark').count(), 0);
    assert.equal(await page.getAttribute('#uploadButton', 'title'), null);
    // The listing has no totals at its foot: only the range of the page and the rows.
    assert.equal(await page.locator('#folderCount, .tableFoot__count, .bucketStatus').count(), 0);
    assert.match(await page.textContent('.tableFoot'), /^\d+ to \d+ of \d+\+?Rows/);
    assert.equal(await visibleText(page, ' folders, '), 0);
    // The information page: what the bucket holds, the quota of 20 GB and the storage of 4 GB, each with its gauge.
    await page.click(`${FACTS} [data-fact="usage"]`);
    await page.waitForURL(/\?info$/);
    await page.waitForSelector('[data-gauge="storage"]');
    const holds = await page.textContent('[data-gauge="holds"]');
    assert.match(holds, answer.approximate ? /This bucket holds.*~[\d.]+ (B|KB|MB|GB)/s : /This bucket holds.*[\d.]+ (B|KB|MB|GB)/s);
    assert.match(holds, new RegExp(`Source${{ 'seaweedfs-main': 'SeaweedFS', 'minio-main': 'MinIO', 'garage-main': 'Garage' }[instance]}`));
    assert.match(await page.textContent('[data-gauge="quota"]'), /Set by the provider.*~?[\d.]+ (B|KB|MB|GB) used of 20 GB/s);
    assert.match(await page.textContent('[data-gauge="storage"]'), /[\d.]+ (B|KB|MB|GB) used of 4 GB.*Free[\d.]+ (MB|GB)/s);
    assert.equal(await page.locator('[data-gauge="quota"] [role="meter"], [data-gauge="storage"] [role="meter"]').count(), 2);
    if (instance === 'garage-main') assert.match(await page.textContent('[data-gauge="quota"]'), /Objects[\d,]+ of 100,000/);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('usage: the state icons of seaweedfs-locked, of the encrypted buckets, and a bucket without quota or without a usage block', async () => {
  const page = await newPage();
  await open(page, '-/seaweedfs/seaweedfs-locked/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"]:not([data-level="loading"])`);
  // A bucket with Object Lock has versioning and its padlock on; it does not encrypt. Its rules: none.
  const states = await page.locator('#storageSwitcherInfo [data-state-icon]').evaluateAll(nodes => Object.fromEntries(nodes.map(node => [node.dataset.stateIcon, node.dataset.tone])));
  assert.deepEqual(states, { read: 'on', write: 'on', delete: 'on', encryption: 'off', 'object-lock': 'on', versioning: 'on', lifecycle: 'off' });
  assert.equal((await stateOf(page, '#storageSwitcherInfo', 'object-lock')).text, 'Object Lock: on. ');
  assert.equal((await stateOf(page, '#storageSwitcherInfo', 'lifecycle')).text, 'Lifecycle rules: none. ');
  // No quota: the chip shows the storage behind the bucket, with its bar.
  assert.match(await usageItem(page).locator('.bucketUsage__text').textContent(), /^Storage: [\d.]+ (B|KB|MB|GB) of 4 GB$/);
  assert.equal(await usageItem(page).locator('.usageMeter').count(), 1);
  assert.equal(await usageItem(page).getAttribute('data-constraint'), 'storage');
  // The key of an encrypted bucket is green; the key of Garage, which does not encrypt, is orange.
  await open(page, '-/seaweedfs/seaweedfs-encrypted/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"]`);
  assert.deepEqual(await stateOf(page, '#storageSwitcherInfo', 'encryption'), { tone: 'on', text: 'Encryption: on (SSE-S3). ' });
  await open(page, '-/garage/garage-encrypted/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"]`);
  assert.deepEqual(await stateOf(page, '#storageSwitcherInfo', 'encryption'), { tone: 'warn', text: 'Encryption: requested but not applied by this provider. ' });
  // A limit of the configuration: the information page says that Stowr sets it.
  await open(page, '-/seaweedfs/seaweedfs-archive/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"]:not([data-level="loading"])`);
  assert.match(await page.getAttribute('.storageSwitcher__trigger', 'title'), /\nusage: ~?[\d.]+ (B|KB|MB|GB) of 10 GB /);
  assert.equal((await apiUsage(page, 'seaweedfs-archive')).limitSource, 'config');
  await page.goto(`${BASE}-/seaweedfs/seaweedfs-archive/?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="quota"]');
  assert.match(await page.textContent('[data-gauge="quota"]'), /Set by Stowr.* of 10 GB/s);

  // An instance without a usage block: no usage fact, no request to /api/usage, and "Not available" on the page.
  const second = await newPage();
  let usageRequests = 0;
  await second.route(/\/api\/instances(\?|$)/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    for (const item of body.instances) item.usageSupported = false;
    await route.fulfill({ response, json: body });
  });
  await second.route(/\/api\/usage\?/, route => { usageRequests += 1; return route.continue(); });
  await open(second, MAIN);
  await second.click('.storageSwitcher__trigger strong');
  await sleep(500);
  assert.equal(await second.locator('[data-fact="usage"]').count(), 0);
  await second.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
  await second.waitForSelector('[data-empty="unsupported"]');
  assert.match(await second.textContent('[data-empty="unsupported"]'), /Not available/);
  assert.equal(usageRequests, 0);
  assert.deepEqual(page.errors, []);
  assert.deepEqual(second.errors, []);
  await page.context().close();
  await second.context().close();
});

test('usage: each entry of the menu has its state icons and its usage, from the server, and no button in its button', async () => {
  const page = await newPage();
  // The server reads the facts of each bucket at start and again after 60 seconds: the seed changed them after the start.
  const deadline = Date.now() + 90000;
  let instances = [];
  for (;;) {
    instances = (await (await page.request.get(`${BASE}api/instances`)).json()).instances;
    const main = instances.find(item => item.id === 'seaweedfs-main');
    if (main.versioningState === 'enabled' && main.lifecycleRules === 1) break;
    assert.ok(Date.now() < deadline, JSON.stringify(main));
    await sleep(2000);
  }
  const byId = Object.fromEntries(instances.map(item => [item.id, item]));
  assert.equal(byId['minio-main'].versioningState, 'enabled');
  assert.equal(byId['garage-main'].versioningState, 'off');
  assert.equal(byId['seaweedfs-locked'].versioningState, 'enabled');
  await open(page, MAIN);
  await openAllBuckets(page);
  const tones = id => page.locator(`${entry(id)} .storageBucket__states [data-state-icon]`).evaluateAll(nodes => Object.fromEntries(nodes.map(node => [node.dataset.stateIcon, node.dataset.tone])));
  // The facts of each entry: the usage only. The rest is in the state icons.
  for (const id of ['seaweedfs-main', 'seaweedfs-locked', 'minio-locked', 'seaweedfs-encrypted', 'garage-main']) assert.deepEqual(await entryFacts(page, id), ['usage'], id);
  assert.equal(await page.textContent(`${entry('seaweedfs-main')} [data-state-icon="lifecycle"] .stateIcon__count`), '1');
  assert.equal((await tones('seaweedfs-main')).lifecycle, 'on');
  for (const id of ['seaweedfs-locked', 'minio-locked']) assert.equal((await tones(id))['object-lock'], 'on', id);
  for (const id of ['seaweedfs-encrypted', 'minio-encrypted']) assert.equal((await tones(id)).encryption, 'on', id);
  assert.equal((await tones('garage-encrypted')).encryption, 'warn');
  for (const id of ['seaweedfs-main', 'seaweedfs-locked', 'seaweedfs-encrypted', 'minio-main', 'minio-locked', 'minio-encrypted']) assert.equal((await tones(id)).versioning, 'on', id);
  for (const id of ['garage-main', 'garage-encrypted']) assert.equal((await tones(id)).versioning, 'off', id);
  // A volume has the three icons of its rights only.
  assert.deepEqual(Object.keys(await tones('files')), ['read', 'write', 'delete']);
  // The more constraining part of each bucket: the storage of 4 GiB, or the quota for an equal part.
  for (const id of ['seaweedfs-main', 'minio-main', 'garage-main', 'seaweedfs-archive', 'minio-locked']) {
    assert.match(await page.textContent(`${entry(id)} .bucketUsage__text`), /^(Storage: [\d.]+ (B|KB|MB|GB) of 4 GB|~?[\d.]+ (B|KB|MB|GB) of (10|20) GB)$/, id);
    assert.equal(await page.locator(`${entry(id)} .usageMeter`).count(), 1, id);
  }
  // No button in a button, and no link in a button.
  assert.equal(await page.locator('.storageBucket button, .storageBucket a, .storageSwitcher__trigger button, .storageSwitcher__trigger a').count(), 0);
  const line = await page.evaluate(() => {
    const row = document.querySelector('.storageBucket__row[data-instance="seaweedfs-locked"]');
    const name = row.querySelector('strong').getBoundingClientRect();
    const facts = row.querySelector('.bucketFacts').getBoundingClientRect();
    return { after: facts.left >= name.right, line: Math.abs(facts.top + facts.height / 2 - name.top - name.height / 2) <= 3, inside: facts.bottom <= row.getBoundingClientRect().bottom };
  });
  assert.deepEqual(line, { after: true, line: true, inside: true });
  // The highlight of the current bucket stays.
  assert.equal(await page.getAttribute('.storageBucket[data-instance="seaweedfs-main"]', 'aria-current'), 'true');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('usage: the menu reads the usage of the buckets at most 4 at a time, with a "…" while it loads, and a failed read shows nothing', async () => {
  const page = await newPage();
  const stats = { active: 0, max: 0, total: 0 };
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  await page.route(/\/api\/usage\?/, async route => {
    const instance = new URL(route.request().url()).searchParams.get('instance');
    stats.total += 1;
    stats.active += 1;
    stats.max = Math.max(stats.max, stats.active);
    try {
      if (instance !== 'seaweedfs-main') await gate;
      if (instance === 'minio-locked') return await route.fulfill({ status: 502, json: { error: { code: 'usage_unavailable', message: 'no answer' } } });
      return await route.continue();
    } finally {
      stats.active -= 1;
    }
  });
  // The reads wait: the page is not idle, so it opens without waiting for the network.
  await page.goto(BASE + MAIN);
  await page.waitForSelector('.dataTable');
  await page.click('.storageSwitcher__trigger strong');
  while (await page.locator('.storageHost__row[aria-expanded="false"]').count()) await page.click('.storageHost__row[aria-expanded="false"]');
  await page.waitForSelector(`${entry('garage-main')} [data-fact="usage"][data-level="loading"]`);
  assert.equal((await page.textContent(`${entry('garage-main')} [data-fact="usage"]`)).trim().startsWith('…'), true);
  await sleep(500);
  assert.ok(stats.max <= 4, JSON.stringify(stats));
  release();
  await until(page, () => !document.querySelector('[data-level="loading"]'), null, 20000);
  assert.ok(stats.max <= 4 && stats.total >= 9, JSON.stringify(stats));
  // A failed read of another bucket shows nothing in its line.
  assert.equal(await page.locator(`${entry('minio-locked')} [data-fact="usage"]`).count(), 0);
  assert.equal(await page.isVisible('#storageSwitcherMenu'), true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('usage: a failed read shows no number, the fact says for a screen reader that the usage is unavailable, and the page keeps working', async () => {
  const page = await newPage();
  const message = 'The usage of the bucket is unavailable: GetBucketInfo: the admin API refused the credentials (HTTP 403).';
  await page.route(/\/api\/usage\?/, route => route.fulfill({ json: { supported: true, available: false, instance: 'seaweedfs-main', source: 'seaweedfs', error: { code: 'usage_unavailable', message } } }));
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-level="unavailable"]`);
  assert.equal(await usageItem(page).locator('.usageMeter').count(), 0);
  assert.equal(await usageItem(page).locator('.visually-hidden').textContent(), `Usage unavailable. ${message}. `);
  assert.doesNotMatch(await usageItem(page).textContent(), /\d+ (B|KB|MB|GB)/);
  // No notice of a server that does not answer: the listing stays as it is.
  assert.equal(await page.locator('#notices > *').count(), 0);
  assert.ok(await page.locator('tr[data-name="README.md"]').count() === 1);
  assert.equal(await page.isHidden('#usageBanner'), true);
  // The information page gives the reason.
  await page.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="holds"]');
  assert.match(await page.textContent('[data-gauge="holds"]'), /Unavailable/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The usage grows after an upload. SeaweedFS and Garage count a write after a few seconds. MinIO counts in a scan of
// about one minute: its number is checked in the first test only.
for (const instance of ['garage-main', 'seaweedfs-main']) {
  test(`usage: ${PROVIDERS[instance].name}: the usage of the selector grows after an upload, with no reload of the page`, async () => {
    const page = await newPage();
    const host = instance.split('-')[0];
    const key = `usage-${Date.now()}.bin`;
    await open(page, `-/${host}/${instance}/`);
    await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
    const before = Number(await usageItem(page).getAttribute('data-used-bytes'));
    const requests = [];
    page.on('request', request => { if (/\/api\/usage\?/.test(request.url())) requests.push(request.url()); });
    await uploadWithPage(page, key, require('crypto').randomBytes(3 << 20));
    // The page reads the usage again after the upload; the reload icon reads it again too.
    const end = Date.now() + 60000;
    let after = before;
    while (after < before + (3 << 20) * 0.9 && Date.now() < end) {
      await sleep(2000);
      after = Number(await usageItem(page).getAttribute('data-used-bytes'));
      if (after < before + (3 << 20) * 0.9 && requests.length >= 2) await page.click('#refreshButton');
    }
    assert.ok(after >= before + (3 << 20) * 0.9, `before ${before}, after ${after}`);
    assert.ok(requests.some(url => /fresh=1/.test(url)), requests.join('\n'));
    await page.request.delete(`${BASE}s3?instance=${instance}&key=${encodeURIComponent(key)}`);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  });
}

test('usage: the tooltip of a bucket in the selector has its usage', async () => {
  const page = await newPage();
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  assert.match(await page.getAttribute('.storageSwitcher__trigger', 'title'), /\nusage: ~[\d.]+ (B|KB|MB|GB) of 20 GB \([\d.]+ %\)\nstorage: [\d.]+ (B|KB|MB|GB) of 4 GB \([\d.]+ %\), [\d.]+ (MB|GB) left$/);
  await page.click('.storageSwitcher__trigger strong');
  await until(page, () => /usage: [\d.]+ (B|KB|MB|GB) of 20 GB/.test(document.querySelector('.storageBucket[data-instance="garage-main"]')?.title || ''));
  await until(page, () => /usage: ~[\d.]+ (B|KB|MB) used\nstorage: /.test(document.querySelector('.storageBucket[data-instance="minio-locked"]')?.title || ''));
  // The menu stays open while the lines change.
  assert.equal(await page.isVisible('#storageSwitcherMenu'), true);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The geometry of the bucket selector: the trigger, the elements over it, and the scroll of the page. A centre is the
// vertical centre of a box, in whole pixels. Only the elements that show count.
function selectorGeometry(page) {
  return page.evaluate(() => {
    const rect = node => node.getBoundingClientRect();
    const shown = node => Boolean(node) && node.getClientRects().length > 0 && rect(node).width > 0;
    const centre = node => Math.round(rect(node).top + rect(node).height / 2);
    const facts = document.querySelector('.storageSwitcher__facts');
    const trigger = rect(document.querySelector('.storageSwitcher__trigger'));
    const name = document.querySelector('.storageSwitcher__trigger strong');
    const short = facts.querySelector('.bucketUsage__short');
    const amount = facts.querySelector('.bucketUsage__amount');
    const icons = Array.from(document.querySelectorAll('.storageSwitcher__states [data-state-icon]'));
    const parts = [name, document.querySelector('.storageSwitcher__states'), facts, ...facts.querySelectorAll('.bucketFact'), document.querySelector('.storageSwitcher__chevron'),
      document.querySelector('.storageSwitcher__icon')].filter(shown);
    const scroller = document.querySelector('[data-page-scroll]');
    return {
      view: window.innerWidth, page: scroller.scrollWidth - scroller.clientWidth,
      width: Math.round(trigger.width), selector: Math.round(rect(document.querySelector('#storageSwitcher')).width),
      marks: document.querySelectorAll('.storageSwitcher__trigger .bucketMark').length, height: Math.round(trigger.height), centres: parts.map(centre),
      fit: !shown(facts) || facts.scrollWidth <= facts.clientWidth, inside: !shown(facts) || (rect(facts).left >= trigger.left && rect(facts).right <= trigger.right),
      linkInside: rect(document.querySelector('#storageSwitcherInfo')).right <= trigger.right,
      name: Math.round(rect(name).width), nameFirst: parts.every(node => node === name || node.closest('.storageSwitcher__trigger') || rect(node).left >= rect(name).right - 1),
      facts: Array.from(facts.querySelectorAll('.bucketFact')).filter(shown).map(node => node.dataset.fact + (node.dataset.fact === 'usage' && !shown(short) ? ':bar' : '')),
      short: shown(short) ? short.textContent : '', amount: shown(amount) ? amount.textContent : '',
      icons: icons.filter(shown).map(node => node.dataset.stateIcon)
    };
  });
}

const spread = values => Math.max(...values) - Math.min(...values);

// The instances of the test stack that show each kind of state: usage with a limit and a rule count, Object Lock,
// encryption, no versioning, and the volume.
const LINE_INSTANCES = ['seaweedfs-main', 'seaweedfs-locked', 'seaweedfs-encrypted', 'garage-main', 'minio-main', 'files'];

// The cases of the selector that are not in the data of the test stack: a long name with every state and a full
// bucket, one visible bucket (a static trigger), and a failed read of the usage.
const WORST_NAME = 'A bucket with a very long name that never fits in the trigger of the selector';
async function routeSelectorCase(page, name) {
  if (name === 'worst') {
    await page.route(/\/api\/instances(\?|$)/, async route => {
      const response = await route.fetch();
      const body = await response.json();
      Object.assign(body.instances.find(item => item.id === 'seaweedfs-main'), { name: WORST_NAME, encryption: 'sse-s3', encryptionApplied: 'yes', objectLockSupported: true, lifecycleRules: 12, versioningState: 'enabled' });
      await route.fulfill({ response, json: body });
    });
    await page.route(/\/api\/lifecycle\?/, route => route.fulfill({ json: { rules: Array.from({ length: 12 }, (_, index) => ({ id: `r${index}`, enabled: true })), editor: { versioning: 'Enabled' } } }));
    await routeUsage(page, { 'seaweedfs-main': usageAnswer('seaweedfs-main', 96) });
  } else if (name === 'single') {
    await page.route(/\/api\/instances(\?|$)/, async route => {
      const response = await route.fetch();
      const body = await response.json();
      body.instances = body.instances.filter(item => item.id === 'seaweedfs-locked');
      await route.fulfill({ response, json: body });
    });
  } else if (name === 'unavailable') {
    await page.route(/\/api\/usage\?/, route => route.fulfill({ json: { supported: true, available: false, instance: 'seaweedfs-main', source: 'seaweedfs', error: { code: 'usage_unavailable', message: 'The provider does not answer.' } } }));
  }
}

const SELECTOR_CASES = [...LINE_INSTANCES.map(id => ({ name: id, path: id === 'files' ? '-/volume/files/' : `-/${id.split('-')[0]}/${id}/`, usage: true })),
  { name: 'worst', path: MAIN, usage: true }, { name: 'single', path: '-/seaweedfs/seaweedfs-locked/', usage: true }, { name: 'unavailable', path: MAIN, usage: false }];

test('selector: the trigger is never on two lines, at every width, with a fine and a coarse pointer, for each instance, a long name with every state, one bucket and a failed usage', async () => {
  for (const coarse of [false, true]) {
    for (const item of SELECTOR_CASES) {
      const page = await newPage({ viewport: { width: 1440, height: 900 }, ...(coarse ? { isMobile: true, hasTouch: true } : {}) });
      await routeSelectorCase(page, item.name);
      await open(page, item.path);
      await page.waitForSelector(item.usage ? `${FACTS} [data-fact="usage"][data-used-bytes]` : `${FACTS} [data-fact="usage"][data-level="unavailable"]`);
      for (const colorScheme of ['light', 'dark']) {
        await page.emulateMedia({ colorScheme });
        for (const width of HEADER_WIDTHS) {
          await page.setViewportSize({ width, height: 900 });
          const box = await selectorGeometry(page);
          const where = `${item.name} at ${width}${coarse ? ' coarse' : ''} ${colorScheme}: ${JSON.stringify(box)}`;
          // One row: 28 px (44 px on a coarse pointer), and every element that shows has the same vertical centre.
          assert.equal(box.height, coarse ? 44 : 28, where);
          assert.ok(spread(box.centres) <= 2, where);
          // The facts never wrap, are never cut, and stay in the trigger, after the name. The selector is 460 px at most.
          assert.equal(box.fit, true, where);
          assert.equal(box.inside, true, where);
          assert.equal(box.linkInside, true, where);
          assert.equal(box.nameFirst, true, where);
          assert.ok(box.width <= 460 && box.width <= box.selector, where);
          // The name keeps 32 px; with the marks of a full bucket after it, a few letters at least.
          assert.ok(box.name >= (box.marks ? 8 : 32), where);
          assert.ok(box.page <= 0, where);
          // The seven icons of a bucket stay at every width; a volume has the three icons of its rights.
          assert.deepEqual(box.icons, item.name === 'files' ? ['read', 'write', 'delete'] : STATE_ICONS, where);
        }
      }
      assert.deepEqual(page.errors, []);
      await page.context().close();
    }
  }
});

test('selector: a narrow selector drops the amount of the usage, then its short text, then the usage; the seven state icons stay; the tooltip keeps each line', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  await routeSelectorCase(page, 'worst');
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  const seen = async width => {
    await page.setViewportSize({ width, height: 900 });
    const box = await selectorGeometry(page);
    return { width: box.width, facts: box.facts, amount: box.amount, short: box.short, icons: box.icons.length };
  };
  const wide = await seen(1440);
  assert.equal(wide.width, 460);
  assert.deepEqual(wide.facts, ['usage']);
  assert.match(wide.amount, /^[\d.]+ GB$/);
  assert.equal(wide.short, '96 %');
  assert.equal(wide.icons, 7);
  // Each width shows the parts of the wider widths minus the first ones of the order.
  const steps = [];
  for (const width of [1440, 760, 600, 560, 520, 480, 440, 420, 390, 360, 320]) steps.push(await seen(width));
  const rank = box => [box.amount ? 1 : 0, box.short ? 1 : 0, box.facts.some(fact => fact.startsWith('usage')) ? 1 : 0];
  for (let index = 1; index < steps.length; index += 1) {
    const [before, after] = [rank(steps[index - 1]), rank(steps[index])];
    assert.ok(after.every((value, position) => value <= before[position]), JSON.stringify(steps));
  }
  // The order: the amount goes first, then the short text (the bar stays), then the usage.
  assert.ok(steps.some(box => !box.amount && box.short), JSON.stringify(steps));
  assert.ok(steps.some(box => !box.short && box.facts.includes('usage:bar')), JSON.stringify(steps));
  assert.deepEqual(steps.at(-1).facts, []);
  for (const box of steps) assert.equal(box.icons, 7, JSON.stringify(box));
  // The tooltip of the trigger gives each state at every width.
  const title = await page.getAttribute('.storageSwitcher__trigger', 'title');
  for (const line of ['Versioning: on', 'Encryption: on (SSE-S3)', 'Object Lock: on', 'Lifecycle rules: 12 on of 12']) {
    assert.ok(title.includes(`\n${line}`), `${line} in ${title}`);
  }
  assert.match(title, /\nusage: [\d.]+ GB of 20 GB \(96 %\)$/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('selector: each bucket and each host of the menu is one line at every width; a root prefix follows the name, the icons make one column, and the menu has the edges of the selector', async () => {
  for (const coarse of [false, true]) {
    const page = await newPage({ viewport: { width: 1440, height: 900 }, ...(coarse ? { isMobile: true, hasTouch: true } : {}) });
    await page.route(/\/api\/instances(\?|$)/, async route => {
      const response = await route.fetch();
      const body = await response.json();
      body.instances.find(item => item.id === 'seaweedfs-archive').rootPrefix = 'tenant-a/with/a/long/root/prefix/';
      await route.fulfill({ response, json: body });
    });
    await open(page, MAIN);
    await openAllBuckets(page);
    await until(page, () => document.querySelectorAll('.storageBucket__info [data-fact="usage"][data-used-bytes]').length >= 6);
    for (const width of HEADER_WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      const menu = await page.evaluate(() => {
        const rect = node => node.getBoundingClientRect();
        const shown = node => Boolean(node) && node.getClientRects().length > 0 && rect(node).width > 0;
        const centre = node => Math.round(rect(node).top + rect(node).height / 2);
        const box = rect(document.querySelector('#storageSwitcherMenu'));
        const trigger = rect(document.querySelector('.storageSwitcher__trigger'));
        const rows = Array.from(document.querySelectorAll('.storageBucket__row, .storageHost__row')).map(node => {
          const name = node.querySelector('strong');
          const parts = [name, node.querySelector('small'), node.querySelector('.storageBucket__states'), node.querySelector('.bucketFacts')].filter(shown);
          const facts = node.querySelector('.bucketFacts');
          const prefix = node.querySelector('.storageBucket__prefix');
          return { id: node.dataset.instance || node.dataset.host, height: Math.round(rect(node).height), spread: Math.max(...parts.map(centre)) - Math.min(...parts.map(centre)),
            fit: !shown(facts) || facts.scrollWidth <= facts.clientWidth, inside: rect(node).right <= box.right + 0.5,
            order: parts.every((part, index) => index === 0 || rect(part).left >= rect(parts[index - 1]).right - 1),
            icons: shown(node.querySelector('.storageBucket__states')) ? Math.round(rect(node.querySelector('.storageBucket__states')).left) : null,
            prefix: prefix ? { text: prefix.textContent, after: !shown(prefix) || rect(prefix).left >= rect(name).right - 1 } : null };
        });
        const scroller = document.querySelector('[data-page-scroll]');
        return { left: box.left - trigger.left, right: box.right - trigger.right, rows, scroll: scroller.scrollWidth - scroller.clientWidth };
      });
      const where = `${width}${coarse ? ' coarse' : ''}: ${JSON.stringify(menu)}`;
      assert.ok(Math.abs(menu.left) <= 1 && Math.abs(menu.right) <= 1, where);
      assert.ok(menu.scroll <= 0, where);
      // Nine buckets and three hosts of S3, then the volume "files": its host has one entry and no row.
      assert.equal(menu.rows.length, 13, where);
      for (const row of menu.rows) {
        assert.ok(row.height <= (coarse ? 48 : 40) && row.spread <= 2 && row.fit && row.inside && row.order, `${JSON.stringify(row)} at ${width}`);
        if (row.id === 'seaweedfs-archive') assert.deepEqual(row.prefix, { text: '/tenant-a/with/a/long/root/prefix/', after: true }, `${width}`);
      }
      const columns = new Set(menu.rows.map(row => row.icons).filter(value => value !== null));
      assert.equal(columns.size, 1, `the state icons in one column at ${width}: ${[...columns]}`);
    }
    // A host is one line: its name, then the provider and the number of buckets.
    assert.match(await page.textContent('.storageHost__row'), /seaweedfs\s*S3 · 4 buckets/);
    assert.deepEqual(page.errors, []);
    await page.context().close();
  }
});

test('selector: the state icons have one of three tones: green, orange for a problem, grey; no red and no strike', async () => {
  const page = await newPage();
  await open(page, '-/garage/garage-encrypted/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`);
  const colours = await page.evaluate(() => {
    const colour = key => getComputedStyle(document.querySelector(`#storageSwitcherInfo [data-state-icon="${key}"]`)).color;
    const token = name => {
      const probe = document.createElement('span');
      probe.style.color = `var(${name})`;
      document.body.append(probe);
      const value = getComputedStyle(probe).color;
      probe.remove();
      return value;
    };
    const decorations = Array.from(document.querySelectorAll('#storageSwitcherInfo [data-state-icon]')).map(node => getComputedStyle(node, '::after').content);
    return { read: colour('read'), encryption: colour('encryption'), versioning: colour('versioning'), success: token('--success'), warning: token('--warning'), muted: token('--muted'), danger: token('--danger'), decorations };
  });
  assert.equal(colours.read, colours.success, JSON.stringify(colours));
  assert.equal(colours.encryption, colours.warning, JSON.stringify(colours));
  assert.equal(colours.versioning, colours.muted, JSON.stringify(colours));
  assert.ok(![colours.read, colours.encryption, colours.versioning].includes(colours.danger), JSON.stringify(colours));
  assert.ok(colours.decorations.every(value => value === 'none' || value === 'normal'), JSON.stringify(colours));
  // A suspended versioning is orange.
  const second = await newPage();
  await second.route(/\/api\/instances(\?|$)/, async route => {
    const response = await route.fetch();
    const body = await response.json();
    body.instances.find(item => item.id === 'minio-main').versioningState = 'suspended';
    await route.fulfill({ response, json: body });
  });
  await second.route(/\/api\/lifecycle\?/, route => route.fulfill({ json: { rules: [], editor: { versioning: 'Suspended' } } }));
  await open(second, '-/minio/minio-main/');
  await until(second, () => document.querySelector('#storageSwitcherInfo [data-state-icon="versioning"]')?.dataset.tone === 'warn');
  assert.equal(await second.textContent('#storageSwitcherInfo [data-state-icon="versioning"] .visually-hidden'), 'Versioning: suspended. ');
  assert.deepEqual(page.errors, []);
  await page.context().close();
  await second.context().close();
});

test('selector: on a phone (390 px, coarse pointer) the trigger is one row with the seven icons, the menu is in the window, and the link works', async () => {
  const page = await newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await open(page, '-/seaweedfs/seaweedfs-locked/');
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`, { state: 'attached' });
  const box = await selectorGeometry(page);
  assert.equal(box.height, 44, JSON.stringify(box));
  assert.deepEqual(box.icons, STATE_ICONS, JSON.stringify(box));
  // No "i" button and no popover.
  assert.equal(await page.locator('#bucketInfoButton, #bucketChips, #usagePopover').count(), 0);
  await page.tap('.storageSwitcher__trigger strong');
  const menu = await page.evaluate(() => Array.from(document.querySelectorAll('.storageHost[data-host="seaweedfs"] .storageBucket__row')).map(node => node.getBoundingClientRect().right <= window.innerWidth));
  assert.deepEqual(menu, [true, true, true, true]);
  assert.ok(await page.evaluate(() => { const scroller = document.querySelector('[data-page-scroll]'); return scroller.scrollWidth - scroller.clientWidth; }) <= 0);
  await page.tap(`${entry('seaweedfs-locked')} .storageBucket__info [data-state-icon="read"]`);
  await page.waitForURL(/\/-\/seaweedfs\/seaweedfs-locked\/\?info$/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The answer of /api/usage for a part of the limit of 20 GiB, as the server gives it.
function usageAnswer(instance, percent, { approximate = false, limitBytes = QUOTA_BYTES, limitObjects = 0 } = {}) {
  const usedBytes = Math.round(QUOTA_BYTES * percent / 100);
  return { supported: true, available: true, instance, bucket: 'default', source: 'seaweedfs', usedBytes, usedObjects: 10, limitBytes, limitObjects,
    limitSource: 'provider', percent, level: percent >= 95 ? 'error' : percent >= 80 ? 'warning' : 'ok', approximate,
    approximateReason: approximate ? 'SeaweedFS counts the allocated bytes of its volumes.' : '', asOf: new Date().toISOString() };
}

// Answers /api/usage with the answers of plan (by instance); the other instances read the server.
async function routeUsage(page, plan) {
  await page.route(/\/api\/usage\?/, route => {
    const instance = new URL(route.request().url()).searchParams.get('instance');
    return plan[instance] ? route.fulfill({ json: plan[instance] }) : route.continue();
  });
}

test('usage: a bucket at 90 % or more of its limit gives a banner, a mark on the selector and the tooltip of the upload; at 50 % nothing', async () => {
  const page = await newPage();
  const plan = { 'seaweedfs-main': usageAnswer('seaweedfs-main', 91) };
  await routeUsage(page, plan);
  await open(page, MAIN);
  await page.waitForSelector('#usageBanner:not([hidden])');
  const text = 'This bucket is 91 % full: 18.2 GB used of 20 GB. New uploads can fail.';
  assert.equal(await page.getAttribute('#usageBanner', 'role'), 'alert');
  assert.equal((await page.textContent('#usageBanner')).trim(), text);
  assert.equal(await page.locator('#usageBanner .ic-alert-triangle').count(), 1);
  // The banner is at the top of the card of the listing, as wide as the listing.
  const box = await page.evaluate(() => {
    const rect = selector => document.querySelector(selector).getBoundingClientRect();
    return { first: document.querySelector('#browser').firstElementChild.id, banner: rect('#usageBanner'), crumbs: rect('.browserHead'), listing: rect('#listing') };
  });
  assert.equal(box.first, 'usageBanner');
  assert.ok(box.banner.bottom <= box.crumbs.top && Math.abs(box.banner.width - box.listing.width) < 1, JSON.stringify(box));
  // The selector: the coloured border and the alert icon on the trigger, a red bar, and the alert icon of the bucket in the menu.
  assert.equal(await page.locator('.storageSwitcher.is-full').count(), 1);
  assert.equal(await page.getAttribute('.storageSwitcher__trigger [data-mark="full"]', 'title'), text);
  assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.storageSwitcher__trigger')).borderTopColor),
    await page.evaluate(() => getComputedStyle(document.querySelector('.storageSwitcher__trigger [data-mark="full"]')).color));
  assert.match(await usageItem(page).getAttribute('class'), /is-full/);
  const red = await page.evaluate(() => getComputedStyle(document.querySelector('.storageSwitcher__facts .usageMeter__fill')).backgroundColor);
  await page.click('.storageSwitcher__trigger strong');
  assert.equal(await page.locator('.storageBucket[data-instance="seaweedfs-main"] [data-mark="full"] .ic-alert-triangle').count(), 1);
  assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.storageBucket__row[data-instance="seaweedfs-main"] .usageMeter__fill')).backgroundColor), red);
  await page.keyboard.press('Escape');
  // The upload stays possible, with the warning in its tooltip.
  assert.equal(await page.isDisabled('#uploadButton'), false);
  assert.equal(await page.getAttribute('#uploadButton', 'title'), text);

  // 100 %: a stronger text. The approximate numbers have a "~" and the reason.
  plan['seaweedfs-main'] = usageAnswer('seaweedfs-main', 100, { approximate: true });
  await page.click('#refreshButton');
  await until(page, () => /^This bucket is full/.test(document.querySelector('#usageBanner').textContent));
  assert.equal((await page.textContent('#usageBanner')).trim(),
    'This bucket is full: ~20 GB used of 20 GB (~100 %). New uploads fail. The numbers are approximate: SeaweedFS counts the allocated bytes of its volumes.');
  assert.equal(await page.textContent('#usageBanner strong'), 'This bucket is full: ~20 GB used of 20 GB (~100 %). New uploads fail.');

  // The banner goes when the number goes down; nothing marks the bucket at 50 %.
  plan['seaweedfs-main'] = usageAnswer('seaweedfs-main', 50);
  await page.click('#refreshButton');
  await page.waitForSelector('#usageBanner', { state: 'hidden' });
  assert.equal(await page.locator('.storageSwitcher.is-full, .bucketMark').count(), 0);
  assert.equal(await page.getAttribute('#uploadButton', 'title'), null);
  assert.doesNotMatch(await usageItem(page).getAttribute('class'), /is-full/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('usage: a bucket without a limit of bytes never warns, and a full bucket other than the current one marks the selector', async () => {
  const page = await newPage();
  // No limit of bytes: a percent of the objects does not give the warning.
  await routeUsage(page, { 'seaweedfs-main': usageAnswer('seaweedfs-main', 97, { limitBytes: 0, limitObjects: 10 }), 'seaweedfs-archive': usageAnswer('seaweedfs-archive', 96) });
  await open(page, MAIN);
  await page.waitForSelector('.storageSwitcher__trigger [data-mark="other"]');
  assert.equal(await page.isHidden('#usageBanner'), true);
  assert.equal(await page.locator('.storageSwitcher.is-full, .storageSwitcher__trigger [data-mark="full"]').count(), 0);
  // The other bucket: a small mark on the trigger with its name, and the alert icon and a red bar in its entry.
  assert.equal(await page.getAttribute('.storageSwitcher__trigger [data-mark="other"]', 'title'), 'Another bucket is almost full: SeaweedFS - archive');
  await page.click('.storageSwitcher__trigger strong');
  assert.equal(await page.locator('.storageBucket[data-instance="seaweedfs-archive"] [data-mark="full"]').count(), 1);
  assert.match(await page.getAttribute('.storageBucket__row[data-instance="seaweedfs-archive"] [data-fact="usage"]', 'class'), /is-full/);
  assert.equal(await page.locator('.storageBucket[data-instance="seaweedfs-main"] [data-mark]').count(), 0);
  // The other bucket opens with its banner.
  await page.click('.storageBucket[data-instance="seaweedfs-archive"] strong');
  await page.waitForSelector('#usageBanner:not([hidden])');
  assert.match(await page.textContent('#usageBanner'), /^This bucket is 96 % full: 19.2 GB used of 20 GB\. New uploads can fail\.$/);
  assert.equal(await page.locator('.storageSwitcher__trigger [data-mark="other"]').count(), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The details of a file: the pills of the summary strip, and the sections that start closed.
test('details: the summary strip shows the version and the tags, and the sections other than "Storage object" and "Tags" start closed', async () => {
  const page = await newPage({ sections: 'default' });
  await open(page, `${MAIN}versions/`);
  await openDetails(page, 'report.txt');
  await page.waitForSelector('[data-details-summary] [data-pill="version"]:not([hidden])');
  assert.equal((await page.textContent('[data-pill="version"]')).trim(), 'v3 of 3');
  // The last version of versions/report.txt has the tags status=final and team=data.
  await page.waitForSelector('[data-details-summary] [data-pill="tags"]:not([hidden])');
  assert.equal((await page.textContent('[data-pill="tags"]')).trim(), '2 tags');
  assert.equal(await page.isHidden('[data-pill="encryption"]'), true);
  assert.equal(await page.isHidden('[data-pill="lock"]'), true);
  const expanded = name => page.getAttribute(`[data-section-toggle="${name}"]`, 'aria-expanded');
  assert.equal(await expanded('storage'), 'true');
  assert.equal(await expanded('tags'), 'true');
  await page.waitForSelector('[data-section-toggle="metadata"]');
  assert.equal(await expanded('metadata'), 'false');
  assert.equal(await page.isHidden('[data-metadata-section] .metaEditor__view'), true);
  // The content of a closed section stays in the page: only its display is off.
  assert.equal(await page.locator('[data-metadata-section] .metaEditor__view').count(), 1);
  // The pill of the version opens the version picker.
  await page.click('[data-pill="version"]');
  await page.waitForSelector('.versionPicker__item', { state: 'visible' });
  await page.keyboard.press('Escape');
  // The pill of the tags moves to the section Tags.
  await page.click('[data-pill="tags"]');
  assert.equal(await expanded('tags'), 'true');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('details: a closed section opens and closes with the keyboard, its Edit button opens it, and the browser keeps the choice', async () => {
  const page = await newPage({ sections: 'default' });
  await open(page, MAIN);
  await openDetails(page, 'README.md');
  await page.waitForSelector('[data-section-toggle="metadata"]');
  await page.focus('[data-section-toggle="metadata"]');
  await page.keyboard.press('Enter');
  assert.equal(await page.getAttribute('[data-section-toggle="metadata"]', 'aria-expanded'), 'true');
  await page.waitForSelector('[data-metadata-section] .metaEditor__view', { state: 'visible' });
  await page.keyboard.press('Space');
  assert.equal(await page.getAttribute('[data-section-toggle="metadata"]', 'aria-expanded'), 'false');
  assert.equal(await page.isHidden('[data-metadata-section] .metaEditor__view'), true);
  // The Edit button of a closed section opens it with the form.
  await page.click('[data-metadata-edit]');
  await page.waitForSelector('[data-metadata-section] .metaEditor__form', { state: 'visible' });
  assert.equal(await page.getAttribute('[data-section-toggle="metadata"]', 'aria-expanded'), 'true');
  // A click on the title of the section File metadata opens it too.
  if (await page.locator('[data-section="file"]').count()) {
    await page.click('[data-section="file"] .bb-details-section-title');
    assert.equal(await page.getAttribute('[data-section-toggle="file"]', 'aria-expanded'), 'true');
  }
  await closeWindow(page);
  // The next details keep the open section.
  await openDetails(page, 'README.md');
  await page.waitForSelector('[data-section-toggle="metadata"][aria-expanded="true"]');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('details: the pills of the encryption and of the lock open their sections', async () => {
  const page = await newPage({ sections: 'default' });
  // An object with SSE-S3: the server asks for it on the instance seaweedfs-encrypted.
  const encryptedKey = `summary/enc-${Date.now()}.txt`;
  await putWithServer(page, 'seaweedfs-encrypted', encryptedKey, 'summary of an encrypted object\n');
  await open(page, '-/seaweedfs/seaweedfs-encrypted/summary/');
  await openDetails(page, encryptedKey.split('/').pop());
  await page.waitForSelector('[data-pill="encryption"]:not([hidden])');
  assert.equal((await page.textContent('[data-pill="encryption"]')).trim(), 'SSE-S3');
  await page.waitForSelector('[data-section-toggle="encryption"][aria-expanded="false"]');
  await page.click('[data-pill="encryption"]');
  await page.waitForSelector('[data-encryption-section] [data-encryption-field="object"]', { state: 'visible' });
  await closeWindow(page);
  await deleteEncrypted(page, 'seaweedfs-encrypted', encryptedKey);

  // A version with a legal hold: the pill says it and opens the section Retention.
  const lockedKey = `summary/hold-${Date.now()}.txt`;
  await makeLockedObject('seaweedfs-locked', lockedKey);
  const version = (await apiVersions(page, 'seaweedfs-locked', lockedKey))[0].version;
  assert.equal((await putLock(page, 'seaweedfs-locked', lockedKey, version, { legalHold: true })).status(), 200);
  await open(page, '-/seaweedfs/seaweedfs-locked/summary/');
  await openDetails(page, lockedKey.split('/').pop());
  await page.waitForSelector('[data-pill="lock"]:not([hidden])');
  assert.equal((await page.textContent('[data-pill="lock"]')).trim(), 'legal hold');
  await page.click('[data-pill="lock"]');
  await page.waitForSelector('[data-lock-section] [data-lock-field="legalHold"]', { state: 'visible' });
  await closeWindow(page);
  assert.equal((await putLock(page, 'seaweedfs-locked', lockedKey, version, { legalHold: false })).status(), 200);
  await purgeVersions(page, 'seaweedfs-locked', lockedKey);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The sections of the details have two columns on a wide window and one column on a narrow window. The tool cards (integrity and
// inspection) have the full width. The order of the sections in the page is the tab order in both layouts.
test('details: on a desktop window the sections are in two columns, Storage object and Tags side by side; on a phone one column', async () => {
  const page = await newPage({ viewport: { width: 1440, height: 900 } });
  await open(page, MAIN);
  await openDetails(page, 'README.md');
  await page.waitForSelector('[data-tags-section] .tagEditor');
  const measure = () => page.evaluate(() => {
    const box = selector => { const rect = document.querySelector(selector).getBoundingClientRect(); return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width }; };
    const body = document.querySelector('.bb-details-body');
    const sections = [...document.querySelectorAll('.bb-details-body [data-section]')].filter(node => node.offsetParent).map(node => node.dataset.section);
    return {
      storage: box('[data-section="storage"]'), tags: box('[data-section="tags"]'), columns: box('.bb-details-columns'),
      integrity: box('[data-details-tool-card="integrity"]'), inspection: box('[data-details-tool-card="inspection"]'),
      sections, overflow: body.scrollWidth - body.clientWidth
    };
  });
  const wide = await measure();
  assert.ok(Math.abs(wide.tags.top - wide.storage.top) <= 3, `top ${wide.storage.top} and ${wide.tags.top}`);
  assert.ok(wide.tags.left >= wide.storage.right, `Tags at ${wide.tags.left}px, Storage object ends at ${wide.storage.right}px`);
  assert.ok(wide.storage.width >= 350 && wide.tags.width >= 350, `widths ${wide.storage.width} and ${wide.tags.width}`);
  // The tool cards have the width of the two columns and come under them.
  for (const card of [wide.integrity, wide.inspection]) {
    assert.ok(Math.abs(card.width - wide.columns.width) <= 2, `card ${card.width}px, columns ${wide.columns.width}px`);
    assert.ok(card.top >= wide.columns.bottom - 1, `card at ${card.top}px, columns end at ${wide.columns.bottom}px`);
  }
  assert.ok(wide.overflow <= 0, `overflow ${wide.overflow}px`);
  // The order of the sections is the reading order: the same list in both layouts.
  assert.deepEqual(wide.sections.filter(name => ['storage', 'metadata', 'tags', 'file'].includes(name)), ['storage', 'metadata', 'tags', 'file']);
  // The window gets narrow: the sections go in one column.
  await page.setViewportSize({ width: 390, height: 844 });
  await until(page, () => document.querySelector('[data-section="tags"]').getBoundingClientRect().left < 100);
  const narrow = await measure();
  assert.ok(narrow.tags.top >= narrow.storage.bottom - 1, `Tags at ${narrow.tags.top}px, Storage object ends at ${narrow.storage.bottom}px`);
  assert.ok(Math.abs(narrow.tags.left - narrow.storage.left) <= 1, `left ${narrow.storage.left} and ${narrow.tags.left}`);
  assert.ok(Math.abs(narrow.tags.width - narrow.storage.width) <= 1, `widths ${narrow.storage.width} and ${narrow.tags.width}`);
  assert.deepEqual(narrow.sections, wide.sections);
  assert.ok(Math.abs(narrow.integrity.width - narrow.columns.width) <= 2);
  // A section that opens or closes does not move to the other column.
  await page.setViewportSize({ width: 1440, height: 900 });
  await until(page, () => document.querySelector('[data-section="tags"]').getBoundingClientRect().left > 400);
  await page.click('[data-section-toggle="metadata"]');
  const again = await measure();
  assert.ok(again.tags.left >= again.storage.right);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// ------------------------------------------------------------------------------------------------------------------
// 13. Volumes: the volume "files" is a Docker volume, a folder of the server (provider fs, address /-/volume/files/).
// The seed puts its files in it (seed.js). The Playwright container mounts the same Docker volume at /volume: the tests
// read the real files and folders there. Each test that writes uses its own folder and removes it at the end.
// ------------------------------------------------------------------------------------------------------------------

const VOLUME = '-/volume/files/';
const volumeFS = require('fs');
const volumePath = require('path');
const volumeFile = relative => volumePath.join(VOLUMES.files.mount, relative);
const temporaryNames = relative => volumeFS.readdirSync(volumeFile(relative)).filter(name => name.startsWith('.stowr-tmp-'));
const rowNames = page => page.locator('tbody tr[data-name]').evaluateAll(rows => rows.map(row => row.dataset.name));

// Removes a test folder of the volume, also when an earlier run stopped in the middle.
function removeVolumeFolder(relative) {
  volumeFS.rmSync(volumeFile(relative), { recursive: true, force: true });
}

async function volumeInfo(page) {
  return (await (await page.request.get(`${BASE}api/instances`)).json()).instances.find(item => item.id === 'files');
}

test('volumes: the volume lists real folders, an empty folder, hidden files and nested folders at the address /-/volume/', async () => {
  const page = await newPage();
  const info = await volumeInfo(page);
  assert.equal(info.host, 'volume');
  assert.equal(info.provider, 'fs');
  await open(page, VOLUME);
  const names = await rowNames(page);
  for (const name of ['data', 'docs', 'empty', 'images', 'links', 'uploads', '.hidden-settings']) assert.ok(names.includes(name), `${name} in ${names}`);
  // The folders come first, then the files; the temporary file of an upload is not in the listing.
  assert.ok(names.indexOf('uploads') < names.indexOf('.hidden-settings'));
  assert.equal(await page.getAttribute('.crumb--root', 'aria-label'), 'Files - volume, volume root');
  assert.equal(await page.locator('.crumb--root .ic-folder-open').count(), 1);
  assert.equal(await page.getAttribute('#searchInput', 'placeholder'), 'Open a prefix in this volume');
  // A nested folder: docs/nested/deep/note.txt.
  await page.click('tr[data-name="docs"] .nameLink');
  await page.waitForSelector('tr[data-name="nested"]');
  await page.click('tr[data-name="nested"] .nameLink');
  await page.waitForSelector('tr[data-name="deep"]');
  await page.click('tr[data-name="deep"] .nameLink');
  await page.waitForSelector('tr[data-name="note.txt"]');
  assert.equal(new URL(page.url()).pathname, `/${VOLUME}docs/nested/deep/`);
  // An empty folder of the volume exists: the page shows it and its empty state.
  await open(page, `${VOLUME}empty/`);
  assert.equal(await page.locator('tbody tr[data-name]').count(), 0);
  await open(page, `${VOLUME}uploads/`);
  assert.equal(await page.locator('tbody tr[data-name]').count(), 0);
  assert.deepEqual(temporaryNames('uploads'), ['.stowr-tmp-5eed0000000000000000000000000001-stale.bin']);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('volumes: the page hides tags, versions, lifecycle rules, Object Lock, encryption and the edit of the metadata', async () => {
  const page = await newPage();
  const info = await volumeInfo(page);
  for (const flag of ['versioningSupported', 'tagsSupported', 'metadataEditSupported', 'lifecycleSupported', 'objectLockSupported', 'bucketEncryptionSupported']) {
    assert.equal(info[flag], false, flag);
  }
  assert.equal(info.multipartUploadsSupported, true);
  assert.equal(info.usageSupported, true);
  const heads = [];
  page.on('request', request => { if (request.method() === 'HEAD') heads.push(request.url()); });
  await open(page, `${VOLUME}docs/`);
  // The facts of the selector: the usage only.
  await page.waitForSelector(`${FACTS} [data-fact="usage"]`);
  assert.deepEqual(await page.locator(`${FACTS} [data-fact]`).evaluateAll(nodes => nodes.map(node => node.dataset.fact)), ['usage']);
  // The state icons: the three rights only, no icon of a setting that a mounted folder cannot have.
  assert.deepEqual(await stateKeys(page, '#storageSwitcherInfo'), ['read', 'write', 'delete']);
  assert.equal(await page.locator('[data-s3]').count(), 0);
  await page.click('#settingsButton');
  for (const selector of ['#versionsSetting', '#tagFilterSetting', '#uploadTagsSetting', '#encryptionSetting']) assert.equal(await page.isHidden(selector), true, selector);
  assert.equal(await page.locator('[data-column="tags"]').isHidden(), true);
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('th.col-tags').isVisible(), false);
  assert.equal(await page.locator('th.col-versions').isVisible(), false);
  await page.click('#actionsButton');
  assert.equal(await page.isHidden('#folderTagsItem'), true);
  assert.equal(await page.isVisible('#newFolderItem'), true);
  assert.equal((await page.textContent('#insightsItem')).trim(), 'Folder insights');
  await page.keyboard.press('Escape');
  // The details of a file: the storage object with the weak ETag, no tags, no edit of the metadata, no retention, no
  // version picker.
  await openDetails(page, 'report.txt');
  await page.waitForSelector('[data-details-tool-card="integrity"]');
  await sleep(500);
  assert.match(await page.textContent('[data-section="storage"]'), /W\/"107-\d+"/);
  for (const selector of ['[data-tags-section]', '[data-lock-section]', 'select[data-details-version]', '[data-metadata-edit]', '.versionPicker']) {
    assert.equal(await page.locator(selector).count(), 0, selector);
  }
  assert.equal(heads.filter(url => url.includes('instance=files')).length, 0, `HEAD requests of the listing: ${heads}`);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('volumes: a link to a folder of the volume opens, a link to a file previews, a link that leaves the volume is unreadable', async () => {
  const page = await newPage();
  await open(page, `${VOLUME}links/`);
  assert.deepEqual((await rowNames(page)).sort(), ['escape', 'inside', 'note']);
  const escape = 'tr[data-name="escape"]';
  assert.equal(await page.getAttribute(escape, 'data-unreadable'), 'true');
  assert.match(await page.getAttribute(`${escape} .nameCell`, 'title'), /leaves the volume/);
  assert.equal(await page.locator(`${escape} .nameLink, ${escape} .bb-kebab`).count(), 0);
  for (const key of ['links/escape', 'links/escape/passwd']) {
    const response = await page.request.get(`${BASE}s3?instance=files&key=${encodeURIComponent(key)}`);
    assert.equal(response.status(), 403, key);
    assert.doesNotMatch(await response.text(), /root:/);
  }
  const write = await page.request.put(`${BASE}s3?instance=files&key=${encodeURIComponent('links/escape/new.txt')}`, { data: Buffer.from('x') });
  assert.equal(write.status(), 403);
  const dots = await page.request.get(`${BASE}s3?instance=files&key=${encodeURIComponent('docs/../../etc/passwd')}`);
  assert.equal(dots.status(), 400);
  await page.click('tr[data-name="inside"] .nameLink');
  await page.waitForSelector('tr[data-name="readme.md"]');
  await open(page, `${VOLUME}links/`);
  await page.click('tr[data-name="note"] .nameLink');
  await page.waitForSelector('text=The report of the volume.');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('volumes: New folder makes a real folder; copy, rename and delete of a file; copy, rename and delete of a folder', async () => {
  const page = await newPage();
  removeVolumeFolder('t-folders');
  await open(page, VOLUME);
  await page.click('#actionsButton');
  await page.click('#newFolderItem');
  await page.fill('.bb-overlay .bb-input', 't-folders');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('tr[data-name="t-folders"]');
  assert.equal(volumeFS.statSync(volumeFile('t-folders')).isDirectory(), true);
  await page.click('tr[data-name="t-folders"] .nameLink');
  await page.waitForSelector('.empty, .dataTable');
  // A name with "/" is refused in the page.
  await page.click('#actionsButton');
  await page.click('#newFolderItem');
  await page.fill('.bb-overlay .bb-input', 'a/b');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('text=A name cannot have a "/".');
  await page.click('#actionsButton');
  await page.click('#newFolderItem');
  await page.fill('.bb-overlay .bb-input', 'sub');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('tr[data-name="sub"]');
  await page.setInputFiles('#fileInput', [{ name: 'file.txt', mimeType: 'text/plain', buffer: Buffer.from('a file of the volume\n') }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 30000 });
  // A small upload is one PUT: the server checks its SHA-256.
  assert.match(await page.textContent('.uploadDock .summary'), /^1 file uploaded \([\d.]+ B\), 1 checked with SHA-256\.$/);
  await page.click('.uploadDock .button >> text=Close');
  await page.waitForSelector('tr[data-name="file.txt"]');
  const before = volumeFS.statSync(volumeFile('t-folders/file.txt'));
  await rowActionWithName(page, 'file.txt', 'Copy', 'copy.txt');
  await page.waitForSelector('tr[data-name="copy.txt"]');
  assert.equal(volumeFS.readFileSync(volumeFile('t-folders/copy.txt'), 'utf8'), 'a file of the volume\n');
  await rowActionWithName(page, 'file.txt', 'Rename', 'renamed.txt');
  await page.waitForSelector('tr[data-name="renamed.txt"]');
  await page.waitForSelector('tr[data-name="file.txt"]', { state: 'detached' });
  // The rename is one rename of the file: the same inode.
  assert.equal(volumeFS.statSync(volumeFile('t-folders/renamed.txt')).ino, before.ino);
  await page.click('tr[data-name="copy.txt"] .bb-kebab');
  await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('tr[data-name="copy.txt"]', { state: 'detached' });
  assert.equal(volumeFS.existsSync(volumeFile('t-folders/copy.txt')), false);
  // A folder: copy (with its empty folder), rename (the source folder goes), delete (the folder goes).
  volumeFS.mkdirSync(volumeFile('t-folders/sub/void'));
  volumeFS.chownSync(volumeFile('t-folders/sub/void'), VOLUMES.files.uid, VOLUMES.files.uid);
  await page.request.put(`${BASE}s3?instance=files&key=${encodeURIComponent('t-folders/sub/inner.txt')}`, { data: Buffer.from('inner\n') });
  await rowActionWithName(page, 'sub', 'Copy', 'sub-copy');
  await page.waitForSelector('tr[data-name="sub-copy"]', { timeout: 30000 });
  await waitForVolume(() => volumeFS.existsSync(volumeFile('t-folders/sub-copy/void')) && volumeFS.existsSync(volumeFile('t-folders/sub-copy/inner.txt')));
  await rowActionWithName(page, 'sub-copy', 'Rename', 'moved');
  await page.waitForSelector('tr[data-name="moved"]', { timeout: 30000 });
  await waitForVolume(() => !volumeFS.existsSync(volumeFile('t-folders/sub-copy')));
  assert.equal(volumeFS.statSync(volumeFile('t-folders/moved/void')).isDirectory(), true);
  assert.equal(volumeFS.readFileSync(volumeFile('t-folders/moved/inner.txt'), 'utf8'), 'inner\n');
  await page.click('tr[data-name="moved"] .bb-kebab');
  await page.click('.bb-menu.is-open .bb-menu-item >> text=Delete');
  await page.click('.bb-overlay .bb-btn-primary');
  await page.waitForSelector('tr[data-name="moved"]', { state: 'detached', timeout: 30000 });
  await waitForVolume(() => !volumeFS.existsSync(volumeFile('t-folders/moved')));
  assert.deepEqual(temporaryNames('t-folders'), []);
  assert.deepEqual(page.errors, []);
  removeVolumeFolder('t-folders');
  await page.context().close();
});

// Waits for a condition on the files of the volume, 15 seconds at most.
async function waitForVolume(check) {
  for (let tries = 0; tries < 150; tries += 1) {
    if (check()) return;
    await sleep(100);
  }
  assert.fail('the volume did not reach the expected state');
}

test('volumes: a large upload goes in chunks to a temporary file, resumes after a lost chunk, and has no temporary file at the end', async () => {
  const page = await newPage();
  removeVolumeFolder('t-upload');
  await page.request.put(`${BASE}s3?instance=files&key=${encodeURIComponent('t-upload/')}`, { data: Buffer.alloc(0) });
  await open(page, `${VOLUME}t-upload/`);
  const content = Buffer.alloc(40 * 1024 * 1024);
  for (let index = 0; index < content.length; index += 4096) content.writeUInt32BE(index, index);
  let chunks = 0;
  let duringUpload = [];
  await page.route(/\/api\/uploads$/, async route => {
    if (route.request().method() !== 'PUT') return route.continue();
    chunks += 1;
    if (chunks === 2) {
      // The first chunk is in the temporary file, in the folder of the file.
      duringUpload = temporaryNames('t-upload');
      return route.abort('connectionreset');
    }
    return route.continue();
  });
  await page.setInputFiles('#fileInput', [{ name: 'large.bin', mimeType: 'application/octet-stream', buffer: content }]);
  await page.waitForSelector('.uploadDock[data-state="done"]', { timeout: 120000 });
  assert.equal(duringUpload.length, 1, `temporary files during the upload: ${duringUpload}`);
  assert.match(duringUpload[0], /^\.stowr-tmp-[0-9a-f]{32}-large\.bin$/);
  assert.ok(chunks >= 3, `chunks: ${chunks}`);
  assert.equal(sha256(volumeFS.readFileSync(volumeFile('t-upload/large.bin'))), sha256(content));
  assert.deepEqual(temporaryNames('t-upload'), []);
  // Download with a range: 206 and the bytes of the range.
  const range = await page.request.get(`${BASE}s3?instance=files&key=${encodeURIComponent('t-upload/large.bin')}`, { headers: { Range: 'bytes=8192-8199' } });
  assert.equal(range.status(), 206);
  assert.equal(range.headers()['content-range'], `bytes 8192-8199/${content.length}`);
  assert.deepEqual(await range.body(), content.subarray(8192, 8200));
  const head = await page.request.head(`${BASE}s3?instance=files&key=${encodeURIComponent('t-upload/large.bin')}`);
  assert.match(head.headers().etag, new RegExp(`^W/"${content.length}-\\d+"$`));
  assert.deepEqual(page.errors, []);
  removeVolumeFolder('t-upload');
  await page.context().close();
});

test('volumes: the previews of a CSV file, a Markdown file and an image, the insights of a folder, and the prefix search', async () => {
  const page = await newPage();
  await open(page, `${VOLUME}data/`);
  await page.click('tr[data-name="orders.csv"] .nameLink');
  await page.waitForSelector('table >> text=North');
  await page.goto(`${BASE}${VOLUME}images/banner.svg`, { waitUntil: 'networkidle' });
  await page.waitForSelector('img, svg image, .imageViewer, [data-image-preview]');
  await page.goto(`${BASE}${VOLUME}docs/readme.md`, { waitUntil: 'networkidle' });
  await page.waitForSelector('text=Figures are provisional.');
  // The insights of the folder docs/: three files.
  await open(page, `${VOLUME}docs/`);
  await page.click('#actionsButton');
  await page.click('#insightsItem');
  await page.waitForSelector('.folder-insights-tabs', { timeout: 30000 });
  assert.match(await page.textContent('.bb-overlay'), /3 (objects|files)/);
  await page.click('.bb-modal-x');
  // The prefix search opens a folder of the volume.
  await page.fill('#searchInput', 'nested/deep/');
  await page.press('#searchInput', 'Enter');
  await page.waitForSelector('tr[data-name="note.txt"]');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('volumes: the window Incomplete uploads lists the temporary file of an old upload, with its last write, and aborts it', async () => {
  const page = await newPage();
  const stale = 'uploads/.stowr-tmp-5eed0000000000000000000000000001-stale.bin';
  await open(page, `${VOLUME}uploads/`);
  await openMultiparts(page);
  assert.deepEqual(await multipartRows(page), ['uploads/stale.bin']);
  assert.equal((await page.textContent('[data-multiparts] th.multiparts__started')).trim(), 'Last write');
  assert.equal(await page.locator('[data-multiparts] .multiparts__parts').count(), 0);
  assert.equal((await page.textContent('[data-multiparts] tr[data-upload-id] .multiparts__age')).trim(), '3 days');
  assert.match(await page.textContent('[data-multiparts] .multiparts__note'), /temporary file/);
  assert.equal((await page.textContent('[data-multiparts] .multiparts__scope')).trim(), 'Under uploads/');
  await page.click('[data-multipart-abort-older]');
  await confirmAbort(page);
  await until(page, () => document.querySelector('[data-multipart-message]')?.dataset.tone === 'ok');
  await waitForVolume(() => !volumeFS.existsSync(volumeFile(stale)));
  await until(page, () => !document.querySelector('[data-multiparts] tr[data-upload-id]'));
  // The seed file comes back for the next runs of this test.
  volumeFS.writeFileSync(volumeFile(stale), Buffer.alloc(4096, 's'));
  volumeFS.chownSync(volumeFile(stale), VOLUMES.files.uid, VOLUMES.files.uid);
  const old = new Date(Date.now() - 3 * 24 * 3600 * 1000);
  volumeFS.utimesSync(volumeFile(stale), old, old);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

// The size of the files of the volume, as Stowr counts it: the regular files, no symbolic link, no temporary file.
function volumeSize(relative = '') {
  let bytes = 0;
  let files = 0;
  for (const entry of volumeFS.readdirSync(volumeFile(relative), { withFileTypes: true })) {
    const child = volumePath.join(relative, entry.name);
    if (entry.isDirectory()) {
      const inner = volumeSize(child);
      bytes += inner.bytes;
      files += inner.files;
    } else if (entry.isFile() && !entry.name.startsWith('.stowr-tmp-')) {
      bytes += volumeFS.statSync(volumeFile(child)).size;
      files += 1;
    }
  }
  return { bytes, files };
}

// A usage answer of a volume with a file system at a part of its size.
function volumeUsageAnswer(usage, percent, { approximate = false } = {}) {
  const totalBytes = 1000 * 1024 ** 3;
  const usedBytes = Math.round(totalBytes * percent / 100);
  return { ...usage, constraint: 'storage', percent, level: percent >= 95 ? 'error' : percent >= 80 ? 'warning' : 'ok',
    storage: { ...usage.storage, usedBytes, totalBytes, freeBytes: totalBytes - usedBytes, percent, level: percent >= 95 ? 'error' : percent >= 80 ? 'warning' : 'ok',
      approximate, approximateReason: approximate ? 'The volume is on a FUSE or network file system: it can count the used bytes with a delay.' : '' } };
}

test('volumes: the usage gives what the volume holds and its file system, in the selector, the popover and the banner of a full file system', async () => {
  const page = await newPage();
  // A write through the API makes the count of the folder old; the count then gives the files of the Docker volume.
  await page.request.put(`${BASE}s3?instance=files&key=${encodeURIComponent('t-size/one.bin')}`, { data: Buffer.alloc(1234, 1) });
  let usage;
  for (let tries = 0; tries < 100; tries += 1) {
    usage = await (await page.request.get(`${BASE}api/usage?instance=files`)).json();
    if (usage.selfState === 'ready') break;
    await sleep(100);
  }
  const expected = volumeSize();
  assert.equal(usage.source, 'statfs');
  assert.equal(usage.selfState, 'ready', JSON.stringify(usage));
  assert.equal(usage.selfBytes, expected.bytes);
  assert.equal(usage.usedObjects, expected.files);
  assert.equal(usage.limitBytes, 0);
  // The file system of the volume is a tmpfs of 1 GiB (compose.yaml).
  assert.equal(usage.storage.scope, 'filesystem');
  assert.equal(usage.storage.totalBytes, 1024 ** 3);
  assert.equal(usage.storage.approximate, false);
  assert.equal(usage.constraint, 'storage');
  await open(page, VOLUME);
  await page.waitForSelector(`${FACTS} [data-fact="usage"] .usageMeter`);
  assert.match(await page.textContent(`${FACTS} .bucketUsage__text`), /^Storage: [\d.]+ (B|KB|MB) of 1 GB$/);
  // The amount is what the folder holds, before the percent of its file system.
  assert.match(await page.textContent(`${FACTS} .bucketUsage__amount`), /^[\d.]+ (B|KB|MB)$/);
  // A volume has the three icons of its rights only.
  assert.deepEqual(await stateKeys(page, '#storageSwitcherInfo'), ['read', 'write', 'delete']);
  // The information page: what the volume holds, then the storage with its gauge; no part of the lifecycle rules.
  await page.goto(`${BASE}${VOLUME}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="storage"]');
  assert.match(await page.textContent('[data-gauge="holds"]'), new RegExp(`This volume holds.*${expected.files} files`, 's'));
  assert.match(await page.textContent('[data-gauge="storage"]'), /[\d.]+ (B|KB|MB) used of 1 GB.*Free/s);
  assert.equal(await page.locator('[data-gauge="storage"] [role="meter"]').count(), 1);
  assert.equal(await page.locator('#info-lifecycle, [data-row="lifecycle"], [data-gauge="quota"]').count(), 0);
  assert.doesNotMatch(await page.textContent('#main'), /\/data\/volume/);
  await page.request.delete(`${BASE}s3?instance=files&key=${encodeURIComponent('t-size/one.bin')}`);
  await page.request.delete(`${BASE}s3?instance=files&key=${encodeURIComponent('t-size/')}`);
  // A file system at 91 %: the banner, the mark and the tooltip of the upload name the file system of the volume.
  await open(page, VOLUME);
  await page.route(/\/api\/usage\?instance=files/, route => route.fulfill({ json: volumeUsageAnswer(usage, 91) }));
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForSelector('#usageBanner:not([hidden])');
  assert.equal((await page.textContent('#usageBanner')).trim(), 'This volume is on a file system that is 91 % full: 910 GB of 1000 GB, 90 GB left. New uploads can fail.');
  assert.match(await page.getAttribute('#uploadButton', 'title'), /^This volume is on a file system that is 91 % full/);
  assert.equal((await page.textContent(`${FACTS} .bucketUsage__short`)).trim(), '91 %');
  // A FUSE or network file system counts with a delay: "~", and the reason on the information page.
  await page.unroute(/\/api\/usage\?instance=files/);
  await page.route(/\/api\/usage\?instance=files/, route => route.fulfill({ json: volumeUsageAnswer(usage, 20, { approximate: true }) }));
  await page.reload({ waitUntil: 'networkidle' });
  await until(page, () => document.querySelector('.storageSwitcher__facts .bucketUsage__short')?.textContent.trim() === '~20 %');
  await page.goto(`${BASE}${VOLUME}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="storage"] .infoChip--warn');
  assert.match(await page.getAttribute('[data-gauge="storage"] .infoChip--warn', 'title'), /The volume is on a FUSE or network file system/);
  await page.goto(`${BASE}${VOLUME}`, { waitUntil: 'networkidle' });
  // The volume is the only entry of the host "volume": it has no row of a host. The trigger and its entry are one line,
  // on a wide window and on a phone.
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 860 });
    await page.click('.storageSwitcher__trigger');
    await page.waitForSelector('.storageBucket[data-instance="files"]');
    const layout = await page.evaluate(() => {
      const rect = node => node.getBoundingClientRect();
      return { trigger: Math.round(rect(document.querySelector('.storageSwitcher__trigger')).height), host: document.querySelectorAll('.storageHost__row[data-host="volume"]').length,
        entry: Math.round(rect(document.querySelector('.storageBucket__row[data-instance="files"]')).height) };
    });
    assert.equal(layout.trigger, 28, `${width}: ${JSON.stringify(layout)}`);
    assert.ok(layout.host === 0 && layout.entry <= 40, `${width}: ${JSON.stringify(layout)}`);
    await page.keyboard.press('Escape');
  }
  assert.match(await page.getAttribute('.storageBucket[data-instance="files"]', 'title'), /volume: files\nprovider: a folder of the server \(FS\)/);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('usage: a phone shows no usage in the trigger; the link of the state icons opens the information page, and the Actions menu has no item "Usage"', async () => {
  const page = await newPage({ viewport: { width: 390, height: 844 } });
  await open(page, MAIN);
  await page.waitForSelector(`${FACTS} [data-fact="usage"][data-used-bytes]`, { state: 'attached' });
  assert.equal(await usageItem(page).isVisible(), false);
  await page.click('#actionsButton');
  assert.equal(await page.locator('#usageItem').count(), 0);
  await page.keyboard.press('Escape');
  await page.click('#storageSwitcherInfo [data-state-icon="read"]');
  await page.waitForURL(/\?info$/);
  await page.waitForSelector('[data-gauge="storage"]');
  assert.equal(await page.locator('#usagePopover').count(), 0);
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

test('usage: the warning names the more constraining part: the storage at 91 % with a quota at 50 %, and the quota at 93 % with the storage at 10 %', async () => {
  const page = await newPage();
  const storage = percent => {
    const totalBytes = 1000 * 1024 ** 3;
    const usedBytes = Math.round(totalBytes * percent / 100);
    return { scope: 'cluster', usedBytes, totalBytes, freeBytes: totalBytes - usedBytes, percent, level: percent >= 95 ? 'error' : percent >= 80 ? 'warning' : 'ok',
      approximate: false, note: 'raw capacity of the data partitions of 3 storage nodes, before the replication', asOf: new Date().toISOString() };
  };
  const plan = { 'seaweedfs-main': { ...usageAnswer('seaweedfs-main', 50), quotaPercent: 50, quotaLevel: 'ok', constraint: 'storage', percent: 91, level: 'warning', storage: storage(91) } };
  await routeUsage(page, plan);
  await open(page, MAIN);
  await page.waitForSelector('#usageBanner:not([hidden])');
  const storageText = 'The storage of this bucket is 91 % full: 910 GB of 1000 GB, 90 GB left. New uploads can fail.';
  assert.equal((await page.textContent('#usageBanner')).trim(), storageText);
  assert.equal(await page.getAttribute('#uploadButton', 'title'), storageText);
  assert.equal((await page.textContent(`${FACTS} .bucketUsage__short`)).trim(), '91 %');
  assert.equal(await usageItem(page).getAttribute('data-constraint'), 'storage');
  // The information page gives each part: the quota at 50 % and the storage at 91 %, in the colour of a warning.
  await page.goto(`${BASE}${MAIN}?info`, { waitUntil: 'networkidle' });
  await page.waitForSelector('[data-gauge="storage"]');
  assert.equal(await page.textContent('[data-gauge="quota"] .infoGauge__percent'), '50 %');
  assert.match(await page.textContent('[data-gauge="quota"]'), /10 GB used of 20 GB/);
  assert.equal(await page.textContent('[data-gauge="storage"] .infoGauge__percent'), '91 %');
  assert.equal(await page.getAttribute('[data-gauge="storage"]', 'data-fill'), 'warn');
  assert.match(await page.textContent('[data-gauge="storage"]'), /910 GB used of 1000 GB.*Free90 GB/s);
  assert.equal((await page.textContent('#infoBanner')).trim(), storageText);
  await open(page, MAIN);
  await page.waitForSelector('#usageBanner:not([hidden])');
  // The reverse: the quota is the constraining part.
  plan['seaweedfs-main'] = { ...usageAnswer('seaweedfs-main', 93), quotaPercent: 93, quotaLevel: 'warning', constraint: 'quota', storage: storage(10) };
  await page.click('#refreshButton');
  await until(page, () => /^This bucket is 93 % full/.test(document.querySelector('#usageBanner').textContent));
  assert.equal((await page.textContent('#usageBanner')).trim(), 'This bucket is 93 % full: 18.6 GB used of 20 GB. New uploads can fail.');
  assert.equal(await usageItem(page).getAttribute('data-constraint'), 'quota');
  assert.deepEqual(page.errors, []);
  await page.context().close();
});

(async () => {
  browser = await chromium.launch();
  let failed = 0;
  for (const { name, fn } of tests) {
    if (!selected(name)) continue;
    const started = Date.now();
    try {
      await fn();
      console.log(`ok    ${name} (${Date.now() - started} ms)`);
    } catch (error) {
      failed += 1;
      console.log(`FAIL  ${name}\n      ${String(error.message).split('\n').slice(0, 6).join('\n      ')}`);
    }
  }
  await browser.close();
  console.log(failed ? `${failed} test(s) failed` : `all ${tests.filter(t => selected(t.name)).length} tests passed`);
  process.exit(failed ? 1 : 0);
})();
