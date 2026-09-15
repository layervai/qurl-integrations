import { constantTimeEqual } from './encoding.js';
import { OAuthCoreError } from './errors.js';
import { isOpaqueStateHandle } from './state.js';

export const OAUTH_STATE_COOKIE_NAME = 'qurl_teams_oauth_state';
export const OAUTH_COOKIE_PATH = '/oauth/qurl';
export const OAUTH_COOKIE_MAX_AGE_SECONDS = 5 * 60;

export interface OAuthStateCookie {
  readonly name: typeof OAUTH_STATE_COOKIE_NAME;
  readonly value: string;
  readonly secure: true;
  readonly httpOnly: true;
  readonly sameSite: 'Lax';
  readonly path: typeof OAUTH_COOKIE_PATH;
  readonly maxAgeSeconds: typeof OAUTH_COOKIE_MAX_AGE_SECONDS;
}

export interface ClearedOAuthStateCookie extends Omit<OAuthStateCookie, 'value' | 'maxAgeSeconds'> {
  readonly value: '';
  readonly maxAgeSeconds: 0;
}

export function createOAuthStateCookie(handle: string): OAuthStateCookie {
  if (!isOpaqueStateHandle(handle)) {
    throw new OAuthCoreError('INVALID_STATE', 'OAuth state is invalid.');
  }
  return {
    name: OAUTH_STATE_COOKIE_NAME,
    value: handle,
    secure: true,
    httpOnly: true,
    sameSite: 'Lax',
    path: OAUTH_COOKIE_PATH,
    maxAgeSeconds: OAUTH_COOKIE_MAX_AGE_SECONDS,
  };
}

export function clearOAuthStateCookie(): ClearedOAuthStateCookie {
  return {
    name: OAUTH_STATE_COOKIE_NAME,
    value: '',
    secure: true,
    httpOnly: true,
    sameSite: 'Lax',
    path: OAUTH_COOKIE_PATH,
    maxAgeSeconds: 0,
  };
}

export function verifyDoubleSubmitCookie(cookieValue: string | undefined, callbackState: string): void {
  if (!cookieValue || !isOpaqueStateHandle(cookieValue) || !isOpaqueStateHandle(callbackState)) {
    throw new OAuthCoreError('COOKIE_MISMATCH', 'OAuth state cookie did not match the callback.');
  }
  if (!constantTimeEqual(cookieValue, callbackState)) {
    throw new OAuthCoreError('COOKIE_MISMATCH', 'OAuth state cookie did not match the callback.');
  }
}
