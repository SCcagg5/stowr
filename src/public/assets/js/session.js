/*!
 * BB.session: one clear message for the answers that concern the whole page.
 *
 * - 401 (the identity token is absent or not valid): the sign-in has ended. The page shows one notice with a
 *   "Reload" button: the access gateway then asks for a new sign-in.
 * - 503 with the code pdp_unavailable: the server works, but the access decision service does not answer. The page
 *   shows one notice with a "Try again" button. This is not "the server does not answer".
 * - No answer (a network error, or 502, 503 or 504 from a proxy): the server does not answer. The page shows one
 *   notice with a "Try again" button.
 * The module wraps window.fetch to see each answer of the server. It changes no request and no answer.
 * The upload engine (uploads.js) uses XMLHttpRequest and tells this module with BB.session.signInEnded(). While the
 * upload dock shows its own message for a server that does not answer, the page does not show a second one.
 */
(function () {
  'use strict';

  const BB = (window.BB = window.BB || {});
  const listeners = new Set();
  let signInEnded = false;
  let offline = false;
  let decisionDown = false;

  function notify() {
    for (const listener of listeners) {
      try { listener({ signInEnded, offline, decisionDown }); } catch (error) { console.error(error); }
    }
    render();
  }

  function sameOrigin(input) {
    try {
      const url = new URL(typeof input === 'string' ? input : input?.url || '', document.baseURI);
      return url.origin === window.location.origin;
    } catch (_) {
      return false;
    }
  }

  // A 503 of the server with the code pdp_unavailable: the access decision service gives no decision.
  async function isDecisionDown(response) {
    try {
      if (!(response.headers.get('Content-Type') || '').includes('application/json')) return false;
      const payload = await response.clone().json();
      return payload?.error?.code === 'pdp_unavailable';
    } catch (_) {
      return false;
    }
  }

  const nativeFetch = window.fetch.bind(window);
  window.fetch = async function sessionFetch(input, init) {
    const watched = sameOrigin(input);
    try {
      const response = await nativeFetch(input, init);
      if (watched) {
        if (response.status === 401) markSignInEnded();
        else if (response.status === 503 && await isDecisionDown(response)) {
          markDecisionDown(true);
          if (offline) markOffline(false);
        } else if ([502, 503, 504].includes(response.status)) {
          markOffline(true);
          if (decisionDown) markDecisionDown(false);
        } else {
          if (offline) markOffline(false);
          if (decisionDown) markDecisionDown(false);
        }
      }
      return response;
    } catch (error) {
      if (watched && error?.name !== 'AbortError') markOffline(true);
      throw error;
    }
  };

  function markSignInEnded() {
    if (signInEnded) return;
    signInEnded = true;
    notify();
  }

  function markOffline(value) {
    if (offline === Boolean(value)) return;
    offline = Boolean(value);
    notify();
  }

  function markDecisionDown(value) {
    if (decisionDown === Boolean(value)) return;
    decisionDown = Boolean(value);
    notify();
  }

  function host() {
    let node = document.getElementById('notices');
    if (!node) {
      node = document.createElement('div');
      node.id = 'notices';
      node.className = 'notices notices--floating';
      node.setAttribute('aria-live', 'polite');
      document.body.prepend(node);
    }
    return node;
  }

  function notice(kind, iconName, title, text, buttonLabel, onClick) {
    const box = document.createElement('div');
    box.className = `callout callout--${kind}`;
    box.dataset.notice = iconName;
    box.setAttribute('role', 'alert');
    const icon = document.createElement('i');
    icon.className = `ic ic-${iconName}`;
    icon.setAttribute('aria-hidden', 'true');
    const copy = document.createElement('span');
    copy.className = 'callout__text';
    const strong = document.createElement('strong');
    strong.textContent = `${title} `;
    copy.append(strong, text);
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'button button--small';
    button.textContent = buttonLabel;
    button.addEventListener('click', onClick);
    box.append(icon, copy, button);
    return box;
  }

  function render() {
    const node = host();
    const items = [];
    if (signInEnded) {
      items.push(notice('danger', 'lock', 'Your sign-in has ended.',
        'Reload the page to sign in again. The work in this tab stops until then.', 'Reload', () => window.location.reload()));
    } else if (offline && !(typeof BB.session.ownsOffline === 'function' && BB.session.ownsOffline())) {
      items.push(notice('warn', 'cloud-off', 'The server does not answer.',
        'Check the connection, then try again.', 'Try again', () => {
          markOffline(false);
          document.dispatchEvent(new CustomEvent('bb:retry'));
        }));
    } else if (decisionDown) {
      items.push(notice('warn', 'lock', 'The access decision service does not answer.',
        'Try again later.', 'Try again', () => {
          markDecisionDown(false);
          document.dispatchEvent(new CustomEvent('bb:retry'));
        }));
    }
    node.replaceChildren(...items);
  }

  BB.session = {
    // The upload dock sets ownsOffline: while it shows its own message, the page does not show a second one.
    ownsOffline: null,
    refresh: render,
    onChange(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    signInEnded: markSignInEnded,
    get state() { return { signInEnded, offline, decisionDown }; }
  };
})();
