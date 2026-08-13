/**
 * Shared "require a fresh src/config with these env vars set" helper.
 *
 * config.js resolves and caches its exports at require-time, so any test that
 * exercises a different env shape needs a fresh module graph. The
 * save-env → set-overrides → jest.isolateModules → require → restore-env
 * dance around that is identical everywhere it appears, and was copied into
 * four test files before this helper existed; a fifth arrives with the
 * BASE_URL boot check. Restoring env in a `finally` is the part that MUST NOT
 * drift — a helper that skips it leaks state into every later test in the
 * worker.
 *
 * Four thin entry points over one core. Pick by what the callback needs:
 *
 *   withFreshEnv             — run(); the body requires config itself. Use
 *                              when the test asserts the require THROWS.
 *   withFreshConfig          — run(config)
 *   captureFreshConfig       — run(config, warns), capturing console.warn
 *   withFreshConfigMockingOs — run(config), with `os` mocked
 */

/**
 * @param envOverrides  map of env key → value, or `undefined` to unset the key
 * @param run           receives (config, warns); `warns` is [] unless capturing
 * @param captureWarns  collect console.warn output into `warns`
 * @param mockOs        when set, `os` is mocked with these overrides before
 *                      config is required: { hostname, networkInterfaces }
 * @param loadConfig    require src/config and pass it to `run`. Off for tests
 *                      that assert the require itself throws — those must do
 *                      the require inside their own callback.
 */
function freshConfig(envOverrides, run, { captureWarns = false, mockOs, loadConfig = true } = {}) {
  jest.isolateModules(() => {
    const prevValues = {};
    const origConsoleWarn = console.warn;
    const warns = [];
    if (captureWarns) console.warn = (...args) => warns.push(args.join(' '));
    try {
      for (const [key, value] of Object.entries(envOverrides)) {
        prevValues[key] = process.env[key];
        if (value === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = value;
        }
      }
      if (mockOs) {
        jest.doMock('os', () => {
          const actual = jest.requireActual('os');
          return {
            ...actual,
            hostname: () =>
              (mockOs.hostname !== undefined ? mockOs.hostname : actual.hostname()),
            networkInterfaces: () =>
              (mockOs.networkInterfaces !== undefined
                ? mockOs.networkInterfaces
                : actual.networkInterfaces()),
          };
        });
      }
      run(loadConfig ? require('../../src/config') : undefined, warns);
    } finally {
      console.warn = origConsoleWarn;
      for (const [key, prev] of Object.entries(prevValues)) {
        if (prev === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = prev;
        }
      }
      if (mockOs) jest.dontMock('os');
    }
  });
}

const withFreshEnv = (envOverrides, run) =>
  freshConfig(envOverrides, () => run(), { loadConfig: false });

const withFreshConfig = (envOverrides, run) => freshConfig(envOverrides, run);

const captureFreshConfig = (envOverrides, run) =>
  freshConfig(envOverrides, run, { captureWarns: true });

/**
 * `env` is the env-override map; `hostname` / `networkInterfaces` override the
 * corresponding `os` functions, and fall through to the real ones when unset.
 */
const withFreshConfigMockingOs = ({ env = {}, hostname, networkInterfaces }, run) =>
  // INSTANCE_ID / INSTANCE_IP are cleared unless the case sets them: config
  // derives both at require-time, so an ambient value in the runner's
  // environment would silently win over the mocked `os` shape.
  freshConfig({ INSTANCE_ID: undefined, INSTANCE_IP: undefined, ...env }, run,
    { mockOs: { hostname, networkInterfaces } });

module.exports = {
  withFreshEnv,
  withFreshConfig,
  captureFreshConfig,
  withFreshConfigMockingOs,
};
