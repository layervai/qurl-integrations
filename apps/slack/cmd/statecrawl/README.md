# statecrawl

An operational reconciler for the qURL™ Slack bot's `channel_policies` DynamoDB
table. It crawls every `(team, channel)` policy row and cross-references each
referenced resource against the owning workspace's live qURL resources, then
reports — and optionally repairs — the state that PRs **#654** and **#669**
leave us in.

## Why this exists

- **#654** (merged) taught the bot to cascade `PurgeResourceFromChannel` on
  every revoke, so destroying a resource no longer orphans a channel `$alias`.
  It does **not** backfill orphans that already exist — pre-fix revokes, or
  out-of-band deletes (API / SDK / MCP / CLI / token expiry) the bot never
  observed. `statecrawl` finds those orphans (an `$alias`, or an
  `allowed_resource_ids` member, still pointing at a revoked/deleted resource)
  and, with `-apply`, clears them through the **same** `PurgeResourceFromChannel`
  verb the bot now runs — so the manual backfill is behaviorally identical to
  the live cascade.

- **#669** (open) taught `set/unset-display-name` to resolve a channel `$alias`
  whose name differs from the connector slug. `statecrawl` flags those live
  `name ≠ slug` bindings so you can see which connectors became
  display-name-targetable by alias. These are a **healthy, live** state and are
  **never** purged — informational only.

## Safety

- **Dry run is the default** and makes zero mutations. It only reads: DynamoDB
  `Scan`/`GetItem`, KMS `Decrypt` (for the per-workspace API key), and
  `GET /v1/resources`.
- `-apply` performs the orphan purge. It only ever touches the two surfaces
  `PurgeResourceFromChannel` owns (`allowed_resource_ids` + `alias_bindings`),
  and only for resources **confirmed** revoked/deleted against a fully paginated
  resource list. A workspace whose API key can't be resolved, or whose resource
  list fails to load, is reported as `indeterminate` and is **never** purged.

## Configuration

Each value comes from an env var (the same ones the bot uses) or its `-flag`
override. Run the tool **once per deployment**: point the env at sandbox, run a
dry run, review, optionally `-apply`; then repeat for prod. `-env` only stamps a
label into the report and the apply banner — it does **not** pick tables.

| Env var                         | Flag                         | Purpose                                            |
| ------------------------------- | ---------------------------- | -------------------------------------------------- |
| `QURL_CHANNEL_POLICIES_TABLE`   | `-channel-policies-table`    | `channel_policies` table to crawl                  |
| `QURL_WORKSPACE_MAPPINGS_TABLE` | `-workspace-mappings-table`  | `workspace_mappings` table (constructs the Store)  |
| `WORKSPACE_STATE_TABLE`         | `-workspace-state-table`     | per-workspace qURL API key table                   |
| `WORKSPACE_STATE_KMS_KEY_ARN`   | `-kms-key-arn`               | CMK that envelope-encrypts the API key column      |
| `QURL_ENDPOINT`                 | `-qurl-endpoint`             | qurl-service base URL for the liveness check       |
|                                 | `-env`                       | report/banner label (e.g. `sandbox`, `prod`)       |
|                                 | `-team`                      | restrict the crawl to one Slack `team_id`          |
|                                 | `-page-limit`                | `/v1/resources` page size (default 100)            |
|                                 | `-apply`                     | perform the purge (default: read-only dry run)     |

AWS credentials and region come from the ambient environment. The role needs
`dynamodb:Scan`/`GetItem` on both tables, `kms:Decrypt` on the CMK, and — for
`-apply` — `dynamodb:UpdateItem` on `channel_policies`.

## Usage

```sh
# Dry run against sandbox (env already points at the sandbox deployment):
go run ./apps/slack/cmd/statecrawl -env sandbox

# Scope to one workspace:
go run ./apps/slack/cmd/statecrawl -env sandbox -team T0123ABCD

# Repair the confirmed orphans after reviewing the dry run:
go run ./apps/slack/cmd/statecrawl -env sandbox -apply

# Then repeat for prod once sandbox looks right.
```

## Output

Each finding is one grep-friendly line:

```
[orphan-alias] team=T1 channel=C1 alias=$dashboard resource=r_dead — alias bound to a revoked/deleted resource — #654 purge target
[alias-name-mismatch] team=T1 channel=C2 alias=$dash resource=r_live — live tunnel reachable by alias whose name differs from slug "stats-connector" — #669 makes this display-name targetable
```

Finding kinds:

| Kind                  | Meaning                                                            | `-apply` purges? |
| --------------------- | ----------------------------------------------------------------- | ---------------- |
| `orphan-alias`        | `$alias` bound to a revoked/deleted resource (#654 backfill)      | **yes**          |
| `orphan-allowed-id`   | `allowed_resource_ids` member that is revoked/deleted (#654)      | **yes**          |
| `alias-name-mismatch` | live tunnel reachable by an alias whose name ≠ slug (#669)        | no               |
| `alias-url-target`    | `$alias` bound to a live URL resource (not display-name-able)     | no               |
| `legacy-alias`        | `$alias` whose value is a non-`r_` legacy raw-URL binding         | no               |
| `indeterminate`       | team's liveness couldn't be verified (no key / list failed)       | no               |
