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

// The content script arms real timers that intentionally outlive an insertion: a 30s response
// cache per completed INSERT_LINKS (INSERT_REQUEST_CACHE_TTL_MS) and a 4s toast auto-dismiss.
// Both are correct in a browser, but a sandbox wired straight to Node's global setTimeout keeps
// the whole test process alive until they fire — this file used to spend 30s doing 6ms of work.
//
// Sandboxes that want real timers take these tracked wrappers. The after() hook clears whatever
// is still armed, but first asserts the survivors are exactly the ones the test declared: that
// hang was the only signal this class of bug ever produced, so quietly sweeping up leftovers
// would retire it for good. A timer some future path forgets to clear fails here instead, named
// by its delay. expectedArmed maps a delay to how many of that delay the test means to leave
// behind; omit it for the usual case of none.
//
// The timers stay real — no fake clock, and deliberately no unref(): an unref'd timer still
// fires, which would make running the content script's callbacks depend on how long the rest of
// the suite happens to take.
function createTimerHarness(t, expectedArmed) {
  const armed = new Map();

  t.after(function () {
    const survivors = {};
    armed.forEach(function (delay, timerId) {
      survivors[delay] = (survivors[delay] || 0) + 1;
      globalThis.clearTimeout(timerId);
    });
    armed.clear();
    // Asserted after clearing, so a failure reports instead of hanging on the very timers it names.
    assert.deepEqual(
      survivors,
      expectedArmed || {},
      'timers left armed at teardown should be only the ones this test means to outlive'
    );
  });

  return {
    clearTimeout(timerId) {
      armed.delete(timerId);
      globalThis.clearTimeout(timerId);
    },
    setTimeout(callback, delay, ...args) {
      const timerId = globalThis.setTimeout(function () {
        armed.delete(timerId);
        callback(...args);
      }, delay);
      armed.set(timerId, delay);
      return timerId;
    },
  };
}

// Copies `source` onto `target`, except that a key whose value is `undefined` is deleted instead.
// The content script feature-detects everything optional with `typeof`, so deletion is how a test
// says "this browser has no execCommand" rather than "execCommand returns undefined".
function applyOverrides(target, source) {
  Object.keys(source || {}).forEach(function (key) {
    if (source[key] === undefined) {
      delete target[key];
      return;
    }
    target[key] = source[key];
  });
  return target;
}

// Every test below drives the content script through the same shape of vm sandbox: an object that
// is its own window/globalThis/self, a chrome.runtime.onMessage listener to capture, a document
// stub deep enough for compose discovery plus all three insertion paths, and a formatter that
// yields '<p>links</p>'. Hand-writing it at every call site made every cross-cutting change a
// many-way edit — createSelectionHarness was one such change, createTimerHarness another — so it
// lives here once and the genuinely per-test parts arrive as `config`:
//
//   expectedArmed  forwarded to createTimerHarness for timers the test means to outlive it
//   timers         {setTimeout, clearTimeout} to opt out of the tracked harness altogether
//   document       merged over the document stub (body, querySelectorAll, execCommand, ...)
//   globals        merged over the sandbox itself (requestAnimationFrame, getComputedStyle, ...)
//   decorateRange  mutates each range document.createRange hands out, for the selection paths
//
// Both merges go through applyOverrides, so `undefined` removes a key. `globals` cannot reach
// window/globalThis/self: those are assigned afterwards and have to stay self-referential for the
// content script's `window.foo` lookups to resolve. `t` is only used to register the tracked timer
// harness, so a test supplying its own `timers` leaves it unused.
//
// The returned handle carries what tests assert against: the captured `messageListener`, the
// MutationObserver `observers` (each recording its observe() calls and its disconnect), the
// `documentElement` an observer should fall back to, the nodes `appended` to document.body (the
// failure toast), and the `warnings` console.warn collected.
function createComposeSandbox(t, config) {
  const settings = config || {};
  // Refuse the combination rather than dropping expectedArmed on the floor: it only means anything
  // to the tracked harness, so a test that stubs timers *and* declares survivors is asking for a
  // leaked-timer assertion that would never run.
  assert.ok(
    !(settings.timers && settings.expectedArmed),
    'expectedArmed drives the tracked timer harness; a test supplying its own timers cannot use it'
  );
  const timers = settings.timers || createTimerHarness(t, settings.expectedArmed);
  const selectionHarness = createSelectionHarness();
  const documentElement = { nodeName: 'HTML' };
  const observers = [];
  const appended = [];
  const warnings = [];
  let messageListener = null;

  class RecordingMutationObserver {
    constructor(callback) {
      this.callback = callback;
      this.observeCalls = [];
      this.disconnected = false;
      observers.push(this);
    }

    observe(target, options) {
      this.observeCalls.push({ target, options });
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
    clearTimeout: timers.clearTimeout,
    console: {
      warn(...args) {
        warnings.push(args.map(String).join(' '));
      },
    },
    document: applyOverrides({
      body: {
        appendChild(node) {
          appended.push(node);
        },
      },
      documentElement,
      createElement() {
        return {
          setAttribute() {},
          style: {},
          remove() {},
          textContent: '',
        };
      },
      createRange() {
        const range = selectionHarness.createRange();
        if (settings.decorateRange) {
          settings.decorateRange(range);
        }
        return range;
      },
      execCommand() {
        return true;
      },
      queryCommandSupported() {
        return true;
      },
      querySelectorAll() {
        return [];
      },
      addEventListener() {},
      removeEventListener() {},
    }, settings.document),
    MutationObserver: RecordingMutationObserver,
    requestAnimationFrame(callback) {
      callback();
      return 1;
    },
    setTimeout: timers.setTimeout,
    getComputedStyle() {
      return { display: 'block', visibility: 'visible' };
    },
    getSelection() {
      return selectionHarness.selection;
    },
    QURLComposeFormatter: {
      buildLinkHtml() {
        return '<p>links</p>';
      },
    },
  };

  applyOverrides(sandbox, settings.globals);
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;

  vm.createContext(sandbox);
  vm.runInContext(contentScriptSource, sandbox);

  return {
    appended,
    documentElement,
    messageListener,
    observers,
    warnings,
  };
}

test('findComposeBodyAsync observes documentElement when document.body is not ready', async function (t) {
  let composeBodies = [];
  const execCalls = [];
  const caretMoves = [];
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

  const { documentElement, messageListener, observers } = createComposeSandbox(t, {
    document: {
      body: null,
      execCommand(command, showUi, html) {
        execCalls.push({ command, showUi, html });
        return true;
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
    },
    decorateRange(range) {
      range.selectNodeContents = function (node) {
        caretMoves.push(node);
      };
    },
  });

  const responsePromise = new Promise(function (resolve) {
    const keepAlive = messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve);
    assert.equal(keepAlive, true);
  });

  assert.equal(observers.length, 1);
  const [observer] = observers;
  assert.equal(observer.observeCalls.length, 1);
  assert.equal(observer.observeCalls[0].target, documentElement);
  assert.equal(observer.observeCalls[0].options.childList, true);
  assert.equal(observer.observeCalls[0].options.subtree, true);
  assert.equal('attributes' in observer.observeCalls[0].options, false);

  composeBodies = [composeBody];
  observer.callback();

  const response = await responsePromise;
  assert.equal(response.success, true);
  assert.deepEqual(execCalls, [{
    command: 'insertHTML',
    showUi: false,
    html: '<p>links</p>',
  }]);
  assert.equal(caretMoves.length, 1);
  assert.equal(caretMoves[0], composeBody);
  assert.equal(observer.disconnected, true);
});

test('findComposeBodyAsync performs an immediate post-observe lookup on the next frame', async function (t) {
  let composeBodies = [];
  const rafCallbacks = [];
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      querySelectorAll() {
        return composeBodies;
      },
    },
    globals: {
      requestAnimationFrame(callback) {
        rafCallbacks.push(callback);
        return rafCallbacks.length;
      },
    },
  });

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

test('findComposeBodyAsync leaves no discovery timeout behind when the first lookup settles synchronously', async function (t) {
  // Regression guard for the ordering inside findComposeBodyAsync. Both real schedulers are
  // asynchronous (requestAnimationFrame, or a setTimeout(fn, 16) fallback), so the discovery
  // timeout is always armed before finish() can run. This sandbox supplies a SYNCHRONOUS
  // requestAnimationFrame instead: the first queued lookup finds the compose body and calls
  // finish() while the function body is still running. If the timeout were armed after that
  // lookup, finish()'s clearTimeout would target a still-null timeoutId and the 4s timer would
  // be armed afterwards for an operation that already completed — nothing would ever clear it,
  // and it would fire a full findComposeBody() sweep. createTimerHarness declares no surviving
  // timers, so that leak fails this test at teardown.
  let composeBodies = [];
  let rafCalls = 0;
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      querySelectorAll() {
        return composeBodies;
      },
    },
    globals: {
      requestAnimationFrame(callback) {
        rafCalls += 1;
        // The compose body appears exactly between the initial miss and this first queued lookup,
        // so finish() runs inside findComposeBodyAsync rather than on a later frame.
        composeBodies = [composeBody];
        callback();
        return rafCalls;
      },
    },
  });

  // No requestId, so this takes the no-dedup path and the discovery timeout is the only timer
  // findComposeBodyAsync's caller can leave armed.
  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, true);
  assert.equal(rafCalls, 1, 'the compose body should be found by the first queued lookup');
});

test('duplicate INSERT_LINKS requests with the same requestId only insert once', async function (t) {
  let composeBodies = [];
  let execInsertCount = 0;
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

  const { messageListener, observers } = createComposeSandbox(t, {
    expectedArmed: { 30000: 1 },
    document: {
      body: null,
      execCommand(command) {
        assert.equal(command, 'insertHTML');
        execInsertCount += 1;
        return true;
      },
      querySelectorAll() {
        return composeBodies;
      },
    },
  });

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
  assert.equal(observers.length, 1, 'the duplicate request must not start a second discovery');
  observers[0].callback();

  const [first, second] = await Promise.all([firstResponse, secondResponse]);
  assert.equal(first.success, true);
  assert.equal(second.success, true);
  assert.equal(execInsertCount, 1);
});

test('completed requests are retained (under the cap) so retries replay instead of re-inserting', async function (t) {
  let execInsertCount = 0;
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

  const { messageListener } = createComposeSandbox(t, {
    // Timers are stubbed rather than tracked through createTimerHarness: this test needs the 30s
    // response cache to stay un-expirable across all 33 requests, which a real timer cannot give it.
    timers: {
      clearTimeout() {},
      setTimeout() {
        return 1;
      },
    },
    document: {
      execCommand() {
        execInsertCount += 1;
        return true;
      },
      querySelectorAll() {
        return [composeBody];
      },
    },
  });

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

test('Selection API fallback inserts at the end when execCommand is unavailable', async function (t) {
  const insertedFragments = [];
  const startAfterCalls = [];
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      execCommand: undefined,
      queryCommandSupported() {
        return false;
      },
      querySelectorAll() {
        return [composeBody];
      },
    },
    decorateRange(range) {
      range.insertNode = function (fragment) {
        insertedFragments.push(fragment.html);
      };
      range.setStartAfter = function (node) {
        startAfterCalls.push(node.nodeName);
      };
    },
  });

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

test('Selection API fallback runs when execCommand reports insertion failure', async function (t) {
  const execCalls = [];
  const insertedFragments = [];
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      execCommand(command, showUi, html) {
        execCalls.push({ command, showUi, html });
        return false;
      },
      querySelectorAll() {
        return [composeBody];
      },
    },
    decorateRange(range) {
      range.insertNode = function (fragment) {
        insertedFragments.push(fragment.html);
      };
    },
  });

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

test('findComposeBody prefers the topmost visible compose body when none is focused', async function (t) {
  const caretMoves = [];
  const focusCalls = [];
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      querySelectorAll(selector) {
        return selector.includes(':focus')
          ? []
          : [backgroundCompose, foregroundCompose];
      },
    },
    globals: {
      getComputedStyle(element) {
        if (element === foregroundCompose) {
          return { display: 'block', visibility: 'visible', zIndex: '20' };
        }
        return { display: 'block', visibility: 'visible', zIndex: '1' };
      },
    },
    decorateRange(range) {
      range.selectNodeContents = function (node) {
        caretMoves.push(node);
      };
    },
  });

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

test('findComposeBody breaks a tie between identically placed compose bodies on z-index', async function (t) {
  const caretMoves = [];
  const focusCalls = [];

  // compareComposeBodies leads with z-index and every term after it is geometry, so the
  // tie-break only decides anything once the rects tie outright. Both bodies here report the
  // same top, the same left, and the same area, which leaves z-index as the sole term with an
  // opinion — the test above cannot make that claim, since its rects differ by `top` and so
  // `top` alone already picks the foreground copy.
  //
  // DOM order is load-bearing for the same reason: `matches.sort` is stable, so a comparator
  // that stopped consulting z-index would return 0 for this pair and hand the win to whichever
  // element querySelectorAll yields first. Listing the background copy first is what turns that
  // regression into a failure here instead of a silently identical result.
  function createTiedCompose(label) {
    return {
      classList: {
        contains(name) {
          return name === 'Am' || name === 'Al' || name === 'editable';
        },
      },
      focus() {
        focusCalls.push(label);
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
  }

  const backgroundCompose = createTiedCompose('background');
  const foregroundCompose = createTiedCompose('foreground');

  const { messageListener } = createComposeSandbox(t, {
    document: {
      querySelectorAll(selector) {
        return selector.includes(':focus')
          ? []
          : [backgroundCompose, foregroundCompose];
      },
    },
    globals: {
      getComputedStyle(element) {
        if (element === foregroundCompose) {
          return { display: 'block', visibility: 'visible', zIndex: '20' };
        }
        return { display: 'block', visibility: 'visible', zIndex: '1' };
      },
    },
    decorateRange(range) {
      range.selectNodeContents = function (node) {
        caretMoves.push(node);
      };
    },
  });

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

test('pending INSERT_LINKS requests are not evicted before they complete', function (t) {
  let composeBodies = [];
  const responseOrder = [];
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

  const { messageListener, observers } = createComposeSandbox(t, {
    expectedArmed: { 30000: 33 },
    document: {
      querySelectorAll() {
        return composeBodies;
      },
    },
  });

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
  observers.forEach(function (observer) {
    observer.callback();
  });

  assert.equal(responseOrder.length, 33);
  responseOrder.forEach(function (entry, index) {
    assert.equal(entry.index, index);
    assert.equal(entry.response.success, true);
  });
});

test('insertAdjacentHTML is the last resort when selection insertion fails', async function (t) {
  const insertAdjacentCalls = [];
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

  const { messageListener } = createComposeSandbox(t, {
    document: {
      execCommand: undefined,
      queryCommandSupported() {
        return false;
      },
      querySelectorAll() {
        return [composeBody];
      },
    },
    decorateRange(range) {
      range.createContextualFragment = function () {
        throw new Error('fragment parse failed');
      };
    },
  });

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

test('a missing compose formatter reports failure instead of inserting nothing', async function (t) {
  const insertAdjacentCalls = [];
  const execCalls = [];
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

  const { messageListener, warnings } = createComposeSandbox(t, {
    expectedArmed: { 4000: 1 },
    document: {
      execCommand(command, showUi, html) {
        execCalls.push({ command, showUi, html });
        return true;
      },
      querySelectorAll() {
        return [composeBody];
      },
    },
    // Deliberately no QURLComposeFormatter: buildLinkHtml then yields '',
    // and every insertion path would happily append that empty string.
    globals: { QURLComposeFormatter: undefined },
  });

  const response = await new Promise(function (resolve) {
    assert.equal(messageListener({
      type: 'INSERT_LINKS',
      results: [{ filename: 'demo.txt', link: 'https://files.example.com/q/demo', expiry: null }],
    }, null, resolve), true);
  });

  assert.equal(response.success, false, 'an empty insertion must not be reported as success');
  assert.deepEqual(execCalls, [], 'nothing should be inserted without the formatter');
  assert.deepEqual(insertAdjacentCalls, [], 'the fallback path must not append an empty string either');
  assert.ok(
    warnings.some(function (line) { return line.includes('formatter unavailable'); }),
    'the refusal should be logged'
  );
});

test('findComposeBodyAsync times out and reports failure when no compose body appears', async function (t) {
  const timeoutCallbacks = [];

  const { appended, messageListener, observers } = createComposeSandbox(t, {
    // Timers are stubbed rather than tracked through createTimerHarness: this test fires the 4s
    // discovery timeout by hand (timeoutCallbacks below), which a real timer would stretch to 4s.
    timers: {
      clearTimeout() {},
      setTimeout(callback, delay) {
        timeoutCallbacks.push({ callback, delay });
        return timeoutCallbacks.length;
      },
    },
    // Preserve the browser fallback path this test covered before the shared factory existed:
    // without requestAnimationFrame, compose lookups schedule through setTimeout(fn, 16).
    globals: { requestAnimationFrame: undefined },
  });

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
  assert.ok(
    timeoutCallbacks.some(function (entry) { return entry.delay === 16; }),
    'the requestAnimationFrame fallback should schedule a 16ms lookup'
  );
  composeTimeout.callback();

  const response = await responsePromise;
  assert.equal(response.success, false);
  assert.equal(observers[0].disconnected, true);
  assert.equal(appended.length, 1);
});
