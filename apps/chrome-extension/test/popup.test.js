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
  global.uploadFile = async function () {
    throw new Error('uploadFile should not run in popup helper tests');
  };
  global.normalizeQurlApiBase = resolvedOptions.normalizeQurlApiBase || function (value) {
    return value ? String(value).trim().replace(/\/+$/, '') : null;
  };
  global.isDefaultQurlOrigin = resolvedOptions.isDefaultQurlOrigin || function (value) {
    return value === 'https://getqurllink.layerv.ai';
  };
  global.QURLComposeFormatter = {
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
  assert.equal(confirmText.textContent, 'Allow the extension to access https://custom.example.com for qURL uploads? Chrome will show a permission prompt next.');
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
        result_n_success: 'Inserted $1 qURL links into the Gmail draft.',
        result_n_success_upload_only: '$1 files uploaded successfully',
        result_insertion_only_failed: 'Upload completed successfully. Click "Copy the qURL link" to get the accessible URL.',
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
    'Inserted 2 qURL links into the Gmail draft.'
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
  assert.equal(errorArea.children[0].textContent, 'Upload completed successfully. Click "Copy the qURL link" to get the accessible URL.');
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
