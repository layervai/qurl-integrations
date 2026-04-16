/**
 * Auth state management using persistent browser profiles (user-data-dir).
 * Discord stores auth tokens in IndexedDB, which Playwright's storageState
 * doesn't capture. Persistent profiles preserve everything.
 */

import * as fs from 'fs';
import * as path from 'path';

const AUTH_DIR = path.resolve(__dirname, '..', 'auth');
const PROFILE_TTL_MS = 6 * 60 * 60 * 1000; // 6 hours

export function profileDir(account: 'sender' | 'recipient'): string {
  return path.join(AUTH_DIR, `${account}-profile`);
}

export function authFilePath(account: 'sender' | 'recipient'): string {
  return path.join(AUTH_DIR, `${account}.json`);
}

export function isProfileFresh(account: 'sender' | 'recipient'): boolean {
  const dir = profileDir(account);
  try {
    const stat = fs.statSync(dir);
    return stat.isDirectory() && (Date.now() - stat.mtimeMs < PROFILE_TTL_MS);
  } catch {
    return false;
  }
}

export function isAuthStateFresh(account: 'sender' | 'recipient'): boolean {
  return isProfileFresh(account);
}

export function ensureAuthDir(): void {
  fs.mkdirSync(AUTH_DIR, { recursive: true });
}

export function invalidateAuthState(account: 'sender' | 'recipient'): void {
  const dir = profileDir(account);
  if (fs.existsSync(dir)) {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

export function invalidateAllAuthStates(): void {
  invalidateAuthState('sender');
  invalidateAuthState('recipient');
}
