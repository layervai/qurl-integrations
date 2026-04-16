/**
 * Environment sanity checker — validates all required secrets and config
 * are present before running E2E tests.
 */

import { loadEnv } from '../helpers/env';
import * as fs from 'fs';
import * as path from 'path';

interface CheckResult {
  name: string;
  ok: boolean;
  message: string;
}

async function runChecks(): Promise<CheckResult[]> {
  const results: CheckResult[] = [];

  // Check environment variables
  try {
    const env = loadEnv();
    results.push({
      name: 'Environment variables',
      ok: true,
      message: `All required env vars present (guild=${env.DISCORD_GUILD_ID}, channel=${env.DISCORD_CHANNEL_ID})`,
    });

    // Check optional TOTP
    if (env.DISCORD_SENDER_TOTP_SECRET) {
      results.push({ name: 'Sender TOTP', ok: true, message: 'TOTP secret configured' });
    } else {
      results.push({ name: 'Sender TOTP', ok: true, message: 'No TOTP (2FA not required)' });
    }

    if (env.DISCORD_RECIPIENT_TOTP_SECRET) {
      results.push({ name: 'Recipient TOTP', ok: true, message: 'TOTP secret configured' });
    } else {
      results.push({ name: 'Recipient TOTP', ok: true, message: 'No TOTP (2FA not required)' });
    }
  } catch (error) {
    results.push({
      name: 'Environment variables',
      ok: false,
      message: `${error}`,
    });
  }

  // Check data files
  const dataDir = path.resolve(__dirname, '..', 'data');
  const requiredDataFiles = ['locations.json', 'names.json', 'mime-matrix.json'];

  for (const file of requiredDataFiles) {
    const filePath = path.join(dataDir, file);
    if (fs.existsSync(filePath)) {
      const data = JSON.parse(fs.readFileSync(filePath, 'utf-8'));
      results.push({
        name: `Data file: ${file}`,
        ok: true,
        message: `${Array.isArray(data) ? data.length : 'N/A'} entries`,
      });
    } else {
      results.push({
        name: `Data file: ${file}`,
        ok: false,
        message: 'File not found',
      });
    }
  }

  // Check test files
  const filesDir = path.join(dataDir, 'files');
  if (fs.existsSync(filesDir)) {
    const files = fs.readdirSync(filesDir).filter((f) => f !== '.gitkeep');
    results.push({
      name: 'Test files',
      ok: files.length > 0,
      message: files.length > 0 ? `${files.length} test files generated` : 'No test files — run: npm run generate-files',
    });
  } else {
    results.push({
      name: 'Test files',
      ok: false,
      message: 'files/ directory not found — run: npm run generate-files',
    });
  }

  // Check Playwright
  try {
    require('@playwright/test');
    results.push({ name: 'Playwright', ok: true, message: 'Installed' });
  } catch {
    results.push({ name: 'Playwright', ok: false, message: 'Not installed — run: npm install' });
  }

  return results;
}

async function main(): Promise<void> {
  console.log('QURL Discord Bot E2E — Setup Check\n');
  console.log('='.repeat(60));

  const results = await runChecks();
  let allOk = true;

  for (const r of results) {
    const icon = r.ok ? 'PASS' : 'FAIL';
    console.log(`  [${icon}] ${r.name}: ${r.message}`);
    if (!r.ok) allOk = false;
  }

  console.log('\n' + '='.repeat(60));

  if (allOk) {
    console.log('All checks passed. Ready to run E2E tests.\n');
    process.exit(0);
  } else {
    console.log('Some checks failed. Fix the issues above before running tests.\n');
    process.exit(1);
  }
}

main();
