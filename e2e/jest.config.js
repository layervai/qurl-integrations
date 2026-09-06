/** @type {import('jest').Config} */
module.exports = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.ts'],
  testTimeout: 120_000,
  verbose: true,
  maxWorkers: 1,
  reporters: ['default', '<rootDir>/helpers/discord-reporter.js'],
};
