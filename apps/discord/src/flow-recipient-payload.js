const RECIPIENT_SET_FIELD = '__qurl_recipient_set';

// Read both formats before any worker starts creating canonical recipient lists.
function readFlowRecipients(payload) {
  if (Object.hasOwn(payload, RECIPIENT_SET_FIELD)) throw new TypeError('Flow recipients must be hydrated before use');
  if (Object.hasOwn(payload, 'recipients')) {
    if (!Array.isArray(payload.recipients) || payload.recipients.some(recipient => !recipient
        || typeof recipient.id !== 'string' || !recipient.id || typeof recipient.alias !== 'string')) {
      throw new TypeError('Flow recipients must be an array of id/alias objects');
    }
    return payload.recipients;
  }
  const aliases = payload.recipientAliases || {};
  return (Array.isArray(payload.recipientIds) ? payload.recipientIds : [])
    .map(id => ({ id, alias: aliases[id] }));
}

function withFlowRecipients(payload, recipients) {
  readFlowRecipients({ recipients });
  const next = { ...payload, recipients };
  delete next.recipientIds;
  delete next.recipientAliases;
  return next;
}

module.exports = { RECIPIENT_SET_FIELD, readFlowRecipients, withFlowRecipients };
