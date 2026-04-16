/**
 * Auth state persistence — save/restore browser storageState so we
 * don't re-login on every test run.
 */

import * as fs from 'fs';
import * as path from 'path';

const AUTH_DIR = path.resolve(__dirname, '..', 'auth');
const MAX_AGE_MS = 30 * 60 * 1_000; // 30 minutes

export function authFilePath(account: 'sender' | 'recipient'): string {
  return path.join(AUTH_DIR, `${account}.json`);
}

export function ensureAuthDir(): void {
  if (!fs.existsSync(AUTH_DIR)) {
    fs.mkdirSync(AUTH_DIR, { recursive: true });
  }
}

/**
 * Returns true if the stored auth state exists and is younger than MAX_AGE_MS.
 */
export function isAuthStateFresh(account: 'sender' | 'recipient'): boolean {
  const filePath = authFilePath(account);
  if (!fs.existsSync(filePath)) return false;

  try {
    const stat = fs.statSync(filePath);
    const ageMs = Date.now() - stat.mtimeMs;
    return ageMs < MAX_AGE_MS;
  } catch {
    return false;
  }
}

/**
 * Remove stored auth state, forcing a fresh login on next run.
 */
export function invalidateAuthState(account: 'sender' | 'recipient'): void {
  const filePath = authFilePath(account);
  if (fs.existsSync(filePath)) {
    fs.unlinkSync(filePath);
  }
}

/**
 * Remove all stored auth states.
 */
export function invalidateAllAuthStates(): void {
  invalidateAuthState('sender');
  invalidateAuthState('recipient');
}
