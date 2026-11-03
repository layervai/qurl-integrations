const path = require('node:path');

module.exports = function bootEnvironment(directory, overrides = {}) {
  const env = {
    PATH: process.env.PATH,
    NODE_ENV: 'production',
    LOG_LEVEL: 'error',
    DISCORD_TOKEN: 'synthetic-discord-token',
    METRICS_TOKEN: 'synthetic-metrics-token',
    KEY_ENCRYPTION_KEY: Buffer.alloc(32, 7).toString('base64'),
    QURL_OAUTH_STATE_SECRET: 's'.repeat(64),
    QURL_WEBHOOK_PURE_BYOK: 'true',
    AUTH0_DOMAIN: 'tenant.us.auth0.com',
    AUTH0_CLIENT_ID: 'synthetic-client',
    AUTH0_CLIENT_SECRET: 'synthetic-client-secret',
    AUTH0_AUDIENCE: 'https://api.example.test',
    BASE_URL: 'https://bot.example.test',
    QURL_ENDPOINT: 'https://api.example.test',
    CONNECTOR_URL: 'https://connector.example.test',
    DDB_TABLE_PREFIX: 'boot-test-',
    AWS_CONFIG_FILE: path.join(directory, 'unused-aws-config'),
    AWS_SHARED_CREDENTIALS_FILE: path.join(directory, 'unused-aws-credentials'),
    AWS_REGION: 'us-east-2',
    AWS_ACCESS_KEY_ID: 'synthetic',
    AWS_SECRET_ACCESS_KEY: 'synthetic',
    ...overrides,
  };
  for (const key of Object.keys(env)) if (env[key] === undefined) delete env[key];
  return env;
};
