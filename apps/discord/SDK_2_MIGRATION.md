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

An unreadable batch is not a failed batch. Failure logs retain the batch ID
when known and the original idempotency key. Cleaning up all known IDs does
not clear the unknown-outcome state. The user is told that no new links were
sent and cleanup is unconfirmed. These logs are investigation evidence, not a
durable recovery queue. Without a batch ID, the key alone cannot replay the
request: replay also requires the exact capability and grants, which are
intentionally not logged. The service must provide durable recovery or
cancellation before private activation. A client timeout does not cancel work.

Do not delete an upload parent to compensate a failed send. Public upload
content deduplication can return a parent shared with an earlier send. Current
public cleanup still deletes whole resources; safe operation-scoped child
cleanup remains a merge gate. PR #1420 and infrastructure PR #1553 address
watermarked child revocation but must also preserve shared parents during
failure cleanup. Private cleanup targets delegated qURL IDs only; upload
handles must never be passed to resource DELETE.

Keep PRIVATE_UPLOAD_QURL absent. The signed upload, mint, view, watermark,
detect, revoke, and direct-origin-deny sandbox journey has not been verified
for this head. Do not treat unit tests or green CI as private activation approval.
