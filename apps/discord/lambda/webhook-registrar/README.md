# webhook-registrar Lambda

One-shot Lambda that registers / rotates / reuses the Discord bot's
`qurl.accessed` webhook subscription on each Terraform deploy.

See `index.js`'s header docstring for the full contract (input shape,
output shape, IAM scope, rotation flow).

## Bundling

The handler `require`s shared modules from `../../src/`:

- `../../src/qurl-webhook-registrar` — the registrar library
- `../../src/logger` (transitively, via the registrar)
- `../../src/constants` (transitively, via the registrar)

These are NOT installed as `node_modules` dependencies — they live in
the bot's main `src/` tree. The infra-repo Lambda packaging step
(`qurl-integrations-infra`) is responsible for bundling them into the
Lambda's deployment artifact along with `@aws-sdk/client-ssm`. Two
common options:

1. **esbuild bundle** — `esbuild index.js --bundle --platform=node
   --target=node22 --external:@aws-sdk/* --outfile=dist/index.js`.
   Bundles everything into one file; `@aws-sdk/client-ssm` stays
   external because the Lambda runtime provides it.

2. **zip with hand-rolled file list** — copy `index.js`, the relevant
   subset of `../../src/*.js`, plus a flat `node_modules/@aws-sdk/client-ssm`,
   into a deploy zip. More verbose but avoids a build step.

The bundling config itself lives in the infra repo, not here, so the
in-app surface stays runtime-agnostic.

## Logger safety in Lambda

`src/logger.js` is synchronous (`console.error` / `console.log`
straight to stdio) and reads only `process.env.LOG_LEVEL` at module-
load time — no winston/pino async transports, no boot-time AWS calls,
no env vars required to load. Safe to require in a Lambda execution
context. Setting `LOG_LEVEL=info` on the Lambda function is sufficient
for normal operation.

## Testing

Unit tests live at `apps/discord/tests/lambda/webhook-registrar/index.test.js`
and run as part of the main bot test suite (`npx jest` from
`apps/discord/`). They mock `@aws-sdk/client-ssm` via
`aws-sdk-client-mock` and `global.fetch` for the qurl-service calls.
