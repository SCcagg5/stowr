/*!
 * The object browser page of the interface.
 *
 * The page renders its DOM directly (no framework). The shell has the header, the theme control,
 * a tab for each bucket, parts. The listing is a dense table in the manner of ClickHouse Dash. The page uses the
 * shared modules of assets/js/common (api.js, runtime.js, actions.js, ...).
 */
const config = {
  pageSize: 50,
  allowDownloadAll: true,
  keyExcludePatterns: [/^index\.html$/],
  instanceId: '',
  capabilities: {},
  operations: {},
  runtime: {},
  versioningSupported: false,
  tagsSupported: true,
  metadataEditSupported: true,
  multipartUploadsSupported: true,
  lifecycleSupported: true,
  objectLockSupported: false,
  bucketEncryptionSupported: false
};
window.BB = window.BB || {};
BB.cfg = config;

(function main() {
  'use strict';

  // ---------------------------------------------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------------------------------------------

  function normalizePrefix(value) {
    const clean = String(value || '').replace(/^\/+/, '').replace(/\/+$/, '');
    return clean ? `${clean}/` : '';
  }

  function escapeHTML(value) {
    const span = document.createElement('span');
    span.textContent = String(value == null ? '' : value);
    return span.innerHTML;
  }

  function h(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (value == null || value === false) continue;
      if (key === 'class') node.className = Array.isArray(value) ? value.filter(Boolean).join(' ') : String(value);
      else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value === true ? '' : String(value));
    }
    for (const child of children.flat(Infinity)) {
      if (child == null || child === false) continue;
      node.append(child.nodeType ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  // ic(name): an icon of the shell (Tabler name). mdi(name): an icon name of the shared scripts.
  const ic = (name, extra = '') => h('i', { class: ['ic', `ic-${name}`, extra], 'aria-hidden': 'true' });
  const mdi = (name, extra = '') => h('i', { class: ['mdi', `mdi-${name}`, extra], 'aria-hidden': 'true' });
  const byId = id => document.getElementById(id);

  function plural(count, singular, pluralForm = `${singular}s`) {
    const value = Math.max(0, Number(count) || 0);
    return `${value.toLocaleString('en-US')} ${value === 1 ? singular : pluralForm}`;
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Drag and drop: the files of a dropped selection
  // ---------------------------------------------------------------------------------------------------------------

  function dataTransferHasFiles(dataTransfer) {
    if (!dataTransfer) return false;
    if (Array.from(dataTransfer.types || []).includes('Files')) return true;
    return Array.from(dataTransfer.items || []).some(item => item.kind === 'file');
  }

  function cleanDroppedPath(value) {
    return String(value || '')
      .replace(/\\/g, '/')
      .replace(/^\/+/, '')
      .split('/')
      .filter(segment => segment && segment !== '.')
      .map(segment => (segment === '..' ? '_' : segment))
      .join('/');
  }

  const readDirectoryBatch = reader => new Promise((resolve, reject) => reader.readEntries(resolve, reject));
  const readEntryFile = entry => new Promise((resolve, reject) => entry.file(resolve, reject));

  async function walkEntry(entry, parent, output) {
    if (!entry) return;
    const relative = cleanDroppedPath(parent ? `${parent}/${entry.name}` : entry.name);
    if (entry.isFile) {
      output.push({ file: await readEntryFile(entry), relative });
      return;
    }
    if (!entry.isDirectory) return;
    const reader = entry.createReader();
    for (;;) {
      const batch = await readDirectoryBatch(reader);
      if (!batch.length) break;
      for (const child of batch) await walkEntry(child, relative, output);
    }
  }

  async function droppedEntries(dataTransfer) {
    const output = [];
    const items = Array.from(dataTransfer?.items || []).filter(item => item.kind === 'file');
    const roots = items.map(item => (typeof item.webkitGetAsEntry === 'function' ? item.webkitGetAsEntry() : null)).filter(Boolean);
    if (roots.length) {
      for (const entry of roots) await walkEntry(entry, '', output);
    } else {
      for (const file of Array.from(dataTransfer?.files || [])) {
        output.push({ file, relative: cleanDroppedPath(file.webkitRelativePath || file.name) });
      }
    }
    const unique = new Map();
    for (const entry of output) if (entry.file && entry.relative) unique.set(entry.relative, entry);
    return Array.from(unique.values());
  }

  // ---------------------------------------------------------------------------------------------------------------
  // State
  // ---------------------------------------------------------------------------------------------------------------

  // The columns of the listing, the tag filter and the encryption of the files: the person's choice stays in this
  // browser. Every column is on by default, and the browser keeps only the choices that the person made. The choice of
  // the encryption and the lock is for each bucket: without a choice, it is on.
  const LISTING_KEY = 'stowr.listing';
  const LISTING_VERSION = 1;
  const LISTING_DEFAULT = { columns: { tags: true, versions: true, size: true, modified: true }, tagFilterOn: false };

  function loadListing() {
    const result = { columnChoice: {}, tagFilterOn: LISTING_DEFAULT.tagFilterOn, encryption: {} };
    try {
      const stored = JSON.parse(window.localStorage.getItem(LISTING_KEY) || '{}');
      if (stored?.version !== LISTING_VERSION) return result;
      for (const name of Object.keys(LISTING_DEFAULT.columns)) {
        const value = stored?.columns?.[name];
        if (typeof value === 'boolean') result.columnChoice[name] = value;
      }
      if (typeof stored?.tagFilterOn === 'boolean') result.tagFilterOn = stored.tagFilterOn;
      for (const [id, value] of Object.entries(stored?.encryption || {})) if (typeof value === 'boolean') result.encryption[id] = value;
    } catch (_) {}
    return result;
  }

  const listingColumns = choice => ({ ...LISTING_DEFAULT.columns, ...choice });

  function saveListing() {
    try {
      window.localStorage.setItem(LISTING_KEY, JSON.stringify({ version: LISTING_VERSION, columns: state.columnChoice, tagFilterOn: state.tagFilterOn, encryption: state.encryptionChoice }));
    } catch (_) {}
  }

  const listing = loadListing();

  const state = {
    instances: [],
    build: {},
    defaultInstanceId: '',
    instanceId: '',
    pathPrefix: '',
    rows: [],
    // '' | 'forbidden' | 'signin' | 'offline' | 'error': why the listing has no rows.
    listError: '',
    listErrorText: '',
    previousContinuationTokens: [],
    continuationToken: '',
    nextContinuationToken: '',
    scanOffset: 0,
    localPage: 0,
    navigationSortAvailable: false,
    navigationSortField: '',
    navigationSortDirection: '',
    modifiedTimeMode: 'relative',
    isRefreshing: false,
    isInitializing: true,
    loadError: '',
    pageSize: config.pageSize,
    dragDepth: 0,
    isDragActive: false,
    versionCountRequest: 0,
    refreshRequest: 0,
    // The columns: the choices of the person, and the columns that apply. A provider without tags (501) turns the tag
    // controls off.
    columnChoice: listing.columnChoice,
    columns: listingColumns(listing.columnChoice),
    tagFilterOn: listing.tagFilterOn,
    tagFilter: '',
    tagRequest: 0,
    tagSignature: '',
    tagsUnsupported: false,
    // The encryption of the files: the choice of the settings for each bucket, and the reads of the files on the page.
    encryptionChoice: listing.encryption,
    encryptionRequest: 0,
    encryptionSignature: ''
  };

  const ui = {};

  function currentInstance() {
    return state.instances.find(instance => instance.id === state.instanceId) || null;
  }

  // A volume (provider fs) is a folder of the server: its folders are real, and it has no tags, versions, lifecycle
  // rules, Object Lock or encryption of S3. The page names it a volume, not a bucket.
  const isVolume = instance => String(instance?.provider || '') === 'fs';
  const storageNoun = instance => (isVolume(instance) ? 'volume' : 'bucket');
  const capitalized = text => text.charAt(0).toUpperCase() + text.slice(1);
  const storageIcon = instance => (isVolume(instance) ? 'folder-open' : 'database');

  function can(operation, instance = currentInstance()) {
    if (BB.capabilities?.actionable) return BB.capabilities.actionable(instance, operation);
    const operationState = instance?.operations?.[operation];
    return operationState ? operationState.allowed !== false : true;
  }

  const canRead = () => can('list');
  const canWrite = () => can('upload');
  const canDelete = () => can('delete');
  const canCopy = () => can('copy');
  const canRename = () => can('rename');
  const versioningSupported = () => currentInstance()?.versioningSupported === true;
  const canDownloadAll = () => canRead() && config.allowDownloadAll && state.rows.length > 0;

  function orderedRows() {
    const rows = Array.from(state.rows || []);
    if (!state.navigationSortAvailable || !state.navigationSortField || !state.navigationSortDirection) return rows;
    const field = state.navigationSortField;
    const direction = state.navigationSortDirection === 'desc' ? -1 : 1;
    const nameOf = row => String(row?.name || '').toLocaleLowerCase();
    rows.sort((left, right) => {
      let comparison = 0;
      if (field === 'size') {
        comparison = Number(left?.type === 'content' ? left.size : 0) - Number(right?.type === 'content' ? right.size : 0);
      } else if (field === 'dateModified') {
        comparison = Number(left?.dateModified instanceof Date ? left.dateModified.getTime() : 0)
          - Number(right?.dateModified instanceof Date ? right.dateModified.getTime() : 0);
      } else {
        comparison = nameOf(left).localeCompare(nameOf(right));
      }
      if (comparison === 0) comparison = nameOf(left).localeCompare(nameOf(right));
      return comparison * direction;
    });
    return rows;
  }

  const localPageCount = () => Math.max(1, Math.ceil(orderedRows().length / state.pageSize));

  function visibleRows() {
    const start = state.localPage * state.pageSize;
    return orderedRows().slice(start, start + state.pageSize);
  }

  const rangeStart = () => (visibleRows().length ? state.scanOffset + state.localPage * state.pageSize + 1 : 0);
  const rangeEnd = () => state.scanOffset + state.localPage * state.pageSize + visibleRows().length;
  const hasPreviousPage = () => state.localPage > 0 || state.previousContinuationTokens.length > 0;
  const hasNextPage = () => state.localPage + 1 < localPageCount() || Boolean(state.nextContinuationToken);

  function crumbs() {
    const parts = String(state.pathPrefix || '').replace(/\/+$/, '').split('/').filter(Boolean);
    let prefix = '';
    return parts.map(name => {
      prefix += `${name}/`;
      return { name, prefix };
    });
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Formatting: sizes in base 1024 with one decimal, times relative or exact (UTC)
  // ---------------------------------------------------------------------------------------------------------------

  function byteParts(size) {
    const value = Math.max(0, Number(size) || 0);
    const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    let scaled = value;
    let unit = 0;
    while (scaled >= 1024 && unit < units.length - 1) {
      scaled /= 1024;
      unit += 1;
    }
    const text = unit === 0 ? String(Math.round(scaled)) : scaled.toFixed(scaled >= 100 ? 0 : 1).replace(/\.0$/, '');
    return { value: text, unit: units[unit] };
  }

  function formatBytes(size) {
    const parts = byteParts(size);
    return `${parts.value} ${parts.unit}`;
  }

  function formatRelative(date) {
    const timestamp = new Date(date).getTime();
    if (!Number.isFinite(timestamp)) return '';
    const seconds = Math.round((timestamp - Date.now()) / 1000);
    const ranges = [['year', 31536000], ['month', 2592000], ['week', 604800], ['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]];
    const formatter = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });
    for (const [unit, divisor] of ranges) {
      if (Math.abs(seconds) >= divisor || unit === 'second') return formatter.format(Math.round(seconds / divisor), unit);
    }
    return 'now';
  }

  const formatExact = date => BB.runtime.formatDateTimeUTC(date);
  const formatModified = date => (state.modifiedTimeMode === 'absolute' ? formatExact(date) : formatRelative(date));

  // ---------------------------------------------------------------------------------------------------------------
  // Header: the build
  // ---------------------------------------------------------------------------------------------------------------

  // The version of the server under the name of the product. The page keeps the last text in this browser, so a
  // reload does not show "--" until /api/instances answers.
  const VERSION_KEY = 'stowr.version';

  function versionText(build) {
    const version = String(build.version || build.display || 'dev');
    const commit = String(build.shortCommit || '');
    return commit && commit !== 'unknown' && !version.includes(commit) ? `${version} (${commit})` : version;
  }

  function renderBuild() {
    const build = state.build || {};
    if (!build.version && !build.display) return;
    const text = versionText(build);
    const title = `Server version\n${text}${build.date ? `\nBuild: ${build.date}` : ''}`;
    if (ui.versionBadge.textContent !== text) ui.versionBadge.textContent = text;
    ui.versionBadge.title = title;
    try { localStorage.setItem(VERSION_KEY, JSON.stringify({ text, title })); } catch (_) {}
  }

  function restoreVersion() {
    try {
      const last = JSON.parse(localStorage.getItem(VERSION_KEY) || 'null');
      if (last && last.text) {
        ui.versionBadge.textContent = String(last.text);
        ui.versionBadge.title = String(last.title || '');
      }
    } catch (_) {}
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Bucket selector: the shared selector of the header (switcher.js). A bucket of the list opens in this page.
  // ---------------------------------------------------------------------------------------------------------------

  function renderSwitcher() {
    ui.selector.setState({ instances: state.instances, currentId: state.instanceId, loading: state.isInitializing, loadError: state.loadError });
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Crumbs: the bucket mark and a slash, then the folders
  // ---------------------------------------------------------------------------------------------------------------

  const CRUMB_EDGE = 2;

  function renderCrumbs() {
    const instance = currentInstance();
    const list = crumbs();
    const signature = JSON.stringify([instance?.id, instance?.name, state.pathPrefix]);
    if (ui.crumbsSignature === signature) return;
    ui.crumbsSignature = signature;
    if (!instance) {
      ui.crumbs.replaceChildren();
      return;
    }
    const atRoot = !list.length;
    const where = instance.rootPrefix ? `${instance.bucket}/${instance.rootPrefix}` : instance.bucket;
    const separator = () => h('span', { class: 'crumbs__sep', 'aria-hidden': 'true' }, '/');
    // The root is the bucket mark and a slash, with no button around: the selector names the bucket.
    const nodes = [h('button', {
      type: 'button',
      class: 'crumb crumb--root',
      'aria-current': atRoot ? 'page' : null,
      'aria-label': `${instance.name}, ${storageNoun(instance)} root`,
      title: `Root of ${instance.name} (${where})`,
      'data-crumb': 'root',
      onclick: () => { if (!atRoot) goToPrefix(''); }
    }, ic(storageIcon(instance))), separator()];

    // A deep path keeps the root, a menu for the hidden parents, and the last CRUMB_EDGE folders.
    const hidden = list.length > CRUMB_EDGE + 1 ? list.slice(0, list.length - CRUMB_EDGE) : [];
    const shown = hidden.length ? list.slice(list.length - CRUMB_EDGE) : list;
    if (hidden.length) {
      const menu = h('div', { class: 'menu', role: 'menu', hidden: true },
        hidden.map(crumb => h('button', { type: 'button', class: 'menu__item', role: 'menuitem', title: crumb.prefix, onclick: () => goToPrefix(crumb.prefix) },
          ic('folder'), crumb.name)));
      const trigger = h('button', {
        type: 'button', class: 'crumb crumb--more', 'aria-haspopup': 'menu', 'aria-expanded': 'false',
        'aria-label': `Show ${plural(hidden.length, 'parent folder')}`, title: hidden.map(crumb => crumb.name).join(' / ')
      }, h('span', {}, '…'));
      nodes.push(h('span', { class: 'menuWrap' }, trigger, menu), separator());
      bindMenu(trigger, menu);
    }
    shown.forEach((crumb, index) => {
      const last = index === shown.length - 1;
      nodes.push(h('button', {
        type: 'button',
        class: 'crumb',
        'aria-current': last ? 'page' : null,
        title: crumb.prefix,
        onclick: () => { if (!last) goToPrefix(crumb.prefix); }
      }, h('span', {}, crumb.name)));
      if (!last) nodes.push(separator());
    });
    ui.crumbs.replaceChildren(...nodes);
  }

  function renderTitle() {
    const instance = currentInstance();
    const list = crumbs();
    let title = 'Loading the buckets';
    if (!state.isInitializing && !instance) title = state.loadError ? 'The buckets cannot be loaded' : 'No bucket';
    else if (instance) title = list.length ? list[list.length - 1].name : instance.name;
    // The heading is for screen readers: the crumbs show the place on the screen.
    const heading = instance ? `${title}${list.length ? `, in ${instance.name}` : `, ${storageNoun(instance)} root`}` : title;
    if (ui.title.textContent !== heading) ui.title.textContent = heading;
    document.title = instance ? `${title} - ${instance.name} - Stowr` : 'Stowr';
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Toolbar and menus
  // ---------------------------------------------------------------------------------------------------------------

  let openMenu = null;

  function closeMenu({ focus = false } = {}) {
    if (!openMenu) return;
    const { trigger, menu } = openMenu;
    openMenu = null;
    menu.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    if (focus) trigger.focus();
  }

  // A menu opens under its button, aligned to the side that keeps it inside the window.
  function placeMenu(menu) {
    menu.classList.remove('menu--start', 'menu--end');
    const wrap = menu.parentElement.getBoundingClientRect();
    const width = menu.offsetWidth;
    const fitsEnd = wrap.right - width >= 8;
    const fitsStart = wrap.left + width <= window.innerWidth - 8;
    menu.classList.add(fitsEnd && (!fitsStart || wrap.left + wrap.width / 2 > window.innerWidth / 2) ? 'menu--end' : 'menu--start');
  }

  // stayOnTab: the menu has fields, so Tab moves between them and does not close the menu.
  function bindMenu(trigger, menu, { stayOnTab = false } = {}) {
    const items = () => Array.from(menu.querySelectorAll('.menu__item:not(:disabled)'));
    trigger.addEventListener('click', event => {
      event.stopPropagation();
      if (openMenu?.menu === menu) {
        closeMenu();
        return;
      }
      closeMenu();
      ui.selector.close(false);
      openMenu = { trigger, menu };
      menu.hidden = false;
      trigger.setAttribute('aria-expanded', 'true');
      placeMenu(menu);
    });
    trigger.addEventListener('keydown', event => {
      if (event.key !== 'ArrowDown') return;
      event.preventDefault();
      if (openMenu?.menu !== menu) trigger.click();
      items()[0]?.focus();
    });
    menu.addEventListener('click', event => {
      if (event.target.closest('.menu__item')) closeMenu();
    });
    menu.addEventListener('keydown', event => {
      const list = items();
      const index = list.indexOf(document.activeElement);
      if (event.key === 'ArrowDown') { event.preventDefault(); list[Math.min(list.length - 1, index + 1)]?.focus(); }
      else if (event.key === 'ArrowUp') { event.preventDefault(); list[Math.max(0, index - 1)]?.focus(); }
      else if (event.key === 'Escape') { event.stopPropagation(); closeMenu({ focus: true }); }
      else if (event.key === 'Tab' && !stayOnTab) closeMenu();
    });
  }

  function accessBadge() {
    const instance = currentInstance();
    if (!instance || !canRead() || state.listError) return null;
    const write = canWrite();
    const remove = canDelete();
    if (write && remove) return null;
    if (!write && !remove) return { text: 'Read only', title: `You can read this ${storageNoun(instance)} but not change it.` };
    if (!write) return { text: 'No upload', title: 'You cannot write in this folder.' };
    return { text: 'No delete', title: 'You cannot delete in this folder.' };
  }

  function renderToolbar() {
    const instance = currentInstance();
    const read = Boolean(instance) && canRead();
    ui.searchInput.disabled = !read;
    ui.refreshButton.disabled = !instance || state.isRefreshing;
    ui.actionsButton.disabled = !instance;
    ui.zipItem.disabled = !canDownloadAll();
    ui.insightsItem.disabled = !read || Boolean(state.listError);
    ui.insightsItem.querySelector('span').textContent = state.pathPrefix ? 'Folder insights' : `${capitalized(storageNoun(instance))} insights`;
    ui.newFolderItem.hidden = !isVolume(instance);
    ui.newFolderItem.disabled = !read || !canWrite() || Boolean(state.listError);
    ui.uploadWrap.hidden = !instance || !canWrite() || Boolean(state.listError);
    ui.folderTagsItem.hidden = state.tagsUnsupported;
    ui.folderTagsItem.querySelector('span').textContent = state.pathPrefix ? 'Tag this folder' : 'Tag this bucket';
    ui.folderTagsItem.disabled = !read || !canWrite() || Boolean(state.listError);
    ui.multipartsItem.hidden = !config.multipartUploadsSupported;
    ui.multipartsItem.disabled = !read || Boolean(state.listError);
    ui.tagFilterWrap.hidden = !state.tagFilterOn || state.tagsUnsupported;
    renderSettings();
    const badge = accessBadge();
    ui.accessBadge.hidden = !badge;
    if (badge) {
      ui.accessBadge.replaceChildren(ic('lock'), badge.text);
      ui.accessBadge.title = badge.title;
    }
    const folderName = crumbs().slice(-1)[0]?.name;
    const label = folderName ? `Open a prefix in ${folderName}` : `Open a prefix in this ${storageNoun(instance)}`;
    if (ui.searchInput.placeholder !== label) {
      ui.searchInput.placeholder = label;
      ui.searchLabel.textContent = label;
    }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Listing: states, table and pagination
  // ---------------------------------------------------------------------------------------------------------------

  function emptyBox(iconName, title, text, ...actions) {
    return h('div', { class: 'empty', 'data-empty': iconName }, ic(iconName), h('strong', {}, title), text && h('span', {}, text), actions);
  }

  function skeleton() {
    const widths = [62, 48, 70, 40, 56, 66];
    const bar = width => {
      const node = h('span', { class: 'skeletonBar' });
      node.style.width = `${width}%`;
      return node;
    };
    return h('div', { class: 'tableBox', 'aria-busy': 'true', 'aria-label': 'Loading the folder' },
      h('table', { class: 'dataTable' },
        h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', { class: 'num col-size' }, 'Size'), h('th', { class: 'col-modified' }, 'Modified'), h('th', { class: 'col-menu' }))),
        h('tbody', {}, widths.map(width => h('tr', { class: 'skeletonRow' },
          h('td', {}, bar(width)),
          h('td', { class: 'col-size' }, bar(80)),
          h('td', { class: 'col-modified' }, bar(70)),
          h('td', { class: 'col-menu' }))))));
  }

  function listingMode() {
    if (state.isInitializing) return 'loading';
    if (state.loadError) return 'load-error';
    if (!currentInstance()) return 'none';
    if (!canRead()) return 'unreadable';
    if (state.listError) return `error-${state.listError}`;
    if (!state.rows.length) return state.isRefreshing ? 'loading' : (canWrite() ? 'empty-writable' : 'empty');
    return 'table';
  }

  function renderListing() {
    const mode = listingMode();
    const signature = `${mode}:${state.listErrorText}:${state.loadError}:${state.pathPrefix}`;
    if (signature !== ui.listingSignature) {
      ui.listingSignature = signature;
      ui.table = null;
      let content;
      switch (mode) {
        case 'loading':
          content = skeleton();
          break;
        case 'load-error':
          content = emptyBox('cloud-off', 'The list of buckets cannot be loaded', state.loadError,
            h('button', { type: 'button', class: 'button', onclick: () => initialize() }, ic('refresh'), 'Try again'));
          break;
        case 'none':
          content = emptyBox('lock', 'No bucket is available', 'The configuration of the server has no bucket and no volume.');
          break;
        case 'unreadable':
          content = emptyBox('lock', 'You cannot list this bucket', 'Read access is not available. Other actions stay hidden.');
          break;
        case 'error-forbidden':
          content = emptyBox('lock', 'You cannot open this folder', 'The storage does not let you read it.',
            state.pathPrefix && h('button', { type: 'button', class: 'button', onclick: () => goToPrefix('') }, ic(storageIcon(currentInstance())), `Go to the ${storageNoun(currentInstance())} root`));
          break;
        case 'error-signin':
          // The notice at the top of the page gives the message and the "Reload" button.
          content = emptyBox('lock', 'This folder shows after a new sign-in', '');
          break;
        case 'error-offline':
          content = emptyBox('cloud-off', 'The server does not answer', 'The folder cannot be listed now.',
            h('button', { type: 'button', class: 'button', onclick: () => manualRefresh() }, ic('refresh'), 'Try again'));
          break;
        case 'error-error':
          content = emptyBox('alert-triangle', 'The folder cannot be listed', state.listErrorText,
            h('button', { type: 'button', class: 'button', onclick: () => manualRefresh() }, ic('refresh'), 'Try again'));
          break;
        case 'empty-writable':
          content = emptyBox('folder-open', 'This folder is empty', 'Drop files on the page, or upload files or a folder.',
            h('button', { type: 'button', class: 'button button--primary', onclick: triggerUploadFiles }, ic('upload'), 'Upload files'));
          break;
        case 'empty':
          content = emptyBox('folder-open', 'This folder is empty', '');
          break;
        default:
          content = buildTable();
      }
      ui.listing.replaceChildren(content);
    }
    if (mode === 'table') renderTable();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Facts of the buckets in the selector: versioning, encryption, Object Lock, lifecycle rules and usage
  // ---------------------------------------------------------------------------------------------------------------

  // The facts of the bucket in the selector: switcher.js reads the versioning, the rules and the usage.
  const loadUsage = (instance, options) => ui.selector.loadUsage(instance, options);
  const loadStatus = (instance, options) => ui.selector.loadStatus(instance, options);

  const usageTimers = [];

  // A change of the objects (an upload, a delete, a copy, a restore, the end of a job): the usage is read again after
  // a pause, one time for a group of changes, and one more time after 12 seconds. The server reads the provider again
  // only for a value older than 10 seconds, and SeaweedFS and Garage count a write after a few seconds.
  function noteChange() {
    usageTimers.splice(0).forEach(timer => window.clearTimeout(timer));
    for (const wait of [1500, 12000]) usageTimers.push(window.setTimeout(() => loadUsage(currentInstance(), { fresh: true }), wait));
  }

  // The banner of a full bucket at the top of the listing: it stays while the usage is 90 % or more of the limit. The
  // tooltip of the upload button says it too; the upload stays possible.
  function renderUsageBanner(usage) {
    if (!ui.usageBanner) return;
    const text = usage ? BB.usage.fullText(usage) : '';
    if (ui.usageBanner.dataset.text !== text) {
      ui.usageBanner.dataset.text = text;
      ui.usageBanner.hidden = !text;
      const note = usage ? BB.usage.approximateNote(usage) : '';
      ui.usageBanner.replaceChildren(...(text ? [ic('alert-triangle'),
        h('span', { class: 'usageBanner__text' }, h('strong', {}, BB.usage.fullText(usage, { note: false })), note ? ` ${note}` : null)] : []));
    }
    if (text) ui.uploadButton.title = text;
    else ui.uploadButton.removeAttribute('title');
  }

  function sortHead(field, label) {
    const iconBox = h('span', { class: 'sortHead__icon' });
    const node = h('button', { type: 'button', class: 'sortHead', onclick: () => toggleSort(field) }, label, iconBox);
    return { field, label, node, iconBox };
  }

  function buildTable() {
    const sorts = { name: sortHead('name', 'Name'), size: sortHead('size', 'Size'), dateModified: sortHead('dateModified', 'Modified') };
    const timeToggle = h('button', { type: 'button', class: 'timeToggle', onclick: event => { event.stopPropagation(); toggleTimeMode(); } }, ic('clock'));
    const tbody = h('tbody');
    const box = h('div', { class: 'tableBox' },
      h('table', { class: 'dataTable', 'aria-label': 'Folder content' },
        h('thead', {}, h('tr', {},
          h('th', { scope: 'col' }, sorts.name.node),
          h('th', { scope: 'col', class: 'col-tags' }, h('span', { class: 'visually-hidden' }, 'Tags')),
          h('th', { scope: 'col', class: 'col-versions' }, h('span', { class: 'visually-hidden' }, 'Versions')),
          h('th', { scope: 'col', class: 'num col-size' }, sorts.size.node),
          h('th', { scope: 'col', class: 'col-modified' }, h('span', { class: 'headTools' }, timeToggle, sorts.dateModified.node)),
          h('th', { scope: 'col', class: 'col-menu' }, h('span', { class: 'visually-hidden' }, 'Actions')))),
        tbody));
    const range = h('span', { class: 'tableFoot__range' });
    const pageSize = h('select', {
      'aria-label': 'Rows on each page',
      onchange: event => {
        state.pageSize = Number(event.target.value) || 50;
        config.pageSize = state.pageSize;
        state.localPage = 0;
        update();
        refreshVisibleVersionCounts();
      }
    }, [50, 100, 500].map(size => h('option', { value: size }, size)));
    const previous = h('button', { type: 'button', class: 'button button--small button--icon', 'aria-label': 'Previous page', title: 'Previous page', onclick: () => previousPage() }, ic('chevron-left'));
    const next = h('button', { type: 'button', class: 'button button--small button--icon', 'aria-label': 'Next page', title: 'Next page', onclick: () => nextPage() }, ic('chevron-right'));
    const pager = h('div', { class: 'tableFoot__pager' }, previous, next);
    const foot = h('div', { class: 'tableFoot' },
      range,
      h('label', {}, 'Rows', h('span', { class: 'select' }, pageSize)),
      pager);
    ui.table = { sorts, timeToggle, tbody, box, range, pageSize, previous, next, pager, foot };
    return h('div', { class: 'listing' }, box, foot);
  }

  function sortIcon(field) {
    if (!state.navigationSortAvailable) return '';
    if (state.navigationSortField !== field) return 'selector';
    return state.navigationSortDirection === 'desc' ? 'arrow-down' : 'arrow-up';
  }

  function renderTable() {
    const t = ui.table;
    for (const entry of Object.values(t.sorts)) {
      const available = state.navigationSortAvailable;
      entry.node.disabled = !available;
      const active = state.navigationSortField === entry.field;
      entry.node.setAttribute('aria-sort', active ? (state.navigationSortDirection === 'desc' ? 'descending' : 'ascending') : 'none');
      entry.node.title = available ? `Sort by ${entry.label.toLowerCase()}` : 'The order of the provider. Sorting needs the whole folder in one listing.';
      const name = sortIcon(entry.field);
      entry.iconBox.replaceChildren(...(name ? [ic(name)] : []));
    }
    const absolute = state.modifiedTimeMode === 'absolute';
    t.timeToggle.setAttribute('aria-pressed', absolute ? 'true' : 'false');
    t.timeToggle.title = absolute ? 'Show relative times' : 'Show exact times (UTC)';
    t.timeToggle.setAttribute('aria-label', t.timeToggle.title);
    t.box.classList.toggle('is-busy', state.isRefreshing);

    const table = t.box.querySelector('table');
    table.classList.toggle('hide-tags', !state.columns.tags || state.tagsUnsupported);
    table.classList.toggle('hide-versions', !state.columns.versions || !versioningSupported());
    table.classList.toggle('hide-size', !state.columns.size);
    table.classList.toggle('hide-modified', !state.columns.modified);
    table.classList.toggle('is-exact-time', state.modifiedTimeMode === 'absolute');
    t.tbody.replaceChildren(...visibleRows().map(buildRow));
    applyTagFilter();
    refreshVisibleTags();
    refreshVisibleMarks();
    const total = state.nextContinuationToken ? `${rangeEnd().toLocaleString('en-US')}+` : (state.scanOffset + state.rows.length).toLocaleString('en-US');
    t.range.textContent = `${rangeStart().toLocaleString('en-US')} to ${rangeEnd().toLocaleString('en-US')} of ${total}`;
    t.pageSize.value = String(state.pageSize);
    t.pageSize.disabled = state.isRefreshing;
    t.previous.disabled = !hasPreviousPage() || state.isRefreshing;
    t.next.disabled = !hasNextPage() || state.isRefreshing;
    t.pager.hidden = !hasPreviousPage() && !hasNextPage();
  }

  function menuItem(label, iconName, onclick, { danger = false } = {}) {
    return h('button', { type: 'button', class: ['bb-menu-item', danger && 'danger'], role: 'menuitem', onclick }, mdi(iconName), label);
  }

  function rowMenu(row) {
    const items = row.type === 'content'
      ? [
        canRead() && menuItem('Details', 'information-outline', () => onRowMetadata(row)),
        canRead() && menuItem('Download', 'download', () => onRowDownload(row)),
        canCopy() && menuItem('Copy', 'content-copy', () => onRowCopy(row)),
        canRename() && menuItem('Rename', 'rename-outline', () => onRowRename(row)),
        canDelete() && menuItem('Delete', 'delete-outline', () => onRowDelete(row), { danger: true })
      ]
      : [
        canRead() && menuItem('Insights', 'chart-box-outline', () => onPrefixInsights(row)),
        canRead() && menuItem('Download as ZIP', 'folder-download-outline', () => onPrefixDownload(row)),
        canWrite() && !state.tagsUnsupported && menuItem('Tag this folder', 'tag-multiple-outline', () => tagFolder(row.prefix)),
        canCopy() && menuItem('Copy', 'content-copy', () => onPrefixCopy(row)),
        canRename() && menuItem('Rename', 'rename-outline', () => onPrefixRename(row)),
        canDelete() && menuItem('Delete', 'delete-outline', () => onPrefixDelete(row), { danger: true })
      ];
    return h('div', { class: 'bb-menu', ...(row.type === 'content' ? { 'data-key': row.key } : { 'data-prefix': row.prefix }) },
      h('button', { type: 'button', class: 'bb-kebab', 'aria-haspopup': 'menu', 'aria-expanded': 'false', 'aria-label': `Actions for ${row.name}`, title: 'Actions' }, mdi('dots-vertical')),
      h('div', { class: 'bb-menu-popover', role: 'menu' }, h('div', { class: 'bb-menu-list' }, items)));
  }

  // The versions of a file: the icon and the count, only when the file has more than one version. A click opens the
  // details of the file, where the version picker is.
  function versionCell(row) {
    if (row.type !== 'content' || !versioningSupported() || !Number.isInteger(row.versionCount) || row.versionCount < 2) return null;
    return h('button', {
      type: 'button', class: 'badge versionCount', title: `${plural(row.versionCount, 'version')}: open the details`,
      'aria-label': `${plural(row.versionCount, 'version')} of ${row.name}`, onclick: () => onRowMetadata(row)
    }, ic('git-commit'), row.versionCount.toLocaleString('en-US'));
  }

  // An entry of a volume that the server does not read: no link, no size, no action, and a tooltip that says why.
  function buildUnreadableRow(row) {
    const reason = 'Stowr does not read this entry: it is a symbolic link that leaves the volume or that does not resolve, or it is not a regular file.';
    const tr = h('tr', { 'data-name': row.name, 'data-type': row.type, 'data-unreadable': 'true', class: 'is-unreadable' },
      h('td', {}, h('div', { class: 'nameCell is-unreadable', title: reason },
        ic('ban'), h('span', { class: 'nameText' }, row.name), h('span', { class: 'visually-hidden' }, ` (${reason})`))),
      h('td', { class: 'col-tags' }),
      h('td', { class: 'col-versions' }),
      h('td', { class: 'num col-size' }),
      h('td', { class: 'col-modified' }),
      h('td', { class: 'col-menu' }));
    row.element = tr;
    return tr;
  }

  function buildRow(row) {
    if (row.unreadable) return buildUnreadableRow(row);
    const isContent = row.type === 'content';
    const name = h('div', { class: ['nameCell', !isContent && 'is-folder'] },
      isContent ? mdi(fileIcon(row)) : ic('folder'),
      h('button', { type: 'button', class: 'nameLink', title: isContent ? row.key : row.prefix, onclick: () => (isContent ? openPreview(row) : goToPrefix(row.prefix)) }, row.name),
      isContent ? h('span', { class: 'nameLock' }, objectMarks(row)) : null);
    let size;
    if (isContent) {
      const parts = byteParts(row.size);
      size = h('span', { class: 'sizeCell', title: `${row.size.toLocaleString('en-US')} bytes` }, parts.value, h('span', { class: 'sizeCell__unit' }, parts.unit));
    } else {
      size = h('span', { class: 'visually-hidden' }, 'Folder');
    }
    const modified = row.dateModified
      ? h('time', { class: 'modifiedCell', datetime: row.dateModified.toISOString(), title: formatExact(row.dateModified) }, formatModified(row.dateModified))
      : null;
    const tr = h('tr', { 'data-name': row.name, 'data-type': row.type },
      h('td', {}, name),
      h('td', { class: 'col-tags' }, tagCell(row)),
      h('td', { class: 'col-versions' }, versionCell(row)),
      h('td', { class: 'num col-size' }, size),
      h('td', { class: 'col-modified' }, modified),
      h('td', { class: 'col-menu' }, rowMenu(row)));
    row.element = tr;
    return tr;
  }

  function updateVersionBadges() {
    for (const row of visibleRows()) {
      const cell = row.element?.querySelector('.col-versions');
      if (cell) cell.replaceChildren(...[versionCell(row)].filter(Boolean));
    }
  }

  function renderDropOverlay() {
    ui.dropOverlay.hidden = !state.isDragActive;
    ui.dropTarget.textContent = `${currentInstance()?.name || ''} / ${state.pathPrefix}`;
  }

  function update() {
    renderBuild();
    renderSwitcher();
    renderCrumbs();
    renderTitle();
    renderToolbar();
    renderListing();
    renderDropOverlay();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Navigation and loading
  // ---------------------------------------------------------------------------------------------------------------

  function classify(error) {
    const status = Number(error?.status || 0);
    if (status === 401) return 'signin';
    if (status === 403) return 'forbidden';
    if ([502, 503, 504].includes(status) || (!status && (error instanceof TypeError || /network|fetch/i.test(String(error?.message || ''))))) return 'offline';
    return 'error';
  }

  async function initialize() {
    state.isInitializing = true;
    state.loadError = '';
    update();
    try {
      const response = await BB.api.instances();
      state.instances = (response.instances || []).map(instance => ({ ...instance, host: String(instance.host || instance.id || '') }));
      state.build = response.build || {};
      config.runtime = BB.runtime?.configure(response.runtime || {}) || (response.runtime || {});
      state.defaultInstanceId = response.default || state.instances[0]?.id || '';
      const route = BB.runtime?.storageRoute?.();
      const routeInstance = route?.view === 'browser'
        ? state.instances.find(instance => instance.host === route.host && instance.id === route.bucket)?.id || ''
        : '';
      const queryInstance = new URLSearchParams(location.search).get('instance');
      const candidate = [routeInstance, queryInstance, state.defaultInstanceId]
        .find(id => id && state.instances.some(instance => instance.id === id));
      state.instanceId = candidate || state.instances[0]?.id || '';
      state.isInitializing = false;
      if (!state.instanceId) {
        update();
        return;
      }
      // The selector gets the buckets first: it reads their facts.
      renderSwitcher();
      applyCurrentInstance();
      // The usage of the other buckets: the mark of a full bucket shows also before the menu opens.
      ui.selector.loadAllUsage();
      state.pathPrefix = routeInstance === state.instanceId ? normalizePrefix(route.path) : '';
      ui.searchInput.value = '';
      replaceLocation(state.pathPrefix);
      update();
      await refresh();
    } catch (error) {
      state.isInitializing = false;
      const kind = classify(error);
      state.loadError = kind === 'signin' ? 'Your sign-in has ended. Reload the page to sign in again.'
        : kind === 'offline' ? 'The server does not answer.' : String(error?.message || error);
      update();
    }
  }

  function browserLocation(prefix = '') {
    const url = new URL(BB.api.browserPageURL(prefix, { instance: state.instanceId }));
    return url.pathname + url.search + url.hash;
  }

  function replaceLocation(prefix = '', { push = false } = {}) {
    const target = browserLocation(prefix);
    if (target === location.pathname + location.search + location.hash) return;
    history[push ? 'pushState' : 'replaceState'](null, '', target);
  }

  function applyCurrentInstance() {
    const instance = currentInstance();
    if (!instance) return;
    BB.api.setInstance(instance);
    config.capabilities = instance.capabilities || {};
    config.operations = instance.operations || {};
    config.versioningSupported = instance.versioningSupported === true;
    // The server probes the tag requests at start. The state of the page follows the instance, also after a change of instance.
    config.tagsSupported = instance.tagsSupported !== false;
    state.tagsUnsupported = !config.tagsSupported;
    // The server probes the edit of the metadata at start: the details hide the button for an instance without it.
    config.metadataEditSupported = instance.metadataEditSupported !== false;
    // The server probes the list of the incomplete uploads at start: the menu hides the item for an instance without it.
    config.multipartUploadsSupported = instance.multipartUploadsSupported !== false;
    // The server probes the lifecycle rules at start: the menu hides the item for an instance without them (GCS).
    config.lifecycleSupported = instance.lifecycleSupported !== false;
    // The server probes the Object Lock of the bucket at start: the details show the retention only for a bucket with it.
    config.objectLockSupported = instance.objectLockSupported === true;
    // The server reads the default encryption of the bucket at start: the details show it only when the read worked.
    config.bucketEncryptionSupported = instance.bucketEncryptionSupported === true;
    config.rootPrefix = instance.rootPrefix || '';
    // The provider: "fs" for a volume. The windows of the Actions menu use its words.
    config.provider = String(instance.provider || '');
    // The facts of the bucket in the selector: the versioning, the lifecycle rules and the usage of the bucket.
    loadStatus(instance);
  }

  function resetPagination() {
    state.previousContinuationTokens = [];
    state.continuationToken = '';
    state.nextContinuationToken = '';
    state.scanOffset = 0;
    state.localPage = 0;
    state.navigationSortAvailable = false;
    state.navigationSortField = '';
    state.navigationSortDirection = '';
  }

  function clearListing() {
    state.rows = [];
    state.listError = '';
    state.listErrorText = '';
  }

  async function selectInstance(id) {
    if (!id || id === state.instanceId) return;
    closeMenu();
    state.instanceId = id;
    applyCurrentInstance();
    state.pathPrefix = '';
    ui.searchInput.value = '';
    resetPagination();
    clearListing();
    replaceLocation('', { push: true });
    update();
    await refresh();
  }

  function goToPrefix(prefix) {
    closeMenu();
    const normalized = normalizePrefix(prefix);
    resetPagination();
    if (normalized !== state.pathPrefix) {
      state.pathPrefix = normalized;
      ui.searchInput.value = '';
      clearListing();
      replaceLocation(normalized, { push: true });
    }
    update();
    refresh();
  }

  function updateFromLocation() {
    const route = BB.runtime?.storageRoute?.();
    if (!route || route.view !== 'browser') return;
    const instance = state.instances.find(item => item.host === route.host && item.id === route.bucket);
    if (!instance) return;
    const next = normalizePrefix(route.path);
    if (instance.id === state.instanceId && next === state.pathPrefix) return;
    if (instance.id !== state.instanceId) {
      state.instanceId = instance.id;
      applyCurrentInstance();
    }
    state.pathPrefix = next;
    ui.searchInput.value = '';
    resetPagination();
    clearListing();
    update();
    if (!state.isInitializing) refresh();
  }

  function searchByPrefix() {
    const value = String(ui.searchInput.value || '').trim();
    if (!value) {
      manualRefresh();
      return;
    }
    goToPrefix(normalizePrefix(state.pathPrefix + value));
  }

  function manualRefresh() {
    resetPagination();
    refresh();
    loadStatus(currentInstance(), { fresh: true });
  }

  function toggleSort(field) {
    if (!state.navigationSortAvailable) return;
    const first = field === 'name' ? 'asc' : 'desc';
    const second = first === 'desc' ? 'asc' : 'desc';
    if (state.navigationSortField !== field) {
      state.navigationSortField = field;
      state.navigationSortDirection = first;
    } else if (state.navigationSortDirection === first) {
      state.navigationSortDirection = second;
    } else {
      state.navigationSortField = '';
      state.navigationSortDirection = '';
    }
    state.localPage = 0;
    update();
    refreshVisibleVersionCounts();
  }

  function toggleTimeMode() {
    state.modifiedTimeMode = state.modifiedTimeMode === 'relative' ? 'absolute' : 'relative';
    update();
  }

  async function nextPage() {
    if (state.localPage + 1 < localPageCount()) {
      state.localPage += 1;
      update();
      refreshVisibleVersionCounts();
      return;
    }
    if (!state.nextContinuationToken) return;
    state.previousContinuationTokens.push({ token: state.continuationToken || '', offset: state.scanOffset });
    state.scanOffset += state.rows.length;
    state.continuationToken = state.nextContinuationToken;
    state.localPage = 0;
    await refresh();
  }

  async function previousPage() {
    if (state.localPage > 0) {
      state.localPage -= 1;
      update();
      refreshVisibleVersionCounts();
      return;
    }
    if (!state.previousContinuationTokens.length) return;
    const previous = state.previousContinuationTokens.pop();
    state.continuationToken = previous?.token || '';
    state.scanOffset = Number(previous?.offset || 0);
    await refresh();
    state.localPage = Math.max(0, localPageCount() - 1);
    update();
  }

  async function refresh() {
    if (!currentInstance()) return;
    const request = ++state.refreshRequest;
    const prefix = state.pathPrefix;
    state.isRefreshing = true;
    update();
    try {
      if (!canRead()) {
        clearListing();
        state.nextContinuationToken = '';
        return;
      }
      const data = await BB.api.list({ prefix, delimiter: '/', continuationToken: state.continuationToken });
      if (request !== state.refreshRequest) return;
      state.listError = '';
      state.listErrorText = '';
      state.nextContinuationToken = data.nextContinuationToken || '';
      state.navigationSortAvailable = data.sortAvailable === true;
      if (!state.navigationSortAvailable) {
        state.navigationSortField = '';
        state.navigationSortDirection = '';
      }
      state.rows = (data.items || []).map(item => {
        if (item.type === 'prefix') {
          const name = String(item.name || item.prefix || '').replace(/\/+$/, '').split('/').pop();
          return { type: 'prefix', name, prefix: item.prefix, size: 0, dateModified: null };
        }
        return {
          type: 'content',
          name: item.name || String(item.key || '').split('/').pop(),
          key: item.key,
          size: Number(item.size || 0),
          dateModified: item.lastModified ? new Date(item.lastModified) : null,
          contentType: item.contentType || '',
          etag: item.etag || '',
          // An entry of a volume that the server does not read: a link that leaves the volume, or not a regular file.
          unreadable: item.unreadable === true,
          versionCount: null
        };
      }).filter(row => !config.keyExcludePatterns.some(pattern => pattern.test(String((row.type === 'prefix' ? row.prefix : row.key) || ''))));
      state.localPage = Math.max(0, Math.min(state.localPage, localPageCount() - 1));
    } catch (error) {
      if (request !== state.refreshRequest) return;
      clearListing();
      state.listError = classify(error);
      state.listErrorText = String(error?.message || error);
    } finally {
      if (request === state.refreshRequest) {
        state.isRefreshing = false;
        update();
      }
    }
    if (request === state.refreshRequest) refreshVisibleVersionCounts();
  }

  async function refreshVisibleVersionCounts() {
    const request = ++state.versionCountRequest;
    if (!versioningSupported() || !canRead() || !state.columns.versions) {
      for (const row of state.rows) if (row.type === 'content') row.versionCount = null;
      updateVersionBadges();
      return;
    }
    const instanceId = state.instanceId;
    const keys = visibleRows().filter(row => row.type === 'content' && row.key).map(row => row.key);
    if (!keys.length) return;
    try {
      const response = await BB.api.versionCounts(keys, instanceId);
      if (request !== state.versionCountRequest || instanceId !== state.instanceId) return;
      const counts = response?.counts || {};
      for (const row of state.rows) {
        if (row.type !== 'content' || !Object.prototype.hasOwnProperty.call(counts, row.key)) continue;
        row.versionCount = Number(counts[row.key]?.count || 0);
      }
      updateVersionBadges();
    } catch (error) {
      if (Number(error?.status || 0) !== 501) console.warn('Unable to load object version counts', error);
    }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Tags: the column, the filter, the folder action
  // ---------------------------------------------------------------------------------------------------------------

  function tagCell(row) {
    if (row.type !== 'content') return null;
    if (row.tags === undefined) return state.columns.tags ? h('span', { class: 'tagChips__pending', role: 'img', 'aria-label': 'The tags are loading' }) : null;
    if (row.tags === null) return h('span', { class: 'tagChips__state', title: 'The tags of this object cannot be read' }, '—');
    // The first tags are badges; the other ones are a count, and the details have them all.
    const shown = row.tags.slice(0, 3);
    const extra = row.tags.slice(3);
    return h('span', { class: 'tagChips' },
      BB.tags.chips(shown, { onPick: pickTag, filter: state.tagFilterOn ? state.tagFilter : '' }),
      extra.length ? h('span', { class: 'tagMore', title: extra.map(tag => BB.tags.formatTag(tag)).join(', ') }, `+${extra.length}`) : null);
  }

  // A click on a tag filters the page by it. A second click clears the filter. Ctrl, Cmd or Shift keeps the other words.
  function pickTag(tag, event) {
    state.tagFilterOn = true;
    saveListing();
    state.tagFilter = BB.tags.toggleFilterWord(state.tagFilter, tag, Boolean(event?.ctrlKey || event?.metaKey || event?.shiftKey));
    ui.tagFilter.value = state.tagFilter;
    for (const row of visibleRows()) if (row.type === 'content') updateTagCell(row);
    update();
  }

  function updateTagCell(row) {
    const cell = row.element?.querySelector('.col-tags');
    if (cell) cell.replaceChildren(...[tagCell(row)].filter(Boolean));
  }

  // A row that is not a file stays. A file stays when its tags match the filter; a file whose tags are not read yet waits.
  function applyTagFilter() {
    const filter = state.tagFilterOn ? state.tagFilter.trim() : '';
    const rows = visibleRows().filter(row => row.type === 'content');
    let shown = 0;
    let pending = 0;
    for (const row of rows) {
      if (!row.element) continue;
      let matches = true;
      if (filter) {
        if (row.tags === undefined) { matches = false; pending += 1; } else matches = Array.isArray(row.tags) && BB.tags.matchesFilter(row.tags, filter);
      }
      row.element.hidden = !matches;
      if (matches) shown += 1;
    }
    for (const row of rows) if (row.tags && row.element) updateTagCell(row);
    if (!state.tagFilterOn) ui.tagStatus.textContent = '';
    else if (pending) ui.tagStatus.textContent = `Reading ${pending} of ${rows.length}`;
    else ui.tagStatus.textContent = filter ? `${shown} of ${rows.length}` : '';
  }

  function forgetTags(key) {
    for (const row of state.rows) {
      if (row.type === 'content' && (!key || row.key === key)) row.tags = undefined;
    }
    state.tagSignature = '';
    for (const row of visibleRows()) if (row.type === 'content') updateTagCell(row);
    refreshVisibleTags();
  }

  // The tags are read when the column or the filter needs them.
  const tagsNeeded = () => (state.columns.tags || state.tagFilterOn) && !state.tagsUnsupported;

  function renderSettings() {
    for (const input of ui.settingsMenu.querySelectorAll('[data-column]')) {
      const name = input.dataset.column;
      if (name !== 'name') input.checked = state.columns[name] === true;
    }
    ui.tagFilterToggle.checked = state.tagFilterOn;
    if (document.activeElement !== ui.uploadTagsInput && ui.uploadTagsInput.getAttribute('aria-invalid') !== 'true') ui.uploadTagsInput.value = BB.tags.uploadTagsText();
    ui.settingsMenu.querySelector('#versionsSetting').hidden = !versioningSupported();
    ui.settingsMenu.querySelector('#tagFilterSetting').hidden = state.tagsUnsupported;
    ui.settingsMenu.querySelector('[data-column="tags"]').closest('label').hidden = state.tagsUnsupported;
    ui.settingsMenu.querySelector('#uploadTagsSetting').hidden = state.tagsUnsupported;
    ui.settingsMenu.querySelector('#uploadTagsSeparator').hidden = state.tagsUnsupported;
    ui.encryptionToggle.checked = marksShown();
    ui.settingsMenu.querySelector('#encryptionSetting').hidden = isVolume(currentInstance());
  }

  // The tags of the new uploads: saved when the field changes, with the reason when a tag is wrong.
  function saveUploadTags() {
    const error = BB.tags.saveUploadTagsText(ui.uploadTagsInput.value);
    ui.uploadTagsMessage.textContent = error || (BB.tags.uploadTags().length ? 'The next uploads carry these tags.' : '');
    ui.uploadTagsMessage.dataset.tone = error ? 'error' : '';
    ui.uploadTagsInput.setAttribute('aria-invalid', error ? 'true' : 'false');
    if (!error) ui.uploadTagsInput.value = BB.tags.uploadTagsText();
  }

  function onSettingsChange(event) {
    const input = event.target;
    if (!(input instanceof HTMLInputElement)) return;
    if (input.dataset.column) {
      state.columnChoice = { ...state.columnChoice, [input.dataset.column]: input.checked };
      state.columns = listingColumns(state.columnChoice);
    }
    if (input === ui.tagFilterToggle) {
      state.tagFilterOn = input.checked;
      if (!state.tagFilterOn) { state.tagFilter = ''; ui.tagFilter.value = ''; }
    }
    if (input === ui.encryptionToggle) {
      state.encryptionChoice = { ...state.encryptionChoice, [state.instanceId]: input.checked };
      state.encryptionSignature = '';
    }
    saveListing();
    state.tagSignature = '';
    update();
    if (input.dataset.column === 'versions' && input.checked) refreshVisibleVersionCounts();
    if (input === ui.tagFilterToggle && input.checked) ui.tagFilter.focus();
  }

  function resetSettings() {
    state.columnChoice = {};
    state.columns = listingColumns(state.columnChoice);
    state.tagFilterOn = LISTING_DEFAULT.tagFilterOn;
    state.tagFilter = '';
    ui.tagFilter.value = '';
    state.encryptionChoice = {};
    saveListing();
    state.tagSignature = '';
    state.encryptionSignature = '';
    update();
    refreshVisibleVersionCounts();
  }

  // Reads the tags of the files on the page, 4 at the same time, at most 100.
  async function refreshVisibleTags() {
    if (!tagsNeeded() || !canRead() || state.listError) return;
    const instanceId = state.instanceId;
    const rows = visibleRows().filter(row => row.type === 'content' && row.key).slice(0, 100);
    const signature = `${instanceId}|${rows.map(row => row.key).join('\n')}`;
    if (signature === state.tagSignature && rows.every(row => row.tags !== undefined || row.tagsLoading)) return;
    state.tagSignature = signature;
    const request = ++state.tagRequest;
    const waiting = rows.filter(row => row.tags === undefined && !row.tagsLoading);
    let next = 0;
    const worker = async () => {
      while (next < waiting.length) {
        const row = waiting[next];
        next += 1;
        if (request !== state.tagRequest || instanceId !== state.instanceId) return;
        row.tagsLoading = true;
        try {
          row.tags = (await BB.api.tags(row.key, { instance: instanceId })).tags || [];
        } catch (error) {
          if (Number(error?.status || 0) === 501 || error?.code === 'tagging_unsupported') {
            state.tagsUnsupported = true;
            BB.ui.toast('This storage provider does not support object tags.', { status: 'error', duration: 6000 });
            update();
            return;
          }
          row.tags = null;
        } finally {
          row.tagsLoading = false;
        }
        updateTagCell(row);
        applyTagFilter();
      }
    };
    await Promise.all([worker(), worker(), worker(), worker()]);
    applyTagFilter();
  }

  async function tagFolder(prefix) {
    if (!canWrite() || state.tagsUnsupported) return;
    const done = await BB.actions.tagPrefix(prefix);
    if (done) forgetTags('');
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Encryption and Object Lock: the marks next to the name of a file
  // ---------------------------------------------------------------------------------------------------------------

  // A listing does not give the encryption or the lock of a file: the page reads both from one HEAD request for the
  // files on the page, as for the tags. The setting "Encryption and lock" is on for each bucket, until the person sets
  // it off for the bucket. The saved choice of each bucket keeps its old name (encryption).
  // A volume has no encryption and no Object Lock of S3: its files get no mark and no HEAD request.
  const marksShown = () => !isVolume(currentInstance()) && state.encryptionChoice[state.instanceId] !== false;

  // The answers stay for the ETag and the date of the file: a new listing does not read an unchanged file again. A
  // change of the lock keeps the ETag and the date: the event of the change removes the answers of the file.
  const marksCache = new Map();
  const marksCacheKey = row => `${state.instanceId}|${row.key}|${row.etag}|${row.dateModified?.getTime?.() || ''}`;

  // The key of the encryption, then the padlock of Object Lock. The tooltip of the lock is made at each drawing: a
  // retention whose date is past goes.
  function objectMarks(row) {
    if (row.type !== 'content' || !marksShown() || row.encryptionSkipped) return [];
    if (row.encryption === undefined) return [h('span', { class: 'nameLock__pending', role: 'img', 'aria-label': 'The encryption and the lock are loading' })];
    const lock = BB.objectLock?.lockTitle(row.lock) || '';
    return [
      row.encryption ? h('span', { class: 'nameLock__icon', role: 'img', title: row.encryption, 'aria-label': row.encryption, 'data-mark': 'encryption' }, ic('key')) : null,
      lock ? h('span', { class: 'nameLock__icon nameLock__icon--objectLock', role: 'img', title: lock, 'aria-label': lock, 'data-mark': 'object-lock' }, ic('lock')) : null
    ].filter(Boolean);
  }

  function updateMarksCell(row) {
    const cell = row.element?.querySelector('.nameLock');
    if (cell) cell.replaceChildren(...objectMarks(row));
  }

  // A change of the lock of a file in its details: the page reads the file again.
  function forgetMarks(key) {
    for (const cacheKey of [...marksCache.keys()]) if (cacheKey.startsWith(`${state.instanceId}|${key}|`)) marksCache.delete(cacheKey);
    for (const row of state.rows || []) {
      if (row.key === key) { row.encryption = undefined; row.lock = undefined; updateMarksCell(row); }
    }
    state.encryptionSignature = '';
    void refreshVisibleMarks();
  }

  // Reads the encryption and the lock of the files on the page, 4 at the same time, at most 100. A file that cannot be
  // read gets no mark: the page shows only what the provider reports.
  async function refreshVisibleMarks() {
    if (!marksShown() || !canRead() || state.listError || !BB.encryption) return;
    const instanceId = state.instanceId;
    const files = visibleRows().filter(row => row.type === 'content' && row.key);
    const rows = files.slice(0, 100);
    // The files after the first 100 get no mark: the page does not read them.
    for (const row of files.slice(100)) {
      if (row.encryption === undefined && !row.encryptionSkipped) { row.encryptionSkipped = true; updateMarksCell(row); }
    }
    for (const row of rows) {
      row.encryptionSkipped = false;
      if (row.encryption === undefined && marksCache.has(marksCacheKey(row))) {
        ({ encryption: row.encryption, lock: row.lock } = marksCache.get(marksCacheKey(row)));
        updateMarksCell(row);
      }
    }
    const signature = `${instanceId}|${rows.map(row => row.key).join('\n')}`;
    if (signature === state.encryptionSignature && rows.every(row => row.encryption !== undefined || row.encryptionLoading)) return;
    state.encryptionSignature = signature;
    const request = ++state.encryptionRequest;
    const waiting = rows.filter(row => row.encryption === undefined && !row.encryptionLoading);
    let next = 0;
    const worker = async () => {
      while (next < waiting.length) {
        const row = waiting[next];
        next += 1;
        if (request !== state.encryptionRequest || instanceId !== state.instanceId) return;
        row.encryptionLoading = true;
        try {
          const answer = await BB.encryption.objectEncryption(row.key, instanceId);
          row.encryption = answer.encryption;
          row.lock = BB.objectLock?.lockFromHeaders(answer.headers) || null;
          if (marksCache.size >= 5000) marksCache.clear();
          marksCache.set(marksCacheKey(row), { encryption: row.encryption, lock: row.lock });
        } catch (_) {
          row.encryption = '';
          row.lock = null;
        } finally {
          row.encryptionLoading = false;
        }
        updateMarksCell(row);
      }
    };
    await Promise.all([worker(), worker(), worker(), worker()]);
  }

  function fileIcon(row) {
    return BB.detect.iconForType(BB.detect.resolveType(row.name || '', row.contentType || ''));
  }

  function openPreview(row) {
    if (!canRead() || row.unreadable) return;
    window.location.href = BB.api.previewPageURL(row.key, { instance: state.instanceId });
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Row and folder actions (BB.actions)
  // ---------------------------------------------------------------------------------------------------------------

  function onRowMetadata(row) {
    BB.actions.showMetadata(row.key, { size: row.size, mime: row.contentType, etag: row.etag, lastModified: row.dateModified?.toUTCString?.() || '' });
  }
  function onRowDownload(row) { BB.actions.downloadObject(row.key, row.name); }
  async function onRowCopy(row) { if (await BB.actions.copyObject(row.key)) await refreshAfterChange(); }
  async function onRowRename(row) { if (await BB.actions.renameObject(row.key)) await refreshAfterChange(); }
  async function onRowDelete(row) { if (await BB.actions.deleteObject(row.key)) await refreshAfterChange(); }
  function onPrefixInsights(row) { BB.actions.showPrefixInsights(row.prefix); }
  async function onPrefixCopy(row) { if (await BB.actions.copyPrefix(row.prefix)) await refreshAfterChange(); }
  async function onPrefixRename(row) { if (await BB.actions.renamePrefix(row.prefix)) await refreshAfterChange(); }
  async function onPrefixDelete(row) { if (await BB.actions.deletePrefix(row.prefix)) await refreshAfterChange(); }

  // A new folder of a volume: a real folder of the server, in the listed folder. The name is one part of a path. The
  // server checks the name again.
  async function makeFolder() {
    if (!canWrite() || !isVolume(currentInstance())) return;
    const where = state.pathPrefix ? `the folder ${state.pathPrefix}` : 'the root of the volume';
    const value = await BB.ui.prompt({ title: 'New folder', message: `The name of the new folder in ${where}.`, defaultValue: '' });
    if (value === null) return;
    const name = value.trim();
    let problem = '';
    if (!name) problem = 'The name is empty.';
    else if (name.includes('/')) problem = 'A name cannot have a "/".';
    else if (name === '.' || name === '..') problem = 'The name cannot be "." or "..".';
    else if (name.startsWith('.stowr-tmp-')) problem = 'The names that start with ".stowr-tmp-" are for the temporary files of Stowr.';
    if (problem) {
      BB.ui.toast(`The folder is not made. ${problem}`, { status: 'error', duration: 6500 });
      return;
    }
    try {
      await BB.api.putBlob(`${state.pathPrefix}${name}/`, new Blob([]), 'application/x-directory', { instance: state.instanceId });
      BB.ui.toast(`The folder ${name} is made.`, { duration: 4000 });
      await refreshAfterChange();
    } catch (error) {
      BB.ui.toast(`The folder is not made: ${String(error?.message || error)}`, { status: 'error', duration: 6500 });
    }
  }

  // A change of the objects: the listing, and later the usage of the bucket.
  function refreshAfterChange() {
    noteChange();
    return refresh();
  }

  function onRowContextMenu(event) {
    const row = event.target?.closest?.('.dataTable tbody tr');
    const menu = row?.querySelector('.bb-menu');
    if (!menu || !BB.menu?.openAt) return;
    event.preventDefault();
    event.stopPropagation();
    BB.menu.openAt(menu, event.clientX, event.clientY);
  }

  function downloadArchive({ archiveName, basePrefix = '' }) {
    if (!canRead()) return;
    const url = BB.api.archiveURL({ prefix: basePrefix, name: archiveName, instance: state.instanceId });
    BB.actions.triggerBrowserDownload(url, archiveName);
    BB.ui.toast('The ZIP download started in the browser.', { duration: 5000 });
  }

  function downloadAll() {
    if (!canDownloadAll()) return;
    const name = `${state.pathPrefix.split('/').filter(Boolean).pop() || currentInstance()?.name || 'archive'}.zip`;
    downloadArchive({ archiveName: name, basePrefix: state.pathPrefix });
  }

  function onPrefixDownload(row) {
    const prefix = normalizePrefix(row.prefix);
    downloadArchive({ archiveName: `${prefix.split('/').filter(Boolean).pop() || 'folder'}.zip`, basePrefix: prefix });
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Uploads
  // ---------------------------------------------------------------------------------------------------------------

  let refreshTimer = 0;
  let lastUploadRefresh = 0;

  // A file that arrives in the listed folder shows after a short delay: one refresh every 2.5 s at most.
  function scheduleUploadRefresh(destination, immediate = false) {
    if (destination.instanceId === state.instanceId) noteChange();
    if (destination.instanceId !== state.instanceId || destination.prefix !== state.pathPrefix) return;
    clearTimeout(refreshTimer);
    const wait = immediate ? 300 : Math.max(800, 2500 - (Date.now() - lastUploadRefresh));
    refreshTimer = setTimeout(() => {
      lastUploadRefresh = Date.now();
      if (!state.isRefreshing) refresh();
      else scheduleUploadRefresh(destination);
    }, wait);
  }

  function uploader() {
    if (!ui.uploads) {
      ui.uploads = BB.uploads.create({
        host: ui.uploadDock,
        onFileDone: (_, destination) => scheduleUploadRefresh(destination),
        onBatchEnd: destination => scheduleUploadRefresh(destination, true),
        onSignInEnded: () => BB.session?.signInEnded(),
        onBlockedChange: () => BB.session?.refresh()
      });
      if (BB.session) BB.session.ownsOffline = () => ui.uploads?.batch?.blocked === 'offline';
    }
    return ui.uploads;
  }

  function triggerUploadFiles() {
    if (!canWrite()) return;
    ui.fileInput.value = '';
    ui.fileInput.click();
  }

  function triggerUploadFolder() {
    if (!canWrite()) return;
    ui.dirInput.value = '';
    ui.dirInput.click();
  }

  async function confirmFolder(entries) {
    const totalSize = entries.reduce((sum, entry) => sum + Math.max(0, Number(entry.file.size || 0)), 0);
    const roots = Array.from(new Set(entries.map(entry => String(entry.relative).split('/')[0]).filter(Boolean)));
    const name = roots.length === 1 ? roots[0] : plural(roots.length, 'folder');
    const samples = entries.slice(0, 4).map(entry => `<li class="mono" title="${escapeHTML(entry.relative)}">${escapeHTML(entry.relative)}</li>`).join('');
    const more = entries.length > 4 ? `<li class="muted">and ${escapeHTML(plural(entries.length - 4, 'other file'))}</li>` : '';
    return BB.ui.confirm({
      title: 'Upload a folder',
      confirmLabel: `Upload ${plural(entries.length, 'file')}`,
      cancelLabel: 'Cancel',
      html: `<div class="uploadConfirm">
        <div class="uploadConfirm__what"><i class="ic ic-folder" aria-hidden="true"></i>
          <div><strong>${escapeHTML(name)}</strong><span class="muted">${escapeHTML(plural(entries.length, 'file'))}, ${escapeHTML(formatBytes(totalSize))}</span></div></div>
        <dl><dt>Into</dt><dd><strong>${escapeHTML(currentInstance()?.name || '')}</strong>${state.pathPrefix ? ` / <code>${escapeHTML(state.pathPrefix)}</code>` : ', root'}</dd></dl>
        <ul>${samples}${more}</ul>
        <p class="muted">A file with the same name is replaced.</p>
      </div>`
    });
  }

  function startUpload(entries) {
    if (!canWrite() || !entries.length) return;
    const instance = currentInstance();
    const prefix = state.pathPrefix;
    uploader().add(entries.map(entry => {
      const relative = cleanDroppedPath(entry.relative || entry.file.name);
      return { file: entry.file, relative, key: `${prefix}${relative}`.replace(/^\/+/, '') };
    }).filter(entry => entry.relative), { instanceId: instance.id, prefix, label: instance.name });
  }

  function onFileInput(event) {
    const files = Array.from(event.target.files || []);
    event.target.value = '';
    if (files.length) startUpload(files.map(file => ({ file, relative: file.name })));
  }

  async function onDirInput(event) {
    const files = Array.from(event.target.files || []);
    event.target.value = '';
    if (!files.length) return;
    const entries = files.map(file => ({ file, relative: cleanDroppedPath(file.webkitRelativePath || file.name) }));
    if (await confirmFolder(entries)) startUpload(entries);
  }

  function onDragEnter(event) {
    if (!dataTransferHasFiles(event.dataTransfer)) return;
    event.preventDefault();
    if (!canWrite() || state.listError) return;
    state.dragDepth += 1;
    state.isDragActive = true;
    renderDropOverlay();
  }

  function onDragOver(event) {
    if (!dataTransferHasFiles(event.dataTransfer)) return;
    event.preventDefault();
    const allowed = canWrite() && !state.listError;
    if (event.dataTransfer) event.dataTransfer.dropEffect = allowed ? 'copy' : 'none';
    if (!allowed || state.isDragActive) return;
    state.isDragActive = true;
    renderDropOverlay();
  }

  function onDragLeave(event) {
    if (!state.isDragActive) return;
    event.preventDefault();
    state.dragDepth = Math.max(0, state.dragDepth - 1);
    const outside = !event.relatedTarget || event.clientX <= 0 || event.clientY <= 0
      || event.clientX >= window.innerWidth || event.clientY >= window.innerHeight;
    if (state.dragDepth === 0 || outside) {
      state.dragDepth = 0;
      state.isDragActive = false;
      renderDropOverlay();
    }
  }

  async function onDrop(event) {
    if (!dataTransferHasFiles(event.dataTransfer)) return;
    event.preventDefault();
    state.dragDepth = 0;
    state.isDragActive = false;
    renderDropOverlay();
    if (!canWrite() || state.listError) {
      BB.ui.toast('You cannot upload in this folder.', { status: 'error', duration: 4500 });
      return;
    }
    try {
      const entries = await droppedEntries(event.dataTransfer);
      if (!entries.length) {
        BB.ui.toast('The dropped selection has no file.', { status: 'error', duration: 4500 });
        return;
      }
      if (entries.some(entry => entry.relative.includes('/')) && !await confirmFolder(entries)) return;
      startUpload(entries);
    } catch (error) {
      BB.ui.toast(`The dropped files cannot be read: ${String(error?.message || error)}`, { status: 'error', duration: 6500 });
    }
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Start
  // ---------------------------------------------------------------------------------------------------------------

  function bindElements() {
    for (const [name, id] of Object.entries({
      versionBadge: 'versionBadge', switcher: 'storageSwitcher', usageBanner: 'usageBanner', crumbs: 'crumbs',
      title: 'folderTitle', searchForm: 'searchForm', searchInput: 'searchInput',
      refreshButton: 'refreshButton', actionsButton: 'actionsButton', actionsMenu: 'actionsMenu', zipItem: 'zipItem',
      insightsItem: 'insightsItem', accessBadge: 'accessBadge', uploadWrap: 'uploadWrap',
      uploadButton: 'uploadButton', uploadMenu: 'uploadMenu', uploadFilesItem: 'uploadFilesItem',
      uploadFolderItem: 'uploadFolderItem', uploadTagsInput: 'uploadTagsInput', uploadTagsMessage: 'uploadTagsMessage', folderTagsItem: 'folderTagsItem', multipartsItem: 'multipartsItem',
      newFolderItem: 'newFolderItem',
      tagFilterWrap: 'tagFilterWrap', settingsButton: 'settingsButton', settingsMenu: 'settingsMenu', settingsReset: 'settingsReset',
      tagFilterToggle: 'tagFilterToggle', encryptionToggle: 'encryptionToggle', tagFilter: 'tagFilter', tagStatus: 'tagStatus', listing: 'listing', dropOverlay: 'dropOverlay', dropTarget: 'dropTarget',
      uploadDock: 'uploadDock', fileInput: 'fileInput', dirInput: 'dirInput'
    })) ui[name] = byId(id);
    ui.searchLabel = document.querySelector('label[for="searchInput"]');
    // The selector of the header: a bucket of the list opens in this page. The facts fill the banner of a full bucket.
    ui.selector = BB.switcher.create({
      root: ui.switcher,
      onSelect: entry => { void selectInstance(entry.id); },
      beforeOpen: () => closeMenu(),
      onRefresh: ({ instance, full }) => renderUsageBanner(full ? BB.usage.cached(instance.id) : null)
    });
  }

  function bindEvents() {
    ui.searchForm.addEventListener('submit', event => {
      event.preventDefault();
      searchByPrefix();
    });
    ui.refreshButton.addEventListener('click', manualRefresh);
    bindMenu(ui.actionsButton, ui.actionsMenu);
    bindMenu(ui.uploadButton, ui.uploadMenu);
    ui.zipItem.addEventListener('click', downloadAll);
    ui.insightsItem.addEventListener('click', () => { if (canRead()) BB.actions.showPrefixInsights(state.pathPrefix); });
    ui.uploadFilesItem.addEventListener('click', triggerUploadFiles);
    ui.uploadFolderItem.addEventListener('click', triggerUploadFolder);
    ui.uploadTagsInput.addEventListener('change', saveUploadTags);
    ui.uploadTagsInput.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); saveUploadTags(); } });
    bindMenu(ui.settingsButton, ui.settingsMenu, { stayOnTab: true });
    ui.settingsReset.addEventListener('click', resetSettings);
    ui.settingsMenu.addEventListener('change', onSettingsChange);
    ui.folderTagsItem.addEventListener('click', () => { if (canWrite()) void tagFolder(state.pathPrefix); });
    ui.multipartsItem.addEventListener('click', () => { if (canRead()) BB.multiparts.open({ prefix: state.pathPrefix, canWrite: canWrite() }); });
    ui.newFolderItem.addEventListener('click', () => { void makeFolder(); });
    ui.tagFilter.addEventListener('input', () => { state.tagFilter = ui.tagFilter.value; applyTagFilter(); });
    document.addEventListener('bb:tags-changed', event => forgetTags(event.detail?.key));
    // A save of the metadata changes the date and the versions of the object: the listing reads them again.
    document.addEventListener('bb:metadata-changed', () => { void refreshAfterChange(); });
    document.addEventListener('bb:object-lock-changed', event => forgetMarks(event.detail?.key));
    // A restore or a delete of a version changes the date, the size and the count of the versions of the file.
    document.addEventListener('bb:versions-changed', () => { void refreshAfterChange(); });
    ui.fileInput.addEventListener('change', onFileInput);
    ui.dirInput.addEventListener('change', onDirInput);
    window.addEventListener('popstate', updateFromLocation);
    document.addEventListener('click', event => {
      if (openMenu && !openMenu.menu.contains(event.target) && !openMenu.trigger.contains(event.target)) closeMenu();
    });
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape') {
        closeMenu({ focus: true });
      }
      // "/" puts the focus in the prefix field.
      if (event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey
        && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || '') && !document.querySelector('.bb-overlay')) {
        event.preventDefault();
        ui.searchInput.focus();
      }
    });
    document.addEventListener('bb:retry', () => (state.instances.length ? manualRefresh() : initialize()));
    window.addEventListener('dragenter', onDragEnter);
    window.addEventListener('dragover', onDragOver);
    window.addEventListener('dragleave', onDragLeave);
    window.addEventListener('drop', onDrop);
    document.addEventListener('contextmenu', onRowContextMenu);
  }

  bindElements();
  bindEvents();
  restoreVersion();
  update();
  initialize();

  // The page state, for the browser tests.
  BB.page = {
    get state() { return state; },
    refresh: () => refresh(),
    uploads: () => uploader()
  };
})();
