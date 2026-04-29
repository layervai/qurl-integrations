module.exports = {
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.js'],
  collectCoverageFrom: ['src/**/*.js', '!src/index.js'],
  coverageDirectory: 'coverage',
  coverageThreshold: {
    // 78/68/78/78 floors restored. The /qurl send state-machine redesign
    // removed commands-comprehensive.test.js + coverage-boost.test.js
    // (1900+ lines of describe.skip-able tests pinned to the dead 4-options
    // slash shape). Their replacements:
    //   - qurl-send-state-machine.test.js: front-half flow (handleSend
    //     pre-flight guards, Step 1/2/3 transitions, file/location paths,
    //     end-to-end happy paths, DM-pivot + voice-channel regression).
    //   - qurl-send-back-half.test.js: back-half unit tests
    //     (monitorLinkStatus polling/transitions/stop()-race/LRU eviction,
    //     revokeAllLinks per-link failures, handleAddRecipients pre-flight
    //     guards + file/location paths + DB-failure mid-flow,
    //     mintLinksInBatches batch boundaries).
    // Current coverage on commands.js is 81.78 / 72.61 / 81.75 / 82.89,
    // clear of the 78/68/78/78 floor on every metric.
    //
    // Issue #137 (back-half coverage restoration) is closed by this PR per
    // Justin's hard-blocker review feedback: lowering a quality gate to
    // merge a UX rewrite is the wrong direction even with a tracker.
    global: {
      statements: 78,
      branches: 68,
      functions: 78,
      lines: 78,
    },
  },
  verbose: true,
};
