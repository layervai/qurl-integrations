# qURL webhook rollout — operator runbook

Powers the `👀 N viewed / M pending` counter on `/qurl send` + `/qurl map`
confirmation messages. The counter was stripped in
[`qurl-integrations#455`](https://github.com/layervai/qurl-integrations/pull/455)
because the upstream polling approach broke when `qurl-service` started
stripping the `qurls` array from `GET /v1/qurls/:id` responses for
type=transit resources (every Discord bot send is transit).

This rollout replaces the poll with a `qurl.accessed` webhook receiver.

## Rollout order

1. **`qurl-views` DDB table.** Provisioned by the deploying
   organization's infrastructure (separate from this repo). Without
   it, the bot's monitor `BatchGet` returns the empty map and the
   counter silently stays at `0 viewed / N pending` forever. Confirm
   with:
   ```
   aws dynamodb describe-table \
     --table-name <DDB_TABLE_PREFIX>qurl-views \
     --region <env-region>
   ```
2. **Deploy the bot.** On `isHttp` boot, the bot self-registers its
   `qurl.accessed` webhook subscription with qurl-service using its
   own `QURL_API_KEY` (the same key it uses to mint qURLs — so the
   subscription's `owner_id` matches the qURLs the bot creates).
   Steady-state reuses the secret already in SSM/env. Bootstrap (no
   subscription yet, or SSM seeded with `PLACEHOLDER`) creates or
   rotates the subscription and best-effort writes the secret back
   to SSM via `ssm:PutParameter`.

   No manual operator curl required. The bot's `qURL webhook self-
   registration complete` log line confirms the registration on each
   boot.

   Recovery if auto-register fails: the registrar logs at error
   level and the bot continues booting; manual fallback is the
   `curl` step in the [appendix](#appendix-manual-recovery) below.

## Wire shape (pinned)

- Header: `QURL-Signature` = bare-hex HMAC-SHA256 over the raw body.
  No `sha256=` prefix — that's the GitHub wire shape, NOT this one.
- Body: `{id, type, data:{qurl_id, resource_id, access_count, consumed}, owner_id, timestamp, api_version}`
  (peer is qurl-service's `WebhookEvent` payload shape per its
  published API contract). Field names matter: `type` is the event
  type, `id` is the per-event replay key. Receiver does NOT accept
  `event` / `event_id` as synonyms — see the regression test in
  `tests/qurl-webhook.test.js`.
- `src_ip` + `user_agent` are stripped server-side for type=transit
  resources (the connector-owned privacy boundary). Discord bot sends
  are always transit; we don't read those fields.

## Failure modes

- **Bot starts, but no secret set.** Boot log emits `QURL_WEBHOOK_SECRET
  unset — qURL webhook receiver mounted but will reject all inbound
  traffic with 503`. The view counter renders `0 viewed / N pending`
  on every send (the monitor's `BatchGet` returns the default empty
  map). Recover via step 3 above.
- **Secret rotates without restarting qurl-service subscription.**
  All inbound webhooks return 401 (signature mismatch). The bot's
  per-IP `BAD_SIG_MAX=30` rate limit kicks in after 30 attempts and
  switches to 429. Recover by updating the subscription's `secret`
  field (PATCH the subscription) — but if the lockout already
  triggered, expect a **~1 min blackout** before legit traffic
  unsticks (the rate-limit window is 60s from the last failed sig).
  Operators rotating in production should pre-stage both the SSM
  update and the subscription PATCH so the failed-sig window is as
  narrow as possible.
  - **Paging note**: a >10-min rotation gap will fire both alarms
    in sequence — `qurl-bot-discord-qurl-webhook-signature-invalid`
    at ~5min, then `…-rate-limited` at ~10min — with the same root
    cause. If `signature_invalid` already paged for the same time
    window, ack `rate_limited` as the tail of that incident; do not
    treat it as a new event.
- **`qurl-views` table missing.** The bot's monitor `BatchGet` throws
  `ResourceNotFoundException`. The setInterval's try/catch swallows
  it and logs `Link monitor poll failed` — the counter sticks at
  `0 viewed / N pending`. Recover by applying the terraform.
- **Some links missing `qurl_id`.** Connector running an older
  version (before `qurl_id` was surfaced from `MintLink`) — the bot's
  empty-`qurlId` boundary guard degrades the WHOLE monitor to the
  bare base message (no `👀` line at all) and emits one WARN per
  affected send. Recover by deploying the connector forward.

## Multi-replica safety

Multiple HTTP replicas booting concurrently used to race on
`POST /v1/webhooks/{id}/secret` — server-side last-write-wins meant
N-1 replicas held a stale secret in memory, and the ALB-routed
traffic across them 401'd on roughly `(N-1)/N` of inbound webhooks
until the next restart settled the fleet. The registrar now skips
rotation when (a) an existing subscription is found AND (b) the
SSM-loaded secret is non-empty and non-`PLACEHOLDER`. Steady-state
restarts reuse the SSM value; only the bootstrap path (no sub yet,
or seeded `PLACEHOLDER`) rotates.

### Cold-bootstrap of a fresh environment — DEPLOY AT desired_count=1 FIRST

On the very first deploy to a fresh environment, no subscription
exists yet AND no SSM secret exists yet. If N replicas boot
concurrently in that state, each replica independently `POST`s
`/v1/webhooks` and N duplicate subscriptions are created, each with
a distinct server-generated secret. SSM is last-write-wins so only
one replica's secret persists; the N-1 surviving duplicate
subscriptions deliver events to dead secrets and 401 forever until
manual cleanup.

The registrar's dedupe path (next boot finds the duplicates and
deletes all but the oldest survivor by `created_at`) is RECOVERY,
not prevention. To avoid the race entirely on a fresh environment:

1. Scale the bot HTTP service to `desired_count=1` before the first
   deploy.
2. Confirm one boot completed cleanly (`qURL webhook self-
   registration complete` log line + `created` action).
3. Scale back to the target desired_count.

This procedure is only required on the first deploy of a brand-new
environment. Every subsequent deploy (rolling, blue/green, redeploy)
falls into the steady-state `reused` path because the subscription
already exists and the SSM secret is populated. The durable upstream
fix — making `POST /v1/webhooks` idempotent on `(owner_id, url)` —
is tracked separately.

**Dedupe-recovery boot also needs `desired_count=1`.** If the first
deploy was accidentally run at `desired_count>1` and produced
duplicate subscriptions, the *next* boot enters the dedupe-recovery
path: it deletes the non-survivor duplicates AND force-rotates the
survivor's secret (the survivor's pre-rotate secret belongs to a
deleted replica's POST, so SSM almost certainly disagrees). That
force-rotate runs on every replica concurrently, with the same
last-write-wins SSM race that the steady-state reuse path was
designed to avoid. To exit the duplicate state cleanly:

1. Scale to `desired_count=1`.
2. Confirm one boot completed cleanly (`Webhook subscription
   reconciled (existing found, bootstrap rotate)` + the warn line
   for `Webhook subscription duplicates detected — deleting
   non-survivors`).
3. Scale back to target desired_count.

### Persistence env var

The registrar writes the active secret back to AWS Systems Manager
Parameter Store when `QURL_WEBHOOK_SECRET_SSM_PARAM` is set in the
bot's environment. The value is the full SSM parameter name (e.g.
`/discord-bot/sandbox/QURL_WEBHOOK_SECRET`). When unset, the
registrar runs in-memory-only — usable for local dev but every
restart rotates the secret because there's no place to read the
previous one from.

The IAM role for the bot's task needs `ssm:PutParameter` on that
specific parameter. The bot tolerates `AccessDeniedException`
gracefully (logs `warn`, continues with in-memory secret) so a
missing grant degrades to "the secret rotates each boot, but the
bot still works" rather than a crashloop.

## What the bot does NOT need

- The full `qurl.accessed` payload's `src_ip` / `user_agent` fields —
  they're stripped for transit resources at the qurl-service boundary,
  per the connector-owned redaction policy.

## Appendix — manual recovery

Use this only if auto-register failed and the bot logs show
`qURL webhook self-registration failed` repeatedly. Replace the
placeholder values:

```
# 1. Create or rotate the subscription with the bot's own API key
#    (NOT an admin token — owner_id must match the bot's qURL-mint
#    owner_id, otherwise events get filtered out before delivery).
curl -X POST https://<qurl-service-host>/v1/webhooks \
  -H "Authorization: Bearer $(aws ssm get-parameter --name /<project>/QURL_API_KEY --with-decryption --query 'Parameter.Value' --output text)" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://<bot-host>/webhooks/qurl",
    "events": ["qurl.accessed"],
    "description": "manual recovery"
  }'
# Capture the `secret` from the response.

# 2. Put it in SSM so the next bot boot picks it up.
aws ssm put-parameter \
  --name "/<project>/QURL_WEBHOOK_SECRET" \
  --type SecureString \
  --value "<secret-from-step-1>" \
  --overwrite \
  --region <env-region>

# 3. Force a redeploy.
aws ecs update-service \
  --cluster <cluster> \
  --service <bot-http-service> \
  --force-new-deployment \
  --region <env-region>
```

After step 3, the bot will boot with the SSM secret in env,
the auto-register will find the existing sub + the real
SSM-loaded secret, take the reuse path, and the rotation race
is gone for steady-state.
