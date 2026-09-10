
process.env.DDB_TABLE_PREFIX = process.env.DDB_TABLE_PREFIX || 'jest-test-';
process.env.AWS_REGION = process.env.AWS_REGION || 'us-east-1';

process.env.OAUTH_STATE_SECRET = process.env.OAUTH_STATE_SECRET || '0'.repeat(64);
