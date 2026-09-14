#!/usr/bin/env bash
# Exercise the built production image without published ports or external I/O.
set -euo pipefail

image=${1:?Usage: docker-smoke.sh IMAGE}
container=$(docker run --detach --network none \
  -e HOST=0.0.0.0 -e PORT=3000 -e AWS_REGION=us-east-1 -e AWS_EC2_METADATA_DISABLED=true \
  -e TEAMS_BASE_URL=https://teams.example.com -e QURL_ENDPOINT=https://qurl.example.com \
  -e TEAMS_APP_ID=a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d -e TEAMS_APP_PASSWORD=synthetic-bot-secret \
  -e BOT_TENANT_ID=b1c2d3e4-f5a6-4b7c-8d9e-0f1a2b3c4d5e \
  -e QURL_IMAGE=ghcr.io/layervai/qurl@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  -e QURL_TEAMS_TENANT_PRINCIPALS_TABLE=principals -e QURL_TEAMS_CHANNEL_POLICIES_TABLE=policies \
  -e QURL_TEAMS_PERSONAL_CONVERSATIONS_TABLE=conversations -e QURL_TEAMS_TENANT_CREDENTIALS_TABLE=credentials \
  -e QURL_TEAMS_TENANT_CREDENTIALS_KMS_KEY_ARN=synthetic-key -e OAUTH_STATE_TABLE=oauth-state \
  -e AUTH0_DOMAIN=https://auth.example.com -e AUTH0_CLIENT_ID=synthetic-client \
  -e AUTH0_CLIENT_SECRET=synthetic-secret -e AUTH0_AUDIENCE=https://qurl.example.com \
  -e AUTH0_EXPECTED_AUDIENCE=https://qurl.example.com \
  "$image")
cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then docker logs "$container" >&2 || true; fi
  docker rm --force "$container" >/dev/null || true
  exit "$result"
}
trap cleanup EXIT

docker exec --interactive "$container" node --input-type=module <<'NODE'
import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
assert.equal(process.arch, 'arm64');
assert.equal(process.env.NODE_ENV, 'production');
assert.notEqual(process.getuid(), 0);
assert.equal(existsSync('node_modules/vitest'), false);
const origin = 'http://127.0.0.1:3000';
const request = (path, options) => fetch(`${origin}${path}`, { ...options, signal: AbortSignal.timeout(2000) });
let health;
for (let attempt = 0; attempt < 50; attempt++) {
  try { health = await request('/health'); break; }
  catch (error) { if (attempt === 49) throw error; await new Promise(resolve => setTimeout(resolve, 100)); }
}
assert.equal(health.status, 200);
assert.deepEqual(await health.json(), { ok: true });
assert.equal(health.headers.get('x-powered-by'), null);
for (const [body, status] of [
  ['{"type":"message"}', 401],
  [JSON.stringify({ text: 'x'.repeat(1_048_576) }), 413],
  ['{', 400],
]) {
  const response = await request('/api/messages', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body });
  assert.equal(response.status, status);
  assert.doesNotMatch(await response.text(), /PayloadTooLargeError|SyntaxError|node_modules/);
}
console.log(JSON.stringify({ health: 200, unsignedMessage: 401, oversizedMessage: 413, malformedMessage: 400, node: process.version, uid: process.getuid(), arch: process.arch }));
NODE

docker stop --signal SIGTERM --timeout 30 "$container" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$container")
printf 'SIGTERM exit code: %s\n' "$exit_code"
test "$exit_code" -eq 0
