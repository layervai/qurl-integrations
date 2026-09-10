# SDK 2.x and cleanup gates

Discord uses the published @layervai/qurl 2.0.0 for delegated batch creation,
batch reads, and delegated link revocation. The lockfile includes registry
integrity hashes for the SDK and its native state package. NHP stays at 1.1.

Run `npm ci`, `npm run lint`, and `npm test -- --runInBand` in apps/discord.

A lost POST response is retried only with the same idempotency key and body.
The SDK owns HTTP validation. Discord owns the shared interaction deadline,
recipient mapping, and bounded cleanup of identified links. SDK error details
are not sent to Discord or logs. Partial link IDs are copied into a writable
cleanup list.

An unreadable batch is not a failed batch. Logs retain the batch ID when
known, the original idempotency key, the authority expiry, and bounded groups
of known unrevoked IDs. They never contain capabilities or bearer links.
Cleaning up known IDs does not clear the unknown state. No links are sent
until the full mint result has been validated and persisted.

The service bounds each delegated grant by the signed authority expiry and
stops minting after that expiry. An unknown, undistributed grant therefore
has a bounded lifetime. A timeout does not cancel accepted work. Logs support
investigation, not durable replay: replay needs the exact capability and
grants. Batch cancellation would permit earlier cleanup but is not required
for this bounded-expiry safety model. Do not claim immediate cleanup when the
batch cannot be read or a revoke cannot be confirmed.

Cleanup and send revocation preserve upload parents. Public content
deduplication can share a parent with earlier sends. The Connector classifies
watermarked versus ordinary child tokens; it revokes its watermarked children,
and Discord uses the SDK to revoke ordinary children on the verified parent.
Missing identities or failed confirmation leave the send retryable. Private
cleanup targets delegated qURL IDs, even after the private flag is removed.
Upload handles are never passed to resource DELETE.

Service-owned watermark cleanup requires infrastructure PR #1553 and its
activation prerequisites. If an older Connector returns 404 for the revoke
route, ordinary children can still be revoked through the SDK: the lookup must
match the recorded source, and every per-child DELETE must succeed. A missing
route never proves cleanup. Hidden or foreign-parent children remain retryable;
never bypass that failure with parent deletion.

Keep PRIVATE_UPLOAD_QURL absent until infrastructure PRs #1529/#1530 and the
signed upload, mint, view, watermark, detect, revoke, and direct-origin-deny
sandbox journey pass. The HTTP integration tests use the real SDK against a
local server; they do not establish live private activation readiness.

Private activation changes every guild's send path. Existing guilds must run
`/qurl setup` to create a current external identity binding and key ID before
the flag is enabled. The prior credential alone cannot authorize private
grants. Inventory and migrate these bindings as part of activation.

The 20,000-recipient configuration ceiling is not a validated private send
size. Private batches share a 10-minute mint deadline; do not extend it past
the Discord interaction lifetime. Set QURL_SEND_MAX_RECIPIENTS from the live
file-size/fan-out test before activation, including cleanup under timeout.

A public re-setup cannot replace a stored external binding after flag rollback:
DynamoDB rejects that update atomically. Keep the existing bound credential
during rollback; this consumer does not rotate existing bindings. Binding DELETE on qurl-service
revokes its key atomically. The 24-hour authority bounds the offered 24-hour
links as well as batch work; shortening it to the interaction deadline would
shorten delivered links too. Unknown grants are not distributed.
