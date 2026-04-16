/**
 * Environment configuration for E2E tests.
 * All secrets come from process.env (populated by CI or .env locally).
 */

export interface E2EEnv {
  /** Discord email for the sender account */
  DISCORD_SENDER_EMAIL: string;
  /** Discord password for the sender account */
  DISCORD_SENDER_PASSWORD: string;
  /** TOTP secret for sender account (if 2FA enabled) */
  DISCORD_SENDER_TOTP_SECRET?: string;
  /** Discord email for the recipient account */
  DISCORD_RECIPIENT_EMAIL: string;
  /** Discord password for the recipient account */
  DISCORD_RECIPIENT_PASSWORD: string;
  /** TOTP secret for recipient account (if 2FA enabled) */
  DISCORD_RECIPIENT_TOTP_SECRET?: string;
  /** Discord server (guild) ID for testing */
  DISCORD_GUILD_ID: string;
  /** Discord channel ID where bot commands are sent */
  DISCORD_CHANNEL_ID: string;
  /** Bot application ID */
  DISCORD_BOT_APP_ID?: string;
  /** Recipient Discord username (for DM lookups) */
  DISCORD_RECIPIENT_USERNAME: string;
  /** Whether running in CI */
  CI?: string;
  /** Run headless even locally */
  HEADLESS?: string;
}

const REQUIRED_KEYS: (keyof E2EEnv)[] = [
  'DISCORD_SENDER_EMAIL',
  'DISCORD_SENDER_PASSWORD',
  'DISCORD_RECIPIENT_EMAIL',
  'DISCORD_RECIPIENT_PASSWORD',
  'DISCORD_GUILD_ID',
  'DISCORD_CHANNEL_ID',
  'DISCORD_RECIPIENT_USERNAME',
];

export function loadEnv(): E2EEnv {
  const missing = REQUIRED_KEYS.filter((k) => !process.env[k]);
  if (missing.length > 0) {
    throw new Error(
      `Missing required environment variables for E2E tests:\n  ${missing.join('\n  ')}\n` +
        'Set them in .env or pass via CI secrets.',
    );
  }

  return {
    DISCORD_SENDER_EMAIL: process.env.DISCORD_SENDER_EMAIL!,
    DISCORD_SENDER_PASSWORD: process.env.DISCORD_SENDER_PASSWORD!,
    DISCORD_SENDER_TOTP_SECRET: process.env.DISCORD_SENDER_TOTP_SECRET,
    DISCORD_RECIPIENT_EMAIL: process.env.DISCORD_RECIPIENT_EMAIL!,
    DISCORD_RECIPIENT_PASSWORD: process.env.DISCORD_RECIPIENT_PASSWORD!,
    DISCORD_RECIPIENT_TOTP_SECRET: process.env.DISCORD_RECIPIENT_TOTP_SECRET,
    DISCORD_GUILD_ID: process.env.DISCORD_GUILD_ID!,
    DISCORD_CHANNEL_ID: process.env.DISCORD_CHANNEL_ID!,
    DISCORD_BOT_APP_ID: process.env.DISCORD_BOT_APP_ID,
    DISCORD_RECIPIENT_USERNAME: process.env.DISCORD_RECIPIENT_USERNAME!,
    CI: process.env.CI,
    HEADLESS: process.env.HEADLESS,
  };
}
