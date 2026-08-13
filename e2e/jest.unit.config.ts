import type { Config } from 'jest';

const config: Config = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/unit/**/*.test.ts'],
  verbose: true,
  reporters: ['default'],
};

export default config;
