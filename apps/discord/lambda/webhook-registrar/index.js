// One-shot webhook-registrar Lambda
//
// Replaces the bot's on-boot self-registration (PR 471) with a
// deploy-time Lambda invocation. Why this exists:
//
// The auto-register-on-boot architecture races on a fresh environment
// (N HTTP replicas concurrently POST /v1/webhooks, qurl-service
// creates N duplicate subscriptions each with a distinct secret,
// SSM's last-write-wins picks one, the other N-1 deliver to
// dead-secret subscriptions until the next-boot dedupe). The
// in-app race-recovery (dedupe + force-rotate) leaves a duplicate-
// webhook-traffic window between the bad first deploy and the
// next restart — at the bot's rate-limit window (30 bad-sigs / 60s),
// modest qURL traffic can flap legit clients into 429s.
//
// The deploy-time Lambda eliminates the race entirely: ONE invocation
// per deploy, single-instance by design within a `terraform apply`,
// never racy. Triggered by Terraform `aws_lambda_invocation` (deploy
// succeeds only if registration succeeded — fail-fast, no silent
// half-registered state).
//
// Caveat — the "single-instance" property is a TERRAFORM-level
// invariant, not a Lambda-level one. Within one apply, `aws_lambda_invocation`
// is synchronous → truly serial. Across concurrent applies (CI re-run
// firing on a stale plan, two operators racing apply), the Lambda
// will happily run two copies in parallel. The dedupe-recovery path in
// ensureWebhookSubscription handles this defensively, but the infra-
// repo SHOULD set the Lambda's reserved concurrency to 1 as belt-and-
// suspenders so a stray concurrent invocation queues rather than runs.
//
// The Lambda reuses `apps/discord/src/qurl-webhook-registrar.js`'s
// `ensureWebhookSubscription` + `buildSsmPersistSecret` directly —
// same library, different runtime. Bot HTTP tier now only RECEIVES
// webhooks (reads QURL_WEBHOOK_SECRET from SSM-injected env at boot,
// verifies signatures, writes to DDB). No registration calls from
// the bot ever again.
//
// Rotation flow: re-invoking the Lambda rotates the secret + updates
// SSM. The bot's task definition then needs a redeploy to pick up
// the new secret from env. Operators script this as: invoke Lambda,
// then `aws ecs update-service --force-new-deployment`.
//
// IAM scope (set in qurl-integrations-infra):
//   - ssm:GetParameter on the QURL_API_KEY + QURL_WEBHOOK_SECRET paths
//   - ssm:PutParameter on the QURL_WEBHOOK_SECRET path
//   - logs:CreateLogGroup, logs:CreateLogStream, logs:PutLogEvents
//     (the minimum CloudWatch grant; `logs:*` is overly broad)
//   - NO DDB grants (the bot's webhook-receiver path needs DDB, the
//     registrar does not — keep separation of concerns clean)
//
// Caller (Terraform `aws_lambda_invocation`) is responsible for any
// `bridgeUrl` normalization. The registrar's `canonicalUrl` handles
// trailing slashes between subscriptions during matching, but does
// NOT normalize an internal double-slash from a `BASE_URL` ending in
// `/`. Strip in the Terraform input so `https://bot/` doesn't produce
// `https://bot//webhooks/qurl`.
//
// Input shape (set in Terraform invocation) — all 6 fields are required:
//   {
//     apiEndpoint:         string,  // qurl-service base URL (e.g. https://api.layerv.ai)
//     bridgeUrl:           string,  // public bot URL (e.g. https://discord-bot.example.com/webhooks/qurl)
//     description:         string,  // human-readable, surfaces in qurl-service UI
//     ssmParamName:        string,  // SSM SecureString parameter to write the rotated webhook secret to
//     ssmRegion:           string,  // AWS region for SSM (typically matches Lambda region)
//     apiKeySsmParamName:  string,  // SSM SecureString parameter holding the qurl-service API key
//   }
// The qurl-service API key is read from SSM by NAME (not passed by
// value in the event body) so the value never appears in CloudWatch
// invocation logs or Terraform state.
//
// validateInput trims trailing `/` from bridgeUrl — the registrar's
// canonicalUrl normalizes trailing-slash drift between matched subs,
// but a `BASE_URL` ending in `/` would still concatenate to
// `https://bot//webhooks/qurl` and silently fail to match the bot's
// actual route. Defense-in-depth so caller (Terraform) discipline
// isn't load-bearing.
//
// Output shape (returned to Terraform):
//   {
//     webhookId: string,
//     action:    'created' | 'rotated' | 'reused',
//     // secret is intentionally NOT returned — it's persisted to SSM
//     // and read back from there by the bot. Returning it via the
//     // Lambda response would echo it through invocation logs.
//   }

const {
  SSMClient,
  GetParameterCommand,
  PutParameterCommand,
} = require('@aws-sdk/client-ssm');
const {
  ensureWebhookSubscription,
  buildSsmPersistSecret,
} = require('../../src/qurl-webhook-registrar');

// SSM client at module scope — Lambda reuses the same execution
// context across consecutive invocations within the same container
// lifetime, so reusing the client is a meaningful perf win for
// rotation workflows that invoke the Lambda repeatedly.
//
// We track the region in a closure variable rather than reading it
// back off `client.config.region` — in AWS SDK v3 that field is
// normalized to a Provider<string> (a `() => Promise<string>`), so a
// naive `config.region() !== region` compares a Promise to a string
// (always truthy → cache never invalidates, AND would require an
// `await` to compare correctly, which forces this helper async).
let ssmClientCache = null;
let ssmClientRegion = null;
function getSsmClient(region) {
  if (!ssmClientCache || ssmClientRegion !== region) {
    ssmClientCache = new SSMClient({ region });
    ssmClientRegion = region;
  }
  return ssmClientCache;
}
// Test seam — Lambda execution contexts persist across invocations,
// so a unit test that exercises multi-region behavior or wants warm-
// invocation parity has to reset the singleton between cases.
function _resetSsmClientCacheForTests() {
  ssmClientCache = null;
  ssmClientRegion = null;
}

async function readSsmSecureString({ ssmClient, name }) {
  try {
    const resp = await ssmClient.send(
      new GetParameterCommand({ Name: name, WithDecryption: true }),
      { abortSignal: AbortSignal.timeout(5_000) },
    );
    return resp?.Parameter?.Value ?? null;
  } catch (err) {
    if (err.name === 'ParameterNotFound') return null;
    throw err;
  }
}

// Validates required fields and returns a NORMALIZED event (no
// mutation of the input). Defensive trailing-slash strip on bridgeUrl:
// caller (Terraform) should already normalize, but a stale
// `BASE_URL=https://bot/` would otherwise produce
// `https://bot//webhooks/qurl` and silently fail to match the bot's
// actual receiver path. canonicalUrl normalizes drift BETWEEN subs
// but not an internal `//`.
const DESCRIPTION_WARN_LEN = 200;

function validateAndNormalizeInput(event) {
  const required = ['apiEndpoint', 'bridgeUrl', 'description', 'ssmParamName', 'ssmRegion', 'apiKeySsmParamName'];
  for (const k of required) {
    if (typeof event[k] !== 'string' || !event[k]) {
      throw new Error(`webhook-registrar: missing or invalid input field: ${k}`);
    }
  }
  // Defensive `https://` check on both URL fields — a Terraform
  // input typo (`http://` for either, or a placeholder literal that
  // slipped through) would otherwise fail late. Catches the bad
  // input at the boundary instead.
  for (const k of ['apiEndpoint', 'bridgeUrl']) {
    if (!event[k].startsWith('https://')) {
      throw new Error(`webhook-registrar: ${k} must start with https:// (got: ${event[k].slice(0, 60)})`);
    }
  }
  // Description gets sliced to DESCRIPTION_MAX_LEN at the wire
  // boundary in `createSubscription`. Warn here so an operator-
  // configured oversized description (CI variable expansion gone
  // wrong, copy-pasted multi-line string) is visible at invocation
  // time rather than silently truncated in qurl-service's UI.
  if (event.description.length > DESCRIPTION_WARN_LEN) {
    console.warn(JSON.stringify({
      msg: 'webhook-registrar: description exceeds warn length, will be clipped at wire boundary',
      length: event.description.length,
      limit: DESCRIPTION_WARN_LEN,
    }));
  }
  const normalized = { ...event, bridgeUrl: event.bridgeUrl.replace(/\/$/, '') };
  return normalized;
}

exports._internals = { getSsmClient, _resetSsmClientCacheForTests };

exports.handler = async (event, context) => {
  // Surface invocation context up front so CloudWatch correlates the
  // Lambda run with the Terraform-triggered deploy. Never log the
  // event body verbatim — apiKeySsmParamName is a name, not a value,
  // so the bot's API key never appears in logs.
  console.log(JSON.stringify({
    msg: 'qURL webhook-registrar Lambda invoked',
    requestId: context?.awsRequestId,
    bridgeUrl: event?.bridgeUrl,
    ssmParamName: event?.ssmParamName,
    ssmRegion: event?.ssmRegion,
  }));

  const input = validateAndNormalizeInput(event);

  const ssmClient = getSsmClient(input.ssmRegion);

  // Read the qurl-service API key from SSM by name — keeps the value
  // out of Terraform state, Lambda env, and invocation logs. The
  // Lambda's IAM role has GetParameter on this specific parameter
  // (scoped in qurl-integrations-infra).
  const apiKey = await readSsmSecureString({ ssmClient, name: input.apiKeySsmParamName });
  if (!apiKey) {
    // Falsy catches both `null` (ParameterNotFound mapped) and `''`
    // (parameter present but empty value). Message lists both so a
    // future re-throw / test reader knows what fired.
    throw new Error(`webhook-registrar: SSM parameter ${input.apiKeySsmParamName} returned empty or missing (ParameterNotFound or zero-length value)`);
  }

  // Read existing webhook secret (if any) so the registrar can take
  // the reuse path when steady-state. On first-ever invocation this
  // returns null; ensureWebhookSubscription treats null as "no real
  // initial secret" and creates fresh. On subsequent invocations
  // (manual rotation, scheduled rotation) the SSM value is the
  // previously-persisted secret — ensureWebhookSubscription's
  // initialIsRealSecret guard reuses it if the sub still matches.
  const initialSecret = await readSsmSecureString({ ssmClient, name: input.ssmParamName });

  // Lambda STRICT-persist path: do NOT pass `persistSecret` into
  // `ensureWebhookSubscription` (its `bestEffortPersist` swallows
  // failures, which is correct for the on-boot caller where the
  // in-memory secret keeps the bot working until next deploy — but
  // WRONG here, where SSM persistence IS the deliverable. If the SSM
  // write fails after qurl-service returns a new secret, that secret
  // never reaches the bot and the receiver 503s every webhook. Fail
  // the Lambda loudly → Terraform apply fails → operator notices
  // instead of half-registered state shipping silently.
  const result = await ensureWebhookSubscription({
    apiEndpoint: input.apiEndpoint,
    apiKey,
    bridgeUrl: input.bridgeUrl,
    description: input.description,
    initialSecret,
    // persistSecret intentionally omitted — handled below with strict
    // throw-on-failure semantics.
  });

  // Persist directly so a PutParameter failure propagates out of the
  // handler. Only persist when the secret actually changed
  // (created/rotated) — `reused` returns the SSM-loaded `initialSecret`
  // unchanged, so re-writing the same value is a wasteful no-op + would
  // emit a misleading "secret persisted" log on every steady-state
  // invocation.
  if (result.action !== 'reused') {
    const persist = buildSsmPersistSecret({
      ssmClient,
      paramName: input.ssmParamName,
      PutParameterCommand,
    });
    await persist(result.secret);
  }

  console.log(JSON.stringify({
    msg: 'qURL webhook-registrar Lambda complete',
    requestId: context?.awsRequestId,
    webhookId: result.webhookId,
    action: result.action,
  }));

  return {
    webhookId: result.webhookId,
    action: result.action,
  };
};
