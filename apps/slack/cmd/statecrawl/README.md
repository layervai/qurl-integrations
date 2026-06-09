# statecrawl

An operational reconciler for the qURL™ Slack bot's `channel_policies` DynamoDB
table. It crawls every `(team, channel)` policy row and cross-references each
referenced resource against the owning workspace's live qURL resources, then
reports — and optionally repairs — the state that PRs **#654** and **#669**
leave us in.

It deliberately mirrors the structure and safety model of qurl-service's
operational CLIs (`cmd/qurl-scanner`, `cmd/qurl-bucket-backfill`): a
dry-run-by-default reconciler with parse-time **safety rails**, an
atomic-counter `Stats`/`Snapshot` summary, and structured slog output so every
finding and every mutation is an auditable record.

## Why this exists

- **#654** (merged) taught the bot to cascade `PurgeResourceFromChannel` on
  every revoke, so destroying a resource no longer orphans a channel `$alias`.
  It does **not** backfill orphans that already exist — pre-fix revokes, or
  out-of-band deletes (API / SDK / MCP / CLI / token expiry) the bot never
  observed. `statecrawl` finds those orphans (an `$alias`, or an
  `allowed_resource_ids` member, still pointing at a revoked/deleted resource)
  and, in a mutating run, clears them through the **same**
  `PurgeResourceFromChannel` verb the bot now runs — so the manual backfill is
  behaviorally identical to the live cascade. **This is the lever for unblocking
  a customer** stuck in the contradictory "alias already bound, yet `/qurl list`
  shows nothing" state.

- **#669** (open) taught `set/unset-display-name` to resolve a channel `$alias`
  whose name differs from the connector slug. `statecrawl` flags those live
  `name ≠ slug` bindings so you can see which connectors became
  display-name-targetable by alias. These are a **healthy, live** state and are
  **never** purged — informational only.

## Safety rails

- **`-dry-run` defaults to `true`** and makes zero mutations. It only reads:
  DynamoDB `Scan`/`GetItem`, KMS `Decrypt` (for the per-workspace API key), and
  `GET /v1/resources`. It logs exactly what it **would** purge
  (`dry_run_would_purge` in the summary).
- **Mutating requires `-dry-run=false`.** It only ever touches the two surfaces
  `PurgeResourceFromChannel` owns (`allowed_resource_ids` + `alias_bindings`),
  and only for resources **confirmed** revoked/deleted against a fully paginated
  resource list.
- **Prod purge requires an explicit opt-in.** A mutating run against a
  prod-looking deployment — the `-env` label says `prod`/`production`, **or** any
  resolved table name contains `prod` (defense-in-depth) — is refused unless you
  pass `-allow-prod-purge`. The reject error names the flag.
- **Indeterminate is never purged.** A workspace whose API key can't be resolved,
  or whose resource list fails to load, is reported `indeterminate` and skipped.
- A non-zero `purge_errors` in the summary is **ALERTABLE** and makes the process
  exit non-zero; the purge is idempotent, so a re-run retries cleanly.

## Configuration

Each value comes from an env var (the same ones the bot uses) or its `-flag`
override. Run the tool **once per deployment**: point the env at sandbox, dry
run, review, mutate; then repeat for prod. `-env` only labels the summary and
feeds the prod rail — it does **not** pick tables.

| Env var                         | Flag                         | Purpose                                            |
| ------------------------------- | ---------------------------- | -------------------------------------------------- |
| `QURL_CHANNEL_POLICIES_TABLE`   | `-channel-policies-table`    | `channel_policies` table to crawl                  |
| `QURL_WORKSPACE_MAPPINGS_TABLE` | `-workspace-mappings-table`  | `workspace_mappings` table (constructs the Store)  |
| `WORKSPACE_STATE_TABLE`         | `-workspace-state-table`     | per-workspace qURL API key table                   |
| `WORKSPACE_STATE_KMS_KEY_ARN`   | `-kms-key-arn`               | CMK that envelope-encrypts the API key column      |
| `QURL_ENDPOINT`                 | `-qurl-endpoint`             | qurl-service base URL for the liveness check       |
| `STATECRAWL_ENV`                | `-env`                       | summary/rail label (e.g. `sandbox`, `prod`)        |
|                                 | `-team`                      | restrict the crawl to one Slack `team_id`          |
|                                 | `-page-limit`                | `/v1/resources` page size (default 100)            |
|                                 | `-log-format`                | `json` (default) or `text`                         |
|                                 | `-dry-run`                   | read-only (default `true`); `-dry-run=false` mutates |
|                                 | `-allow-prod-purge`          | required opt-in to purge a prod-looking deployment |

AWS credentials and region come from the ambient environment. The role needs
`dynamodb:Scan`/`GetItem` on both tables, `kms:Decrypt` on the CMK, and — for a
mutating run — `dynamodb:UpdateItem` on `channel_policies`.

## Unblock-a-customer runbook

```sh
# 1. Dry run, scoped to the affected workspace, on sandbox or prod:
go run ./apps/slack/cmd/statecrawl -env prod -team T0123ABCD

# 2. Review the findings. orphan-alias / orphan-allowed-id are the purge targets:
#    each is logged as a "finding" record; the summary's dry_run_would_purge is
#    the count that a mutating run would clear.

# 3. Apply, with the prod opt-in, still scoped to the one workspace:
go run ./apps/slack/cmd/statecrawl -env prod -team T0123ABCD -dry-run=false -allow-prod-purge
```

The customer's `$alias` that reported "already bound" while `/qurl list` showed
nothing is now cleared, exactly as the bot's own revoke cascade would have.

## Output

Structured slog. Each finding and each mutation is one record; the run ends in a
`statecrawl complete` summary carrying the counter snapshot. With `jq`:

```sh
# Just the purge targets:
... -log-format json | jq 'select(.msg=="finding" and (.kind|test("orphan")))'

# The summary counters:
... -log-format json | jq 'select(.msg=="statecrawl complete")'
```

Use `-log-format text` for human-readable triage.

Finding kinds:

| Kind                  | Meaning                                                            | mutating run purges? |
| --------------------- | ----------------------------------------------------------------- | -------------------- |
| `orphan-alias`        | `$alias` bound to a revoked/deleted resource (#654 backfill)      | **yes**              |
| `orphan-allowed-id`   | `allowed_resource_ids` member that is revoked/deleted (#654)      | **yes**              |
| `alias-name-mismatch` | live tunnel reachable by an alias whose name ≠ slug (#669)        | no                   |
| `alias-url-target`    | `$alias` bound to a live URL resource (not display-name-able)     | no                   |
| `legacy-alias`        | `$alias` whose value is a non-`r_` legacy raw-URL binding         | no                   |
| `indeterminate`       | team's liveness couldn't be verified (no key / list failed)       | no                   |
