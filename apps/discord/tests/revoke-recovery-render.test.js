const { buildRevokeHeader, renderRevokeContent, REVOKE_CONTENT_SAFE_MAX } = require('../src/revoke-render');
const sendId = '12345678-1234-1234-1234-123456789abc';

test('unconfirmed results expose a safe recovery ID and no false deletion claim', () => {
  const content = buildRevokeHeader(0, 1, { sendId });
  expect(content).toContain(sendId);
  expect(content).toContain('action inspect, retry, or acknowledge');
  expect(content).toContain('Acknowledgement does not confirm deletion.');
});

test('a confirmed result needs no acknowledgement guidance', () => {
  expect(buildRevokeHeader(1, 1, { sendId })).not.toContain(sendId);
});

test('finalization failure still provides recovery even with zero recipients', () => {
  expect(buildRevokeHeader(0, 0, { sendId, finalizationFailed: true })).toContain(sendId);
});

test('untrusted non-UUID IDs never enter the rendered guidance', () => {
  const malicious = 'bad-id\n@everyone';
  expect(buildRevokeHeader(0, 1, { sendId: malicious })).not.toContain(malicious);
});

test('the recovery header is included when computing recipient-list overflow', () => {
  const rendered = renderRevokeContent({ sendId, success: 200, total: 201, showAll: true,
    names: Array.from({ length: 200 }, (_, i) => `Recipient ${i} with a long display name`),
  });
  expect(rendered.content).toContain(sendId);
  expect(rendered.content.length).toBeLessThanOrEqual(REVOKE_CONTENT_SAFE_MAX);
  expect(rendered.attachmentText).toContain('Recipient 199');
  expect(rendered.needsExpand).toBe(false);
});
