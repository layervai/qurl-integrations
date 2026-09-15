
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

const withFreshConfigMockingOs = ({ env = {}, hostname, networkInterfaces }, run) =>
  freshConfig({ INSTANCE_ID: undefined, INSTANCE_IP: undefined, ...env }, run,
    { mockOs: { hostname, networkInterfaces } });

module.exports = {
  withFreshEnv,
  withFreshConfig,
  captureFreshConfig,
  withFreshConfigMockingOs,
};
