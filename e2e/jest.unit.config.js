/** @type {import('jest').Config} */
module.exports = {
  preset: 'ts-jest',
  testEnvironment: 'node',
  testMatch: ['**/unit/**/*.test.ts'],
  verbose: true,
  reporters: ['default'],
};
