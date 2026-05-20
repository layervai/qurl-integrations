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
// per deploy, single-instance by design, never racy. Triggered by
// Terraform `aws_lambda_invocation` (deploy succeeds only if
// registration succeeded — fail-fast, no silent half-registered state).
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
//   - ssm:GetParameter, ssm:PutParameter on the QURL_WEBHOOK_SECRET path
//   - logs:* for CloudWatch
//   - NO DDB grants (the bot's webhook-receiver path needs DDB, the
//     registrar does not — keep separation of concerns clean)
//
// Input shape (set in Terraform invocation):
//   {
//     apiEndpoint:  string,  // qurl-service base URL (e.g. https://api.qurl.layerv.xyz)
//     bridgeUrl:    string,  // public bot URL (e.g. https://discord-bot.sandbox.qurl.layerv.xyz/webhooks/qurl)
//     description:  string,  // human-readable, surfaces in qurl-service UI
//     ssmParamName: string,  // SSM SecureString parameter to write the rotated secret to
//     ssmRegion:    string,  // AWS region for SSM (typically matches Lambda region)
//   }
// The qurl-service API key is read from SSM by name (env-supplied to
// the Lambda task) so the value never appears in CloudWatch invocation
// logs.
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
let ssmClientCache = null;
function getSsmClient(region) {
  if (!ssmClientCache || ssmClientCache.config.region() !== region) {
    ssmClientCache = new SSMClient({ region });
  }
  return ssmClientCache;
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

function validateInput(event) {
  const required = ['apiEndpoint', 'bridgeUrl', 'description', 'ssmParamName', 'ssmRegion', 'apiKeySsmParamName'];
  for (const k of required) {
    if (typeof event[k] !== 'string' || !event[k]) {
      throw new Error(`webhook-registrar: missing or invalid input field: ${k}`);
    }
  }
}

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

  validateInput(event);

  const ssmClient = getSsmClient(event.ssmRegion);

  // Read the qurl-service API key from SSM by name — keeps the value
  // out of Terraform state, Lambda env, and invocation logs. The
  // Lambda's IAM role has GetParameter on this specific parameter
  // (scoped in qurl-integrations-infra).
  const apiKey = await readSsmSecureString({ ssmClient, name: event.apiKeySsmParamName });
  if (!apiKey) {
    throw new Error(`webhook-registrar: SSM parameter ${event.apiKeySsmParamName} returned null (ParameterNotFound or empty value)`);
  }

  // Read existing webhook secret (if any) so the registrar can take
  // the reuse path when steady-state. On first-ever invocation this
  // returns null; ensureWebhookSubscription treats null as "no real
  // initial secret" and creates fresh. On subsequent invocations
  // (manual rotation, scheduled rotation) the SSM value is the
  // previously-persisted secret — ensureWebhookSubscription's
  // initialIsRealSecret guard reuses it if the sub still matches.
  const initialSecret = await readSsmSecureString({ ssmClient, name: event.ssmParamName });

  const persistSecret = buildSsmPersistSecret({
    ssmClient,
    paramName: event.ssmParamName,
    PutParameterCommand,
  });

  const result = await ensureWebhookSubscription({
    apiEndpoint: event.apiEndpoint,
    apiKey,
    bridgeUrl: event.bridgeUrl,
    description: event.description,
    initialSecret,
    persistSecret,
  });

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
