# qURL webhook rollout — operator runbook

Powers the `👀 N viewed / M pending` counter on `/qurl send` + `/qurl map`
confirmation messages. The counter was stripped in
[`qurl-integrations#455`](https://github.com/layervai/qurl-integrations/pull/455)
because the upstream polling approach broke when `qurl-service` started
stripping the `qurls` array from `GET /v1/qurls/:id` responses for
type=transit resources (every Discord bot send is transit).

This rollout replaces the poll with a `qurl.accessed` webhook receiver.

## Rollout order

Each step blocks the next — do not skip ahead.

1. **`qurl-views` DDB table.** Provisioned by the deploying organization's
   infrastructure (separate from this repo). Without it, the bot's
   monitor `BatchGet` returns the empty map and the counter silently
   stays at `0 viewed / N pending` forever. Confirm with:
   ```
   aws dynamodb describe-table \
     --table-name <DDB_TABLE_PREFIX>qurl-views \
     --region <env-region>
   ```
2. **Deploy the bot.** Code mounts `/webhooks/qurl` unconditionally and
   warns on boot if `QURL_WEBHOOK_SECRET` is unset — that's the operator
   signal that step 3 hasn't happened yet.
3. **Set the SSM secret.**
   ```
   aws ssm put-parameter \
     --name "/<project>/QURL_WEBHOOK_SECRET" \
     --type SecureString \
     --value "$(openssl rand -hex 32)" \
     --overwrite \
     --region <env-region>
   ```
   Then restart the task so the secret is picked up:
   ```
   aws ecs update-service --service <svc> --force-new-deployment ...
   ```
4. **Register the subscription with qurl-service.** The subscription's
   `secret` field MUST be the same hex string as step 3 — the bot's
   verifySignature pins bare-hex HMAC-SHA256 over the raw body.
   ```
   curl -X POST https://<qurl-service-host>/v1/webhooks \
     -H 'Authorization: Bearer <qurl-service-admin-token>' \
     -H 'Content-Type: application/json' \
     -d '{
       "url": "https://<bot-host>/webhooks/qurl",
       "event_types": ["qurl.accessed"],
       "secret": "<same-hex-as-step-3>"
     }'
   ```

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

## Known operator-burden tradeoffs

- **Secret coupled in two places.** Step 3's SSM value and step 4's
  subscription `secret` field must match by hand; rotation is a
  coordinated edit, not a single command. Auto-register-at-boot
  (bot POSTs the subscription on startup with the same value it
  read from SSM) would collapse this to one source of truth —
  deferred because giving the bot a qurl-service admin token
  widens the bot's blast radius beyond the per-guild `QURL_API_KEY`
  it has today. Revisit when the admin-token surface has tighter
  scoping (per-subscription instead of org-wide).

## What the bot does NOT need

- A webhook auto-registration handshake — subscriptions are managed
  out-of-band (curl above). See "operator-burden tradeoffs" for
  why this isn't automated today.
- The full `qurl.accessed` payload's `src_ip` / `user_agent` fields —
  they're stripped for transit resources at the qurl-service boundary,
  per the connector-owned redaction policy.
