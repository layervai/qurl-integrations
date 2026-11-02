const crypto = require('crypto');

module.exports = function attemptHeaders(raw, secret = 'test-qurl-secret', timestamp = String(Math.floor(Date.now() / 1000))) {
  return {
    'QURL-Timestamp': timestamp,
    'QURL-Signature-V2': crypto.createHmac('sha256', secret).update(`v2.${timestamp}.`).update(raw).digest('hex'),
  };
};
