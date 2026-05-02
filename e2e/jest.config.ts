import type { Config } from 'jest';

const config: Config = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.ts'],
  setupFiles: ['dotenv/config'],
  testTimeout: 120_000,
  verbose: true,
  maxWorkers: 1, // Serial — shared Discord channel state
  // `default` keeps jest's stock console output for CI logs;
  // `discord-reporter` posts a per-file pass/fail rollup embed to the
  // test channel after every run (replaces the static "embed
  // delivery" signal that didn't reflect actual results). Reporter is
  // resilient: missing env vars / Discord errors warn but never throw,
  // so a broken reporter cannot turn a green run red.
  // `.js` (not `.ts`): Jest's reporter loader uses Node `require()`
  // directly with no ts-jest hook, so a TS reporter SyntaxErrors at
  // load. See discord-reporter.js header for the full rationale.
  reporters: ['default', '<rootDir>/helpers/discord-reporter.js'],
};

export default config;
