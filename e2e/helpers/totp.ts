/**
 * TOTP generation for Discord 2FA login.
 */

import { authenticator } from 'otplib';

/**
 * Generate a TOTP code from a secret.
 * @param secret - Base32-encoded TOTP secret
 * @returns 6-digit TOTP code
 */
export function generateTotp(secret: string): string {
  return authenticator.generate(secret);
}

/**
 * Generate a TOTP code, waiting until we're at least 5s from the next
 * window boundary to avoid races.
 */
export async function generateFreshTotp(secret: string): Promise<string> {
  const remaining = authenticator.timeRemaining();

  // If fewer than 5 seconds left in this window, wait for next window
  if (remaining < 5) {
    await new Promise((resolve) => setTimeout(resolve, (remaining + 1) * 1_000));
  }

  return authenticator.generate(secret);
}
