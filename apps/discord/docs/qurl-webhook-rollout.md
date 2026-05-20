# qURL webhook rollout — operator runbook

Powers the `👀 N viewed / M pending` counter on `/qurl send` + `/qurl map`
confirmation messages. The counter was stripped in
[`qurl-integrations#455`](https://github.com/layervai/qurl-integrations/pull/455)
because the upstream polling approach broke when `qurl-service` started
stripping the `qurls` array from `GET /v1/qurls/:id` responses for
type=transit resources (every Discord bot send is transit).

This rollout replaces the poll with a `qurl.accessed` webhook receiver.

## Architecture

Three pieces:

1. **`qurl-views` DDB table** — bot writes view events here on inbound
   webhook receipt; the slash-command monitor reads it.
2. **Bot HTTP service** — receives signed webhooks at `/webhooks/qurl`,
   verifies HMAC against `QURL_WEBHOOK_SECRET` (read once at boot from
   env-injected SSM), writes views to DDB.
3. **`webhook-registrar` Lambda** —
   [`apps/discord/lambda/webhook-registrar/`](../lambda/webhook-registrar/).
   Single-instance, race-free by construction. Runs once per deploy via
   Terraform `aws_lambda_invocation`. Creates / rotates / reuses the
   `qurl.accessed` subscription against qurl-service, writes the secret
   to SSM. Bot never registers itself.

## Why a Lambda (not on-boot self-registration)

An earlier auto-register-on-boot design ran the same registration code
inside the bot's HTTP-tier boot path. That has an unavoidable cold-
bootstrap race: on the very first deploy to a fresh environment, N
HTTP replicas concurrently `POST /v1/webhooks` → N duplicate
subscriptions, each with a distinct server-generated secret. SSM's
last-write-wins picks one; the surviving N-1 deliver to dead-secret
subs until manual cleanup. Mitigation in-app (dedupe-on-next-boot,
force-rotate) recovers but leaves a duplicate-webhook-traffic window
between the bad deploy and the next restart.

A single-instance Lambda triggered by Terraform sidesteps the race
entirely: only one execution per deploy, fail-fast on error (deploy
itself fails), no in-app coordination needed.

## Deploy flow

Terraform-side ordering (config lives in `qurl-integrations-infra`):

1. Apply the `qurl-views` DDB table. The `QURL_WEBHOOK_SECRET` SSM
   SecureString parameter is created by the Lambda's `PutParameter`
   call on first invocation — Terraform does NOT pre-seed it with a
   sentinel value. If the Lambda hasn't run yet, the bot's receiver
   503s on inbound webhooks (qurl-service retries), which is the
   correct unconfigured-state behavior.
2. Apply the Lambda function + IAM role (scoped: `ssm:GetParameter` on
   the `QURL_API_KEY` + `QURL_WEBHOOK_SECRET` paths; `ssm:PutParameter`
   on the `QURL_WEBHOOK_SECRET` path; `logs:*`).
3. `aws_lambda_invocation.webhook_registrar` runs synchronously during
   apply with the deploy-specific input (bridge URL, region, param
   names). If the Lambda fails (qurl-service down, IAM missing, etc.),
   the apply fails and the bot's task-def update doesn't fire — no
   half-registered state.
4. Bot ECS service task-def updates with the just-rotated
   `QURL_WEBHOOK_SECRET` injected from SSM as a `secrets` entry.
   Rolling deploy replaces tasks; new tasks read the current secret;
   receiver verifies inbound webhooks.

## Rotation

Re-invoke the Lambda (manual `aws lambda invoke`, scheduled EventBridge
rule, or operator-triggered Terraform plan) → SSM updated → force-
redeploy the bot's ECS service to pick up the new secret:

```
aws lambda invoke --function-name <name> --payload '{...}' /tmp/out.json
aws ecs update-service --cluster <c> --service <s> --force-new-deployment ...
```

The Lambda's reuse-path semantics mean a rotation only fires a server-
side `POST /v1/webhooks/{id}/secret` if the SSM secret has actually
changed. Re-invoking a stable system is idempotent (the registrar
finds the existing sub, sees the SSM secret matches, returns `reused`).

## Wire shape (pinned)

- Header: `QURL-Signature` = bare-hex HMAC-SHA256 over the raw body.
  No `sha256=` prefix — that's the GitHub wire shape, NOT this one.
- Body: `{id, type, data:{qurl_id, resource_id, access_count, consumed},
  owner_id, timestamp, api_version}` (peer is qurl-service's
  `WebhookEvent` payload). Field names matter: `type` is the event
  type, `id` is the per-event replay key. Receiver does NOT accept
  `event` / `event_id` as synonyms — see the regression test in
  `tests/qurl-webhook.test.js`.
- `src_ip` + `user_agent` are stripped server-side for type=transit
  resources (the connector-owned privacy boundary). Discord bot sends
  are always transit; we don't read those fields.

## Failure modes

- **Lambda fails during deploy.** Terraform apply fails, the bot
  task-def update is skipped, no traffic shifts. Existing bot tasks
  keep running with the previous (still-valid) secret. Root-cause in
  CloudWatch logs for the Lambda; re-run apply when fixed.
- **Bot reads empty `QURL_WEBHOOK_SECRET`.** Means the Lambda never
  ran successfully OR ran but SSM `PutParameter` failed (IAM, network).
  Receiver returns 503 (qurl-service retries). Recover by running the
  Lambda manually and verifying CloudWatch logs for the persist call.
- **`qurl-views` table missing.** Bot's monitor `BatchGet` throws
  `ResourceNotFoundException`; the setInterval's try/catch logs
  `Link monitor poll failed` and the counter sticks at
  `0 viewed / N pending`. Recover by applying the terraform.
- **Some links missing `qurl_id`.** Connector running an older version
  (before `qurl_id` was surfaced from `MintLink`) — the bot's empty-
  `qurlId` boundary guard degrades the WHOLE monitor to the bare base
  message (no `👀` line at all) and emits one WARN per affected send.
  Recover by deploying the connector forward.

## Operational notes

- **`description` field staleness**: the human-readable description on
  the qurl-service subscription is written at create-time and not
  reconciled by subsequent Lambda invocations. Region/env rename
  leaves the qurl-service UI label stale until the subscription is
  recreated. Observability-only — the bot keeps working.
- **API-key blast radius**: the Lambda's `QURL_API_KEY` can list /
  create / PATCH / rotate-secret / DELETE webhook subscriptions in
  addition to minting qURLs. Factor into rotation drills.
- **Higher-severity log signal** (alarm on this): `webhook-registrar
  Lambda` CloudWatch error logs. The Lambda is the sole webhook-
  registration code path; failures cascade to "bot can't verify any
  inbound webhook." The bot's `Webhook receiver not configured`
  503-response log is the downstream symptom.

## Appendix — manual operator recovery

If the Lambda is unavailable and you need to register manually:

```
# 1. Create the subscription (use the bot's QURL_API_KEY for owner-
#    scope; an admin token would attach the sub to the wrong owner_id
#    and events would silently filter out before delivery).
curl -X POST https://<qurl-service-host>/v1/webhooks \
  -H "Authorization: Bearer $(aws ssm get-parameter --name /<project>/QURL_API_KEY --with-decryption --query 'Parameter.Value' --output text)" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://<bot-host>/webhooks/qurl",
    "events": ["qurl.accessed"],
    "description": "manual recovery"
  }'
# Capture the `secret` from the response.

# 2. Write it to SSM so the next bot deploy picks it up.
aws ssm put-parameter \
  --name "/<project>/QURL_WEBHOOK_SECRET" \
  --type SecureString \
  --value "<secret-from-step-1>" \
  --overwrite \
  --region <env-region>

# 3. Force a bot redeploy so tasks pick up the new secret from env.
aws ecs update-service \
  --cluster <cluster> \
  --service <bot-http-service> \
  --force-new-deployment \
  --region <env-region>
```

Once the Lambda is restored, the next invocation will find the
manually-created subscription, see the SSM secret matches, and return
`reused` — no double-registration.
