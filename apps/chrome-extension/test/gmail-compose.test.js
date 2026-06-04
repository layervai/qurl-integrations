const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const contentScriptSource = fs.readFileSync(
  path.join(__dirname, '..', 'content', 'gmail-compose.js'),
  'utf8'
);

function createSelectionHarness() {
  let currentRange = null;
  const selection = {
    removeAllRanges() {
      currentRange = null;
    },
    addRange(range) {
      currentRange = range;
    },
    get rangeCount() {
      return currentRange ? 1 : 0;
    },
    getRangeAt() {
      return currentRange;
    },
  };

  return {
    selection,
    createRange() {
      return {
        collapse() {},
        createContextualFragment(html) {
          return {
            html,
            lastChild: { nodeName: 'LAST' },
          };
        },
        insertNode() {},
        selectNodeContents() {},
        setStartAfter() {},
      };
    },
  };
}

test('findComposeBodyAsync observes documentElement when document.body is not ready', async function () {
  const observerCalls = [];
  let composeBodies = [];
  let messageListener = null;
  let observerInstance = null;
  const execCalls = [];
  const caretMoves = [];
  const documentElement = { nodeName: 'HTML' };
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
    insertAdjacentHTML() {
      throw new Error('insertAdjacentHTML should not be reached when execCommand succeeds');
    },
  };

  class MockMutationObserver {
    constructor(callback) {
      this.callback = callback;
      observerInstance = this;
    }

    observe(target, options) {
      observerCalls.push({ target, options });
    }

    disconnect() {
      this.disconnected = true;
    }
  }

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: null,
      documentElement,
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      execCommand(command, showUi, html) {
        execCalls.push({ command, showUi, html });
        return true;
      },
      createRange() {
        const range = selectionHarness.createRange();
        range.selectNodeContents = function (node) {
          caretMoves.push(node);
        };
        return range;
      },
      queryCommandSupported(command) {
        assert.equal(command, 'insertHTML');
        return true;
      },
      querySelectorAll() {
        return composeBodies;
      },
      addEventListener() {
        assert.fail('documentElement observation should avoid waiting for DOMContentLoaded');
      },
      removeEventListener() {},
    },
    MutationObserver: MockMutationObserver,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const responsePromise = new Promise(function (resolve) {
    const keepAlive = messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve);
    assert.equal(keepAlive, true);
  });

  assert.equal(observerCalls.length, 1);
  assert.equal(observerCalls[0].target, documentElement);
  assert.equal(observerCalls[0].options.childList, true);
  assert.equal(observerCalls[0].options.subtree, true);
  assert.equal('attributes' in observerCalls[0].options, false);

  composeBodies = [composeBody];
  observerInstance.callback();

  const response = await responsePromise;
  assert.equal(response.success, true);
  assert.deepEqual(execCalls, [{
    command: 'insertHTML',
    showUi: false,
    html: '<p>links</p>',
  }]);
  assert.equal(caretMoves.length, 1);
  assert.equal(caretMoves[0], composeBody);
  assert.equal(observerInstance.disconnected, true);
});

test('findComposeBodyAsync performs an immediate post-observe lookup on the next frame', async function () {
  let composeBodies = [];
  let messageListener = null;
  const rafCallbacks = [];
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      execCommand() {
        return true;
      },
      createRange() {
        return selectionHarness.createRange();
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return composeBodies;
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    requestAnimationFrame(callback) {
      rafCallbacks.push(callback);
      return rafCallbacks.length;
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const responsePromise = new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(rafCallbacks.length, 1);
  composeBodies = [composeBody];
  rafCallbacks[0]();

  const response = await responsePromise;
  assert.equal(response.success, true);
});

test('duplicate INSERT_LINKS requests with the same requestId only insert once', async function () {
  let composeBodies = [];
  let messageListener = null;
  let observerInstance = null;
  let execInsertCount = 0;
  const documentElement = { nodeName: 'HTML' };
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  class MockMutationObserver {
    constructor(callback) {
      this.callback = callback;
      observerInstance = this;
    }

    observe() {}
    disconnect() {}
  }

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: null,
      documentElement,
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        return selectionHarness.createRange();
      },
      execCommand(command) {
        assert.equal(command, 'insertHTML');
        execInsertCount += 1;
        return true;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return composeBodies;
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: MockMutationObserver,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const message = {
    type: 'INSERT_LINKS',
    requestId: 'same-request',
    results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
  };

  const firstResponse = new Promise(function (resolve) {
    assert.equal(messageListener(message, null, resolve), true);
  });
  const secondResponse = new Promise(function (resolve) {
    assert.equal(messageListener(message, null, resolve), true);
  });

  composeBodies = [composeBody];
  observerInstance.callback();

  const [first, second] = await Promise.all([firstResponse, secondResponse]);
  assert.equal(first.success, true);
  assert.equal(second.success, true);
  assert.equal(execInsertCount, 1);
});

test('completed requests are retained (under the cap) so retries replay instead of re-inserting', async function () {
  let messageListener = null;
  let execInsertCount = 0;
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout() {},
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        return selectionHarness.createRange();
      },
      execCommand() {
        execInsertCount += 1;
        return true;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return [composeBody];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    setTimeout() {
      return 1;
    },
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  for (let i = 0; i < 33; i += 1) {
    const response = await new Promise(function (resolve) {
      assert.equal(messageListener({
        type: 'INSERT_LINKS',
        requestId: `req-${i}`,
        results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
      }, null, resolve), true);
    });
    assert.equal(response.success, true);
  }

  assert.equal(execInsertCount, 33);

  // req-0 completed recently and the map is under the cap, so it must NOT have been evicted.
  // A retry with the same requestId replays the cached response synchronously (listener
  // returns false) and does NOT trigger a second insertion.
  const replayed = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      requestId: 'req-0',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), false);
  });

  assert.equal(replayed.success, true);
  assert.equal(execInsertCount, 33);
});

test('Selection API fallback inserts at the end when execCommand is unavailable', async function () {
  let messageListener = null;
  const insertedFragments = [];
  const startAfterCalls = [];
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        const range = selectionHarness.createRange();
        range.insertNode = function (fragment) {
          insertedFragments.push(fragment.html);
        };
        range.setStartAfter = function (node) {
          startAfterCalls.push(node.nodeName);
        };
        return range;
      },
      queryCommandSupported() {
        return false;
      },
      querySelectorAll() {
        return [composeBody];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, true);
  assert.deepEqual(insertedFragments, ['<p>links</p>']);
  assert.deepEqual(startAfterCalls, ['LAST']);
});

test('Selection API fallback runs when execCommand reports insertion failure', async function () {
  let messageListener = null;
  const execCalls = [];
  const insertedFragments = [];
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        const range = selectionHarness.createRange();
        range.insertNode = function (fragment) {
          insertedFragments.push(fragment.html);
        };
        return range;
      },
      execCommand(command, showUi, html) {
        execCalls.push({ command, showUi, html });
        return false;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return [composeBody];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, true);
  assert.deepEqual(execCalls, [{
    command: 'insertHTML',
    showUi: false,
    html: '<p>links</p>',
  }]);
  assert.deepEqual(insertedFragments, ['<p>links</p>']);
});

test('findComposeBody prefers the topmost visible compose body when none is focused', async function () {
  let messageListener = null;
  const caretMoves = [];
  const focusCalls = [];
  const selectionHarness = createSelectionHarness();
  const backgroundCompose = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {
      focusCalls.push('background');
    },
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24, top: 240, left: 640 };
    },
  };
  const foregroundCompose = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {
      focusCalls.push('foreground');
    },
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24, top: 120, left: 320 };
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      execCommand() {
        return true;
      },
      createRange() {
        const range = selectionHarness.createRange();
        range.selectNodeContents = function (node) {
          caretMoves.push(node);
        };
        return range;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll(selector) {
        return selector.includes(':focus')
          ? []
          : [backgroundCompose, foregroundCompose];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function (element) {
    if (element === foregroundCompose) {
      return { display: 'block', visibility: 'visible', zIndex: '20' };
    }
    return { display: 'block', visibility: 'visible', zIndex: '1' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, true);
  assert.deepEqual(focusCalls, ['foreground']);
  assert.deepEqual(caretMoves, [foregroundCompose]);
});

test('pending INSERT_LINKS requests are not evicted before they complete', function () {
  let messageListener = null;
  const observerInstances = [];
  let composeBodies = [];
  const responseOrder = [];
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
  };

  class MockMutationObserver {
    constructor(callback) {
      this.callback = callback;
      observerInstances.push(this);
    }

    observe() {}
    disconnect() {}
  }

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        return selectionHarness.createRange();
      },
      execCommand() {
        return true;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return composeBodies;
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: MockMutationObserver,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  for (let i = 0; i < 33; i += 1) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      requestId: `pending-${i}`,
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, function (response) {
      responseOrder.push({ index: i, response });
    }), true);
  }

  composeBodies = [composeBody];
  observerInstances.forEach(function (observer) {
    observer.callback();
  });

  assert.equal(responseOrder.length, 33);
  responseOrder.forEach(function (entry, index) {
    assert.equal(entry.index, index);
    assert.equal(entry.response.success, true);
  });
});

test('insertAdjacentHTML is the last resort when selection insertion fails', async function () {
  let messageListener = null;
  const insertAdjacentCalls = [];
  const selectionHarness = createSelectionHarness();
  const composeBody = {
    classList: {
      contains(name) {
        return name === 'Am' || name === 'Al' || name === 'editable';
      },
    },
    focus() {},
    getAttribute(name) {
      if (name === 'contenteditable') return 'true';
      if (name === 'role') return 'textbox';
      if (name === 'aria-multiline') return 'true';
      return null;
    },
    getBoundingClientRect() {
      return { width: 320, height: 24 };
    },
    insertAdjacentHTML(position, html) {
      insertAdjacentCalls.push({ position, html });
    },
  };

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout,
    console: {
      warn() {},
    },
    document: {
      body: {},
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
        };
      },
      createRange() {
        const range = selectionHarness.createRange();
        range.createContextualFragment = function () {
          throw new Error('fragment parse failed');
        };
        return range;
      },
      queryCommandSupported() {
        return false;
      },
      querySelectorAll() {
        return [composeBody];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: class {
      observe() {}
      disconnect() {}
    },
    setTimeout,
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.getSelection = function () {
    return selectionHarness.selection;
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, true);
  assert.deepEqual(insertAdjacentCalls, [{
    position: 'beforeend',
    html: '<p>links</p>',
  }]);
});

test('findComposeBodyAsync times out and reports failure when no compose body appears', async function () {
  let messageListener = null;
  let observerInstance = null;
  const timeoutCallbacks = [];
  const appendedAlerts = [];

  class MockMutationObserver {
    constructor(callback) {
      this.callback = callback;
      observerInstance = this;
    }

    observe() {}

    disconnect() {
      this.disconnected = true;
    }
  }

  const sandbox = {
    chrome: {
      i18n: {
        getMessage() {
          return '';
        },
      },
      runtime: {
        lastError: null,
        onMessage: {
          addListener(listener) {
            messageListener = listener;
          },
        },
      },
    },
    clearTimeout() {},
    console: {
      warn() {},
    },
    document: {
      body: {
        appendChild(node) {
          appendedAlerts.push(node);
        },
      },
      documentElement: { nodeName: 'HTML' },
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
          textContent: '',
        };
      },
      querySelectorAll() {
        return [];
      },
      addEventListener() {},
      removeEventListener() {},
    },
    MutationObserver: MockMutationObserver,
    setTimeout(callback, delay) {
      timeoutCallbacks.push({ callback, delay });
      return timeoutCallbacks.length;
    },
  };

  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;
  sandbox.getComputedStyle = function () {
    return { display: 'block', visibility: 'visible' };
  };
  sandbox.QURLComposeFormatter = {
    buildLinkHtml() {
      return '<p>links</p>';
    },
  };

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  const responsePromise = new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  const composeTimeout = timeoutCallbacks.find(function (entry) {
    return entry.delay === 4000;
  });
  assert.ok(composeTimeout);
  composeTimeout.callback();

  const response = await responsePromise;
  assert.equal(response.success, false);
  assert.equal(observerInstance.disconnected, true);
  assert.equal(appendedAlerts.length, 1);
});
