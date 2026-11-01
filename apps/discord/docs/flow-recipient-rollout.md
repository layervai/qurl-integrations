# Flow recipient format rollout

Issue #310 replaces the parallel recipient ID list and alias map with an ordered
`recipients: [{ id, alias }]` list. A 20,000-recipient encrypted list cannot fit in
DynamoDB's 400 KiB item limit. The reader must support bounded child items before
any worker writes the new format.

## Reader first

Deploy the reader to every Gateway, HTTP interaction and event worker. New flows
still use the legacy fields. Updates preserve the loaded format. The reader
accepts inline canonical lists and version-1 `__qurl_recipient_set` references.
Do not enable canonical creation until the complete worker fleet has this reader.
A three-minute flow lifetime alone does not make a mixed-fleet rollout safe:
active cards can extend their lifetime.

Recipient child items use the existing flow-state table and IAM permissions.
Each immutable set has a random ID, at most 100 chunks, and at most 256 recipients
per chunk. Recipient JSON is compressed before encryption. Each decompressed
chunk is limited to 128 KiB. The parent and encrypted chunks stay below 384 KiB.
The local proof covers the default 20,000-recipient cap, including long aliases;
it does not establish a higher supported cap or production throughput.

All child writes must finish before the parent is published. Reads use strong
consistency, four keys per batch, at most four concurrent batches, bounded retries
and a shared three-second deadline. Missing or incomplete sets fail closed.
There are no table scans, repair queues, or new cloud resources. Component actions
acknowledge Discord before loading children; modal and cancel routes read only
parent metadata. Mutations check the loaded encrypted payload and version so a
delayed action cannot change a deleted and recreated flow with the same version.

A lifetime extension stages a new immutable set with the new parent expiry.
Failed, superseded and cancelled sets retain that expiry for DynamoDB TTL cleanup.
Deleting the parent does not immediately erase child items; physical TTL deletion
can lag. Expired parent cleanup does not require child items to survive.

## Writer and compatibility removal

After reader deployment is confirmed, the writer change creates canonical flows
and converts legacy recipients on payload writes and lifetime-only extensions.
Persisted aliases are retained; absent legacy aliases stay empty so the renderer
can use its existing cache or ID fallback. Keep that change held until fleet evidence
is available. Check real create, repick, menu, modal, cancel, restart and confirm
journeys across worker boundaries before removing the legacy reader.

Remove legacy compatibility only after all old writers have stopped and the last
legacy flow's renewable lifetime and configured grace have elapsed. Do not infer
fleet adoption from merged source or green CI. No deployment or release is part
of the reader change.

## Local proof

Run `DDB_TEST_ENDPOINT=http://127.0.0.1:<port> node scripts/smoke-flow-recipients.js`
from `apps/discord` against an owned DynamoDB Local instance. It uses synthetic
recipients, a unique disposable table and fake AWS credentials. It makes no
Discord calls. The Discord workflow runs the same proof against its local service.
