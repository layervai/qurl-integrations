module.exports = {
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.js'],
  // Global env setup — sets DDB_TABLE_PREFIX + AWS_REGION before any
  // test file loads, so source modules with fail-fast module-load
  // guards (flow-state, ddb-store) can be required without throwing.
  // See tests/setup-env.js for the rationale.
  setupFiles: ['<rootDir>/tests/setup-env.js'],
  // Note: `restoreMocks` is NOT set globally — several specs rely on
  // jest.spyOn results persisting across tests within a describe (the
  // spies are set up in module-level scope and tests assert against
  // accumulated call counts). Spike-style tests that want spy restoration
  // use an explicit `afterEach(() => jest.restoreAllMocks())` at the top
  // of the test file — see gateway-resume-spike.test.js for the pattern.
  collectCoverageFrom: ['src/**/*.js', '!src/index.js'],
  coverageDirectory: 'coverage',
  coverageThreshold: {
    // 78/68/78/78 floors. Coverage on commands.js is held by:
    //   - qurl-file-map.test.js: /qurl file + /qurl map front-half
    //     (slash-option parsing, recipient resolution, confirm-card
    //     render/dispatch, forgery-rejection gates).
    //   - qurl-send-back-half.test.js: executeSendPipeline + back-half
    //     unit tests (monitorLinkStatus polling/transitions/stop()-race/
    //     LRU eviction, revokeAllLinks per-link failures,
    //     handleAddRecipients pre-flight guards + file/location paths +
    //     DB-failure mid-flow, mintLinksInBatches batch boundaries).
    global: {
      statements: 78,
      branches: 68,
      functions: 78,
      lines: 78,
    },
  },
  verbose: true,
};
