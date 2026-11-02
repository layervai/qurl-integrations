const { SQSClient } = require('@aws-sdk/client-sqs');
const { createSqsClient } = require('../../src/utils/sqs');

describe('createSqsClient', () => {
  const originalRegion = process.env.AWS_REGION;
  afterEach(() => {
    if (originalRegion === undefined) delete process.env.AWS_REGION;
    else process.env.AWS_REGION = originalRegion;
  });

  test.each([undefined, '', '   '])('rejects missing region %p with the caller label', (region) => {
    if (region === undefined) delete process.env.AWS_REGION;
    else process.env.AWS_REGION = region;
    for (const surface of ['event publisher', 'event consumer']) {
      expect(() => createSqsClient(surface)).toThrow(`AWS_REGION is required to use the ${surface}.`);
    }
  });

  test('reads and trims the region on each call after import', async () => {
    process.env.AWS_REGION = ' us-east-2 ';
    const first = createSqsClient('event publisher');
    process.env.AWS_REGION = ' eu-west-1 ';
    const second = createSqsClient('event consumer');
    try {
      expect(first).toBeInstanceOf(SQSClient);
      expect(second).toBeInstanceOf(SQSClient);
      expect(await first.config.region()).toBe('us-east-2');
      expect(await second.config.region()).toBe('eu-west-1');
    } finally {
      first.destroy();
      second.destroy();
    }
  });
});
