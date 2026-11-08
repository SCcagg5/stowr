/* Object Browser API client. All requests are scoped to the selected instance. */
(function () {
  'use strict';

  const BB = (window.BB = window.BB || {});
  if (!BB.detect) throw new Error('BB.detect is required before BB.api');

  const RESUMABLE_UPLOAD_THRESHOLD = 32 * 1024 * 1024;

  // SHA-256 of the bytes of an upload, in base64: the server sends it to the provider, which refuses other bytes.
  // The browser has crypto.subtle only on a secure page (HTTPS or localhost): elsewhere the upload has no checksum.
  function canChecksum() {
    return typeof crypto !== 'undefined' && !!crypto.subtle && typeof Blob.prototype.arrayBuffer === 'function';
  }

  async function sha256Base64(blob) {
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer()));
    let binary = '';
    for (const byte of digest) binary += String.fromCharCode(byte);
    return btoa(binary);
  }
  let instanceId = '';
  const instanceRoutes = new Map();

  function selectedInstance() {
    return instanceId || (BB.cfg && BB.cfg.instanceId) || '';
  }

  function rememberInstances(instances) {
    for (const instance of Array.from(instances || [])) {
      const id = String(instance?.id || '');
      if (!id) continue;
      instanceRoutes.set(id, {
        host: String(instance?.host || id),
        bucket: id
      });
    }
  }

  function routeForInstance(id) {
    const selected = String(id || '');
    return instanceRoutes.get(selected) || { host: selected, bucket: selected };
  }

  function withInstance(path, extraParams, explicitInstance = null) {
    const url = BB.runtime.resolveURL(path);
    const id = explicitInstance === null || explicitInstance === undefined
      ? selectedInstance()
      : String(explicitInstance || '');
    if (id) url.searchParams.set('instance', id);
    const parameters = { ...(extraParams || {}) };
    if (Object.prototype.hasOwnProperty.call(parameters, 'key') && !parameters.version) {
      const selectedVersion = new URL(window.location?.href || document.baseURI).searchParams.get('version') || '';
      if (selectedVersion) parameters.version = selectedVersion;
    }
    Object.entries(parameters).forEach(([key, value]) => {
      // Empty strings are meaningful for parameters such as delimiter=. Only
      // undefined and null mean that the caller did not supply a value.
      if (value !== undefined && value !== null) url.searchParams.set(key, String(value));
    });
    return url.href;
  }

  // The words of the answers of the access policy. The server sends the code, the action and Retry-After. The page
  // shows a short text for the person. Other answers keep the message of the server.
  function accessMessage(status, code, action, retryAfter, message) {
    if (status === 401 && code === 'unauthenticated') return 'Your session ended. Reload the page.';
    if (status === 403 && code === 'forbidden' && action) return `The action ${action} is not allowed.`;
    if (status === 429 && code === 'too_many_requests') {
      return retryAfter > 0
        ? `The budget of this action is used. Try again in ${retryAfter} ${retryAfter === 1 ? 'second' : 'seconds'}.`
        : 'The budget of this action is used. Try again later.';
    }
    if (status === 503 && code === 'pdp_unavailable') return 'The access decision service does not answer. Try again later.';
    return message;
  }

  function retryAfterSeconds(value) {
    const seconds = Number.parseInt(String(value || ''), 10);
    return Number.isFinite(seconds) && seconds > 0 ? seconds : 0;
  }

  async function request(path, options) {
    const response = await fetch(BB.runtime.resolvePath(path), { cache: 'no-store', ...(options || {}) });
    if (response.ok) return response;
    let message = `HTTP ${response.status}`;
    let code = '';
    let action = '';
    try {
      const type = response.headers.get('Content-Type') || '';
      if (type.includes('application/json')) {
        const payload = await response.json();
        message = payload?.error?.message || payload?.message || message;
        code = payload?.error?.code || payload?.code || '';
        action = String(payload?.error?.action || '');
      } else {
        const text = (await response.text()).trim();
        if (text) message = text;
      }
    } catch (_) {}
    const retryAfter = retryAfterSeconds(response.headers.get('Retry-After'));
    const error = new Error(accessMessage(response.status, code, action, retryAfter, message));
    error.status = response.status;
    error.code = code;
    error.action = action;
    error.retryAfter = retryAfter;
    throw error;
  }

  function sleep(milliseconds) {
    return new Promise(resolve => window.setTimeout(resolve, milliseconds));
  }


  function abortError() {
    try { return new DOMException('The operation was aborted.', 'AbortError'); }
    catch (_) {
      const error = new Error('The operation was aborted.');
      error.name = 'AbortError';
      return error;
    }
  }

  function sleepWithSignal(milliseconds, signal) {
    if (!signal) return sleep(milliseconds);
    if (signal.aborted) return Promise.reject(abortError());
    return new Promise((resolve, reject) => {
      const timer = window.setTimeout(done, milliseconds);
      function done() {
        signal.removeEventListener('abort', canceled);
        resolve();
      }
      function canceled() {
        window.clearTimeout(timer);
        signal.removeEventListener('abort', canceled);
        reject(abortError());
      }
      signal.addEventListener('abort', canceled, { once: true });
    });
  }

  function xhrFailure(xhr) {
    let message = xhr.status ? `HTTP ${xhr.status}` : 'Network request failed';
    let code = '';
    let action = '';
    try {
      const payload = JSON.parse(xhr.responseText || '{}');
      message = payload?.error?.message || payload?.message || message;
      code = payload?.error?.code || payload?.code || '';
      action = String(payload?.error?.action || '');
    } catch (_) {
      const text = String(xhr.responseText || '').trim();
      if (text) message = text;
    }
    const status = Number(xhr.status || 0);
    const retryAfter = retryAfterSeconds(xhr.getResponseHeader('Retry-After'));
    const error = new Error(accessMessage(status, code, action, retryAfter, message));
    error.status = status;
    error.code = code;
    error.action = action;
    error.retryAfter = retryAfter;
    return error;
  }

  function xhrRequest(path, {
    method = 'GET',
    headers = {},
    body = null,
    signal = null,
    onUploadProgress = null,
    onResponseHeaders = null
  } = {}) {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(abortError());
        return;
      }
      const xhr = new XMLHttpRequest();
      xhr.open(method, BB.runtime.resolvePath(path), true);
      Object.entries(headers).forEach(([name, value]) => xhr.setRequestHeader(name, value));
      xhr.responseType = 'text';

      let settled = false;
      const cleanup = () => signal?.removeEventListener('abort', cancel);
      const finish = callback => value => {
        if (settled) return;
        settled = true;
        cleanup();
        callback(value);
      };
      const succeed = finish(resolve);
      const fail = finish(reject);
      const cancel = () => xhr.abort();

      if (typeof onUploadProgress === 'function') {
        xhr.upload.addEventListener('progress', event => {
          onUploadProgress({
            loaded: Number(event.loaded || 0),
            total: event.lengthComputable ? Number(event.total || 0) : Number(body?.size || 0),
            lengthComputable: !!event.lengthComputable
          });
        });
      }
      xhr.addEventListener('load', () => {
        if (xhr.status < 200 || xhr.status >= 300) {
          fail(xhrFailure(xhr));
          return;
        }
        if (typeof onResponseHeaders === 'function') onResponseHeaders(name => xhr.getResponseHeader(name));
        const text = String(xhr.responseText || '').trim();
        if (!text) {
          succeed(null);
          return;
        }
        try { succeed(JSON.parse(text)); }
        catch (error) { fail(new Error(`Invalid JSON response: ${error.message}`)); }
      });
      xhr.addEventListener('error', () => fail(xhrFailure(xhr)));
      xhr.addEventListener('timeout', () => fail(xhrFailure(xhr)));
      xhr.addEventListener('abort', () => fail(abortError()));
      signal?.addEventListener('abort', cancel, { once: true });
      xhr.send(body);
    });
  }

  function isRetriableUploadError(error) {
    const status = Number(error?.status || 0);
    return status === 0 || status === 408 || status === 425 || status === 429 || status >= 500;
  }

  function alignChunkSize(value, alignment) {
    return Math.max(alignment, Math.ceil(Number(value || 0) / alignment) * alignment);
  }

  function adaptiveUploadChunkSize(upload, currentSize, throughput) {
    const mebibyte = 1024 * 1024;
    const provider = String(upload.provider || '').toLowerCase();
    const remaining = Math.max(0, Number(upload.totalSize || 0) - Number(upload.nextOffset || upload.uploadedBytes || 0));
    const partCount = Math.max(0, Number(upload.partCount || 0));
    let minimum;
    let maximum;
    let alignment;

    if (provider === 's3') {
      const remainingPartSlots = Math.max(1, 10000 - partCount);
      minimum = Math.max(5 * mebibyte, Math.ceil(remaining / remainingPartSlots));
      maximum = 128 * mebibyte;
      alignment = mebibyte;
    } else {
      minimum = 4 * mebibyte;
      maximum = 64 * mebibyte;
      alignment = 256 * 1024;
    }

    minimum = alignChunkSize(minimum, alignment);
    const current = Math.max(minimum, Number(currentSize || upload.chunkSize || minimum));
    if (!Number.isFinite(throughput) || throughput <= 0) return Math.min(maximum, alignChunkSize(current, alignment));

    const targetDurationSeconds = 6;
    const measuredTarget = throughput * targetDurationSeconds;
    const boundedTarget = Math.max(current / 2, Math.min(current * 2, measuredTarget));
    const smoothed = current * 0.6 + boundedTarget * 0.4;
    return Math.min(maximum, Math.max(minimum, alignChunkSize(smoothed, alignment)));
  }

  function uploadStorageKey(instance, key, blob, lastModified) {
    return `object-browser-upload:${instance}:${key}:${blob.size}:${Number(lastModified || 0)}`;
  }

  function readStoredUpload(storageKey) {
    return BB.runtime?.readState(storageKey) || '';
  }

  function storeUpload(storageKey, id) {
    BB.runtime?.writeState(storageKey, id || '');
  }

  const api = {
    setInstance(value) {
      if (value && typeof value === 'object') rememberInstances([value]);
      instanceId = String(value && typeof value === 'object' ? value.id : value || '');
      BB.cfg = BB.cfg || {};
      BB.cfg.instanceId = instanceId;
    },

    getInstance() { return selectedInstance(); },

    async instances() {
      const response = await request('/api/instances');
      const payload = await response.json();
      rememberInstances(payload?.instances);
      return payload;
    },

    urlForKey(key, explicitInstance = null, version = '') {
      const clean = String(key || '').replace(/^\/+/, '');
      const parameters = { key: clean };
      if (version) parameters.version = version;
      return withInstance('/s3', parameters, explicitInstance);
    },

    previewURLForKey(key, explicitInstance = null, version = '') {
      const clean = String(key || '').replace(/^\/+/, '');
      const parameters = { key: clean, preview: 1 };
      if (version) parameters.version = version;
      return withInstance('/s3', parameters, explicitInstance);
    },

    browserPageURL(prefix = '', { instance = null } = {}) {
      const selected = instance === null || instance === undefined ? selectedInstance() : String(instance || '');
      if (!selected) return BB.runtime.resolveURL('index.html').href;
      const route = routeForInstance(selected);
      return BB.runtime.browserPageURL(route.host, route.bucket, prefix).href;
    },

    previewPageURL(path, { instance = null, version = '', entry = '', compare = '' } = {}) {
      const clean = String(path || '').replace(/^\/+/, '');
      const selected = instance === null || instance === undefined ? selectedInstance() : String(instance || '');
      const route = routeForInstance(selected);
      const url = selected
        ? BB.runtime.previewPageURL(route.host, route.bucket, clean)
        : BB.runtime.resolveURL('preview.html');
      if (!selected) url.searchParams.set('path', clean);
      if (version) url.searchParams.set('version', String(version));
      if (entry) url.searchParams.set('entry', String(entry));
      if (compare) url.searchParams.set('compare', String(compare));
      return url.href;
    },

    openURLForKey(key, explicitInstance = null, version = '') {
      const clean = String(key || '').replace(/^\/+/, '');
      const filename = clean.split('/').filter(Boolean).pop() || 'download';
      const parameters = { key: clean };
      if (version) parameters.version = version;
      return withInstance(`/open/${encodeURIComponent(filename)}`, parameters, explicitInstance);
    },

    async head(key, version = '', explicitInstance = null) {
      const response = await request(this.urlForKey(key, explicitInstance, version), { method: 'HEAD' });
      const headers = {};
      response.headers.forEach((value, name) => { headers[name] = value; });
      const rawSize = response.headers.get('Content-Length');
      return {
        mime: response.headers.get('Content-Type') || '',
        size: rawSize === null ? null : Number(rawSize),
        sizeKnown: rawSize !== null && Number.isFinite(Number(rawSize)) && Number(rawSize) >= 0,
        headers
      };
    },

    async versions(key, { pageToken = '', maximum = 250, instance = null } = {}) {
      const response = await request(withInstance('/api/versions', { key, pageToken, max: maximum }, instance));
      return response.json();
    },

    async allVersions(key, { instance = null } = {}) {
      const versions = [];
      let pageToken = '';
      const seen = new Set();
      do {
        const page = await this.versions(key, { pageToken, maximum: 1000, instance });
        versions.push(...(page.versions || []));
        const next = String(page.nextPageToken || '');
        if (!next) break;
        if (next === pageToken || seen.has(next)) throw new Error('The provider returned a repeated version page token.');
        seen.add(next);
        pageToken = next;
      } while (versions.length <= 100000);
      if (versions.length > 100000) throw new Error('The object has more than 100,000 versions.');
      return versions;
    },

    async versionCounts(keys, instance = null) {
      const response = await request('/api/version-counts', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance || selectedInstance(), keys })
      });
      return response.json();
    },

    async restoreVersion(key, version, instance = null) {
      const response = await request('/api/versions/restore', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance || selectedInstance(), key, version })
      });
      return response.json();
    },

    async deleteVersion(key, version, instance = null) {
      const response = await request(withInstance('/api/versions', { key, version }, instance), { method: 'DELETE' });
      return response.status === 204;
    },

    async integrity({ key = '', version = '', instance = null } = {}) {
      const response = await request('/api/integrity', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance || selectedInstance(), key, version })
      });
      return response.json();
    },

    async tags(key, { version = '', instance = null } = {}) {
      const response = await request(withInstance('/api/tags', { key, version }, instance));
      return response.json();
    },

    async putTags(key, tags, { version = '', instance = null } = {}) {
      const response = await request('/api/tags', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance === null ? selectedInstance() : String(instance || ''), key, version, tags })
      });
      return response.json();
    },

    async metadata(key, { version = '', instance = null } = {}) {
      const response = await request(withInstance('/api/metadata', { key, version }, instance));
      return response.json();
    },

    // The complete state of the metadata: a header or a user metadata that the request does not give is removed.
    async putMetadata(key, { etag, lastModified = '', headers = {}, metadata = [], version = '', instance = null } = {}) {
      const response = await request('/api/metadata', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance === null ? selectedInstance() : String(instance || ''), key, version, etag, lastModified, headers, metadata })
      });
      return response.json();
    },

    // The default encryption of the bucket (null when it has none) and the encryption of the uploads of the instance.
    async encryption(instance = null) {
      const response = await request(withInstance('/api/encryption', {}, instance));
      return response.json();
    },

    // The usage and the quota of the bucket of an instance. fresh asks the server to read the provider again (it does so
    // only when its value is older than 10 seconds). A failed read answers 200 with available false and the reason.
    async usage(instance = null, { fresh = false } = {}) {
      const response = await request(withInstance('/api/usage', { fresh: fresh ? '1' : null }, instance));
      return response.json();
    },

    // The lock of one version: the retention, the legal hold and the default retention of the bucket. Without a version
    // the answer is for the current version, and it names that version.
    async objectLock(key, { version = '', instance = null } = {}) {
      const response = await request(withInstance('/api/object-lock', { key, version }, instance));
      return response.json();
    },

    // Changes the legal hold, the retention or both for the version that the previous read named. bypassGovernance
    // is true only after the person confirmed a change that needs the bypass of a Governance retention.
    async putObjectLock(key, { version, instance = null, legalHold, retention, bypassGovernance = false } = {}) {
      const body = { instance: instance === null ? selectedInstance() : String(instance || ''), key, version };
      if (legalHold !== undefined) body.legalHold = legalHold;
      if (retention) body.retention = retention;
      if (bypassGovernance) body.bypassGovernance = true;
      const response = await request('/api/object-lock', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      return response.json();
    },

    // One page of the incomplete multipart uploads under a prefix. The token comes from the previous page.
    async multiparts({ prefix = '', token = '', instance = null } = {}) {
      const response = await request(withInstance('/api/multiparts', { prefix, token: token || null }, instance));
      return response.json();
    },

    // Give uploads ([{ key, uploadId }]) to abort them at any age: the answer is { aborted, skipped, failed }. Give
    // olderThanDays to start the job that aborts by age: the answer is the job, for waitForJob.
    async abortMultiparts({ uploads = null, olderThanDays = 0, prefix = '', instance = null } = {}) {
      const body = { instance: instance === null ? selectedInstance() : String(instance || '') };
      if (uploads) body.uploads = uploads;
      else Object.assign(body, { olderThanDays, prefix });
      const response = await request('/api/multiparts/abort', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      return response.json();
    },

    async tagPrefix(prefix, { add = [], remove = [], instance = null } = {}) {
      const response = await request('/api/tags/bulk', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance === null ? selectedInstance() : String(instance || ''), prefix, add, remove })
      });
      return response.json();
    },

    async lifecycle(instance = null) {
      const response = await request(withInstance('/api/lifecycle', {}, instance));
      return response.json();
    },

    // The rules of the lifecycle editor replace the editable rules of the bucket. hash is the hash that lifecycle()
    // gave: the server refuses the save (409 lifecycle_changed) when the rules of the bucket changed since.
    async putLifecycle({ hash, rules = [], instance = null } = {}) {
      const response = await request('/api/lifecycle', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance === null ? selectedInstance() : String(instance || ''), hash, rules })
      });
      return response.json();
    },

    async inspect(key, { version = '', instance = null } = {}) {
      const response = await request(withInstance('/api/inspect', { key, version }, instance));
      return response.json();
    },

    archiveEntryURL(key, entry, { inline = false, instance = null, version = '', size = 0, etag = '', lastModified = '' } = {}) {
      const parameters = { key, version, entry, inline: inline ? 1 : 0 };
      if (Number(size) > 0) parameters.size = Number(size);
      if (etag) parameters.etag = etag;
      if (lastModified) parameters.lastModified = lastModified;
      return withInstance('/api/archive-entry', parameters, instance);
    },

    async archiveEntryHead(key, entry, { instance = null, version = '' } = {}) {
      const response = await request(this.archiveEntryURL(key, entry, { inline: true, instance, version }), { method: 'HEAD' });
      const headers = {};
      response.headers.forEach((value, name) => { headers[name] = value; });
      const rawSize = response.headers.get('Content-Length');
      return {
        mime: response.headers.get('Content-Type') || '',
        size: rawSize === null ? null : Number(rawSize),
        sizeKnown: rawSize !== null && Number.isFinite(Number(rawSize)) && Number(rawSize) >= 0,
        headers
      };
    },

    async archiveEntryIntegrity(key, entry, { version = '', instance = null } = {}) {
      const response = await request('/api/archive-entry/integrity', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: instance || selectedInstance(), key, version, entry })
      });
      return response.json();
    },

    async extractArchive(key, entries, { version = '', target = '', targetInstance = null, instance = null } = {}) {
      const response = await request('/api/archive-extract', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          instance: instance || selectedInstance(), key, version, entries,
          targetInstance: targetInstance || instance || selectedInstance(), target
        })
      });
      return response.json();
    },

    async spreadsheet({
      key,
      sheet = 0,
      page = 0,
      pageSize = 100,
      filters = {},
      sortColumn = '',
      sortDirection = '',
      search = '',
      size = 0,
      version = '',
      instance = null,
      signal = null
    } = {}) {
      const parameters = {
        key,
        sheet,
        page,
        pageSize,
        filters: JSON.stringify(filters || {}),
        search
      };
      if (Number(size) > 0) parameters.size = Math.floor(Number(size));
      if (version) parameters.version = String(version);
      if (sortColumn && sortDirection) {
        parameters.sortColumn = sortColumn;
        parameters.sortDirection = sortDirection;
      }
      const response = await request(withInstance('/api/spreadsheet', parameters, instance), { signal });
      return response.json();
    },

    async delimitedPage({ key, cursor = '', etag = '', pageSize = 100, version = '', instance = null, signal = null } = {}) {
      const parameters = { key, pageSize };
      if (version) parameters.version = String(version);
      if (cursor) parameters.cursor = cursor;
      if (etag) parameters.etag = etag;
      const response = await request(withInstance('/api/delimited', parameters, instance), { signal });
      return response.json();
    },

    async documentCount({ key, size = 0, version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (Number(size) > 0) parameters.size = Math.floor(Number(size));
      const response = await request(withInstance('/api/document-count', parameters, instance), { signal });
      return response.json();
    },

    async wordPreview({ key, size = 0, etag = '', version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (Number(size) > 0) parameters.size = Math.floor(Number(size));
      if (etag) parameters.etag = String(etag);
      const response = await request(withInstance('/api/document/word', parameters, instance), { signal });
      return response.json();
    },

    async parquetPreview({ key, size = 0, etag = '', version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (Number(size) > 0) parameters.size = Math.floor(Number(size));
      if (etag) parameters.etag = String(etag);
      const response = await request(withInstance('/api/parquet', parameters, instance), { signal });
      return response.json();
    },

    async jsonRaw({ key, cursor = '', etag = '', version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (cursor) parameters.cursor = cursor;
      if (etag) parameters.etag = etag;
      const response = await request(withInstance('/api/json/raw', parameters, instance), { signal });
      return response.json();
    },

    async jsonBeautify({ key, cursor = '', etag = '', version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (cursor) parameters.cursor = cursor;
      if (etag) parameters.etag = etag;
      const response = await request(withInstance('/api/json/beautify', parameters, instance), { signal });
      return response.json();
    },

    async jsonSummary({ key, etag = '', version = '', instance = null, signal = null } = {}) {
      const parameters = { key };
      if (version) parameters.version = String(version);
      if (etag) parameters.etag = etag;
      const response = await request(withInstance('/api/json/summary', parameters, instance), { signal });
      return response.json();
    },

    async searchDocument({ key, query, version = '', instance = null, signal = null } = {}) {
      const response = await request(withInstance('/api/search', { key, q: query, version }, instance), { signal });
      return response.json();
    },

    async jsonTree({
      key,
      start = 0,
      cursor = '',
      index = 0,
      type = '',
      limit = 50,
      etag = '',
      version = '',
      instance = null,
      signal = null
    } = {}) {
      const parameters = { key, limit };
      if (version) parameters.version = String(version);
      if (Number(start) > 0) parameters.start = Math.floor(Number(start));
      if (String(cursor || '').trim()) parameters.cursor = String(cursor);
      if (Number(index) > 0) parameters.index = Math.floor(Number(index));
      if (type) parameters.type = type;
      if (etag) parameters.etag = etag;
      const response = await request(withInstance('/api/json/tree', parameters, instance), { signal });
      return response.json();
    },

    async createSQLiteSession({ key, size = 0, version = '', instance = null, signal = null } = {}) {
      const response = await request(withInstance('/api/sqlite/sessions', { key, version }, instance), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        signal,
        body: JSON.stringify({
          instance: String(instance || selectedInstance()),
          key,
          size: Math.max(0, Math.floor(Number(size) || 0))
        })
      });
      return response.json();
    },

    async sqliteTable({ id, table, page = 0, pageSize = 100, filters = {}, sortColumn = '', sortDirection = '', signal = null } = {}) {
      const path = `/api/sqlite/sessions/${encodeURIComponent(id)}/table`;
      const parameters = { table, page, pageSize };
      const activeFilters = Object.fromEntries(Object.entries(filters || {}).filter(([, value]) => String(value || '').trim()));
      if (Object.keys(activeFilters).length) parameters.filters = JSON.stringify(activeFilters);
      if (sortColumn && (sortDirection === 'asc' || sortDirection === 'desc')) {
        parameters.sortColumn = sortColumn;
        parameters.sortDirection = sortDirection;
      }
      const response = await request(withInstance(path, parameters, ''), { signal });
      return response.json();
    },

    async deleteSQLiteSession(id, options = {}) {
      if (!id) return;
      await request(`/api/sqlite/sessions/${encodeURIComponent(id)}`, {
        method: 'DELETE',
        keepalive: !!options.keepalive,
        signal: options.signal || null
      });
    },

    async mediaInfo(key, options = {}) {
      const parameters = { key };
      if (options.version) parameters.version = String(options.version);
      const size = Number(options.size);
      if (Number.isFinite(size) && size >= 0) parameters.size = String(size);
      if (options.mime) parameters.mime = String(options.mime);
      if (options.etag) parameters.etag = String(options.etag);
      if (options.lastModified) parameters.lastModified = String(options.lastModified);
      const response = await request(withInstance('/api/media-info', parameters, options.instance ?? null), {
        signal: options.signal || null
      });
      return response.json();
    },

    async structuredPreview(key, options = {}) {
      const parameters = { key };
      if (options.version) parameters.version = String(options.version);
      const size = Number(options.size);
      if (Number.isFinite(size) && size >= 0) parameters.size = String(size);
      if (options.mime) parameters.mime = String(options.mime);
      if (options.etag) parameters.etag = String(options.etag);
      if (options.lastModified) parameters.lastModified = String(options.lastModified);
      const response = await request(withInstance('/api/structured-preview', parameters, options.instance ?? null), {
        signal: options.signal || null
      });
      return response.json();
    },

    async archivePreview(key, options = {}) {
      const parameters = { key };
      if (options.version) parameters.version = String(options.version);
      const size = Number(options.size);
      if (Number.isFinite(size) && size >= 0) parameters.size = String(size);
      if (options.etag) parameters.etag = String(options.etag);
      if (options.lastModified) parameters.lastModified = String(options.lastModified);
      const response = await request(withInstance('/api/archive-preview', parameters, options.instance ?? null), {
        signal: options.signal || null
      });
      return response.json();
    },

    imagePreviewURL(key, options = {}) {
      const parameters = { key };
      if (options.version) parameters.version = String(options.version);
      return withInstance('/api/image-preview', parameters, options.instance ?? null);
    },

    async putBlob(key, blob, mime, options = {}) {
      const onProgress = typeof options.onProgress === 'function' ? options.onProgress : () => {};
      const signal = options.signal || null;
      const startedAt = Date.now();
      let speedBps = 0;
      let previousBytes = 0;
      let previousAt = startedAt;
      const putHeaders = { 'Content-Type': mime || 'application/octet-stream' };
      const taggingHeader = BB.tags?.uploadTagsHeader?.() || '';
      if (taggingHeader) putHeaders['X-Amz-Tagging'] = taggingHeader;
      if (canChecksum()) putHeaders['X-Checksum-Sha256'] = await sha256Base64(blob);
      let checksumVerified = false;
      await xhrRequest(this.urlForKey(key, options.instance), {
        method: 'PUT',
        headers: putHeaders,
        body: blob,
        signal,
        onResponseHeaders(header) { checksumVerified = header('X-Checksum-Sha256-Verified') === 'true'; },
        onUploadProgress(event) {
          const now = Date.now();
          const uploadedBytes = Math.min(blob.size, Number(event.loaded || 0));
          const elapsed = Math.max(1, now - previousAt);
          if (uploadedBytes > previousBytes && elapsed >= 120) {
            const instantaneous = (uploadedBytes - previousBytes) * 1000 / elapsed;
            speedBps = speedBps ? speedBps * 0.72 + instantaneous * 0.28 : instantaneous;
            previousBytes = uploadedBytes;
            previousAt = now;
          }
          onProgress({
            status: 'uploading',
            phase: 'uploading',
            provider: 'direct',
            resumable: false,
            uploadedBytes,
            totalSize: blob.size,
            progress: blob.size ? uploadedBytes / blob.size : 1,
            speedBps,
            etaSeconds: speedBps > 0 ? Math.max(0, (blob.size - uploadedBytes) / speedBps) : null
          });
        }
      });
      const result = { status: 'completed', uploadedBytes: blob.size, totalSize: blob.size, checksumVerified };
      onProgress({
        ...result,
        phase: 'completed',
        provider: 'direct',
        resumable: false,
        progress: 1,
        speedBps: blob.size * 1000 / Math.max(1, Date.now() - startedAt),
        etaSeconds: 0
      });
      return result;
    },

    async createUpload({ key, size, contentType, instance = '' }, options = {}) {
      const targetInstance = String(instance || options.instance || selectedInstance());
      const response = await request('/api/uploads', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          instance: targetInstance,
          key,
          size,
          contentType: contentType || 'application/octet-stream',
          checksum: canChecksum() ? 'sha256' : '',
          tags: BB.tags?.uploadTagsToSend?.() || []
        }),
        signal: options.signal || null
      });
      return response.json();
    },

    async resumeUpload(resumeToken, options = {}) {
      const response = await request('/api/uploads/resume', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ resumeToken }),
        signal: options.signal || null
      });
      return response.json();
    },

    async uploadStatus(resumeToken, options = {}) {
      return this.resumeUpload(resumeToken, options);
    },

    async uploadChunk(resumeToken, blob, start, total, options = {}) {
      const end = start + blob.size - 1;
      return xhrRequest('/api/uploads', {
        method: 'PUT',
        headers: {
          'Content-Type': 'application/octet-stream',
          'Content-Range': `bytes ${start}-${end}/${total}`,
          'X-Stowr-Resume-Token': resumeToken,
          ...(options.checksum ? { 'X-Checksum-Sha256': options.checksum } : {})
        },
        body: blob,
        signal: options.signal || null,
        onUploadProgress: options.onProgress || null
      });
    },

    async cancelUpload(resumeToken) {
      const response = await request('/api/uploads', {
        method: 'DELETE',
        headers: { 'X-Stowr-Resume-Token': resumeToken }
      });
      return response.json();
    },

    async uploadBlob(key, blob, mime, options = {}) {
      if (!(blob instanceof Blob)) throw new TypeError('uploadBlob requires a Blob or File');
      const onProgress = typeof options.onProgress === 'function' ? options.onProgress : () => {};
      const onSession = typeof options.onSession === 'function' ? options.onSession : () => {};
      const signal = options.signal || null;
      const instance = String(options.instance || selectedInstance());
      if (signal?.aborted) throw abortError();
      if (blob.size < RESUMABLE_UPLOAD_THRESHOLD) {
        return this.putBlob(key, blob, mime, { onProgress, signal, instance });
      }

      const storageKey = uploadStorageKey(instance, key, blob, options.lastModified);
      let upload = null;
      let resumed = false;
      const storedToken = readStoredUpload(storageKey);
      if (storedToken) {
        try {
          const candidate = await this.uploadStatus(storedToken, { signal });
          if (candidate.instance === instance && candidate.key === key && candidate.totalSize === blob.size && candidate.status === 'uploading') {
            upload = candidate;
            resumed = Number(candidate.uploadedBytes || 0) > 0;
            storeUpload(storageKey, candidate.resumeToken || storedToken);
            onSession(candidate);
          } else if (candidate.status === 'completed' && candidate.key === key && candidate.totalSize === blob.size) {
            storeUpload(storageKey, '');
            onProgress({ ...candidate, phase: 'completed', resumable: true, resumed: true, progress: 1, etaSeconds: 0 });
            return candidate;
          } else {
            storeUpload(storageKey, '');
          }
        } catch (error) {
          if ([400, 404, 410].includes(Number(error.status || 0))) storeUpload(storageKey, '');
          else throw error;
        }
      }
      if (!upload) {
        upload = await this.createUpload({ instance, key, size: blob.size, contentType: mime }, { signal });
        storeUpload(storageKey, upload.resumeToken);
        onSession(upload);
      }

      let selectedChunkSize = Math.max(1, Number(upload.chunkSize || 8 * 1024 * 1024));
      let speedBps = 0;
      let lastMeterBytes = Number(upload.uploadedBytes || 0);
      let lastMeterAt = Date.now();

      const emit = (uploadedBytes, extra = {}) => {
        const now = Date.now();
        const boundedBytes = Math.max(Number(upload.uploadedBytes || 0), Math.min(blob.size, Number(uploadedBytes || 0)));
        const elapsed = Math.max(1, now - lastMeterAt);
        if (boundedBytes > lastMeterBytes && elapsed >= 120) {
          const instantaneous = (boundedBytes - lastMeterBytes) * 1000 / elapsed;
          speedBps = speedBps ? speedBps * 0.75 + instantaneous * 0.25 : instantaneous;
          lastMeterBytes = boundedBytes;
          lastMeterAt = now;
        }
        onProgress({
          ...upload,
          status: extra.status || upload.status || 'uploading',
          phase: extra.phase || 'uploading',
          resumable: true,
          resumed,
          uploadedBytes: boundedBytes,
          totalSize: blob.size,
          progress: blob.size ? boundedBytes / blob.size : 1,
          speedBps,
          etaSeconds: speedBps > 0 ? Math.max(0, (blob.size - boundedBytes) / speedBps) : null,
          selectedChunkSize,
          retryAttempt: Number(extra.retryAttempt || 0),
          retryInSeconds: Number(extra.retryInSeconds || 0)
        });
      };

      emit(Number(upload.uploadedBytes || 0), { phase: resumed ? 'resuming' : 'starting' });

      while (upload.status === 'uploading' && upload.nextOffset < blob.size) {
        if (signal?.aborted) throw abortError();
        const start = Number(upload.nextOffset || upload.uploadedBytes || 0);
        selectedChunkSize = adaptiveUploadChunkSize(upload, selectedChunkSize, speedBps);
        const end = Math.min(blob.size, start + selectedChunkSize);
        const chunk = blob.slice(start, end);
        const chunkChecksum = upload.checksum === 'sha256' && canChecksum() ? await sha256Base64(chunk) : '';
        if (upload.checksum === 'sha256' && !chunkChecksum) throw new Error('This page cannot compute the SHA-256 checksum that the upload needs.');
        const chunkStartedAt = Date.now();
        let attempt = 0;

        for (;;) {
          try {
            upload = await this.uploadChunk(upload.resumeToken, chunk, start, blob.size, {
              signal,
              checksum: chunkChecksum,
              onProgress: event => emit(start + Math.min(chunk.size, Number(event.loaded || 0)), { phase: 'uploading' })
            });
            storeUpload(storageKey, upload.resumeToken || storedToken || '');
            const chunkThroughput = chunk.size * 1000 / Math.max(1, Date.now() - chunkStartedAt);
            speedBps = speedBps ? speedBps * 0.65 + chunkThroughput * 0.35 : chunkThroughput;
            selectedChunkSize = adaptiveUploadChunkSize(upload, selectedChunkSize, speedBps);
            emit(Number(upload.uploadedBytes || upload.nextOffset || end), { phase: upload.status === 'completed' ? 'completed' : 'uploading' });
            break;
          } catch (error) {
            if (error?.name === 'AbortError') throw error;
            if (error.code === 'offset_mismatch' || error.status === 409) {
              upload = await this.uploadStatus(upload.resumeToken, { signal });
              storeUpload(storageKey, upload.resumeToken || storedToken || '');
              emit(Number(upload.uploadedBytes || 0), { phase: 'resuming' });
              break;
            }
            if (!isRetriableUploadError(error) || attempt >= 4) throw error;
            attempt++;
            const delay = Math.min(8000, 500 * (2 ** (attempt - 1))) + Math.round(Math.random() * 250);
            emit(start, { phase: 'retrying', retryAttempt: attempt, retryInSeconds: delay / 1000 });
            await sleepWithSignal(delay, signal);
            try {
              const status = await this.uploadStatus(upload.resumeToken, { signal });
              upload = status;
              storeUpload(storageKey, status.resumeToken || storedToken || '');
              if (Number(status.nextOffset || status.uploadedBytes || 0) > start || status.status !== 'uploading') {
                emit(Number(status.uploadedBytes || 0), { phase: status.status === 'completed' ? 'completed' : 'resuming' });
                break;
              }
            } catch (statusError) {
              if (statusError?.name === 'AbortError') throw statusError;
            }
          }
        }
      }
      if (upload.status !== 'completed') { upload = await this.uploadStatus(upload.resumeToken, { signal }); storeUpload(storageKey, upload.resumeToken || storedToken || ''); }
      if (upload.status !== 'completed') throw new Error(upload.error || `Upload stopped with status ${upload.status}.`);
      storeUpload(storageKey, '');
      emit(blob.size, { phase: 'completed', status: 'completed' });
      return upload;
    },

    async del(key) {
      await request(this.urlForKey(key), { method: 'DELETE' });
      return true;
    },

    async list({ prefix = '', delimiter = '/', continuationToken = '', exclude = '', signal = null, instance = null } = {}) {
      const response = await request(
        withInstance('/api/list', { prefix, delimiter, continuationToken, exclude }, instance),
        { signal }
      );
      return response.json();
    },

    async listAllItems(prefix = '', options = {}) {
      const items = [];
      const signal = options.signal || null;
      const exclude = options.exclude || '';
      const instance = options.instance ?? null;
      const onPage = typeof options.onPage === 'function' ? options.onPage : () => {};
      let continuationToken = '';
      do {
        const page = await this.list({
          prefix,
          delimiter: '',
          continuationToken,
          exclude,
          signal,
          instance
        });
        for (const item of page.items || []) {
          if (item.type === 'content') items.push(item);
        }
        onPage({ count: items.length, itemsCount: items.length, page });
        continuationToken = page.nextContinuationToken || '';
      } while (continuationToken);
      return items;
    },

    archiveURL({ prefix = '', name = 'archive.zip', instance = null } = {}) {
      return withInstance('/api/archive', { prefix, name }, instance);
    },

    async listAll(prefix = '', options = {}) {
      const items = await this.listAllItems(prefix, options);
      return items.map(item => item.key);
    },

    async copy({ src, dst, isPrefix = false }) {
      const response = await request('/api/copy', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: selectedInstance(), src, dst, isPrefix: !!isPrefix })
      });
      return response.json();
    },

    async rename({ src, dst, isPrefix = false }) {
      const response = await request('/api/rename', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: selectedInstance(), src, dst, isPrefix: !!isPrefix })
      });
      return response.json();
    },

    async deletePrefix(prefix) {
      const response = await request('/api/delete-prefix', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ instance: selectedInstance(), prefix })
      });
      return response.json();
    },

    async stats(prefix = '') {
      const response = await request(withInstance('/api/stats', { prefix }));
      return response.json();
    },

    async job(id) {
      const response = await request(`/api/jobs/${encodeURIComponent(id)}`);
      return response.json();
    },

    async jobAction(id, action) {
      const response = await request(`/api/jobs/${encodeURIComponent(id)}/${encodeURIComponent(action)}`, { method: 'POST' });
      return response.json();
    },

    async waitForJob(initialJob, options = {}) {
      let job = typeof initialJob === 'string' ? await this.job(initialJob) : initialJob;
      const onUpdate = typeof options.onUpdate === 'function' ? options.onUpdate : () => {};
      const pollInterval = Math.max(250, Number(options.pollInterval || 750));
      for (;;) {
        onUpdate(job);
        if (job.status === 'completed') return job;
        if (job.status === 'failed') throw new Error(job.error || 'Background job failed.');
        if (job.status === 'canceled') throw new Error('Background job was canceled.');
        if (job.status === 'paused') return job;
        await sleep(pollInterval);
        job = await this.job(job.id);
      }
    }
  };

  BB.api = api;
})();
