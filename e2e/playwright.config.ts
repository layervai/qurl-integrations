import { defineConfig, devices } from '@playwright/test';
import * as path from 'path';

const CI = !!process.env.CI;

export default defineConfig({
  testDir: './tests',
  timeout: 180_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: CI ? 2 : 0,
  forbidOnly: CI,
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: 'playwright-report' }],
    ['json', { outputFile: 'playwright-report/results.json' }],
    ['junit', { outputFile: 'playwright-report/junit.xml' }],
  ],
  // globalSetup disabled — login handled in fixtures via launchPersistentContext
  // globalSetup: require.resolve('./global-setup.ts'),
  use: {
    baseURL: 'https://discord.com',
    headless: CI ? true : !!process.env.HEADLESS,
    actionTimeout: 20_000,
    navigationTimeout: 45_000,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'retain-on-failure',
    viewport: { width: 1440, height: 900 },
    locale: 'en-US',
    timezoneId: 'America/Los_Angeles',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  outputDir: path.resolve(__dirname, 'test-results'),
});
