const test = require('node:test');
const assert = require('node:assert/strict');

const popupModulePath = require.resolve('../popup/popup.js');

const originalGlobals = {
  Blob: global.Blob,
  ClipboardItem: global.ClipboardItem,
  chrome: global.chrome,
  confirm: global.confirm,
  document: global.document,
  getStoredQurlApiBase: global.getStoredQurlApiBase,
  isDefaultQurlOrigin: global.isDefaultQurlOrigin,
  navigator: global.navigator,
  normalizeQurlApiBase: global.normalizeQurlApiBase,
  QURLI18n: global.QURLI18n,
  QURLComposeFormatter: global.QURLComposeFormatter,
  requestQurlHostPermission: global.requestQurlHostPermission,
  setStoredQurlApiBase: global.setStoredQurlApiBase,
  uploadFile: global.uploadFile,
  window: global.window,
};

function createClassList() {
  const classes = new Set(['hidden']);
  return {
    add(name) {
      classes.add(name);
    },
    contains(name) {
      return classes.has(name);
    },
    remove(name) {
      classes.delete(name);
    },
    toggle(name, force) {
      if (force === undefined) {
        if (classes.has(name)) {
          classes.delete(name);
          return false;
        }
        classes.add(name);
        return true;
      }
      if (force) {
        classes.add(name);
      } else {
        classes.delete(name);
      }
      return force;
    },
  };
}

function createElement(tagName) {
  const listeners = new Map();
  const element = {
    addEventListener(type, handler) {
      if (!listeners.has(type)) {
        listeners.set(type, []);
      }
      listeners.get(type).push(handler);
    },
    append(...nodes) {
      this.children.push(...nodes);
    },
    appendChild(node) {
      this.children.push(node);
      return node;
    },
    children: [],
    classList: createClassList(),
    click() {},
    closest() {
      return null;
    },
    dataset: {},
    disabled: false,
    files: [],
    focus() {},
    innerHTML: '',
    querySelectorAll() {
      return [];
    },
    removeEventListener(type, handler) {
      if (!listeners.has(type)) {
        return;
      }
      listeners.set(type, listeners.get(type).filter(function (candidate) {
        return candidate !== handler;
      }));
    },
    select() {},
    setAttribute(name, value) {
      this[name] = value;
    },
    async trigger(type, event) {
      const handlers = listeners.get(type) || [];
      for (const handler of handlers) {
        await handler(Object.assign({
          currentTarget: this,
          preventDefault() {},
          stopPropagation() {},
          target: this,
        }, event));
      }
    },
    value: '',
  };
  element.tagName = String(tagName || 'div').toUpperCase();
  let innerHTML = '';
  let textContent = '';
  Object.defineProperty(element, 'innerHTML', {
    get() {
      return innerHTML;
    },
    set(value) {
      innerHTML = String(value);
      textContent = '';
      element.children = [];
    },
    configurable: true,
  });
  Object.defineProperty(element, 'textContent', {
    get() {
      return textContent;
    },
    set(value) {
      textContent = String(value);
      innerHTML = '';
      element.children = [];
    },
    configurable: true,
  });
  return element;
}

function loadPopup(sendMessageImpl, timerImpl, options) {
  delete require.cache[popupModulePath];
  const resolvedOptions = options || {};

  const elements = new Map();
  [
    'fileInput',
    'selectBtn',
    'fileCount',
    'fileList',
    'uploadBtn',
    'progressArea',
    'resultArea',
    'errorArea',
    'settingsBtn',
    'settingsPanel',
    'settingsCloseBtn',
    'apiBaseInput',
    'saveConfigBtn',
    'resetConfigBtn',
    'permissionConfirmPanel',
    'permissionConfirmText',
    'permissionConfirmContinueBtn',
    'permissionConfirmCancelBtn',
    'configHint',
    'copyArea',
    'copyBtn',
  ].forEach(function (id) {
    elements.set(id, createElement());
  });

  const footer = createElement();
  const i18nElements = resolvedOptions.i18nElements || [];
  const i18nAttrElements = resolvedOptions.i18nAttrElements || [];

  global.document = {
    addEventListener() {},
    createElement,
    execCommand() {
      return true;
    },
    getElementById(id) {
      return elements.get(id);
    },
    querySelector(selector) {
      return selector === '.footer' ? footer : null;
    },
    querySelectorAll(selector) {
      if (selector === '[data-i18n]') {
        return i18nElements;
      }
      if (selector === '[data-i18n-attr]') {
        return i18nAttrElements;
      }
      return [];
    },
    removeEventListener() {},
    title: 'Popup',
  };

  global.chrome = {
    i18n: {
      getMessage(key) {
        return (resolvedOptions.chromeMessages && resolvedOptions.chromeMessages[key]) || '';
      },
    },
    runtime: {
      sendMessage: sendMessageImpl,
    },
  };
  global.QURLI18n = {
    getMessage(key, fallback, substitutions) {
      const template = (resolvedOptions.chromeMessages && resolvedOptions.chromeMessages[key]) || fallback || '';
      return String(template).replace(/\$(\d+)/g, function (match, rawIndex) {
        const index = Number(rawIndex) - 1;
        return substitutions && substitutions[index] !== undefined ? String(substitutions[index]) : match;
      });
    },
  };

  global.getStoredQurlApiBase = async function () {
    return resolvedOptions.getStoredQurlApiBase ? resolvedOptions.getStoredQurlApiBase() : null;
  };
  global.setStoredQurlApiBase = async function () {
    return resolvedOptions.setStoredQurlApiBase ? resolvedOptions.setStoredQurlApiBase() : null;
  };
  global.requestQurlHostPermission = async function (value) {
    return resolvedOptions.requestQurlHostPermission ? resolvedOptions.requestQurlHostPermission(value) : true;
  };
  global.uploadFile = resolvedOptions.uploadFile || async function () {
    throw new Error('uploadFile should not run in popup helper tests');
  };
  global.normalizeQurlApiBase = resolvedOptions.normalizeQurlApiBase || function (value) {
    return value ? String(value).trim().replace(/\/+$/, '') : null;
  };
  global.isDefaultQurlOrigin = resolvedOptions.isDefaultQurlOrigin || function (value) {
    return value === 'https://getqurllink.layerv.ai';
  };
  global.QURLComposeFormatter = {
    // Mirrors the real formatter: empty for a missing/invalid expiry, " (Expires: …)" otherwise.
    buildExpirySuffix(expiry) {
      return expiry ? ` (Expires: ${expiry})` : '';
    },
    buildLinkHtml() {
      return '';
    },
    buildLinkPlainText() {
      return '';
    },
    escapeHtml(str) {
      if (!str) return '';
      return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    },
    formatExpiry() {
      return null;
    },
    normalizeAllowedLink() {
      return null;
    },
  };
  global.navigator = {};
  global.ClipboardItem = undefined;
  global.window = global;
  global.window.confirm = resolvedOptions.confirm || function () {
    return true;
  };
  global.window.setTimeout = timerImpl.setTimeout;
  global.window.clearTimeout = timerImpl.clearTimeout;

  const popup = require('../popup/popup.js');
  popup.__testElements = elements;
  return popup;
}

test.afterEach(function () {
  delete require.cache[popupModulePath];
  Object.keys(originalGlobals).forEach(function (key) {
    global[key] = originalGlobals[key];
  });
});

test('sendRuntimeMessageWithTimeout clears the timeout when sendMessage succeeds', async function () {
  const cleared = [];
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout(_callback, _delay) {
        return 77;
      },
      clearTimeout(id) {
        cleared.push(id);
      },
    }
  );

  const response = await popup.sendRuntimeMessageWithTimeout({ type: 'PING' }, 4000);

  assert.deepEqual(response, { success: true });
  assert.deepEqual(cleared, [77]);
});

test('sendRuntimeMessageWithRetry retries retryable runtime failures once before succeeding', async function () {
  const sendAttempts = [];
  const timerCalls = [];
  let attempt = 0;
  const popup = loadPopup(
    function () {
      sendAttempts.push(attempt);
      if (attempt === 0) {
        attempt += 1;
        return Promise.reject(new Error('transient failure'));
      }
      return Promise.resolve({ success: true });
    },
    {
      setTimeout(callback, delay) {
        timerCalls.push(delay);
        if (delay === 250) {
          callback();
        }
        return delay;
      },
      clearTimeout() {},
    }
  );

  const response = await popup.sendRuntimeMessageWithRetry({ type: 'PING' }, 2);

  assert.deepEqual(response, { success: true });
  assert.equal(sendAttempts.length, 2);
  assert.equal(timerCalls.filter(function (delay) { return delay === 250; }).length, 1);
});

test('sendRuntimeMessageWithRetry does not retry non-retryable timeout failures', async function () {
  let sendAttempts = 0;
  const popup = loadPopup(
    function () {
      sendAttempts += 1;
      return new Promise(function () {});
    },
    {
      setTimeout(callback, delay) {
        if (delay === popup.RUNTIME_MESSAGE_TIMEOUT_MS) {
          callback();
        }
        return delay;
      },
      clearTimeout() {},
    }
  );

  await assert.rejects(
    popup.sendRuntimeMessageWithRetry({ type: 'PING' }, 2),
    function (err) {
      assert.equal(err.qurlRetryable, false);
      assert.equal(err.qurlErrorCode, 'timeout');
      return true;
    }
  );
  assert.equal(sendAttempts, 1);
});

test('applyLocalizedText populates text and attributes from i18n keys', function () {
  const label = createElement();
  label.dataset.i18n = 'upload_btn';

  const titled = createElement();
  titled.dataset.i18n = 'file_remove_label';
  titled.dataset.i18nAttr = 'title, aria-label';

  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        ext_name: 'Localized Popup',
        upload_btn: 'Upload now',
        file_remove_label: 'Remove file',
      },
      i18nElements: [label],
      i18nAttrElements: [titled],
    }
  );

  popup.applyLocalizedText();

  assert.equal(global.document.title, 'Localized Popup');
  assert.equal(label.textContent, 'Upload now');
  assert.equal(titled.title, 'Remove file');
  assert.equal(titled['aria-label'], 'Remove file');
});

test('applyLocalizedText supports a separate i18n key for attribute localization', function () {
  const attributed = createElement();
  attributed.dataset.i18nAttr = 'title';
  attributed.dataset.i18nAttrKey = 'settings_label';

  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        ext_name: 'Localized Popup',
        settings_label: 'Settings',
      },
      i18nAttrElements: [attributed],
    }
  );

  popup.applyLocalizedText();

  assert.equal(attributed.title, 'Settings');
});

test('resetting the custom server keeps the settings panel open for immediate re-entry', async function () {
  const timerCalls = [];
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout(_callback, delay) {
        timerCalls.push(delay);
        return delay;
      },
      clearTimeout() {},
    },
    {
      setStoredQurlApiBase: async function () {
        return null;
      },
    }
  );

  const settingsPanel = popup.__testElements.get('settingsPanel');
  const resetButton = popup.__testElements.get('resetConfigBtn');
  settingsPanel.classList.remove('hidden');

  await resetButton.trigger('click');

  assert.equal(settingsPanel.classList.contains('hidden'), false);
  assert.equal(timerCalls.includes(1200), false);
});

test('saving a custom server shows an inline confirmation before requesting origin access', async function () {
  let setStoredCalled = false;
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      isDefaultQurlOrigin() {
        return false;
      },
      normalizeQurlApiBase(value) {
        return String(value).trim().replace(/\/api\/upload$/, '');
      },
      setStoredQurlApiBase: async function () {
        setStoredCalled = true;
        return 'https://custom.example.com';
      },
    }
  );

  const apiBaseInput = popup.__testElements.get('apiBaseInput');
  const saveButton = popup.__testElements.get('saveConfigBtn');
  const confirmPanel = popup.__testElements.get('permissionConfirmPanel');
  const confirmText = popup.__testElements.get('permissionConfirmText');
  await Promise.resolve();
  apiBaseInput.value = 'https://custom.example.com/api/upload';

  await saveButton.trigger('click');

  assert.equal(setStoredCalled, false);
  assert.equal(confirmPanel.classList.contains('hidden'), false);
  assert.equal(confirmText.textContent, 'Allow the extension to access https://custom.example.com for qURL uploads? Edge will show a permission prompt next.');
});

test('saving an invalid custom server surfaces the validation error inline', async function () {
  let setStoredCalled = false;
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      normalizeQurlApiBase() {
        throw new Error('qURL server URL must start with https://');
      },
      setStoredQurlApiBase: async function () {
        setStoredCalled = true;
        return null;
      },
    }
  );

  const apiBaseInput = popup.__testElements.get('apiBaseInput');
  const saveButton = popup.__testElements.get('saveConfigBtn');
  const configHint = popup.__testElements.get('configHint');
  const confirmPanel = popup.__testElements.get('permissionConfirmPanel');
  await Promise.resolve();
  apiBaseInput.value = 'http://custom.example.com';

  await saveButton.trigger('click');

  assert.equal(setStoredCalled, false);
  assert.equal(configHint.textContent, 'qURL server URL must start with https://');
  assert.equal(configHint.classList.contains('error'), true);
  assert.equal(confirmPanel.classList.contains('hidden'), true);
});

test('continuing the inline custom-server confirmation proceeds with saving', async function () {
  let setStoredCalled = false;
  let requestedOrigin = null;
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      isDefaultQurlOrigin() {
        return false;
      },
      normalizeQurlApiBase(value) {
        return String(value).trim().replace(/\/api\/upload$/, '');
      },
      requestQurlHostPermission(value) {
        requestedOrigin = value;
        return Promise.resolve(true);
      },
      setStoredQurlApiBase: async function () {
        setStoredCalled = true;
        return 'https://custom.example.com';
      },
    }
  );

  const apiBaseInput = popup.__testElements.get('apiBaseInput');
  const saveButton = popup.__testElements.get('saveConfigBtn');
  const continueButton = popup.__testElements.get('permissionConfirmContinueBtn');
  const confirmPanel = popup.__testElements.get('permissionConfirmPanel');
  await Promise.resolve();
  apiBaseInput.value = 'https://custom.example.com/api/upload';

  await saveButton.trigger('click');
  await continueButton.trigger('click');

  assert.equal(requestedOrigin, 'https://custom.example.com');
  assert.equal(setStoredCalled, true);
  assert.equal(confirmPanel.classList.contains('hidden'), true);
});

test('denied custom-server permission does not persist the override', async function () {
  let setStoredCalled = false;
  let requestedOrigin = null;
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      isDefaultQurlOrigin() {
        return false;
      },
      normalizeQurlApiBase(value) {
        return String(value).trim().replace(/\/api\/upload$/, '');
      },
      requestQurlHostPermission(value) {
        requestedOrigin = value;
        return Promise.resolve(false);
      },
      setStoredQurlApiBase: async function () {
        setStoredCalled = true;
        return 'https://custom.example.com';
      },
    }
  );

  const apiBaseInput = popup.__testElements.get('apiBaseInput');
  const saveButton = popup.__testElements.get('saveConfigBtn');
  const continueButton = popup.__testElements.get('permissionConfirmContinueBtn');
  const configHint = popup.__testElements.get('configHint');
  await Promise.resolve();
  apiBaseInput.value = 'https://custom.example.com/api/upload';

  await saveButton.trigger('click');
  await continueButton.trigger('click');

  assert.equal(requestedOrigin, 'https://custom.example.com');
  assert.equal(setStoredCalled, false);
  assert.equal(configHint.textContent, 'Permission to access this qURL server was not granted.');
  assert.equal(configHint.classList.contains('error'), true);
});

test('formatFileSize uses a GB tier for large files', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  assert.equal(popup.formatFileSize(2 * 1024 * 1024 * 1024), '2.0 GB');
});

test('buildCopyUrlText copies only accessible https URLs', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    if (String(link).startsWith('https://')) {
      return String(link);
    }
    return null;
  };

  assert.equal(
    popup.buildCopyUrlText([
      { filename: 'report.pdf', link: 'https://files.example.com/a' },
      { filename: 'bad.txt', link: 'http://files.example.com/b' },
      { filename: 'notes.txt', link: 'https://files.example.com/c' },
    ]),
    'https://files.example.com/a\nhttps://files.example.com/c'
  );
});

test('buildCopyUrlHtml returns escaped anchor tags joined with breaks', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        ext_name: 'Popup',
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  assert.equal(
    popup.buildCopyUrlHtml([
      { link: 'https://files.example.com/a?x=1&y=<two>' },
      { link: 'http://files.example.com/b' },
      { link: 'https://files.example.com/c' },
    ]),
    '<a href="https://files.example.com/a?x=1&amp;y=&lt;two&gt;">https://files.example.com/a?x=1&amp;y=&lt;two&gt;</a><br><a href="https://files.example.com/c">https://files.example.com/c</a>'
  );
});

test('showResults uses insertion-aware success summaries', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        result_n_inserted: 'Inserted $1 qURL links into your Gmail draft',
        result_n_uploaded: '$1 files uploaded successfully',
        result_insertion_only_failed: 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL link.',
        result_insertion_only_failed_plural: 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL links.',
        result_insertion_only_failed_no_copy: 'Couldn\'t insert into your Gmail draft, and no accessible qURL link is available to copy.',
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    null
  );

  const successSummary = popup.__testElements.get('resultArea').children[0];
  assert.equal(
    successSummary.textContent,
    'Inserted 2 qURL links into your Gmail draft'
  );
  assert.equal(successSummary.className, 'result-summary all-success');

  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    'Active tab is not Gmail.'
  );

  const resultArea = popup.__testElements.get('resultArea');
  const errorArea = popup.__testElements.get('errorArea');
  const uploadOnlySummary = resultArea.children[0];

  assert.equal(
    uploadOnlySummary.textContent,
    '2 files uploaded successfully'
  );
  assert.equal(uploadOnlySummary.className, 'result-summary partial');
  assert.equal(errorArea.children[0].textContent, 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL links.');
});

test('showResults withholds the copy fallback when no accessible links are available', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function () {
    return null;
  };

  popup.showResults(
    [
      { filename: 'a.txt', link: 'http://files.example.com/a', expiry: null },
    ],
    [],
    'Active tab is not Gmail.'
  );

  // Nothing is copyable, so the fallback is withheld rather than shown greyed out.
  assert.equal(popup.__testElements.get('copyArea').classList.contains('hidden'), true);
  assert.equal(popup.__testElements.get('copyBtn').disabled, true);
  assert.equal(
    popup.__testElements.get('errorArea').children[0].textContent,
    'Couldn\'t insert into your Gmail draft, and no accessible qURL link is available to copy.'
  );

  // The same holds on the green path the review flagged: uploads and insertion both fine, but
  // no result carries an https link, so there is no dead button under the success banner.
  popup.showResults(
    [{ filename: 'a.txt', link: 'http://files.example.com/a', expiry: null }],
    [],
    null
  );
  assert.equal(popup.__testElements.get('copyArea').classList.contains('hidden'), true);
  assert.equal(popup.__testElements.get('copyBtn').disabled, true);
});

test('RUNTIME_MESSAGE_TIMEOUT_MS leaves enough budget for the background relay', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  // Assert the documented budget inequality against background's own constants so the chain
  // can't silently drift: popup budget must exceed the fixed relay legs (two pings + the
  // INSERT_LINKS relay) with real headroom left over for the cold-tab content-script reinject.
  const background = require('../background.js');
  const fixedLegs = (2 * background.TAB_MESSAGE_TIMEOUT_MS) + background.INSERT_LINKS_TAB_MESSAGE_TIMEOUT_MS;
  assert.ok(
    popup.RUNTIME_MESSAGE_TIMEOUT_MS > fixedLegs,
    `popup budget ${popup.RUNTIME_MESSAGE_TIMEOUT_MS} must exceed fixed relay legs ${fixedLegs}`
  );
  // At least a few seconds of reinject headroom (executeScript on a cold tab loads three files).
  assert.ok(popup.RUNTIME_MESSAGE_TIMEOUT_MS - fixedLegs >= 5000);
});

test('getMessage fallbacks stay in sync with _locales/en/messages.json', function () {
  // popup.js passes an inline English fallback to every getMessage call, for the non-extension
  // contexts where chrome.i18n is absent. Edge itself always serves messages.json, so drift
  // between the two is invisible at runtime and has silently crept in before — the fallbacks
  // kept the pre-rename "N file(s) uploaded successfully" copy after messages.json moved on.
  // Check every call site rather than the handful that changed most recently.
  const fs = require('node:fs');
  const messages = require('../_locales/en/messages.json');
  const source = fs.readFileSync(popupModulePath, 'utf8');

  // Resolve a messages.json entry the way getMessage's fallback path expects it: named
  // placeholders ($COUNT$) collapse to the positional form the fallback spells out ($1).
  // Edge matches placeholder names case-insensitively, so "$origin$" resolves too.
  function resolveMessage(key) {
    const entry = messages[key];
    if (!entry) return null;
    return Object.entries(entry.placeholders || {}).reduce(
      function (text, [name, placeholder]) {
        return text.replace(new RegExp('\\$' + name + '\\$', 'gi'), placeholder.content);
      },
      entry.message
    );
  }

  const callPattern = /getMessage\(\s*'([a-z0-9_]+)'\s*,\s*'((?:[^'\\]|\\.)*)'/g;
  const checked = [];
  let match;
  while ((match = callPattern.exec(source)) !== null) {
    const key = match[1];
    const fallback = match[2].replace(/\\'/g, "'");
    const expected = resolveMessage(key);

    assert.ok(expected !== null, `getMessage('${key}', …) has no _locales/en/messages.json entry`);
    assert.equal(
      fallback,
      expected,
      `inline fallback for '${key}' drifted from messages.json`
    );
    checked.push(key);
  }

  // Guard the guard: a pattern that stops matching would otherwise pass vacuously. Rather than
  // a hard-coded floor (which unrelated churn in the number of call sites would trip), account
  // for every getMessage( occurrence in the file: each one is either checked above or is a known
  // site with no literal fallback to compare — the function's own definition, its delegation to
  // QURLI18n, the ext_name/document.title call, and applyLocalizedText's runtime-key lookups.
  // Adding a call site of either kind keeps the arithmetic balanced or fails loudly here.
  const totalCallSites = (source.match(/getMessage\(/g) || []).length;
  const nonLiteralCallSites = (source.match(
    /function getMessage\(|QURLI18n\.getMessage\(|getMessage\('ext_name', document\.title\)|getMessage\(key, ''\)/g
  ) || []).length;
  assert.equal(
    checked.length + nonLiteralCallSites,
    totalCallSites,
    `getMessage call sites do not add up: ${checked.length} checked + ${nonLiteralCallSites} exempt != ${totalCallSites} total. `
    + 'If you added a call site with a literal fallback, callPattern should have matched it — check the pattern still fits. '
    + 'It only matches single-quoted fallbacks: a double-quoted literal slips past it, so write copy containing an '
    + 'apostrophe as a single-quoted string with a backslash escape. '
    + 'If you added one without a literal fallback (a runtime key, a non-literal fallback), add its shape to nonLiteralCallSites.'
  );
  assert.ok(checked.includes('result_one_inserted') && checked.includes('result_n_inserted'));
});

test('showResults styles an insertion-only failure as a notice, not an error', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const results = [{ filename: 'a.txt', link: 'https://files.example.com/a', expiry: null }];
  const errorArea = popup.__testElements.get('errorArea');

  // Uploads all fine, only the Gmail insertion failed: the box names that failure but pairs it
  // with a pointer at the copy fallback, so it must not render in the error-red styling.
  popup.showResults(results, [], 'Active tab is not Gmail.');
  assert.equal(errorArea.classList.contains('notice'), true);

  // A real upload failure alongside the insertion failure is an error again.
  popup.showResults(results, [{ filename: 'b.txt', error: 'boom' }], 'Active tab is not Gmail.');
  assert.equal(errorArea.classList.contains('notice'), false);

  // Insertion failed and nothing is copyable: the popup offers no way left to reach the
  // upload, so this is an error state no matter how well the upload itself went.
  popup.showResults(
    [{ filename: 'a.txt', link: 'http://files.example.com/a', expiry: null }],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(errorArea.classList.contains('notice'), false);

  // And the modifier must not survive into the next run.
  popup.showResults(results, [], 'Active tab is not Gmail.');
  assert.equal(errorArea.classList.contains('notice'), true);
  popup.showResults(results, [], null);
  assert.equal(errorArea.classList.contains('notice'), false);
});

test('showResults labels the copy button for the number of links it copies', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        copy_btn: 'Copy the qURL link',
        copy_btn_plural: 'Copy the qURL links',
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const copyBtn = popup.__testElements.get('copyBtn');

  popup.showResults([{ filename: 'a.txt', link: 'https://files.example.com/a', expiry: null }], [], null);
  assert.equal(copyBtn.textContent, 'Copy the qURL link');

  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    null
  );
  assert.equal(copyBtn.textContent, 'Copy the qURL links');

  // Two results but only one is an accessible https link — the label follows the copyable
  // count, not the result count.
  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'http://files.example.com/b', expiry: null },
    ],
    [],
    null
  );
  assert.equal(copyBtn.textContent, 'Copy the qURL link');
});

test('the copy button keeps the plural label after the "Copied" text reverts', async function () {
  // The revert fires ~1.5s after a click. Capture the callback instead of waiting on a timer.
  let revert = null;
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout(callback) {
        revert = callback;
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        copy_btn: 'Copy the qURL link',
        copy_btn_plural: 'Copy the qURL links',
        copy_done: 'Copied',
      },
      uploadFile: async function (buffer, filename) {
        return { success: true, qurl_link: `https://files.example.com/${filename}` };
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const fileInput = popup.__testElements.get('fileInput');
  const uploadBtn = popup.__testElements.get('uploadBtn');
  const copyBtn = popup.__testElements.get('copyBtn');

  function fakeFile(name) {
    return {
      name,
      size: 10,
      type: 'text/plain',
      arrayBuffer: async function () {
        return new ArrayBuffer(10);
      },
    };
  }

  fileInput.files = [fakeFile('a.txt'), fakeFile('b.txt')];
  await fileInput.trigger('change');
  await uploadBtn.trigger('click');

  // Two accessible links, so the rendered label is plural.
  assert.equal(copyBtn.textContent, 'Copy the qURL links');

  await copyBtn.trigger('click');
  assert.equal(copyBtn.textContent, 'Copied');

  assert.ok(revert, 'clicking copy should schedule a label revert');
  revert();

  // The revert must follow the same count the render used, not fall back to the singular label
  // while both links are still copyable.
  assert.equal(copyBtn.textContent, 'Copy the qURL links');
});

test('the success summary counts links actually inserted, not files uploaded', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        result_one_inserted: 'Inserted the qURL link into your Gmail draft',
        result_n_inserted: 'Inserted $1 qURL links into your Gmail draft',
        result_one_uploaded: '1 file uploaded successfully',
        result_n_uploaded: '$1 files uploaded successfully',
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const resultArea = popup.__testElements.get('resultArea');
  const summary = function () {
    return resultArea.children[0].textContent;
  };

  // Two uploads but only one accessible link: buildLinkHtml renders the non-https one as
  // filename-only text, so only one link reached the draft.
  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'http://files.example.com/b', expiry: null },
    ],
    [],
    null
  );
  assert.equal(summary(), 'Inserted the qURL link into your Gmail draft');

  // Nothing was linkable, so there is no honest "inserted" sentence — report the upload.
  popup.showResults(
    [
      { filename: 'a.txt', link: 'http://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'http://files.example.com/b', expiry: null },
    ],
    [],
    null
  );
  assert.equal(summary(), '2 files uploaded successfully');

  popup.showResults([{ filename: 'a.txt', link: 'ftp://files.example.com/a', expiry: null }], [], null);
  assert.equal(summary(), '1 file uploaded successfully');

  // All linkable: the count matches both the files and the links.
  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    null
  );
  assert.equal(summary(), 'Inserted 2 qURL links into your Gmail draft');
});

test('the insertion-only message agrees with the copy button about how many links wait there', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    },
    {
      chromeMessages: {
        copy_btn: 'Copy the qURL link',
        copy_btn_plural: 'Copy the qURL links',
        result_insertion_only_failed: 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL link.',
        result_insertion_only_failed_plural: 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL links.',
        result_insertion_only_failed_no_copy: 'Couldn\'t insert into your Gmail draft, and no accessible qURL link is available to copy.',
      },
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const errorArea = popup.__testElements.get('errorArea');
  const copyBtn = popup.__testElements.get('copyBtn');

  popup.showResults(
    [{ filename: 'a.txt', link: 'https://files.example.com/a', expiry: null }],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(errorArea.children[0].textContent, 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL link.');
  assert.equal(copyBtn.textContent, 'Copy the qURL link');

  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(errorArea.children[0].textContent, 'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL links.');
  assert.equal(copyBtn.textContent, 'Copy the qURL links');

  // Nothing copyable: the message must not point at a button that has nothing to give.
  popup.showResults(
    [{ filename: 'a.txt', link: 'http://files.example.com/a', expiry: null }],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(errorArea.children[0].textContent, 'Couldn\'t insert into your Gmail draft, and no accessible qURL link is available to copy.');
  assert.equal(copyBtn.disabled, true);
});

test('the insertion-only failure copy renders the shipped wording for every link count', function () {
  // Deliberately no chromeMessages stub: getMessage falls through to popup.js's own inline
  // fallbacks, so these assertions pin the wording a user actually reads. The stubbed test
  // above can only pin key selection — it asserts the very strings it just supplied, so a
  // reworded messages.json entry passes it untouched. Keep both: that test proves which of
  // the three keys each link count picks, this one proves what those keys say.
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const errorArea = popup.__testElements.get('errorArea');

  popup.showResults(
    [{ filename: 'a.txt', link: 'https://files.example.com/a', expiry: null }],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(
    errorArea.children[0].textContent,
    'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL link.'
  );

  popup.showResults(
    [
      { filename: 'a.txt', link: 'https://files.example.com/a', expiry: null },
      { filename: 'b.txt', link: 'https://files.example.com/b', expiry: null },
    ],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(
    errorArea.children[0].textContent,
    'Couldn\'t insert into your Gmail draft. Use the copy button below to get the accessible qURL links.'
  );

  popup.showResults(
    [{ filename: 'a.txt', link: 'http://files.example.com/a', expiry: null }],
    [],
    'Active tab is not Gmail.'
  );
  assert.equal(
    errorArea.children[0].textContent,
    'Couldn\'t insert into your Gmail draft, and no accessible qURL link is available to copy.'
  );
});

test('copied links carry their expiry in both clipboard flavors', function () {
  const popup = loadPopup(
    function () {
      return Promise.resolve({ success: true });
    },
    {
      setTimeout() {
        return 1;
      },
      clearTimeout() {},
    }
  );

  global.QURLComposeFormatter.normalizeAllowedLink = function (link) {
    return String(link).startsWith('https://') ? String(link) : null;
  };

  const results = [
    { filename: 'a.txt', link: 'https://files.example.com/a', expiry: '2026-01-02T03:04:05Z' },
    { filename: 'b.txt', link: 'http://files.example.com/b', expiry: '2026-01-02T03:04:05Z' },
    { filename: 'c.txt', link: 'https://files.example.com/c', expiry: null },
  ];

  // The copy fallback is reached exactly when insertion failed, so the expiry has to survive
  // into what the user pastes. Non-https links are still dropped, and a null expiry adds nothing.
  assert.equal(
    popup.buildCopyUrlText(results),
    'https://files.example.com/a (Expires: 2026-01-02T03:04:05Z)\nhttps://files.example.com/c'
  );

  assert.equal(
    popup.buildCopyUrlHtml(results),
    '<a href="https://files.example.com/a">https://files.example.com/a</a> (Expires: 2026-01-02T03:04:05Z)'
      + '<br><a href="https://files.example.com/c">https://files.example.com/c</a>'
  );
});
