/*
 * The information page of a bucket or a volume: /-/<host>/<bucket>/?info. The bucket selector opens it (the link of
 * the state icons and of the usage, in the trigger and in each entry of the list). The header has the selector of the
 * listing (switcher.js): a bucket of its list opens the information page of that bucket. The page has the visual
 * language of the System page of ClickHouse Dash: parts with a heading, cards with key-value lists, gauges for the used
 * parts, and status chips. It has no head: the selector names the bucket. Each row shows only when it is known. The
 * parts, one under the other on one scroll:
 * - identity and permissions: the name, the id, the kind of storage, the host, the bucket, the root prefix and the
 *   region; read, write and delete with the reason of each decision (the ceiling of the configuration,
 *   server.access_mode = "force_read_only", the access policy, a refusal of the provider). It never shows an
 *   endpoint, an admin URL, a token or the path of a volume on the server;
 * - storage: what the bucket or the volume holds, its quota, and the capacity of the storage behind it (GET /api/usage);
 * - functions: versioning, encryption, Object Lock, lifecycle rules, tags, the edit of the metadata, the incomplete
 *   uploads and the SHA-256 checksum of the uploads;
 * - lifecycle rules (#info-lifecycle): the editor of the rules (lifecycle.js), in the page, with no window. A write
 *   needs the right to write in the bucket.
 * The chips have the colours of the state icons of the selector: green (on, allowed), orange (on, with a problem) and
 * grey (off, not available, denied). The data comes from /api/instances, /api/usage and /api/lifecycle. A source that
 * the person cannot read gives "Not allowed".
 */
(function () {
  'use strict';

  const BB = (window.BB = window.BB || {});

  function h(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [name, value] of Object.entries(attrs || {})) {
      if (value === null || value === undefined || value === false) continue;
      if (name === 'class') node.className = Array.isArray(value) ? value.filter(Boolean).join(' ') : value;
      else if (name.startsWith('on') && typeof value === 'function') node.addEventListener(name.slice(2), value);
      else node.setAttribute(name, value === true ? '' : String(value));
    }
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  const ic = name => h('i', { class: `ic ic-${name}`, 'aria-hidden': 'true' });
  const byId = id => document.getElementById(id);
  const count = value => Number(value || 0).toLocaleString('en-US');
  const plural = (value, one, many = `${one}s`) => `${count(value)} ${Number(value) === 1 ? one : many}`;
  const bytes = value => BB.usage.formatBytes(value);
  const mark = approximate => (approximate ? '~' : '');
  // A percent as the selector shows it: "<1 %" for a small part, else the whole number.
  const percentText = (value, approximate = false) => {
    const number = Number(value) || 0;
    return number > 0 && number < 1 ? '<1 %' : `${mark(approximate)}${Math.floor(number)} %`;
  };
  // The tone of a used part: a warning from 80 %, an error from 95 % (the level of the server).
  const levelTone = level => (level === 'error' ? 'error' : level === 'warning' ? 'warn' : '');

  const FLAVOURS = { aws: 'AWS S3', minio: 'MinIO', seaweedfs: 'SeaweedFS', garage: 'Garage' };
  const NOT_ALLOWED = 'You cannot read this bucket.';

  const isVolume = instance => String(instance?.provider || '') === 'fs';
  const noun = instance => (isVolume(instance) ? 'volume' : 'bucket');
  const can = (instance, operation) => (BB.capabilities?.actionable ? BB.capabilities.actionable(instance, operation) : true);

  // --- Parts of the page ----------------------------------------------------------------------------------------------

  // A status chip: "on" (green), "warn" (orange), "off" (grey) or "accent".
  const chip = (tone, text, title = '') => h('span', { class: ['infoChip', `infoChip--${tone}`], 'data-tone': tone, title: title || null }, text);

  // A part of the page: a heading (h2), a count or a note, and the body.
  function part(key, title, extra, ...body) {
    return h('section', { class: 'infoPart', id: `info-${key}`, 'data-part': key, 'aria-labelledby': `info-${key}-title` },
      h('div', { class: 'infoPart__head' }, h('h2', { class: 'infoPart__title', id: `info-${key}-title` }, title),
        extra ? h('span', { class: 'infoPart__count' }, extra) : null),
      h('div', { class: 'infoPart__body' }, body));
  }

  // A card: a title (h3), a count and a chip, then the content.
  function card(key, title, { countText = '', badge = null } = {}, ...body) {
    return h('section', { class: 'infoCard', 'data-card': key },
      h('div', { class: 'infoCard__head' }, h('h3', { class: 'infoCard__title' }, title),
        countText ? h('span', { class: 'infoCard__count' }, countText) : null, badge),
      body);
  }

  // A row of a key-value list: the term, the value (text or nodes) and a note. A row without a value is left out.
  const row = (term, value, note = '', key = '') => (value === null || value === undefined || value === '' ? null : { term, value, note, key });

  function kv(rows, label) {
    return h('dl', { class: 'infoKv', 'aria-label': label || null }, rows.filter(Boolean).map(item =>
      h('div', { class: 'infoKv__row', 'data-row': item.key || null },
        h('dt', { class: 'infoKv__key' }, item.term),
        h('dd', { class: 'infoKv__value' }, h('span', { class: 'infoKv__main' }, item.value),
          item.note ? h('span', { class: 'infoKv__note' }, item.note) : null))));
  }

  // Pairs on lines that wrap: a muted term, then its value. The pairs take the width, not one line each.
  function pairs(rows, label) {
    return h('dl', { class: 'infoPairs', 'aria-label': label || null }, rows.filter(Boolean).map(item =>
      h('div', { class: 'infoPair', 'data-row': item.key || null },
        h('dt', { class: 'infoPair__key' }, item.term),
        h('dd', { class: 'infoPair__value' }, item.value, item.note ? h('span', { class: 'infoKv__note' }, item.note) : null))));
  }

  // A gauge, as a disk of the System page of Dash: its name and chips, the percent over a track, a summary and facts.
  function gauge({ key, title, chips = [], percent = null, level = '', approximate = false, value = '', summary = '', facts = [], note = '' }) {
    const tone = levelTone(level);
    const meter = percent === null ? null : h('div', { class: 'infoGauge__meter', 'data-fill': tone || 'ok' },
      h('span', { class: 'infoGauge__percent' }, percentText(percent, approximate)),
      h('span', { class: 'infoGauge__track', role: 'meter', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(percent), 'aria-label': `Used part: ${title}` },
        h('span', { class: 'infoGauge__fill' })));
    if (meter) meter.querySelector('.infoGauge__fill').style.width = `${Math.min(100, Math.max(Number(percent) || 0, percent > 0 ? 1 : 0))}%`;
    return h('section', { class: 'infoGauge', 'data-gauge': key, 'data-fill': tone || 'ok', 'aria-label': title },
      h('header', { class: 'infoGauge__head' }, h('h3', { class: 'infoGauge__name' }, title), h('span', { class: 'infoGauge__chips' }, chips)),
      meter, value ? h('div', { class: 'infoGauge__value' }, value) : null,
      // The second line: the summary, then the facts, on the same line while they fit.
      summary || facts.filter(Boolean).length || note ? h('div', { class: 'infoGauge__detail' },
        summary ? h('p', { class: 'infoGauge__summary' }, summary) : null,
        facts.filter(Boolean).length ? h('dl', { class: 'infoGauge__facts' }, facts.filter(Boolean).map(item =>
          h('div', { class: 'infoGauge__fact', 'data-fact': item.key || null }, h('dt', {}, item.term), h('dd', {}, item.value)))) : null,
        note ? h('p', { class: 'infoGauge__note' }, note) : null) : null);
  }

  // A part without numbers: a title and the reason, in a dashed box.
  const empty = (title, text, key = '') => h('div', { class: 'infoEmpty', 'data-empty': key || null }, h('strong', {}, title), text ? h('span', {}, text) : null);

  // --- Identity and permissions ---------------------------------------------------------------------------------------

  function kindText(instance) {
    if (isVolume(instance)) return 'Mounted folder of the server';
    if (instance.provider === 'gcs') return 'Google Cloud Storage';
    return FLAVOURS[instance.flavour] || 'S3';
  }

  function identityRows(instance) {
    return [
      row('Name', instance.name, '', 'name'),
      row('Kind', kindText(instance), '', 'kind'),
      row('Host', h('span', { class: 'mono' }, instance.host), '', 'host'),
      isVolume(instance) ? null : row('Bucket', h('span', { class: 'mono' }, instance.bucket), '', 'bucket'),
      row('Region', instance.region ? h('span', { class: 'mono' }, instance.region) : '', '', 'region')
    ];
  }

  // The reason of a decision, in the words of its source.
  function decisionSource(state) {
    switch (String(state?.source || '')) {
      case 'configuration': return 'The permissions of the block in the configuration do not give it.';
      case 'runtime': return 'server.access_mode = "force_read_only" refuses each write and each delete.';
      case 'access': return 'The access policy does not give it to you.';
      case 'provider': return 'The storage provider refused a real request a short time ago.';
      default: return '';
    }
  }

  // Allowed and allowed on use are green, denied is grey: the colours of the icons of the selector.
  const permissionTone = state => (BB.capabilities.normalizeState(state) === 'denied' ? 'off' : 'on');

  // The three permissions on one line: a chip for each, with the reason of a refusal in its title.
  function permissionRows(instance) {
    const chips = BB.states.permissions(instance).map(item => {
      const name = BB.capabilities.normalizeState(item.state);
      const reasons = [name === 'denied' ? decisionSource(item.state) : '', String(item.state.reason || '')].filter(Boolean);
      return h('span', { class: ['infoChip', `infoChip--${permissionTone(item.state)}`], 'data-permission': item.key, 'data-state': name, title: reasons.join(' ') || null },
        ic(item.icon), `${item.label}: ${BB.capabilities.stateLabel(item.state)}`);
    });
    return [row('Permissions', h('span', { class: 'infoChips' }, chips), '', 'permissions')];
  }

  // --- Storage --------------------------------------------------------------------------------------------------------

  const usageShown = usage => usage?.supported === true && (usage.available === true || Boolean(usage.storage));

  // What the bucket holds: "~12 MB", and for a volume "computing…" or "at least 10 GB" while the count runs.
  function holdsText(usage) {
    const volume = usage.source === 'statfs';
    if (!volume) return `${mark(usage.approximate)}${bytes(usage.usedBytes)}`;
    const amount = Number(usage.selfBytes ?? usage.usedBytes) || 0;
    if (usage.selfState === 'computing') return amount > 0 ? `≥ ${bytes(amount)}` : 'computing…';
    return `${usage.selfState === 'partial' ? '≥ ' : ''}${bytes(amount)}`;
  }

  function holdsSub(usage) {
    const volume = usage.source === 'statfs';
    if (volume && usage.selfState !== 'ready') return usage.selfState === 'computing' ? 'the count runs' : 'the count stopped at its limit';
    return Number(usage.usedObjects) >= 0 ? plural(usage.usedObjects, volume ? 'file' : 'object') : '';
  }

  function storageBody(instance, usage) {
    if (instance.usageSupported !== true) {
      return empty('Not available', isVolume(instance) ? 'The size of the file system is not readable.' : 'No usage block in the configuration.', 'unsupported');
    }
    if (!can(instance, 'list')) return empty('Not allowed', NOT_ALLOWED, 'not-allowed');
    if (usage === undefined) return empty('Loading the usage…', '', 'loading');
    if (!usage || usage.supported !== true) return empty('Not available', '', 'unavailable');
    const volume = isVolume(instance);
    const holdsTerm = volume ? 'This volume holds' : 'This bucket holds';
    const counted = usage.asOf ? BB.runtime.formatDateTimeUTC(usage.asOf) : '';
    const cards = [];
    if (usage.available === true) {
      cards.push(gauge({
        key: 'holds', title: holdsTerm, value: holdsText(usage), summary: holdsSub(usage),
        chips: usage.approximate ? [chip('warn', 'Approximate', usage.approximateReason || '')] : [],
        facts: [
          { key: 'source', term: 'Source', value: BB.usage.sourceName(usage) },
          counted ? { key: 'counted', term: 'Counted', value: counted } : null,
          volume && usage.selfState === 'partial' ? { key: 'limit', term: 'Count of the files', value: 'Stopped at its limit: 1,000,000 entries or 20 seconds' } : null
        ]
      }));
      if (Number(usage.limitBytes) > 0) {
        cards.push(gauge({
          key: 'quota', title: 'Quota', percent: Number(usage.quotaPercent ?? usage.percent) || 0, level: usage.quotaLevel ?? usage.level, approximate: usage.approximate,
          chips: [chip('accent', usage.limitSource === 'config' ? 'Set by Stowr' : 'Set by the provider')],
          summary: `${mark(usage.approximate)}${bytes(usage.usedBytes)} used of ${bytes(usage.limitBytes)}`,
          facts: [
            { key: 'left', term: 'Left', value: bytes(Math.max(0, Number(usage.limitBytes) - Number(usage.usedBytes))) },
            Number(usage.limitObjects) > 0 && Number(usage.usedObjects) >= 0 ? { key: 'objects', term: 'Objects', value: `${count(usage.usedObjects)} of ${count(usage.limitObjects)}` } : null
          ]
        }));
      }
    } else if (usage.error) {
      cards.push(gauge({ key: 'holds', title: holdsTerm, chips: [chip('warn', 'Unavailable')], note: usage.error.message || '' }));
    }
    if (usage.storage) {
      const storage = usage.storage;
      cards.push(gauge({
        key: 'storage', title: 'Storage', percent: Number(storage.percent) || 0, level: storage.level, approximate: storage.approximate,
        chips: storage.approximate ? [chip('warn', 'Approximate', storage.approximateReason || '')] : [],
        summary: `${mark(storage.approximate)}${bytes(storage.usedBytes)} used of ${bytes(storage.totalBytes)}`,
        facts: [{ key: 'free', term: 'Free', value: bytes(storage.freeBytes) }]
      }));
    } else if (usage.storageError) {
      cards.push(gauge({ key: 'storage', title: 'Storage', chips: [chip('warn', 'Unavailable')], note: usage.storageError.message || '' }));
    }
    return cards.length ? h('div', { class: 'infoGauges' }, cards) : empty('Not available', '', 'unavailable');
  }

  // --- Functions ------------------------------------------------------------------------------------------------------

  // The versioning: the read of the rules of the page, else the probe of the server at start.
  function versioningOf(instance, lifecycle) {
    const editor = String(lifecycle?.editor?.versioning || '');
    if (lifecycle && Array.isArray(lifecycle.rules)) return { Enabled: 'enabled', Suspended: 'suspended' }[editor] || 'off';
    return String(instance.versioningState || '');
  }

  const rulesOf = (instance, lifecycle) => (Array.isArray(lifecycle?.rules) ? lifecycle.rules : Number.isInteger(instance.lifecycleRules) ? instance.lifecycleRules : null);

  function versioningRow(instance, lifecycle) {
    if (instance.versioningSupported !== true) return row('Versioning', chip('off', 'Not available'), '', 'versioning');
    switch (versioningOf(instance, lifecycle)) {
      case 'enabled': return row('Versioning', chip('on', 'Enabled'), '', 'versioning');
      case 'suspended': return row('Versioning', chip('warn', 'Suspended'), '', 'versioning');
      case 'off': return row('Versioning', chip('off', 'Never enabled'), '', 'versioning');
      default: return row('Versioning', chip('off', 'Unknown'), '', 'versioning');
    }
  }

  function encryptionRows(instance) {
    const lock = BB.encryption?.bucketLock(instance);
    const asked = instance.encryption === 'sse-s3';
    const configured = asked
      ? chip(instance.encryptionApplied === 'no' ? 'warn' : 'on', 'SSE-S3')
      : chip('off', 'No header');
    const name = BB.states.ENCRYPTION_NAMES[instance.defaultEncryption] || instance.defaultEncryption;
    const bucketDefault = instance.bucketEncryptionSupported === true ? (instance.defaultEncryption ? chip('on', name) : chip('off', 'None')) : '';
    const applied = { yes: chip('on', 'Yes'), no: [chip('warn', 'No'), ' the provider does not encrypt'], unknown: chip('off', 'Unknown') }[instance.encryptionApplied] || '';
    return [
      row('Encryption', configured, '', 'encryption'),
      row('Default encryption', bucketDefault, '', 'default-encryption'),
      row('Applied by the provider', applied, lock?.tone === 'warn' ? lock.title : '', 'encryption-applied')
    ];
  }

  // The state of the rules: a chip "on" with the number of the rules that are on, else "off". The chip is a link to
  // the part "Lifecycle rules" of the page.
  function lifecycleRow(instance, lifecycle) {
    if (instance.lifecycleSupported === false || !can(instance, 'list')) return null;
    if (lifecycle === undefined) return row('Lifecycle rules', 'Loading…', '', 'lifecycle');
    const rules = rulesOf(instance, lifecycle);
    const enabled = Array.isArray(rules) ? rules.filter(item => item.enabled).length : Number(rules) || 0;
    const on = enabled > 0;
    const link = h('a', { class: ['infoChip', `infoChip--${on ? 'on' : 'off'}`, 'infoChip--link'], href: '#info-lifecycle', 'data-info-lifecycle': '', 'data-tone': on ? 'on' : 'off',
      'aria-label': on ? `Lifecycle rules: ${count(enabled)} on` : 'Lifecycle rules: off' }, on ? `on ${count(enabled)}` : 'off');
    return row('Lifecycle rules', link, '', 'lifecycle');
  }

  function checksumRow(instance) {
    if (instance.provider === 's3') return row('SHA-256', chip('on', 'Yes'), '', 'checksum');
    if (isVolume(instance)) return row('SHA-256', chip('on', 'Yes'), '', 'checksum');
    return row('SHA-256', chip('off', 'No'), '', 'checksum');
  }

  const supported = (value, yes = 'Yes') => (value ? chip('on', yes) : chip('off', 'Not available'));

  function functionCards(instance, lifecycle) {
    if (isVolume(instance)) {
      return [card('volume', 'Functions of a mounted folder', {}, pairs([
        row('Folders', chip('on', 'Real folders'), '', 'folders'),
        row('Incomplete uploads', supported(instance.multipartUploadsSupported === true), '', 'multipart'),
        checksumRow(instance)
      ], 'Functions'))];
    }
    const lockDefault = instance.objectLockSupported === true && instance.objectLockDefault ? `Default retention: ${BB.objectLock.defaultText(instance.objectLockDefault)}` : '';
    return [
      card('protection', 'Data protection', {}, pairs([
        versioningRow(instance, lifecycle),
        ...encryptionRows(instance),
        row('Object Lock', instance.objectLockSupported === true ? chip('on', 'On') : chip('off', 'Off'), lockDefault, 'object-lock')
      ], 'Data protection')),
      card('operations', 'Operations', {}, pairs([
        lifecycleRow(instance, lifecycle),
        row('Tags', supported(instance.tagsSupported === true), '', 'tags'),
        row('Metadata edit', supported(instance.metadataEditSupported === true), '', 'metadata'),
        row('Incomplete uploads', supported(instance.multipartUploadsSupported === true), '', 'multipart'),
        checksumRow(instance)
      ], 'Operations'))];
  }

  // --- Page -----------------------------------------------------------------------------------------------------------

  const page = { instance: null, usage: undefined, lifecycle: undefined };

  // The selector of the header: the selector of the listing (switcher.js). A bucket of the list opens its information
  // page. The page reads the rules itself (the editor of the rules needs the whole answer), and gives them to the
  // selector for its state icons.
  const selector = BB.switcher.create({
    root: byId('storageSwitcher'),
    onSelect: entry => { if (entry.id !== page.instance?.id) window.location.assign(BB.switcher.infoPageURL(entry)); },
    watchLifecycle: false
  });

  function render() {
    const instance = page.instance;
    const sections = byId('infoSections');
    // Three columns: the identity, then the permissions and the functions, then the storage.
    sections.replaceChildren(h('div', { class: 'infoColumns' },
      h('div', { class: 'infoColumn', 'data-column': 'identity' }, card('identity', 'Identity', {}, kv(identityRows(instance), 'Identity'))),
      h('div', { class: 'infoColumn', 'data-column': 'functions' },
        card('permissions', 'Permissions', {}, kv(permissionRows(instance), 'Permissions')), ...functionCards(instance, page.lifecycle)),
      h('div', { class: 'infoColumn', 'data-column': 'storage' }, storageBody(instance, page.usage))));
    sections.removeAttribute('aria-busy');
    const banner = byId('infoBanner');
    const text = page.usage ? BB.usage.fullText(page.usage) : '';
    banner.hidden = !text;
    banner.replaceChildren(...(text ? [ic('alert-triangle'), h('span', { class: 'usageBanner__text' }, text)] : []));
  }

  function renderBuild(build) {
    const badge = byId('versionBadge');
    const version = String(build?.version || build?.display || '');
    if (!badge || !version) return;
    const commit = String(build.shortCommit || '');
    badge.textContent = commit && commit !== 'unknown' && !version.includes(commit) ? `${version} (${commit})` : version;
    badge.title = `Server version\n${badge.textContent}${build.date ? `\nBuild: ${build.date}` : ''}`;
  }

  // A page that cannot show a bucket: a small message with the reason. The rest of the page stays empty.
  function fail(title, text) {
    byId('infoTitle').textContent = title;
    byId('infoFailureTitle').textContent = title;
    byId('infoFailureText').textContent = text;
    byId('infoFailure').hidden = false;
    byId('infoSections').removeAttribute('aria-busy');
  }

  // The rules and the versioning of the bucket, for the part Functions and for the state icons of the selector.
  async function loadLifecycle(instance) {
    try {
      page.lifecycle = await BB.api.lifecycle(instance.id);
      selector.setBucketInfo(instance.id, page.lifecycle);
    } catch (_) {
      page.lifecycle = null;
    }
  }

  // The part "Lifecycle rules": the editor of the rules (lifecycle.js) in the page, under the part Functions. The page
  // builds it one time: a new read of the usage does not touch the form. ?info&lifecycle-abort=<prefix> (the link of
  // the window of the incomplete uploads) adds a new rule that aborts the incomplete uploads under that prefix.
  function renderLifecycle(instance) {
    const box = byId('infoLifecycle');
    // A volume, a provider without rules or a person who cannot list: the part is not shown.
    if (isVolume(instance) || instance.lifecycleSupported === false || !can(instance, 'list')) {
      box.replaceChildren();
      return;
    }
    const query = new URLSearchParams(window.location.search);
    const abort = query.has('lifecycle-abort') ? String(query.get('lifecycle-abort') || '') : null;
    if (abort !== null) window.history.replaceState(null, '', `${window.location.pathname}?info${window.location.hash}`);
    const body = h('div', { class: 'lifecycle', 'data-lifecycle': '' });
    // The editor reads the rules only in an element of the page: it mounts after the part is in the page.
    box.replaceChildren(part('lifecycle', 'Lifecycle rules', '', body));
    BB.lifecycle.mount(body, {
      instance: instance.id,
      canWrite: can(instance, 'upload'),
      prefill: abort === null ? null : { id: 'abort-incomplete-uploads', prefix: abort, abortMultipartDays: 7 }
    });
    if (window.location.hash === '#info-lifecycle') byId('info-lifecycle')?.scrollIntoView({ block: 'start' });
  }

  // The usage. While the count of the files of a volume runs, the page reads it again every 3 seconds, 20 times at most.
  async function loadUsage(instance, fresh, rounds = 20) {
    page.usage = await BB.usage.load(instance, { fresh });
    selector.refresh();
    if (page.usage?.selfState === 'computing' && rounds > 0) {
      window.setTimeout(() => { void loadUsage(instance, true, rounds - 1).then(render); }, 3000);
    }
  }

  async function start() {
    const route = BB.runtime.storageRoute();
    let payload;
    try {
      payload = await BB.api.instances();
    } catch (error) {
      selector.setState({ instances: [], currentId: '', loading: false, loadError: String(error?.message || error) });
      fail('The information cannot be loaded', String(error?.message || error));
      return;
    }
    renderBuild(payload?.build);
    const instance = (payload?.instances || []).find(item => String(item.host || item.id || '') === route?.host && item.id === route?.bucket);
    const instances = (payload?.instances || []).map(item => ({ ...item, host: String(item.host || item.id || '') }));
    if (!instance) {
      selector.setState({ instances, currentId: '', loading: false, emptyText: 'This bucket does not exist, or you cannot see it' });
      fail('No such bucket', 'This bucket does not exist, or you cannot see it.');
      return;
    }
    page.instance = instance;
    BB.api.setInstance(instance);
    selector.setState({ instances, currentId: instance.id, loading: false });
    // The usage of the other buckets: the mark of a full bucket shows also before the list opens.
    selector.loadAllUsage();
    // The way to the files of the bucket: the link "Browse" in the header, after the brand.
    const back = byId('infoBack');
    back.href = BB.api.browserPageURL('', { instance: instance.id });
    back.setAttribute('aria-label', `Back to the files of ${instance.name || instance.id}`);
    back.hidden = false;
    document.title = `${instance.name || instance.id} - Information - Stowr`;
    byId('infoTitle').textContent = `Information of the ${noun(instance)} ${instance.name || instance.id}`;
    render();
    renderLifecycle(instance);
    // A save of the rules changes the part Functions and the state icons of the selector.
    document.addEventListener('bb:lifecycle-changed', () => { void loadLifecycle(instance).then(render); });
    const reads = [];
    if (instance.usageSupported === true && can(instance, 'list')) reads.push(loadUsage(instance, false));
    if (instance.lifecycleSupported !== false && !isVolume(instance) && can(instance, 'list')) reads.push(loadLifecycle(instance));
    await Promise.all(reads.map(read => read.then(render)));
    render();
  }

  void start();
})();
