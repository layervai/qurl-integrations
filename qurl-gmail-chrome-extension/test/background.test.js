const test = require('node:test');
const assert = require('node:assert/strict');

const backgroundModulePath = require.resolve('../background.js');
const originalChrome = global.chrome;

function loadBackground(mockChrome) {
  delete require.cache[backgroundModulePath];
  global.chrome = mockChrome;
  return require('../background.js');
}

test.afterEach(function () {
  delete require.cache[backgroundModulePath];
  global.chrome = originalChrome;
});

test('ensureGmailContentScript reinjects after any ping failure and retries the ping', async function () {
  const sendCalls = [];
  const executeCalls = [];
  let pingAttempt = 0;
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      sendMessage(tabId, message, callback) {
        sendCalls.push({ tabId, message });
        if (message.type === 'QURL_PING' && pingAttempt === 0) {
          pingAttempt += 1;
          chrome.runtime.lastError = { message: 'A transient ping failure' };
          callback(undefined);
          chrome.runtime.lastError = null;
          return;
        }

        chrome.runtime.lastError = null;
        callback({ success: true });
      },
    },
    scripting: {
      async executeScript(details) {
        executeCalls.push(details);
      },
    },
  };

  const background = loadBackground(chrome);

  await background.ensureGmailContentScript({
    id: 42,
    url: 'https://mail.google.com/mail/u/0/#inbox',
  });

  assert.deepEqual(
    sendCalls.map(function (call) { return call.message.type; }),
    ['QURL_PING', 'QURL_PING']
  );
  assert.deepEqual(executeCalls, [{
    target: { tabId: 42 },
    files: ['lib/qurl-i18n.js', 'lib/qurl-compose-format.js', 'content/gmail-compose.js'],
  }]);
});

test('ensureGmailContentScript surfaces the second ping failure after reinjection', async function () {
  let pingAttempt = 0;
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      sendMessage(_tabId, message, callback) {
        if (message.type === 'QURL_PING') {
          pingAttempt += 1;
          chrome.runtime.lastError = { message: pingAttempt === 1 ? 'Missing content script' : 'Still not responding' };
          callback(undefined);
          chrome.runtime.lastError = null;
          return;
        }
        callback({ success: true });
      },
    },
    scripting: {
      async executeScript() {},
    },
  };

  const background = loadBackground(chrome);
  await assert.rejects(
    background.ensureGmailContentScript({
      id: 42,
      url: 'https://mail.google.com/mail/u/0/#inbox',
    }),
    /Still not responding/
  );
});

test('relayInsertLinks returns a fallback error when the content script replies with no payload', async function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      async query() {
        return [{ id: 7, url: 'https://mail.google.com/mail/u/0/#inbox' }];
      },
      sendMessage(_tabId, message, callback) {
        chrome.runtime.lastError = null;
        if (message.type === 'QURL_PING') {
          callback({ success: true });
          return;
        }
        callback(null);
      },
    },
    scripting: {
      async executeScript() {
        throw new Error('executeScript should not be called when ping succeeds');
      },
    },
  };

  const background = loadBackground(chrome);
  const response = await background.relayInsertLinks({
    type: 'INSERT_LINKS',
    results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
  });

  assert.deepEqual(response, {
    success: false,
    error: 'No response from content script',
  });
});

test('relayInsertLinks returns a no-active-tab error when Chrome has no active tab', async function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      async query() {
        return [];
      },
    },
    scripting: {
      async executeScript() {
        throw new Error('executeScript should not run without an active tab');
      },
    },
  };

  const background = loadBackground(chrome);
  const response = await background.relayInsertLinks({
    type: 'INSERT_LINKS',
    results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
  });

  assert.deepEqual(response, {
    success: false,
    error: 'No active tab found',
  });
});

test('ensureGmailContentScript rejects non-mail.google.com/mail URLs before injection', async function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      sendMessage() {
        throw new Error('sendMessage should not run for non-Gmail tabs');
      },
    },
    scripting: {
      async executeScript() {
        throw new Error('executeScript should not run for non-Gmail tabs');
      },
    },
  };

  const background = loadBackground(chrome);
  await assert.rejects(
    background.ensureGmailContentScript({
      id: 42,
      url: 'https://mail.google.com/chat/u/0/',
    }),
    /Active tab is not Gmail/
  );
});

test('runtime message listener ignores INSERT_LINKS from untrusted senders', function () {
  let listener = null;
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      id: 'trusted-extension-id',
      lastError: null,
      onMessage: {
        addListener(registeredListener) {
          listener = registeredListener;
        },
      },
    },
  };

  loadBackground(chrome);

  const sendResponse = function () {
    assert.fail('untrusted senders should be ignored');
  };

  assert.equal(
    listener({ type: 'INSERT_LINKS', results: [] }, { id: 'other-extension-id' }, sendResponse),
    false
  );
});

test('isTrustedInsertLinksSender fails closed when runtime id is unavailable', function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
  };

  const background = loadBackground(chrome);
  assert.equal(background.isTrustedInsertLinksSender({ id: 'trusted-extension-id' }), false);
});

test('INSERT_LINKS relay timeout is longer than the ping timeout budget', function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      id: 'trusted-extension-id',
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
  };

  const background = loadBackground(chrome);
  assert.equal(background.INSERT_LINKS_TAB_MESSAGE_TIMEOUT_MS > background.TAB_MESSAGE_TIMEOUT_MS, true);
});

test('sendMessageToTab rejects when the content script does not respond before timeout', async function () {
  const chrome = {
    i18n: {
      getMessage() {
        return '';
      },
    },
    runtime: {
      id: 'trusted-extension-id',
      lastError: null,
      onMessage: {
        addListener() {},
      },
    },
    tabs: {
      sendMessage() {},
    },
  };

  const background = loadBackground(chrome);

  await assert.rejects(
    background.sendMessageToTab(7, { type: 'QURL_PING' }, 10),
    /Timed out while waiting for the Gmail tab to respond\./
  );
});
