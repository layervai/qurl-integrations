import type { Config } from 'jest';

const config: Config = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/tests/**/*.test.ts'],
  setupFiles: ['dotenv/config'],
  testTimeout: 120_000,
  verbose: true,
  maxWorkers: 1, // Serial — shared Discord channel state
};

export default config;
