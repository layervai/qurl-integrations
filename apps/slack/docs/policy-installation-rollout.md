# Policy installation rollout

Policy writes capture the current admin, owner, authorization generation, and
installation before upstream work. Each mutation checks that snapshot in the
same DynamoDB transaction as the policy change. Ownership transfer changes the
authorization generation and preserves the installation and its configuration.
A new bind creates a new installation. If ownership transfer reaches an old
binding before its first policy write, the owner gate initializes the legacy
identity before the Slack target lookup. The transfer uses that same captured
identity at commit and never obtains fresh authority after the lookup.

Admin membership writes and agent-toggle writes also carry the first mutation
gate's snapshot. Ownership transfer also requires a captured owner role and
the original generation/installation at commit. Agent status and usage replies stay read-only. The mutation
condition and current caller/owner/generation/installation checks run in one
workspace-row update. Revocation or reinstall during a command cannot authorize
an old admin to change the new roster. Missing authorization has no direct-write
fallback.

Binding age is separate from edit time. Fresh binds and legacy reclaims set
`policy_bound_at_unix_nano`. Admin changes, ownership transfer, and agent toggles
preserve that value while updating the audit timestamps. Their first mutation
of a legacy row preserves its prior binding cutoff before changing edit time.
A late settings update must not make an old binding appear freshly installed.
Bindings which still lack the new field use the existing legacy cutoff rule.
Historical ambiguous rows still require the reconciliation described below.

## Default behavior

`QURL_SLACK_LEGACY_POLICY_WRITERS_RETIRED` defaults to `false`. An invalid boolean
prevents the admin store from starting. Keep this switch unset during a mixed
version deployment.

An active binding created before installation IDs existed receives its IDs once
at its first policy authorization. It receives `policy_legacy=true`. Its existing
untagged policy rows remain part of that active installation; a normal policy
write stamps the row without removing its grants or aliases. This initialization
does not move the binding's lifecycle cutoff timestamp.

A new binding never adopts an untagged row. While writer retirement is unconfirmed,
a write to that row fails closed with `policy_reconciliation_required`. This
requires operator reconciliation; repeating the command cannot repair it. A
lifecycle job retains the row and returns
`policy_purge_legacy_ownership_unknown`, so the durable queue can retry. Do not
acknowledge that job or discard it as a permanent failure.

A row with a known previous installation ID can be cleared under the captured
current authority and an exact old-row identity check. The requested mutation
then retries once. It does not copy prior grants into the new installation.

## Confirm retirement before enabling legacy reconciliation

Use the normal candidate and blue/green deployment path. Test the guarded build
with this switch unset. Record the exact image, task revisions, table identities,
and candidate journey results. A green build or a successful task start does not
establish writer retirement.

Before setting the switch to `true` in the normal deployment configuration:

1. Verify that every process with policy-write access runs the guarded build.
   Include HTTP handlers, async workers, operator tools, and any retained tasks.
2. Verify that no pre-guard process can accept new work. Confirm that its tasks
   and in-flight writes have ended. A routing change or an elapsed timer alone
   is insufficient. The current async budget is 25 seconds, but verify the actual
   versions and tasks being retired rather than assuming that budget applies.
3. Keep active pre-upgrade bindings classified as legacy. Do not reset their
   binding IDs or remove `policy_legacy` to force cleanup.
4. Rehearse uninstall/reinstall with an old untagged row, an active legacy row,
   and a concurrent fresh write. Verify both the durable job result and the
   stored policy. Record the deployed IAM transaction result as well.

Only then enable the switch and run the same checks through normal deployment.
This file does not authorize a deployment or an environment change.

With retirement confirmed, guarded writers cannot create an untagged policy row.
An untagged row under a new, non-legacy binding therefore belongs to a retired
writer. Cleanup may delete it while checking that the new binding is unchanged
and that the row is still untagged. A new write may perform the same guarded
cleanup and retry once. A concurrently stamped row wins and is preserved.
Active legacy bindings continue to retain their untagged configuration.

## Retry and rollback

Unknown transaction failures remain errors. There is no direct-write fallback.
A failed cleanup or final retry does not obtain a new authorization snapshot.
Retry the command from a fresh user action or let the durable cleanup job retry.

To stop legacy reconciliation, deploy the guarded build with the switch false
and verify that all tasks which loaded `true`, including their in-flight work,
have ended. Ambiguous rows then stay queued again. Do not reintroduce pre-guard
writers alongside a guarded cleanup worker. Prefer a guarded forward fix; an old
binary rollback can restore the original write-after-purge race.

Inspect and retry retained jobs after recovery. Do not manually tag an ambiguous
row as current or delete it based only on its wall-clock timestamp. Historical
rows which cannot be attributed require a separate operator reconciliation based
on current binding and upstream resource evidence.

Mutating requests validate their captured workspace authority before and after API key lookup. This prevents a revoked caller or an old installation from selecting a replacement installation credential. Display-name changes carry the same captured scope as URL creation. Ordinary read contexts do not add these workspace reads. This check cannot cancel an upstream request that was already sent before a later revocation.

## Statecrawl apply

Dry runs remain read-only. Apply captures the current workspace owner before
reading and changing a policy. It cannot repair an unbound workspace; use the
durable lifecycle cleanup path for that residue. Applying to an active legacy
binding can initialize its generation, installation and legacy marker in the
workspace mapping. This preserves its policy ownership and binding cutoff.

The operator role needs workspace mapping reads and `UpdateItem` for legacy
initialization, plus transaction permissions for a workspace `ConditionCheckItem`
and policy `UpdateItem`/`DeleteItem`. AWS authorizes each action in the
transaction against its table. Verify the actual operator role as well as the
bot task role before apply. Do not assume a bot IAM receipt covers statecrawl.

## Reclaiming pre-pivot ownership

A setup reclaim of a legacy Auth0-style owner is a new installation, not an
ownership transfer. It does not adopt the previous installation's channel
grants or aliases. Untagged rows require operator reconciliation while old
writers may still exist. Previously tagged rows belong to the retired
installation and can be cleared atomically before a new policy write. The
operator must review and recreate any intended configuration under the new
owner; reclaim does not silently carry it forward. Preserve this distinction
when planning legacy migrations.

## Read isolation

Policy authorization and alias listing read the workspace binding and channel
policy in one `TransactGetItems` snapshot. A retired installation, an absent
binding, or an untagged row under a new non-legacy binding supplies no grants
or aliases. This hides ambiguous data without deleting it; the durable cleanup
job and operator reconciliation still remain necessary. Active legacy bindings
can read their existing untagged configuration. Malformed identity and unknown
transaction errors fail closed. A mutating context must also match the binding
in this snapshot; it cannot follow a reinstall into new configuration.

This replaces each policy GetItem with one bounded two-item request. It adds no
per-user cache, global coordinator, or separate workspace-read round trip.
Ordinary reads do not project the admin roster. Transactional reads still
increase read capacity use and can conflict with writes. Verify real task and
operator GetItem permissions on both tables, the native paired-read result,
and deployed read capacity, conflict rate and customer latency before rollout.
Local emulator success does not establish AWS IAM or production capacity.

Policy reads retry only explicit two-item transaction conflicts, at most twice,
with 10–20 ms jitter between calls and the caller's deadline. Unknown reasons,
validation errors and throttling are not retried by this read loop. Persistent
conflicts still fail closed. Measure workspace-wide conflict rates and latency;
this bounded retry does not replace the rollout capacity gate.
