/*!
 * BB.uploads: the bulk upload of the interface.
 *
 * One batch holds the files of one or more selections. The batch:
 * - sends MAX_ACTIVE files at the same time, never more (bounded concurrency);
 * - shows the overall progress (files, bytes, rate, time left), the files in progress (MAX_ACTIVE rows at most) and the
 *   failed files with their reasons (MAX_FAILED_ROWS rows at most, then a count). The DOM is updated at most every
 *   RENDER_MS, so hundreds of files cost the same as four (bounded rendering);
 * - keeps one message when the server does not answer: the files wait in the queue, the batch asks /healthz again
 *   with a growing delay and continues when the server answers;
 * - gives "Retry failed", "Cancel" and a summary at the end.
 * The batch uses only BB.api.uploadBlob and BB.api.cancelUpload: the API does not change.
 */
(function () {
  'use strict';

  const BB = (window.BB = window.BB || {});

  const MAX_ACTIVE = 4;
  const MAX_FAILED_ROWS = 50;
  const RENDER_MS = 200;
  const RATE_WINDOW_MS = 6000;
  const PROBE_DELAYS_MS = [2000, 4000, 8000, 15000, 30000];

  // ---------------------------------------------------------------------------------------------------------------
  // Formatting
  // ---------------------------------------------------------------------------------------------------------------

  function formatBytes(value) {
    const bytes = Math.max(0, Number(value) || 0);
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let size = bytes;
    let unit = 0;
    while (size >= 1024 && unit < units.length - 1) {
      size /= 1024;
      unit += 1;
    }
    const text = unit === 0 ? String(Math.round(size)) : size.toFixed(size >= 100 ? 0 : 1).replace(/\.0$/, '');
    return `${text} ${units[unit]}`;
  }

  function formatDuration(seconds) {
    const total = Math.max(0, Math.round(Number(seconds) || 0));
    if (total < 60) return `${total} s`;
    const minutes = Math.floor(total / 60);
    if (minutes < 60) {
      const rest = total % 60;
      return rest && minutes < 10 ? `${minutes} min ${rest} s` : `${minutes} min`;
    }
    const hours = Math.floor(minutes / 60);
    return `${hours} h ${minutes % 60} min`;
  }

  function count(value, singular, plural = `${singular}s`) {
    const number = Math.max(0, Number(value) || 0);
    return `${number.toLocaleString('en-US')} ${number === 1 ? singular : plural}`;
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Error classes
  // ---------------------------------------------------------------------------------------------------------------

  function isAbort(error) {
    return error?.name === 'AbortError' || BB.actions?.isAbortError?.(error) === true;
  }

  // No answer, or the answer of a proxy for a backend that does not answer.
  function isUnreachable(error) {
    if (!error || isAbort(error)) return false;
    const status = Number(error.status);
    // A 503 with the code pdp_unavailable is not a server that does not answer: the access decision service fails.
    if (status === 503 && error.code === 'pdp_unavailable') return false;
    if (Number.isFinite(status) && status !== 0) return status === 502 || status === 503 || status === 504;
    return error instanceof TypeError || /network|failed to fetch|load failed/i.test(String(error.message || ''));
  }

  function reasonOf(error) {
    const status = Number(error?.status || 0);
    if (status === 401) return 'Your session ended. Reload the page.';
    if (status === 403) {
      // The server names the action in its answer: "The action object.write is not allowed."
      const action = String(error?.action || '').trim();
      return action ? `The action ${action} is not allowed.` : 'Not allowed (403).';
    }
    if (status === 429 && error?.code === 'too_many_requests') return String(error.message || '').trim();
    if (status === 503 && error?.code === 'pdp_unavailable') return String(error.message || '').trim();
    if (status === 413) return 'Too large for the server (413).';
    const message = String(error?.message || error || 'Upload failed.').trim();
    return status && !message.includes(String(status)) ? `${message} (${status}).` : message;
  }

  // ---------------------------------------------------------------------------------------------------------------
  // DOM helpers
  // ---------------------------------------------------------------------------------------------------------------

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

  const ic = name => h('i', { class: `ic ic-${name}`, 'aria-hidden': 'true' });

  function setText(node, text) {
    const value = String(text);
    if (node.textContent !== value) node.textContent = value;
  }

  // The fill of a bar scales with --ratio (a transform, so no layout work).
  function setWidth(node, ratio) {
    const value = (Math.max(0, Math.min(1, Number(ratio) || 0))).toFixed(4);
    if (node.style.getPropertyValue('--ratio') !== value) node.style.setProperty('--ratio', value);
  }

  // ---------------------------------------------------------------------------------------------------------------
  // The batch
  // ---------------------------------------------------------------------------------------------------------------

  function create({ host, onFileDone = () => {}, onBatchEnd = () => {}, onSignInEnded = () => {}, onBlockedChange = () => {} }) {
    let batch = null;
    let sequence = 0;
    let renderTimer = 0;
    let lastRender = 0;
    let view = null;
    let collapsed = false;

    function newBatch(destination) {
      return {
        id: ++sequence,
        destination,
        entries: [],
        queue: [],
        running: new Set(),
        failed: [],
        doneCount: 0,
        checksumCount: 0,
        canceledCount: 0,
        totalBytes: 0,
        doneBytes: 0,
        startedAt: Date.now(),
        endedAt: 0,
        samples: [],
        blocked: '',
        probeAttempt: 0,
        probeAt: 0,
        probeTimer: 0,
        canceling: false,
        failedSignature: ''
      };
    }

    function finished(current = batch) {
      return Boolean(current) && current.queue.length === 0 && current.running.size === 0 && !current.blocked;
    }

    function inFlightBytes(current) {
      let sum = 0;
      for (const entry of current.running) sum += entry.uploadedBytes;
      return sum;
    }

    function sentBytes(current) {
      return current.doneBytes + inFlightBytes(current);
    }

    function sample(current) {
      const now = Date.now();
      current.samples.push([now, sentBytes(current)]);
      while (current.samples.length > 2 && now - current.samples[0][0] > RATE_WINDOW_MS) current.samples.shift();
    }

    function rate(current) {
      if (current.blocked || current.samples.length < 2) return 0;
      const [firstAt, firstBytes] = current.samples[0];
      const [lastAt, lastBytes] = current.samples[current.samples.length - 1];
      const seconds = (lastAt - firstAt) / 1000;
      return seconds >= 1 ? Math.max(0, (lastBytes - firstBytes) / seconds) : 0;
    }

    // add(files, destination): files are { file, key, relative }. destination is { instanceId, prefix, label }.
    function add(files, destination) {
      const list = Array.from(files || []).filter(item => item && item.file && item.key);
      if (!list.length) return;
      if (!batch || finished(batch) || batch.destination.instanceId !== destination.instanceId) {
        if (batch && !finished(batch)) {
          BB.ui.toast('Wait for the uploads in progress to finish before you upload to another bucket.', { status: 'error', duration: 6000 });
          return;
        }
        clearTimeout(batch?.probeTimer);
        batch = newBatch(destination);
        collapsed = false;
        view = null;
      }
      for (const item of list) {
        const entry = {
          id: `${batch.id}-${batch.entries.length + 1}`,
          file: item.file,
          key: item.key,
          relative: item.relative || item.file.name,
          size: Math.max(0, Number(item.file.size) || 0),
          uploadedBytes: 0,
          status: 'queued',
          reason: '',
          controller: null,
          resumeToken: ''
        };
        batch.entries.push(entry);
        batch.queue.push(entry);
        batch.totalBytes += entry.size;
      }
      announce(`${count(batch.entries.length, 'file')} in the upload queue.`);
      dispatch();
      render(true);
    }

    function dispatch() {
      const current = batch;
      if (!current) return;
      if (finished(current)) {
        if (!current.endedAt) end(current);
        return;
      }
      if (current.blocked || current.canceling) return;
      while (current.running.size < MAX_ACTIVE && current.queue.length) {
        const entry = current.queue.shift();
        if (entry.status !== 'queued') continue;
        void run(current, entry);
      }
      if (finished(current) && !current.endedAt) end(current);
    }

    async function run(current, entry) {
      entry.status = 'running';
      entry.reason = '';
      entry.controller = new AbortController();
      current.running.add(entry);
      schedule();
      try {
        const result = await BB.api.uploadBlob(entry.key, entry.file, entry.file.type || 'application/octet-stream', {
          instance: current.destination.instanceId,
          lastModified: entry.file.lastModified,
          signal: entry.controller.signal,
          onSession(upload) { entry.resumeToken = String(upload?.resumeToken || ''); },
          onProgress(progress) {
            entry.uploadedBytes = Math.min(entry.size, Math.max(0, Number(progress?.uploadedBytes || 0)));
            schedule();
          }
        });
        entry.status = 'done';
        entry.uploadedBytes = entry.size;
        if (result?.checksumVerified === true) current.checksumCount += 1;
        current.doneCount += 1;
        current.doneBytes += entry.size;
        onFileDone(entry, current.destination);
      } catch (error) {
        if (current.canceling) {
          entry.status = 'canceled';
          current.canceledCount += 1;
          if (entry.resumeToken) BB.api.cancelUpload(entry.resumeToken).catch(() => {});
        } else if (isAbort(error) && current.blocked) {
          // The batch stopped this file because the server does not answer: the file waits in the queue. A resumable
          // upload keeps its session and continues from its last confirmed part.
          requeue(current, entry);
        } else if (isUnreachable(error)) {
          requeue(current, entry);
          block(current, 'offline');
        } else if (Number(error?.status) === 401) {
          requeue(current, entry);
          block(current, 'signin');
          onSignInEnded();
        } else {
          entry.status = 'failed';
          entry.reason = isAbort(error) ? 'Stopped.' : reasonOf(error);
          current.failed.push(entry);
        }
      } finally {
        entry.controller = null;
        current.running.delete(entry);
        if (entry.status === 'done') entry.file = null;
        dispatch();
        schedule();
      }
    }

    function requeue(current, entry) {
      entry.status = 'queued';
      entry.uploadedBytes = 0;
      current.queue.unshift(entry);
    }

    function block(current, reason) {
      if (current.blocked === reason) return;
      current.blocked = reason;
      current.samples = [];
      onBlockedChange(reason);
      // Stop the other files: they would fail one by one with the same cause.
      for (const entry of Array.from(current.running)) entry.controller?.abort(new DOMException('blocked', 'AbortError'));
      if (reason === 'offline') {
        announce('The server does not answer. The upload waits and continues when the server answers.');
        probeLater(current);
      } else {
        announce('Your sign-in has ended. The upload waits.');
      }
    }

    function probeLater(current) {
      clearTimeout(current.probeTimer);
      const delay = PROBE_DELAYS_MS[Math.min(current.probeAttempt, PROBE_DELAYS_MS.length - 1)];
      current.probeAttempt += 1;
      current.probeAt = Date.now() + delay;
      current.probeTimer = setTimeout(() => probe(current), delay);
    }

    async function probe(current) {
      if (batch !== current || current.blocked !== 'offline') return;
      clearTimeout(current.probeTimer);
      current.probeAt = 0;
      render(true);
      let reachable = false;
      try {
        const response = await fetch(BB.runtime.resolvePath('healthz'), { cache: 'no-store' });
        reachable = ![502, 503, 504].includes(response.status);
      } catch (_) {
        reachable = false;
      }
      if (batch !== current || current.blocked !== 'offline') return;
      if (reachable) resume(current);
      else probeLater(current);
      render(true);
    }

    function resume(current = batch) {
      if (!current || !current.blocked) return;
      clearTimeout(current.probeTimer);
      current.blocked = '';
      current.probeAttempt = 0;
      current.probeAt = 0;
      onBlockedChange('');
      announce('The upload continues.');
      dispatch();
      render(true);
    }

    function retryFailed() {
      const current = batch;
      if (!current || !current.failed.length) return;
      const failed = current.failed.splice(0);
      for (const entry of failed) {
        entry.status = 'queued';
        entry.reason = '';
        entry.uploadedBytes = 0;
        current.queue.push(entry);
      }
      current.endedAt = 0;
      current.canceling = false;
      announce(`${count(failed.length, 'file')} in the queue again.`);
      dispatch();
      render(true);
    }

    function cancel() {
      const current = batch;
      if (!current || finished(current)) return;
      current.canceling = true;
      clearTimeout(current.probeTimer);
      current.blocked = '';
      onBlockedChange('');
      for (const entry of current.queue.splice(0)) {
        entry.status = 'canceled';
        current.canceledCount += 1;
      }
      for (const entry of Array.from(current.running)) entry.controller?.abort();
      announce('Upload canceled.');
      if (finished(current) && !current.endedAt) end(current);
      render(true);
    }

    function end(current) {
      current.endedAt = Date.now();
      current.samples = [];
      const parts = [`${count(current.doneCount, 'file')} uploaded`];
      if (current.failed.length) parts.push(`${current.failed.length.toLocaleString('en-US')} failed`);
      if (current.canceledCount) parts.push(`${current.canceledCount.toLocaleString('en-US')} canceled`);
      announce(`Upload finished: ${parts.join(', ')}.`);
      onBatchEnd(current.destination, current);
      render(true);
    }

    function close() {
      if (batch && !finished(batch)) return;
      clearTimeout(batch?.probeTimer);
      batch = null;
      view = null;
      host.replaceChildren();
      host.hidden = true;
    }

    // -------------------------------------------------------------------------------------------------------------
    // View
    // -------------------------------------------------------------------------------------------------------------

    function announce(text) {
      if (!view) build();
      setText(view.live, text);
    }

    function build() {
      const live = h('span', { class: 'visually-hidden', role: 'status', 'aria-live': 'polite' });
      const stateIcon = h('span', { class: 'uploadDock__state', 'aria-hidden': 'true' }, ic('upload'));
      const title = h('div', { class: 'uploadDock__title' });
      const dest = h('div', { class: 'uploadDock__dest' });
      const collapseButton = h('button', {
        type: 'button', class: 'button button--ghost button--small button--icon',
        onclick: () => { collapsed = !collapsed; render(true); }
      }, ic('minimize'));
      const closeButton = h('button', {
        type: 'button', class: 'button button--ghost button--small button--icon', 'aria-label': 'Close', title: 'Close',
        onclick: close
      }, ic('x'));
      const mini = h('div', { class: 'uploadDock__mini', 'aria-hidden': 'true' }, h('span'));

      const notice = h('div', { class: 'callout callout--danger', hidden: true });
      const bar = h('div', { class: 'progress', role: 'progressbar', 'aria-label': 'Upload progress', 'aria-valuemin': '0', 'aria-valuemax': '100' }, h('span'));
      const stat = name => h('dd', { 'data-stat': name });
      const stats = h('dl', { class: 'stats' },
        h('div', {}, h('dt', {}, 'Files sent'), stat('files')),
        h('div', {}, h('dt', {}, 'Data'), stat('bytes')),
        h('div', {}, h('dt', {}, 'Rate'), stat('rate')),
        h('div', {}, h('dt', {}, 'Time left'), stat('eta')));
      const meter = h('div', { class: 'meter' }, bar, stats);

      const activeHead = h('div', { class: 'subhead' });
      const activeList = h('ul', { class: 'fileList', 'aria-label': 'Files in progress' });
      const active = h('section', { class: 'meter' }, activeHead, activeList);

      const retryButton = h('button', { type: 'button', class: 'button button--small', onclick: retryFailed }, ic('rotate'), 'Retry failed');
      const failedHead = h('div', { class: 'subhead' }, h('span', { 'data-failed-title': '' }), retryButton);
      const failedList = h('ul', { class: 'fileList fileList--failed', 'aria-label': 'Failed files' });
      const failed = h('section', { class: 'meter' }, failedHead, failedList);

      const summary = h('p', { class: 'summary' });
      const cancelButton = h('button', { type: 'button', class: 'button button--danger button--small', onclick: cancel }, 'Cancel upload');
      const doneButton = h('button', { type: 'button', class: 'button button--small', onclick: close }, 'Close');
      const foot = h('div', { class: 'uploadDock__foot' }, cancelButton, doneButton);

      const head = h('div', { class: 'uploadDock__head' }, stateIcon, h('div', { class: 'uploadDock__titles' }, title, dest), collapseButton, closeButton);
      const body = h('div', { class: 'uploadDock__body' }, notice, meter, active, failed, summary, foot);
      host.replaceChildren(head, mini, body, live);
      host.hidden = false;
      view = {
        live, stateIcon, title, dest, collapseButton, closeButton, mini: mini.firstChild, notice, bar, stats,
        activeHead, activeList, active, failed, failedTitle: failedHead.querySelector('[data-failed-title]'), failedList,
        retryButton, summary, cancelButton, doneButton, rows: [], stateName: '', failedSignature: '', noticeSignature: ''
      };
    }

    function schedule() {
      if (renderTimer) return;
      const wait = Math.max(0, RENDER_MS - (Date.now() - lastRender));
      renderTimer = setTimeout(() => {
        renderTimer = 0;
        render(true);
      }, wait);
    }

    function stateName(current) {
      if (current.blocked === 'offline') return 'offline';
      if (current.blocked === 'signin') return 'failed';
      if (!current.endedAt) return 'running';
      if (current.canceling || (current.canceledCount && !current.doneCount)) return 'canceled';
      return current.failed.length ? 'failed' : 'done';
    }

    function render(force) {
      const current = batch;
      if (!current) return;
      if (!force) return schedule();
      clearTimeout(renderTimer);
      renderTimer = 0;
      lastRender = Date.now();
      if (!view) build();
      sample(current);

      const total = current.entries.length;
      const settled = current.doneCount + current.failed.length + current.canceledCount;
      const sent = sentBytes(current);
      const ratio = current.totalBytes > 0 ? sent / current.totalBytes : (total ? settled / total : 0);
      const speed = rate(current);
      const remaining = Math.max(0, current.totalBytes - sent);
      const state = stateName(current);
      const ended = Boolean(current.endedAt);

      host.dataset.state = state;
      host.classList.toggle('is-collapsed', collapsed);
      if (view.stateName !== state) {
        view.stateName = state;
        const iconName = { running: 'upload', offline: 'cloud-off', failed: 'alert-triangle', canceled: 'circle-x', done: 'circle-check' }[state];
        view.stateIcon.replaceChildren(ic(iconName));
      }
      view.collapseButton.replaceChildren(ic(collapsed ? 'maximize' : 'minimize'));
      view.collapseButton.setAttribute('aria-label', collapsed ? 'Show the upload details' : 'Hide the upload details');
      view.collapseButton.title = view.collapseButton.getAttribute('aria-label');
      view.closeButton.hidden = !ended;

      const titles = {
        running: `Uploading ${count(total, 'file')}`,
        offline: `Upload paused at ${current.doneCount.toLocaleString('en-US')} of ${count(total, 'file')}`,
        failed: current.blocked === 'signin' ? 'Upload paused: sign-in ended' : `${current.failed.length.toLocaleString('en-US')} of ${count(total, 'file')} failed`,
        canceled: 'Upload canceled',
        done: `${count(current.doneCount, 'file')} uploaded`
      };
      setText(view.title, titles[state]);
      const destination = current.destination;
      if (!view.dest.firstChild) {
        view.dest.append('into ', h('strong', {}, destination.label), ...(destination.prefix ? [' / ', h('code', {}, destination.prefix)] : [', root']));
        view.dest.title = `${destination.label} / ${destination.prefix || ''}`;
      }

      // Notice: one message for an unreachable server or an ended sign-in.
      let noticeSignature = '';
      if (current.blocked === 'offline') {
        const seconds = current.probeAt ? Math.max(0, Math.ceil((current.probeAt - Date.now()) / 1000)) : 0;
        noticeSignature = `offline:${current.queue.length}:${seconds}`;
        if (view.noticeSignature !== noticeSignature) {
          view.notice.className = 'callout callout--warn';
          view.notice.replaceChildren(ic('cloud-off'),
            h('span', { class: 'callout__text' },
              h('strong', {}, 'The server does not answer. '),
              `${count(current.queue.length, 'file')} wait in the queue and nothing is lost. `,
              seconds ? `Next try in ${seconds} s.` : 'Trying now.'),
            h('button', { type: 'button', class: 'button button--small', onclick: () => probe(current) }, 'Try now'));
        }
        if (current.probeAt) schedule();
      } else if (current.blocked === 'signin') {
        noticeSignature = `signin:${current.queue.length}`;
        if (view.noticeSignature !== noticeSignature) {
          view.notice.className = 'callout callout--danger';
          view.notice.replaceChildren(ic('lock'),
            h('span', { class: 'callout__text' },
              h('strong', {}, 'Your sign-in has ended. '),
              `${count(current.queue.length, 'file')} wait. Sign in again in another tab, then continue.`),
            h('button', { type: 'button', class: 'button button--small', onclick: () => resume(current) }, 'Continue'));
        }
      }
      if (view.noticeSignature !== noticeSignature) {
        view.noticeSignature = noticeSignature;
        view.notice.hidden = !noticeSignature;
      }

      // Overall progress.
      setWidth(view.bar.firstChild, ratio);
      setWidth(view.mini, ratio);
      view.bar.setAttribute('aria-valuenow', String(Math.round(ratio * 100)));
      view.bar.classList.toggle('is-done', ended && !current.failed.length && !current.canceledCount);
      view.bar.classList.toggle('is-stalled', Boolean(current.blocked));
      const statNode = name => view.stats.querySelector(`[data-stat="${name}"]`);
      setText(statNode('files'), `${current.doneCount.toLocaleString('en-US')} / ${total.toLocaleString('en-US')}`);
      setText(statNode('bytes'), `${formatBytes(sent)} / ${formatBytes(current.totalBytes)}`);
      if (ended) {
        const seconds = Math.max(1, (current.endedAt - current.startedAt) / 1000);
        setText(statNode('rate'), `${formatBytes(current.doneBytes / seconds)}/s`);
        setText(statNode('eta'), `took ${formatDuration(seconds)}`);
      } else {
        setText(statNode('rate'), current.blocked ? 'paused' : (speed > 0 ? `${formatBytes(speed)}/s` : 'measuring'));
        setText(statNode('eta'), current.blocked ? 'waiting' : (speed > 0 ? formatDuration(remaining / speed) : 'measuring'));
      }

      // Files in progress: MAX_ACTIVE rows at most, reused.
      const running = Array.from(current.running);
      view.active.hidden = ended || (!running.length && !current.queue.length);
      const waiting = current.queue.length.toLocaleString('en-US');
      setText(view.activeHead, !running.length ? `Waiting ${waiting}`
        : (current.queue.length ? `In progress ${running.length}, waiting ${waiting}` : `In progress ${running.length}`));
      while (view.rows.length < running.length) {
        const name = h('span', { class: 'fileList__name' });
        const meta = h('span', { class: 'fileList__meta' });
        const rowBar = h('div', { class: 'progress progress--thin', 'aria-hidden': 'true' }, h('span'));
        const li = h('li', {}, name, meta, rowBar);
        view.rows.push({ li, name, meta, bar: rowBar });
      }
      view.activeList.hidden = !running.length;
      view.rows.forEach((row, index) => {
        const entry = running[index];
        if (!entry) {
          row.li.remove();
          return;
        }
        if (row.li.parentNode !== view.activeList) view.activeList.append(row.li);
        setText(row.name, entry.relative);
        row.name.title = entry.relative;
        const part = entry.size ? entry.uploadedBytes / entry.size : 0;
        setText(row.meta, entry.size ? `${Math.floor(part * 100)}% of ${formatBytes(entry.size)}` : formatBytes(0));
        setWidth(row.bar.firstChild, entry.size ? part : 1);
      });

      // Failed files: MAX_FAILED_ROWS rows at most; the list changes only when the failures change.
      const failedSignature = `${current.failed.length}:${current.failed[0]?.id || ''}`;
      view.failed.hidden = !current.failed.length;
      if (view.failedSignature !== failedSignature) {
        view.failedSignature = failedSignature;
        setText(view.failedTitle, `${count(current.failed.length, 'file')} failed`);
        const rows = current.failed.slice(0, MAX_FAILED_ROWS).map(entry => h('li', {},
          h('span', { class: 'fileList__name', title: entry.relative }, entry.relative),
          h('span', { class: 'fileList__reason' }, entry.reason)));
        if (current.failed.length > MAX_FAILED_ROWS) {
          rows.push(h('li', { class: 'fileList__more' }, `and ${count(current.failed.length - MAX_FAILED_ROWS, 'other file')}`));
        }
        view.failedList.replaceChildren(...rows);
      }
      view.retryButton.disabled = Boolean(current.blocked) || current.canceling;

      // Summary and buttons.
      view.summary.hidden = !ended;
      if (ended) {
        const parts = [`${count(current.doneCount, 'file')} uploaded (${formatBytes(current.doneBytes)})`];
        if (current.checksumCount) parts.push(`${current.checksumCount.toLocaleString('en-US')} checked with SHA-256`);
        if (current.failed.length) parts.push(`${current.failed.length.toLocaleString('en-US')} failed`);
        if (current.canceledCount) parts.push(`${current.canceledCount.toLocaleString('en-US')} canceled`);
        setText(view.summary, `${parts.join(', ')}.`);
      }
      view.cancelButton.hidden = ended;
      view.doneButton.hidden = !ended;
    }

    window.addEventListener('beforeunload', event => {
      if (!batch || finished(batch)) return;
      event.preventDefault();
      event.returnValue = '';
    });

    return {
      add,
      cancel,
      retryFailed,
      resume: () => resume(),
      get busy() { return Boolean(batch) && !finished(batch); },
      get batch() { return batch; }
    };
  }

  BB.uploads = { create, formatBytes, formatDuration, MAX_ACTIVE };
})();
