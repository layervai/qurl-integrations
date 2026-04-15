module.exports = {
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.js'],
  collectCoverageFrom: ['src/**/*.js', '!src/index.js', '!src/loadtest-standalone.js'],
  coverageDirectory: 'coverage',
  verbose: true,
};
