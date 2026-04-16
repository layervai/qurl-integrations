/**
 * Environment configuration for API-based E2E tests.
 */

export interface E2EEnv {
  BOT_TOKEN: string;
  BOT_CLIENT_ID: string;
  QURL_API_KEY: string;
  UPLOAD_API_URL: string;
  MINT_API_URL: string;
  GUILD_ID: string;
  CHANNEL_ID: string;
}

const REQUIRED: (keyof E2EEnv)[] = [
  'BOT_TOKEN', 'BOT_CLIENT_ID', 'QURL_API_KEY',
  'UPLOAD_API_URL', 'MINT_API_URL', 'GUILD_ID', 'CHANNEL_ID',
];

export function loadEnv(): E2EEnv {
  const env: Record<string, string | undefined> = {
    BOT_TOKEN: process.env.BOT_TOKEN,
    BOT_CLIENT_ID: process.env.BOT_CLIENT_ID,
    QURL_API_KEY: process.env.QURL_API_KEY,
    UPLOAD_API_URL: process.env.UPLOAD_API_URL,
    MINT_API_URL: process.env.MINT_API_URL,
    GUILD_ID: process.env.GUILD_ID,
    CHANNEL_ID: process.env.CHANNEL_ID,
  };
  const missing = REQUIRED.filter((k) => !env[k]);
  if (missing.length > 0) {
    throw new Error(`Missing env vars: ${missing.join(', ')}`);
  }
  return env as unknown as E2EEnv;
}
