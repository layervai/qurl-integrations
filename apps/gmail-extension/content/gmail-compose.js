/**
 * qURL Gmail Extension — Gmail Compose Content Script
 *
 * Injected into Gmail pages. Listens for messages from the popup
 * and inserts qURL links into the active compose draft body.
 */

(function () {
  'use strict';

  // Prevent double injection. This guard is safe because MV3 content scripts run in an
  // isolated world — page scripts cannot observe or tamper with this property.
  if (window.__QURL_COMPOSE_INJECTED__) return;
  window.__QURL_COMPOSE_INJECTED__ = true;
  const INSERT_REQUEST_CACHE_TTL_MS = 30000;
  // Soft cap: prune is attempted past this, but completed entries younger than
  // INSERT_REQUEST_RETAIN_MS (below) are exempt. Because RETAIN equals the TTL, completed
  // entries are normally only removed by their TTL timer; under load the map rides up to the
  // hard cap (INSERT_REQUEST_PENDING_MAX_ENTRIES) rather than evicting entries a retry needs.
  const INSERT_REQUEST_CACHE_MAX_ENTRIES = 32;
  const INSERT_REQUEST_PENDING_MAX_ENTRIES = 64;
  // A completed entry must survive long enough that a same-requestId retry (SDK-level
  // sendRuntimeMessageWithRetry, which may fire only after the popup's message timeout)
  // replays the cached response instead of triggering a SECOND insertion. Below this age a
  // done entry is exempt from soft-cap eviction; the hard cap still bounds total memory.
  const INSERT_REQUEST_RETAIN_MS = 30000;
  const INSERT_REQUEST_PENDING_TIMEOUT_MS = 8000;
  const COMPOSE_BODY_DISCOVERY_TIMEOUT_MS = 4000;
  const COMPOSE_BODY_SELECTORS = [
    '.Am.Al.editable:not([contenteditable="false"])',
    '[role="dialog"] [role="textbox"][contenteditable="true"][aria-label]',
    '[role="textbox"][contenteditable="true"][g_editable="true"]',
  ];
  const insertRequestState = new Map();

  // ==================== Message Listener ====================

  chrome.runtime.onMessage.addListener(function (message, sender, sendResponse) {
    if (message.type === 'QURL_PING') {
      sendResponse({ success: true });
      return false;
    }

    if (message.type === 'INSERT_LINKS') {
      return handleInsertLinksMessage(message, sendResponse);
    }
    return false;
  });

  function handleInsertLinksMessage(message, sendResponse) {
    const results = message.results || [];
    if (results.length === 0) {
      sendResponse({ success: false, error: 'No results to insert' });
      return false;
    }

    const requestId = typeof message.requestId === 'string' ? message.requestId : '';
    if (!requestId) {
      // Backward-compatible path for callers that do not participate in request deduplication.
      insertLinksIntoGmailDraft(results, function (ok) {
        sendResponse({ success: ok });
      });
      return true;
    }

    const existing = insertRequestState.get(requestId);
    if (existing) {
      if (existing.status === 'done') {
        sendResponse(existing.response);
        return false;
      }

      existing.callbacks.push(sendResponse);
      return true;
    }

    insertRequestState.set(requestId, {
      callbacks: [sendResponse],
      cleanupTimerId: null,
      pendingTimerId: window.setTimeout(function () {
        finishTrackedInsertRequest(requestId, {
          success: false,
          error: getMessage(
            'compose_insert_timeout_error',
            'Timed out while waiting to insert links into Gmail.'
          ),
        });
      }, INSERT_REQUEST_PENDING_TIMEOUT_MS),
      response: null,
      status: 'pending',
    });
    pruneInsertRequestState();

    insertLinksIntoGmailDraft(results, function (ok) {
      finishTrackedInsertRequest(requestId, { success: ok });
    });
    return true;
  }

  function pruneInsertRequestState() {
    const now = Date.now();
    while (insertRequestState.size > INSERT_REQUEST_CACHE_MAX_ENTRIES) {
      let evictedRequestId = null;
      let evictedEntry = null;

      for (const [requestId, entry] of insertRequestState.entries()) {
        // Only evict completed entries old enough that any in-flight retry has settled.
        // Evicting a freshly-completed entry would let a retried requestId re-run insertion
        // and duplicate links in the draft — the exact case this cache exists to prevent.
        if (entry.status === 'done' && (now - (entry.doneAt || 0)) >= INSERT_REQUEST_RETAIN_MS) {
          evictedRequestId = requestId;
          evictedEntry = entry;
          break;
        }
      }

      // Keep pending requests alive until they settle so callers are not left waiting forever.
      // This means the cache can temporarily grow beyond INSERT_REQUEST_CACHE_MAX_ENTRIES if
      // all entries are still pending. The overflow is bounded by INSERT_REQUEST_PENDING_TIMEOUT_MS
      // (currently 8s), after which pending requests are forcibly marked done. In practice the
      // trust boundary (only extension UI can send INSERT_LINKS) limits misbehavior risk.
      //
      // However, enforce a hard cap (INSERT_REQUEST_PENDING_MAX_ENTRIES) to prevent unbounded
      // memory growth from a misbehaving caller flooding requests during the timeout window.
      if (!evictedRequestId) {
        if (insertRequestState.size > INSERT_REQUEST_PENDING_MAX_ENTRIES) {
          // Hard cap reached: evict the oldest entry (first in insertion order) regardless of
          // status or age. Memory safety wins here; reaching this requires a flood that the
          // INSERT_LINKS trust boundary already makes implausible for the legitimate popup.
          const oldestPending = insertRequestState.entries().next().value;
          if (oldestPending) {
            evictedRequestId = oldestPending[0];
            evictedEntry = oldestPending[1];
          }
        }
        if (!evictedRequestId) {
          // Between the soft and hard caps with only freshly-completed entries: let the map
          // grow rather than evict an entry a retry may still need.
          break;
        }
      }

      if (evictedEntry && evictedEntry.cleanupTimerId !== null) {
        window.clearTimeout(evictedEntry.cleanupTimerId);
      }
      if (evictedEntry && evictedEntry.pendingTimerId !== null) {
        window.clearTimeout(evictedEntry.pendingTimerId);
      }
      // Notify pending callbacks before eviction so callers receive immediate feedback
      // instead of hanging until their own timeout fires.
      if (evictedEntry && evictedEntry.status === 'pending' && evictedEntry.callbacks.length > 0) {
        const evictionError = {
          success: false,
          error: getMessage(
            'compose_insert_evicted_error',
            'Insertion is taking longer than expected — check the draft before retrying.'
          ),
        };
        evictedEntry.callbacks.forEach(function (callback) {
          try {
            callback(evictionError);
          } catch (err) {
            console.warn('[qURL] Eviction callback failed:', err);
          }
        });
      }
      insertRequestState.delete(evictedRequestId);
    }
  }

  function finishTrackedInsertRequest(requestId, response) {
    const entry = insertRequestState.get(requestId);
    if (!entry) {
      return;
    }

    entry.status = 'done';
    entry.response = response;
    entry.doneAt = Date.now();
    const callbacks = entry.callbacks.slice();
    entry.callbacks.length = 0;

    if (entry.pendingTimerId !== null) {
      window.clearTimeout(entry.pendingTimerId);
      entry.pendingTimerId = null;
    }

    callbacks.forEach(function (callback) {
      try {
        callback(response);
      } catch (err) {
        console.warn('[qURL] INSERT_LINKS callback failed:', err);
      }
    });

    if (entry.cleanupTimerId !== null) {
      window.clearTimeout(entry.cleanupTimerId);
    }

    // Completed requests are cached briefly so duplicate retries can receive the same response
    // even if the popup re-sends before this content-script instance is replaced.
    //
    // Race behavior note: if the pending timeout fires first, callbacks receive the timeout error.
    // When the real insertion completes later, it overwrites entry.response and resets the cleanup
    // timer. A subsequent retry with the same requestId during the TTL window will receive the
    // stale "success" reply. This is intentional caching behavior — the insertion did eventually
    // succeed, and returning that result is more accurate than a spurious timeout.
    //
    // IMPORTANT: This dedup cache only covers SDK-level retries (sendRuntimeMessageWithRetry),
    // which reuse the same requestId. User-initiated retries (clicking "Upload" again) generate
    // a new crypto.randomUUID() in popup.js, so they will NOT hit this cache and will trigger
    // a fresh insertion. If the user sees a timeout error but insertion actually succeeded,
    // a manual retry may result in duplicate links in the draft.
    entry.cleanupTimerId = window.setTimeout(function () {
      insertRequestState.delete(requestId);
    }, INSERT_REQUEST_CACHE_TTL_MS);
  }

  // ==================== Gmail Compose Body Finder ====================

  /**
   * Finds the active Gmail compose body element.
   * Gmail uses a contenteditable div with class "Am Al editable".
   *
   * @returns {HTMLElement|null}
   */
  function findComposeBody() {
    const focused = findVisibleComposeBody(document, true);
    if (focused) return focused;

    // When no compose body is focused, prefer the visible draft that appears topmost.
    // Gmail can keep multiple compose windows mounted at once, so DOM order alone is unreliable.
    const visible = findVisibleComposeBody(document, false);
    if (visible) return visible;

    return null;
  }

  /**
   * Finds the compose body, retrying until the DOM is ready.
   * @param {function} callback - called with (bodyElement or null)
   */
  function findComposeBodyAsync(callback) {
    const existing = findComposeBody();
    if (existing) {
      callback(existing);
      return;
    }

    const root = document.body || document.documentElement;
    if (!root) {
      document.addEventListener('DOMContentLoaded', function () {
        callback(findComposeBody());
      }, { once: true });
      return;
    }

    let settled = false;
    let observer = null;
    let timeoutId = null;

    function finish(body) {
      if (settled) return;
      settled = true;
      if (observer) {
        observer.disconnect();
      }
      window.clearTimeout(timeoutId);
      callback(body || null);
    }

    let lookupScheduled = false;
    const scheduleLookup = typeof window.requestAnimationFrame === 'function'
      ? window.requestAnimationFrame.bind(window)
      : function (fn) { return window.setTimeout(fn, 16); };
    function queueLookup() {
      if (lookupScheduled) {
        return;
      }
      lookupScheduled = true;
      scheduleLookup(function () {
        lookupScheduled = false;
        const body = findComposeBody();
        if (body) {
          finish(body);
        }
      });
    }

    observer = new MutationObserver(function () {
      queueLookup();
    });
    observer.observe(root, {
      childList: true,
      subtree: true,
    });
    queueLookup();

    timeoutId = window.setTimeout(function () {
      finish(findComposeBody());
    }, COMPOSE_BODY_DISCOVERY_TIMEOUT_MS);
  }

  /**
   * Checks if an element is visible in the viewport.
   * @param {HTMLElement} el
   * @returns {boolean}
   */
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden') return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  }

  function findVisibleComposeBody(root, focusedOnly) {
    const matches = [];
    const seen = new Set();

    for (const selector of COMPOSE_BODY_SELECTORS) {
      const query = focusedOnly ? `${selector}:focus` : selector;
      const selectorMatches = root.querySelectorAll(query);
      for (const match of selectorMatches) {
        if (seen.has(match)) {
          continue;
        }
        seen.add(match);
        if (isLikelyComposeBody(match) && isVisible(match)) {
          matches.push(match);
        }
      }
    }

    if (matches.length === 0) {
      return null;
    }

    if (focusedOnly || matches.length === 1) {
      return matches[0];
    }

    matches.sort(compareComposeBodies);
    return matches[0];
  }

  function compareComposeBodies(a, b) {
    const zIndexDelta = getComposeBodyZIndex(b) - getComposeBodyZIndex(a);
    if (zIndexDelta !== 0) {
      return zIndexDelta;
    }

    const aRect = a.getBoundingClientRect();
    const bRect = b.getBoundingClientRect();

    if (aRect.top !== bRect.top) {
      return aRect.top - bRect.top;
    }

    if (aRect.left !== bRect.left) {
      return aRect.left - bRect.left;
    }

    return (bRect.width * bRect.height) - (aRect.width * aRect.height);
  }

  function getComposeBodyZIndex(element) {
    const zIndex = Number(window.getComputedStyle(element).zIndex);
    return Number.isFinite(zIndex) ? zIndex : 0;
  }

  function isLikelyComposeBody(element) {
    if (!element || element.getAttribute('contenteditable') === 'false') {
      return false;
    }

    if (element.classList.contains('Am') && element.classList.contains('Al') && element.classList.contains('editable')) {
      return true;
    }

    if (element.getAttribute('role') === 'textbox' && element.getAttribute('contenteditable') === 'true') {
      return element.getAttribute('aria-multiline') === 'true' || Boolean(element.closest('[role="dialog"]'));
    }

    return false;
  }

  // ==================== Link HTML Builder ====================

  /**
   * Builds HTML content for insertion into the compose body.
   * Mirrors Helpers.buildUploadResultsHtml() from the Gmail Apps Script add-on.
   *
   * @param {Array<{filename: string, link: string, expiry: string|null}>} results
   * @returns {string}
   */
  function buildLinkHtml(results) {
    if (!results || results.length === 0) return '';
    if (window.QURLComposeFormatter) {
      // All compose HTML should be produced by the shared formatter so labels are escaped
      // and links are constrained to allowed http(s) URLs before insertion or clipboard copy.
      return window.QURLComposeFormatter.buildLinkHtml(results);
    }
    return '';
  }

  function placeCaretAtComposeEnd(composeBody) {
    if (!composeBody || typeof window.getSelection !== 'function' || typeof document.createRange !== 'function') {
      return null;
    }

    const selection = window.getSelection();
    if (!selection) {
      return null;
    }

    const range = document.createRange();
    range.selectNodeContents(composeBody);
    range.collapse(false);
    selection.removeAllRanges();
    selection.addRange(range);
    return selection;
  }

  // ==================== Draft Insertion ====================

  /**
   * Inserts QURL link HTML into the active Gmail compose draft.
   *
   * @param {Array<{filename: string, link: string, expiry: string|null}>} results
   * @param {function} callback - called with (boolean) success
   */
  function insertLinksIntoGmailDraft(results, callback) {
    findComposeBodyAsync(function (composeBody) {
      if (!composeBody) {
        console.warn('[qURL] Could not find Gmail compose body element.');
        showGmailNotification(getMessage(
          'compose_body_missing_error',
          'qURL: Could not find compose window. Please open a compose window and try again.'
        ));
        callback(false);
        return;
      }

      const html = buildLinkHtml(results);

      // Deprecated, but still the most reliable path into Gmail's contenteditable editor.
      composeBody.focus();
      const canPlaceCaret = Boolean(placeCaretAtComposeEnd(composeBody));

      const canInsertHtml = typeof document.queryCommandSupported === 'function'
        && document.queryCommandSupported('insertHTML');

      if (canInsertHtml && canPlaceCaret) {
        try {
          // HTML comes from buildLinkHtml(), which only emits escaped labels and safe links.
          const inserted = document.execCommand('insertHTML', false, html);
          if (inserted) {
            callback(true);
            return;
          }
        } catch (e) {
          console.warn('[qURL] execCommand insertHTML failed:', e);
        }
      }

      // Fallback: insert at the end of the editable div using Selection API
      try {
        const selection = placeCaretAtComposeEnd(composeBody);

        if (selection && selection.rangeCount > 0) {
          const insertRange = selection.getRangeAt(0);
          // HTML comes from buildLinkHtml(), which only emits escaped labels and safe links.
          const fragment = insertRange.createContextualFragment(html);
          const lastNode = fragment.lastChild;
          insertRange.insertNode(fragment);

          if (lastNode) {
            insertRange.setStartAfter(lastNode);
            insertRange.collapse(true);
            selection.removeAllRanges();
            selection.addRange(insertRange);
          }

          callback(true);
          return;
        }
      } catch (e) {
        console.warn('[qURL] Selection API insertion failed:', e);
      }

      // Last resort: append without reparsing existing Gmail DOM
      try {
        composeBody.insertAdjacentHTML('beforeend', html);
        callback(true);
      } catch (e) {
        console.warn('[qURL] innerHTML append failed:', e);
        showGmailNotification(getMessage(
          'compose_insert_failed_notification',
          'qURL: Failed to insert links. Please copy them manually from the popup.'
        ));
        callback(false);
      }
    });
  }

  // ==================== Notification ====================

  /**
   * Shows a notification banner in Gmail.
   * @param {string} message
   */
  function showGmailNotification(message) {
    // Try Gmail's native toast/notification area
    const toast = document.createElement('div');
    toast.setAttribute('role', 'alert');
    toast.setAttribute('aria-live', 'polite');
    toast.style.cssText = [
      'position:fixed',
      'top:16px',
      'right:16px',
      'z-index:10000',
      'background:#202124',
      'color:#fff',
      'padding:12px 20px',
      'border-radius:8px',
      'font-family:Google Sans,Segoe UI,sans-serif',
      'font-size:13px',
      'box-shadow:0 4px 12px rgba(0,0,0,0.25)',
      'max-width:320px',
      'line-height:1.4',
    ].join(';');

    toast.textContent = message;
    document.body.appendChild(toast);

    setTimeout(function () {
      toast.style.transition = 'opacity 0.3s';
      toast.style.opacity = '0';
      setTimeout(function () { toast.remove(); }, 300);
    }, 4000);
  }

  function getMessage(key, fallback, substitutions) {
    if (typeof QURLI18n !== 'undefined' && typeof QURLI18n.getMessage === 'function') {
      return QURLI18n.getMessage(key, fallback, substitutions);
    }
    return fallback || '';
  }

})();
