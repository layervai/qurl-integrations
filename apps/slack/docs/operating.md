# Operating the qURL Secure Access Agent for Slack

This guide is for operators **running the Secure Access Agent themselves** — endpoints,
environment variables, Slack app configuration, and local development. If
you're a Slack user or workspace admin, you want the
[README](../README.md) instead.

## Command dispatch

User commands live under `/qurl`; admin commands live under a separate
`/qurl-admin` slash command. Both POST to the same request endpoint and share
the same signature verification — Slack stamps which command was invoked in
the `command` field, and the Secure Access Agent dispatches on it.

**Deploy prerequisite:** `/qurl-admin` must be registered as a slash command
in the Slack app config pointing at the **same request URL** as `/qurl`. The
admin verbs are inert until that registration exists.

Admin enforcement is **in-code** — every admin verb checks the qURL admin set
(`admin_slack_user_ids`) via `requireAdminSync`, so the AdminStore must be
wired. The "admins only" restriction on the `/qurl-admin` registration is a
cosmetic Slack-picker hint, not the enforcement boundary (Slack does not gate
slash-command invocation on workspace-admin role).

`setup` is deliberately **not** an admin verb — it stays on `/qurl` so the
first user to connect an unbound workspace can reach it (qURL is
first-come-claims). The overwrite guard for an already-bound workspace lives
at the OAuth-callback bind layer.

## Architecture

- **Runtime:** stateless HTTP service, shipped as a small container. Sits
  behind a TLS-terminating load balancer that routes `/slack/*`,
  `/oauth/slack/*`, `/oauth/qurl/*`, and `/health`.
- **Auth:** per-workspace qURL API key, established via `/qurl setup <email>`
  → `/oauth/qurl/start` → Auth0 → `/oauth/qurl/callback`. Supplying an email
  address on setup stores it in signed state, sends Auth0 `login_hint`, and
  requires the verified Auth0 email claim to match before any workspace bind
  or key mint. By default the Secure Access Agent does not force an Auth0 `connection`; the
  Auth0 application and tenant-level Actions own the login method and the
  cross-connection uniqueness policy. Prefer passwordless on the existing
  database connection when available, or enforce account-linking /
  duplicate-deny behavior before enabling a separate passwordless `email`
  connection for the same audience. `AUTH0_EMAIL_CONNECTION` is an optional
  recovery override when a deployment must force a specific connection. The
  callback's security gate is the verified email claim, not the connection
  hint by itself. If a workspace already has a qURL API key and qurl-service
  still accepts it, normal setup reuses that key instead of minting another one.
  Explicit owner requests (`/qurl setup <email> --rotate` or `--repoint`)
  take the rotation path instead: Slack strongly reads the stored qURL `key_id`,
  revokes that old key with the freshly authenticated qURL account, then mints
  and stores the replacement key plus its new `key_id`. Rotation mints the
  replacement through qURL's API-key endpoint, not the external-binding create
  endpoint; the workspace binding remains in qurl-service, and Slack's
  workspace operations authorize through the active key's qURL scopes.
  `--repoint` is accepted as a same-account alias for this flow; non-owning
  cross-account transfer still fails closed and is tracked in
  qurl-integrations#790. Rotation failure modes to expect:
  - If the stored row predates key metadata, or if the signed-in account cannot
    revoke the current key, rotation fails closed before any replacement is
    minted.
  - Missing or revoked legacy stored keys without key identity ask qURL to
    provision the Slack workspace key; if qURL reports that the workspace is
    already connected but the stored key cannot be recovered, setup stops with
    admin-facing recovery guidance.
  - Once Slack has stored a qURL `key_id`, an invalid stored key requires
    explicit `--rotate`/`--repoint`; plain setup will not mint around that old
    key identity because it may be the recovery path for a prior rotation
    persist-failure.
  - If the key was deleted at qURL rather than revoked, both plain setup and
    Slack-side rotation fail closed until an operator verifies the stale key
    identity or rotates the workspace key from qURL account/API-key management.
  - Because rotation revokes before minting, a revoke-success/mint-failure
    window leaves the workspace unable to use qURL until the owner retries
    `--rotate`. DDB may still hold the old key value and `key_id`, but that key
    is revoked, so workspace operations fail qurl-service validation until the
    retry stores the replacement.
  - If the replacement was minted but not stored, the retry uses qURL API-key
    idempotency to recover and store the same replacement key instead of
    spending another live key slot. That recovery is bounded by the qurl-service
    API-key mint idempotency window; after it expires, a replacement that was
    minted but never stored can become an orphan and must be revoked through
    qURL account/API-key management or operator tooling before retrying.
  If the callback reports that a qURL key was provisioned but not stored,
  rerun `/qurl setup <email>` for the same workspace and qURL account within
  qurl-service's configured external-binding idempotency window. qurl-service
  currently owns a 24-hour default (`QURL_BINDING_IDEMPOTENCY_TTL_CONTRACT`,
  source: layervai/qurl-service#904). The Slack task emits the same default
  unless its own `QURL_BINDING_IDEMPOTENCY_TTL_CONTRACT` environment variable is
  set to a canonical positive whole-hour duration such as `24h` at startup.
  qURL can replay the setup key during that window. After the window expires,
  if the stored Slack key is lost/revoked, or if the admin abandons setup, use
  qURL account/API-key management or operator tooling to revoke the unused
  workspace key before retrying; binding-level transfer/recovery for rows
  without stored key metadata is tracked in layervai/qurl-service#910.
  Rerunning setup without `--rotate`/`--repoint` is intentionally not a
  healthy-key rotation or qURL-account switch command.
  Keys are field-level encrypted at rest using KMS envelope encryption, with
  `workspace_id` bound as AAD.
  Rollout order: the Slack app may deploy before the qURL API binding route is
  enabled; route-missing or dark-launch responses fall back to legacy key
  provisioning. Avoid rolling the qURL API binding route back during an active
  setup persist-failure retry window, because that retry can no longer replay
  the binding key and will mint through the legacy fallback instead.
- **Slack app install:** customer workspaces install qURL through
  `/oauth/slack/install`, which redirects to Slack OAuth with the bot scopes
  needed by the slash command and modal surfaces. The callback stores Slack's
  workspace bot token using the same KMS envelope-encryption posture as qURL
  API keys. `SLACK_BOT_TOKEN` is only a legacy single-workspace fallback;
  customers do not manually provide bot tokens, and production guided setup
  should use the per-workspace token captured by Slack install OAuth.
  Enterprise Grid org-level installs are also supported: the enterprise-scoped
  bot token is stored under the Slack `enterprise_id`, while qURL API keys and
  admin state remain scoped to each invoking workspace's `team_id`.
- **Connector onboarding:** `/qurl-admin protect-connector` provisions a qURL
  Connector sidecar.
  - **Entry points** — a guided modal (opens with the bot token for the
    invoking workspace; the admin chooses the qURL Connector ID, optional
    channel alias, local port, and target environment: Docker, Docker Compose,
    ECS/Fargate, or Kubernetes), or the typed
    `/qurl-admin protect-connector <id>` (or `$id`) for CLI-style admins.
  - **Backend work (both paths)** — use the workspace API key to
    find-or-create a qURL Connector resource scoped to the connected qURL
    account, bind `$<id>` or the `alias:` override in the current Slack
    channel, and mint a one-hour bootstrap API key. When `alias:` is omitted,
    the ID doubles as the channel alias.
  - **Idempotency** — retrying the install within the modal's 25-minute
    validity window reuses the same bootstrap-key idempotency bucket. Retrying
    after that window can mint a new key, so operators should run the newest
    Slack install block and discard older bootstrap-key messages.
  - **Output** — hides the internal resource id and is tailored to the selected
    environment:
    - **Docker / Docker Compose** — guarded pasteable shell blocks that write
      `qurl-proxy.yaml`, create a bootstrap-key file, create/chown
      per-connector durable agent state, pass `QURL_API_KEY_FILE`, and pass
      `QURL_CONNECTOR_ID=<id>` to the client.
    - **ECS/Fargate / Kubernetes** — the same contract as deployment snippets:
      co-locate the sidecar with the target container, mount durable
      per-instance state at `/var/lib/layerv/agent`, mount or inject the
      bootstrap key through the runtime's secret mechanism, and remove the key
      after the logs show a successful connection.
  - **Key delivery** — ECS/Fargate uses the client's supported `QURL_API_KEY`
    fallback because AWS injects task secrets as environment variables; Docker,
    Docker Compose, and Kubernetes prefer `QURL_API_KEY_FILE`.
    Non-interactive operators should inject `QURL_BOOTSTRAP_KEY` from their
    secret manager before running a pasted block; interactive runs prompt for
    the bootstrap key with terminal echo disabled when possible.
  - **Constraint** — do not share one agent state volume across concurrently
    running sidecars.
  - **Cleanup edge** — if the bot cannot confirm Slack delivery after minting
    a bootstrap key, it retries the final text post once, revokes the key, and
    posts a discard notice when possible. Cleanup uses the handler's base
    context so request cancellation does not strand a key, but process shutdown
    can still interrupt the five-second cleanup window. If that happens, the
    bootstrap key remains bounded by its one-hour TTL; revoke it manually if
    logs show `tunnel_bootstrap_cleanup_failed`.

### Bootstrap-key DM live smoke

Run this smoke before relying on connector bootstrap-key DM delivery in a new
Slack app shape, especially an Enterprise Grid org install. Use a real admin
user who has not already opened a DM with the bot when possible. The smoke posts
only non-secret text; do not paste bootstrap keys into the command or result.
Any `-text` value is sent to Slack, so keep it short, non-secret, and at most
4000 bytes after cleanup. The message text is not written to the JSON evidence.
Line breaks, tabs, and control characters in `-text` are normalized before the
message is sent: line breaks and tabs become spaces, while other control
characters become `?`.

Production-path DM delivery uses `conversations.open(users=<user_id>)`, then
`chat.postMessage(channel=<dm_channel_id>)`. Run it with the exact bot token
shape being validated:

```sh
printf 'Slack bot token: ' >&2
read -rs SLACK_BOT_TOKEN
printf '\n' >&2
export SLACK_BOT_TOKEN
go run ./apps/slack/cmd/slack-dm-smoke \
  -user U0123456789 \
  -workspace-shape 'Enterprise Grid org install; workspace token unavailable' \
  -token-owner enterprise \
  -scopes 'commands,chat:write,im:write'
```

`-timeout` is the total budget for `auth.test`, DM opening, message posting,
and any optional direct-user probe. `-request-timeout` caps each Slack Web API
request. The CLI rejects `-timeout` values below three times
`-request-timeout`, or below four times `-request-timeout` when
`-direct-user-probe` is enabled; leave more headroom when validating a slow
workspace or network path. This guard is a conservative lower bound, not a
guarantee that every later call can consume its full per-request timeout.
Only override `-base-url` for a trusted Slack Web API endpoint or local test
server. **The smoke sends the bearer token to that base URL**, so treat any
remote override as trusted production infrastructure before running it. Remote
overrides must use HTTPS; HTTP is accepted only for localhost or loopback test
servers. The smoke honors Go's standard `HTTP_PROXY`, `HTTPS_PROXY`, and
`NO_PROXY` environment handling; treat configured proxies as part of the trusted
network path. The smoke is one-shot and does not retry `http_429` or 5xx
responses.
If Slack returns `http_429`, the JSON evidence includes `retry_after` when Slack
sends that header; Slack normally sends seconds, though HTTP-date values should
be treated as "wait until that time" before rerunning the smoke. Network
and cancellation failures are recorded as `request_failed`, `request_timeout`,
`budget_exhausted`, or `request_canceled` so evidence consumers can distinguish
transport failures, single slow Slack Web API requests, overall smoke budget
expiry, and cancellations. Transport-level failures serialize `status_code: 0`.
Unexpectedly large Slack responses are recorded as `response_too_large` instead
of parsed Slack evidence.

For Enterprise Grid fallback, pair the token smoke with the actual guided
connector setup in a workspace where the org-install token is the delivery
token. Confirm the admin receives the bootstrap-key DM and that the key-free
install instructions post separately. The local fallback contract is covered by
`TestSlackPostDMFuncOpensIMThenPostsWithGridFallback`; the live smoke confirms
Slack accepts the org-install token for the real workspace shape.

To record Slack's current behavior for the bare-user-id path without depending
on it for production, add `-direct-user-probe`. It may send a second non-secret
test message tagged with ` (direct-user probe)` if Slack accepts the direct
path; near-limit text is trimmed rune-safely before adding that tag so the
probe message stays within the same 4000-byte cap. A `channel_not_found` or
similar direct-channel rejection is useful evidence; it should not block
the production path when the open-then-post steps succeed. Without strict mode,
Slack/API/transport failures from this optional probe are recorded and the smoke
still exits 0 after the production path succeeds; overall timeout or cancellation
still fails the smoke. Always inspect `direct_user_probe` in the JSON evidence
when the probe is enabled, especially to distinguish Slack rejections from
transport errors such as `request_failed`; exit 0 only means the production
open-then-post path succeeded unless `-strict-direct-user-probe` was used. Use
`-strict-direct-user-probe` only with `-direct-user-probe` when that optional
probe should fail the command.

For the failure smoke, use a staging install with the DM-opening scope withheld
or revoked, then trigger guided connector setup with a disposable connector id.
Confirm that setup does not post install instructions, the user-facing response
asks the admin to reinstall or reauthorize the Slack app, and the generated key
cannot be used. The unit coverage for the same safety contract is
`TestTunnelInstallRevokesBootstrapKeyWhenDMSendFails`,
`TestTunnelInstallMissingScopeDMFailureMentionsSlackReinstall`, and
`TestTunnelInstallRevokesBootstrapKeyWhenSlackFollowupFails`.

Record the result in the PR or issue using this shape. If `conversations.open`
returns `ok:true` without a usable `channel.id`, the smoke records that
production step as `missing_dm_channel_id`; treat it as a failed DM-open step
even though Slack's raw response said `ok:true`. Pre-flight validation errors
such as invalid flags, empty token/user input, or unsafe `-base-url` exit before
contacting Slack and print stderr only; JSON evidence is emitted for runtime
smoke attempts.

```text
Workspace shape:
Token owner:
Slack scopes:
Fresh app-user DM user:
Production path: conversations.open=<ok/error>, chat.postMessage(D...)=<ok/error>
Direct user probe, if run: chat.postMessage(U...)=<ok/error>
Forced failure: instructions posted? <yes/no>; key usable? <yes/no>; user-facing copy:
Operator setup notes:
```

## Binding-backed setup visibility

`event="setup_binding_backed_persist_failure"` means qURL provisioned a
binding-backed workspace key, but the Slack app failed to store it locally. Treat
every event as actionable: the admin can recover by rerunning `/qurl setup
<email>` with the same qURL account only during the binding replay window. The
emitted `retry_window_hours` reports that window, and the event timestamp starts
the operator clock. The emitted window comes from the Slack task's
`QURL_BINDING_IDEMPOTENCY_TTL_CONTRACT` runtime override when set, otherwise it
uses the 24-hour qurl-service default mirror. Invalid override values fail
startup; accepted values use the canonical `Nh` form such as `24h` or `48h`
with no leading zero.

`cleanup_after_window_hours` is intentionally coincident with the same threshold
today; prefer retry at the exact boundary and treat rows older than the emitted
cleanup window as cleanup candidates until qurl-service exposes a separate
cleanup TTL.

Run this CloudWatch Logs Insights query against the Slack app log group with the
time range set to the current replay window:

```text
fields @timestamp, team_id, key_id, retry_window_hours, error
| filter event = "setup_binding_backed_persist_failure"
| sort @timestamp desc
| limit 50
```

Threshold: any result opens an on-call ticket to help the workspace admin rerun
setup before the replay window expires. For automated alerting, use a metric
filter (or scheduled Logs Insights query that publishes a metric) matching this
event and alarm on `count >= 1` in a 5-minute evaluation period; the query lists
rows for triage instead of returning `stats count()`. If the admin reruns setup
and the Slack storage write succeeds, close the ticket.

For post-window cleanup, run the same event query over the older incident window
(for example, a multi-day range whose end time is older than the emitted cleanup
window):

```text
fields @timestamp, team_id, key_id, cleanup_after_window_hours, error
| filter event = "setup_binding_backed_persist_failure"
| sort @timestamp asc
| limit 100
```

Threshold: any unresolved row older than its emitted `cleanup_after_window_hours`
is a cleanup candidate. Use qURL account/API-key management or operator tooling
to revoke or recover the unused workspace key before asking the admin to retry.
Customer self-service recovery for revoked or stale external-identity bindings is
tracked in
[layervai/qurl-service#910](https://github.com/layervai/qurl-service/issues/910).
The setup-binding rollout notes live in
[qurl-integrations PR #703](https://github.com/layervai/qurl-integrations/pull/703),
paired with [qurl-service PR #904](https://github.com/layervai/qurl-service/pull/904).

## Explicit rotation visibility

`event="setup_rotation_replacement_persist_failure"` means the Slack app revoked
the previous workspace key and qURL provisioned a replacement, but the app failed
to store the replacement locally. Treat every event as actionable: the admin can
recover by rerunning `/qurl setup <email> --rotate` with the same qURL account
during the idempotency replay window. The emitted `retry_window_hours` reports
that window. The emitted window comes from the Slack task's
`QURL_API_KEY_MINT_IDEMPOTENCY_TTL_CONTRACT` runtime override when set,
otherwise it uses the 24-hour qurl-service API-key mint default mirror. Invalid
override values fail startup; accepted values use the canonical positive
whole-hour `Nh` form such as `24h` or `48h` with no leading zero.
For post-metadata rows, do not advise plain `/qurl setup <email>` as recovery
for revoked or deleted keys; use explicit `--rotate`/`--repoint`, qURL
account/API-key management, or operator tooling.

Run this CloudWatch Logs Insights query against the Slack app log group with the
time range set to the current replay window:

```text
fields @timestamp, team_id, key_id, retry_window_hours, operator_action, error
| filter event = "setup_rotation_replacement_persist_failure"
| sort @timestamp desc
| limit 50
```

Threshold: any result opens an on-call ticket to help the workspace admin rerun
setup before the replay window expires. For automated alerting, use a metric
filter (or scheduled Logs Insights query that publishes a metric) matching this
event and alarm on `count >= 1` in a 5-minute evaluation period. After the replay
window expires, use qURL account/API-key management or operator tooling to verify
whether the replacement is live or should be revoked.

If the callback reports `qurl-service revoked API key pagination exceeded`, says
the key could not be confirmed as revoked, or logs
`oauth/minter revoked API key scan page cap exceeded`, do not keep asking the
admin to retry. The old key already returns 404 and the owner-scoped revoked
list could not confirm it within the bounded scan window. qURL currently lists
API keys by creation time, not revoke time, so an old workspace key can be buried
behind newer key history even when it was just revoked. If the key was deleted
instead of revoked, it will also be absent from the revoked list. Repeated
retries will keep failing until an operator verifies the key status or uses a
direct owner-scoped lookup when qURL exposes one.

## Uninstall revocation visibility

`/qurl uninstall` strongly reads the stored qURL `key_id` and revokes the
upstream workspace key (a `qurl:write` self-revoke: the workspace key
authenticates the `DELETE /v1/api-keys/{key_id}`) before removing local
credentials. `event`-free log lines under the `/qurl uninstall:` prefix report
the outcome; the terminal `/qurl uninstall: disconnected workspace Slack
commands` line carries `upstream_revoked` so a single query separates real
revokes from local-only disconnects:

```text
fields @timestamp, team_id, caller_user_id, key_id, upstream_revoked
| filter @message like "/qurl uninstall:"
| sort @timestamp desc
| limit 50
```

Two outcomes need operator follow-up because the upstream key is **not** revoked
but the workspace was disconnected locally:

- `legacy workspace row has no qURL key id — local-only disconnect`: the
  workspace connected before `key_id` persistence (added in #791, which this
  uninstall revocation builds on), so Slack cannot identify which key to revoke.
  Revoke it through qURL account/API-key management if the disconnect was
  security-motivated.
- `upstream refused workspace key self-revoke — local-only disconnect`
  (`status=401`/`403`): the deployment's qURL service does not permit a
  workspace key to revoke itself. Revoke through operator tooling and confirm
  the self-revoke contract with the qURL service team.

`upstream qURL key revoke failed — aborting to preserve key id` (and the
`could not read stored qURL key id before revoke` / `could not build client to
revoke upstream key` variants) means nothing was disconnected: the stored
`key_id` is intentionally preserved so the admin can retry, and a transient
failure self-heals on the next uninstall.

One expected intermediate state: if a revoke succeeds but the local removal then
fails, the workspace keeps pointing at a now-revoked key until the retry, so
every `/qurl` command in that window returns 401. A short burst of
workspace-wide 401s between a failed uninstall and its retry is expected, not a
separate incident. On that retry the already-revoked key's DELETE is treated as
revoked (404), or — if the service checks auth before existence — lands on the
401/403 local-only path above; either way the disconnect completes. Persistent
failures past a few retries warrant the same upstream-key verification as the
rotation path above.

## Endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /slack/commands` | Slash command handler (ack-then-async) |
| `POST /slack/events` | Event subscriptions (link unfurling planned) |
| `POST /slack/interactions` | Interactive components and modals |
| `GET /oauth/slack/install` | Begin Slack app install; redirects to Slack OAuth |
| `GET /oauth/slack/callback` | Slack OAuth redirect target; encrypts + persists workspace bot token |
| `GET /oauth/qurl/start` | Begin OAuth flow (state token required) |
| `GET /oauth/qurl/callback` | Auth0 redirect target; provisions + persists key |
| `GET /health` | Load-balancer health probe |

## Development

```bash
# Run tests
go test -race -count=1 ./apps/slack/...

# Run locally — requires AWS creds with read/write on the
# workspace_state table and KMS Decrypt/GenerateDataKey on the CMK.
WORKSPACE_STATE_TABLE=workspace-state-dev \
WORKSPACE_STATE_KMS_KEY_ARN=arn:aws:kms:us-east-1:...:key/... \
SLACK_SIGNING_SECRET=... \
SLACK_CLIENT_ID=... \
SLACK_CLIENT_SECRET=... \
QURL_ENDPOINT=https://api.layerv.ai \
QURL_CONNECTOR_IMAGE_FALLBACK=dev-sandbox \
AUTH0_DOMAIN=layerv.us.auth0.com \
AUTH0_CLIENT_ID=... \
AUTH0_CLIENT_SECRET=... \
AUTH0_AUDIENCE=https://api.layerv.ai \
SLACK_BASE_URL=https://slack-bot.example \
OAUTH_STATE_SECRET=$(openssl rand -hex 32) \
  go run ./apps/slack/cmd/

# Build the production container (linux/arm64 to match the deploy target)
docker buildx build --platform linux/arm64 \
  -f apps/slack/Dockerfile -t qurl-bot-slack:dev .
```

## Slack Markdown Renderer Validation

Before enabling the Slack agent standard-Markdown reply surface broadly, run the
live renderer validation in a real Slack workspace and attach the JSON output to
the GA enablement issue or to
[`qurl-integrations#767`](https://github.com/layervai/qurl-integrations/issues/767).
Run it only in a dedicated validation channel or thread, not a customer-visible
space: the command intentionally leaves the posted Slack messages in place as
review evidence and has no dry-run or cleanup mode.
The command posts the hardened Markdown output for the exact renderer-risk cases
that CI cannot prove: formatting, inline masked links, reference-style links,
Slack angle links, raw HTML tag starts, image syntax, the
notification/screen-reader fallback text, and the `markdown_text` compatibility
body used when Slack rejects `markdown` blocks.
It is compiled into the same bot binary intentionally so it exercises the same
wire helpers as production; it only runs when invoked with the explicit
`validate-slack-markdown-renderer` subcommand and a validation bot token/channel.
Keeping it in the shipped binary lets on-call rerun the exact production
transport path during GA validation instead of rebuilding a tagged diagnostic
variant.
Treat the validation bot token as permission to post persistent evidence
messages to the configured validation channel; run it only with a token and
channel approved for GA validation evidence. The command requires
`SLACK_MARKDOWN_VALIDATION_ACK_PERSISTENT_MESSAGES=true` or
`--ack-persistent-messages` so this side effect is explicit on every live run.
The overall run timeout defaults to 5 minutes; set
`SLACK_MARKDOWN_VALIDATION_TIMEOUT` or `--timeout` for slower workspaces.

```bash
SLACK_MARKDOWN_VALIDATION_BOT_TOKEN=xoxb-... \
SLACK_MARKDOWN_VALIDATION_CHANNEL=C0123456789 \
SLACK_MARKDOWN_VALIDATION_ACK_PERSISTENT_MESSAGES=true \
SLACK_MARKDOWN_VALIDATION_TEAM_ID=T0123456789 \
  go run ./apps/slack/cmd/ validate-slack-markdown-renderer \
  > slack-markdown-renderer-validation.json
```

To validate the assistant-pane streaming surface in the same run, start from a
workspace where the Slack app has the Agents & AI Apps feature and
`assistant:write` scope enabled, then include the pane thread identifiers:
The streamed validation body is one `chat.appendStream` append hardened with the
same stream hardener used by production replies; production streaming also keeps
that hardener's state across multiple deltas. This artifact validates Slack's
rendering of the delivered single-append output and the per-delta stream
hardener, not chunk-boundary coalescing or the `anywhere=true` reconcile fallback;
keep those covered by unit tests.
If all assistant-pane identifiers are omitted, the JSON report marks that surface
as `skipped` and exits 0 after validating channel replies. If only some
assistant-pane identifiers are set, the command treats that as a config error
and exits before writing JSON or posting messages, so a misconfigured assistant
validation cannot look like a complete run.

```bash
SLACK_MARKDOWN_VALIDATION_BOT_TOKEN=xoxb-... \
SLACK_MARKDOWN_VALIDATION_CHANNEL=C0123456789 \
SLACK_MARKDOWN_VALIDATION_ACK_PERSISTENT_MESSAGES=true \
SLACK_MARKDOWN_VALIDATION_TEAM_ID=T0123456789 \
SLACK_MARKDOWN_VALIDATION_ASSISTANT_CHANNEL=D0123456789 \
SLACK_MARKDOWN_VALIDATION_ASSISTANT_THREAD_TS=1700000000.000100 \
SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_TEAM_ID=T0123456789 \
SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_USER_ID=U0123456789 \
  go run ./apps/slack/cmd/ validate-slack-markdown-renderer \
  > slack-markdown-renderer-validation.json
```

The JSON evidence records the delivery `status`, `renderer_verdict`,
`review_instructions`, each case's original Markdown, the hardened Markdown sent
to Slack, the fallback text for notification/screen-reader previews, Slack
timestamps, request shape, and any `error_code` observed when a `markdown` block
is rejected and the compatibility retry runs. A `status` of `delivered` only
means Slack accepted the validation messages; `renderer_verdict:
operator_review_required` means an operator must still check the posted messages
in Slack against each case's `operator_check`.
Delivery/API failures still write the partial JSON report with `status:
delivery_failed`, `renderer_verdict: delivery_incomplete`, and `error` before
exiting nonzero, so attach that artifact when investigating a renderer or Slack
API failure, then fix the delivery error and rerun before renderer review.
CLI config and usage errors exit
before writing JSON. Slack rate limits abort the current run as a delivery
failure rather than sleeping and retrying; rerun in the dedicated validation
channel after the `Retry-After` window. If any Slack-rendered form still hides a
destination, add code and tests before enablement.

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `SLACK_SIGNING_SECRET` | Yes | Slack app signing secret |
| `SLACK_CLIENT_ID` | Slack install | Slack app client ID used by `/oauth/slack/install`. Required for customer installs that capture per-workspace bot tokens. |
| `SLACK_CLIENT_SECRET` | Slack install | Slack app client secret used by `/oauth/slack/callback` to exchange Slack's OAuth code. |
| `SLACK_INSTALL_STATE_SECRET` | Slack install | HMAC-SHA256 key for Slack install state signing. Must be ≥32 bytes. Use a distinct production secret from `OAUTH_STATE_SECRET`; the fallback is only for local/dev compatibility. |
| `SLACK_BOT_SCOPES` | No | Comma/space-separated extra bot scopes requested by `/oauth/slack/install`. Empty defaults to `commands,chat:write,im:write`; when set, those required defaults are still included so the captured token can receive slash commands, open 1:1 DMs, and deliver private messages for `dm:true`, agent replies, and qURL Connector bootstrap keys. See [Slack app configuration](#slack-app-configuration) for the full conversation-mode scope list. |
| `SLACK_BOT_TOKEN` | Legacy | Single-workspace fallback token for `views.open` when a workspace has not yet completed Slack install OAuth. Accepts `xoxb-` and `xoxe.xoxb-` token shapes. Production multi-customer installs should not depend on this fallback. |
| `SLACK_MARKDOWN_VALIDATION_BOT_TOKEN` | Validation | Bot token used only by `validate-slack-markdown-renderer`. Required for live renderer validation and intentionally separate from production token lookup. |
| `SLACK_MARKDOWN_VALIDATION_CHANNEL` | Validation | Slack channel id that receives channel-reply validation messages. Required for live renderer validation. |
| `SLACK_MARKDOWN_VALIDATION_ACK_PERSISTENT_MESSAGES` | Validation | Set to `true` to acknowledge that live renderer validation posts persistent evidence messages. Can also be set with `--ack-persistent-messages`. |
| `SLACK_MARKDOWN_VALIDATION_TEAM_ID` | Validation | Slack team id recorded in the JSON artifact and used for validation request metadata. |
| `SLACK_MARKDOWN_VALIDATION_ENTERPRISE_ID` | Validation | Optional Slack Enterprise Grid id recorded in the JSON artifact and used for validation request metadata. |
| `SLACK_MARKDOWN_VALIDATION_TIMEOUT` | Validation | Optional Go duration for the overall validation run timeout. Defaults to `5m`; can also be set with `--timeout`. |
| `SLACK_MARKDOWN_VALIDATION_ASSISTANT_CHANNEL` | Validation | Assistant-pane channel id for optional `chat.startStream`/`chat.appendStream` validation. |
| `SLACK_MARKDOWN_VALIDATION_ASSISTANT_THREAD_TS` | Validation | Assistant-pane thread timestamp for optional streaming validation. |
| `SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_TEAM_ID` | Validation | Recipient team id for optional streaming validation. |
| `SLACK_MARKDOWN_VALIDATION_ASSISTANT_RECIPIENT_USER_ID` | Validation | Recipient user id for optional streaming validation. |
| `QURL_ENDPOINT` | Yes | qURL API base URL (e.g. `https://api.layerv.ai`) |
| `WORKSPACE_STATE_TABLE` | Yes | DynamoDB table holding per-workspace API keys (provisioned by `qurl-integrations-infra`) |
| `WORKSPACE_STATE_KMS_KEY_ARN` | Yes | KMS CMK ARN used to envelope-encrypt workspace API keys and Slack bot tokens |
| `AUTH0_DOMAIN` | OAuth | Auth0 tenant FQDN, e.g. `layerv.us.auth0.com`. Scheme prefix and trailing slash are stripped at config-load. |
| `AUTH0_CLIENT_ID` | OAuth | Auth0 application client_id for the Secure Access Agent |
| `AUTH0_CLIENT_SECRET` | OAuth | Auth0 application client_secret |
| `AUTH0_AUDIENCE` | OAuth | Auth0 audience identifier for the qurl-service API |
| `AUTH0_EMAIL_CONNECTION` | No | Optional Auth0 connection name to force during `/qurl setup <email>` (for example `Username-Password-Authentication`). Empty sends no `connection` hint and lets the Auth0 application choose from its enabled connections. |
| `SLACK_BASE_URL` | OAuth/Slack install | Public origin of the Secure Access Agent, e.g. `https://slack-bot.example`. Used to compose Slack install, Slack callback, Auth0 callback, and `/qurl setup <email>` URLs. |
| `OAUTH_STATE_SECRET` | OAuth | HMAC-SHA256 key for state-token signing. Must be ≥32 bytes. |
| `QURL_BINDING_IDEMPOTENCY_TTL_CONTRACT` | No | Runtime mirror of qurl-service's external-binding replay window for setup persist-failure logs. Empty uses the current 24-hour default from layervai/qurl-service#904. Set only when qurl-service changes the binding idempotency TTL before this Slack app redeploys; value must use the canonical positive whole-hour `Nh` form such as `24h`, otherwise startup fails. |
| `QURL_API_KEY_MINT_IDEMPOTENCY_TTL_CONTRACT` | No | Runtime mirror of qurl-service's API-key mint replay window for rotation persist-failure logs. Empty uses the current 24-hour qurl-service default mirror. Set only when qurl-service changes the API-key mint idempotency TTL before this Slack app redeploys; value must use the canonical positive whole-hour `Nh` form such as `24h`, otherwise startup fails. |
| `QURL_CONNECTOR_IMAGE` | Yes in production | Container image reference rendered by `/qurl-admin protect-connector`. Production must set this to a specific non-latest release tag or lowercase SHA-256 digest, for example `ghcr.io/layervai/qurl-connector@sha256:<digest>`; pin **v0.3.0 or newer**, since the rendered snippets emit the v0.3.0 client contract (route `id` / `QURL_CONNECTOR_ID`) that older sidecar clients won't read. Empty values, omitted tags, `:latest` in any case, uppercase registry/repository paths, malformed digests, or characters outside the narrow image-reference allowlist fail startup validation. Use a digest pin when byte-for-byte image immutability is required. |
| `QURL_CONNECTOR_IMAGE_FALLBACK` | No | Set `dev-sandbox` (case-insensitive) to allow an empty `QURL_CONNECTOR_IMAGE` to render the `ghcr.io/layervai/qurl-connector:latest` fallback in local or sandbox deployments. Leave unset in production; production should fail startup unless `QURL_CONNECTOR_IMAGE` is pinned. |
| `QURL_SLACK_MAX_CONCURRENT_ASYNC` | No | Pool cap for in-flight async slash-command workers. Empty/0 uses the built-in default (50). Tune up if a workspace's load shape sustains `:warning: Secure Access Agent is busy` acks; tune down if memory pressure during retry storms is observed. |
| `QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_GATE_ASYNC` | No | Pool cap for the short **channel thread follow-up admission gate**: workspace-toggle read plus "is this already an agent thread?" transcript read. Empty/0 uses the built-in default (10). Each gate attempt has a 5s fail-closed budget; slow reads log as `agent: thread-continuity lookup failed; dropping channel reply`. During staged enablement, watch that line plus `agent: follow-up gate pool saturated — dropping channel reply`, and tune from observed DynamoDB latency and read volume. |
| `QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_ASYNC` | No | Pool cap for in-flight **admitted channel thread follow-up turns** — separate from `QURL_SLACK_MAX_CONCURRENT_ASYNC` so a busy channel's follow-up work can't saturate the main pool that `@mention`/DM/slash/interaction work shares. Empty/0 uses the built-in default (same as the main pool, 50). During staged enablement, watch `agent: follow-up turn pool saturated — dropping admitted channel reply`; main-pool isolation holds at any size. |

Use a full repository path for dotted or port-bearing registries. Slashless
`host:tag`-style values such as `gcr.io:v1` are rejected as ambiguous; use
`gcr.io/<org>/<image>:v1` or a digest pin instead.

Channel follow-up concurrency is split into short gate reads and long turns. The worst
steady-state in-flight turn ceiling is `QURL_SLACK_MAX_CONCURRENT_ASYNC` +
`QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_ASYNC`; gate reads add only the short
`QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_GATE_ASYNC` budget. Go's HTTP clients do not set a
fixed `MaxConnsPerHost` cap here, so connection contention should be governed by these
semaphores and upstream service limits rather than a smaller local connection pool.

`WORKSPACE_STATE_TABLE` + `WORKSPACE_STATE_KMS_KEY_ARN` are unconditionally
required at startup — the Secure Access Agent needs DynamoDB + KMS for per-workspace key
lookups even on `/qurl get` / `/qurl list`.

Before promoting a build with the `QURL_CONNECTOR_IMAGE` startup check, verify
the deployment manifest or task definition injects a pinned connector image; the
production manifest is intentionally managed outside this public repository.

The `Slack install` group is required for low-friction customer onboarding.
Without it, a deployment can still use a manually supplied `SLACK_BOT_TOKEN`
fallback, but customers cannot self-install the Secure Access Agent.

The `OAuth` group is required only to serve the
`/oauth/qurl/{start,callback}` surface. Without it the Secure Access Agent still serves
`/slack/*` and `/health`; `/qurl setup <email>` replies "OAuth is not
configured" until the OAuth env vars are set.

## Slack app configuration

For customer Slack installs, configure the Slack app with:

- OAuth redirect URL: `https://<SLACK_BASE_URL host>/oauth/slack/callback`
- Customer install link: `https://<SLACK_BASE_URL host>/oauth/slack/install`
- Slash command request URL: `https://<SLACK_BASE_URL host>/slack/commands`
- Interactivity request URL: `https://<SLACK_BASE_URL host>/slack/interactions`
- Bot scopes: `commands,chat:write,im:write` plus any extra scopes from
  `SLACK_BOT_SCOPES` (`commands` installs the slash command surface and
  `chat:write` lets the app post messages; `im:write` lets it open 1:1 DMs for
  `dm:true` and qURL Connector bootstrap-key delivery)
  - Conversation-mode installs should include `reactions:write` so the agent can
    add and clear the working-on-it reaction on admitted channel turns and DM
    turns that still use the reaction fallback before assistant-pane status is
    enabled. Without it, agent replies still post, but the best-effort reaction
    ack is absent. This scope is not part of the install defaults; declare it in
    the app manifest and add it explicitly to `SLACK_BOT_SCOPES`.
  - Conversation-mode installs that answer channel/private-channel/group-DM
    threads should include `channels:read` / `groups:read` / `mpim:read` with the
    corresponding event scopes. The confirm flow snapshots Slack's event
    `channel_type`; fresh group-DM (`mpim`) `get` proposals answer directly
    instead of posting unusable confirm cards, and legacy cards refuse before
    minting `get` links.
    `mpim:read` lets the legacy fallback classify snapshot-less `G`
    conversations via `conversations.info`; without it, ambiguous `G`
    conversations preserve the existing ephemeral delivery path for private
    channels. This fallback boundary is best-effort: transient
    `conversations.info` failures also fall back to the ephemeral path and emit
    an operator warning. Pending cards created before this `channel_type`
    snapshot shipped also use the fallback until their short TTL expires.
- Installation mode: workspace-level installs or Enterprise Grid org-level
  installs. Org-level bot tokens are stored under Slack `enterprise_id`; qURL
  workspace setup and admin checks still use workspace `team_id`.
- Token posture: non-rotating bot tokens. The validator accepts `xoxe.xoxb-`
  shapes defensively, but qURL does not request or persist Slack refresh
  tokens yet, so rotation-enabled apps need refresh support before production
  use.

With per-workspace token storage in place, existing customer workspaces must
reinstall or reauthorize the Slack app so Slack issues a per-workspace bot
token. Before deploying a build that depends on newly required Slack scopes,
send affected workspaces through the reinstall link so guided connector setup
does not fail closed on day one. New installs through `/oauth/slack/install`
store that token automatically, and guided `/qurl-admin protect-connector` uses
it for `views.open` plus bootstrap-key DM delivery. If Slack tells a customer
guided connector setup needs the latest qURL Slack app install, send them
through this reinstall link.
Monitor the guided setup open path after deploys: the synchronous admin gate is
budgeted at 800 ms and the `views.open` call at 1500 ms, leaving headroom inside
Slack's roughly three-second trigger window. Sustained p99 latency near either
budget should be treated as an operator action item before customers see setup
window-expired prompts.
