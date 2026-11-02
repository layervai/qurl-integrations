const { SQSClient } = require('@aws-sdk/client-sqs');

function createSqsClient(surface) {
  // Resolve at start, not import time; SDK credentials stay lazy until send.
  const region = (process.env.AWS_REGION ?? '').trim();
  if (!region) {
    throw new Error(`AWS_REGION is required to use the ${surface}. Set it in the deployment template (e.g. \`us-east-2\`).`);
  }
  return new SQSClient({ region });
}

module.exports = { createSqsClient };
