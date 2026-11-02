# Policy installation rollout

Policy writes capture the current admin, owner, authorization generation, and
installation before upstream work. Each mutation checks that snapshot in the
same DynamoDB transaction as the policy change. Ownership transfer changes the
authorization generation and preserves the installation and its configuration.
A new bind creates a new installation.

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
a write to that row fails closed. A lifecycle job retains the row and returns
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
