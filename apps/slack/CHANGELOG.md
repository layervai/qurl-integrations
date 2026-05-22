# Changelog

## [0.1.1](https://github.com/layervai/qurl-integrations/compare/slack-v0.1.0...slack-v0.1.1) (2026-05-22)


### Features

* **cli:** add QURL CLI tool ([#8](https://github.com/layervai/qurl-integrations/issues/8)) ([e8ed942](https://github.com/layervai/qurl-integrations/commit/e8ed942b78990db4eb93fef33da7fab633176758))
* scaffold qurl-integrations monorepo ([#1](https://github.com/layervai/qurl-integrations/issues/1)) ([071e7ff](https://github.com/layervai/qurl-integrations/commit/071e7ffd4dba4db4c0c6c9a2cc2b3e9fa2ea1cab))
* **slack:** ack-then-async slash commands with idempotency ([#158](https://github.com/layervai/qurl-integrations/issues/158)) ([a9cad40](https://github.com/layervai/qurl-integrations/commit/a9cad40e23eb55e5bd20dc3497c07ba6ba253042))
* **slack:** add /qurl setalias and /qurl unsetalias verbs (rewrite of [#230](https://github.com/layervai/qurl-integrations/issues/230) for net/http) ([#347](https://github.com/layervai/qurl-integrations/issues/347)) ([61cef42](https://github.com/layervai/qurl-integrations/commit/61cef427fbde5b759f1ef4f6ecf9a092f6baafaa))
* **slack:** add once:true flag on /qurl get for one-time-use links ([#481](https://github.com/layervai/qurl-integrations/issues/481)) ([0f3fa6d](https://github.com/layervai/qurl-integrations/commit/0f3fa6de9a8bfc61a201dc95eee668e6550dca14))
* **slack:** collapse /qurl setup + admin claim — installer becomes seed admin ([#482](https://github.com/layervai/qurl-integrations/issues/482)) ([ba130bf](https://github.com/layervai/qurl-integrations/commit/ba130bf7a09d73fdff930f295d648100ded57f98))
* **slack:** consolidate /qurl create into /qurl get, fix alias resolution, rewrite user-facing copy ([#447](https://github.com/layervai/qurl-integrations/issues/447)) ([ed86775](https://github.com/layervai/qurl-integrations/commit/ed86775f161c1e1d3e6592257e23b57a8033765f))
* **slack:** parser + idempotency helper + views ([#228](https://github.com/layervai/qurl-integrations/issues/228)) ([dfb2879](https://github.com/layervai/qurl-integrations/commit/dfb2879b46c8170901724b221491d7f0bc7c2dd8))
* **slack:** per-workspace OAuth — DDBProvider + /oauth/qurl/{start,callback} ([#254](https://github.com/layervai/qurl-integrations/issues/254)) ([14693eb](https://github.com/layervai/qurl-integrations/commit/14693eba625c7f04f4b64fe88c43ee2e404ba151))
* **slack:** qurl get + aliases + admin claim with async response_url ([#233](https://github.com/layervai/qurl-integrations/issues/233)) ([0880fb3](https://github.com/layervai/qurl-integrations/commit/0880fb3f037882c8ac5a406f2c522fdaecef97be))
* **slack:** refactor /qurl list to resources, add $r_* fallback in /qurl get ([#234](https://github.com/layervai/qurl-integrations/issues/234)) ([f7b0457](https://github.com/layervai/qurl-integrations/commit/f7b04575ef0da5498e064ea01824774c5cf8ce50))
* **slack:** scope-cut admin surface to claim/revoke/add/remove/list (v1 beta) ([#231](https://github.com/layervai/qurl-integrations/issues/231)) ([2723dae](https://github.com/layervai/qurl-integrations/commit/2723daebc426cd805fcca2e953d5a58dc0a0096b))
* **slack:** wire AliasStore so /qurl setalias and /qurl unsetalias actually work ([#431](https://github.com/layervai/qurl-integrations/issues/431)) ([bf50961](https://github.com/layervai/qurl-integrations/commit/bf5096167c50dbb6f10a31a3ac980fc03dd3a484))


### Bug Fixes

* **slack,shared:** mint-by-resource_id calls /v1/resources/{id}/qurls ([#454](https://github.com/layervai/qurl-integrations/issues/454)) ([270ef1a](https://github.com/layervai/qurl-integrations/commit/270ef1ac6a3bb3c0b511e8812f0bf332cd0f67a3))
* **slack:** verify HMAC signature on every Slack endpoint ([#71](https://github.com/layervai/qurl-integrations/issues/71)) ([#73](https://github.com/layervai/qurl-integrations/issues/73)) ([410746a](https://github.com/layervai/qurl-integrations/commit/410746a0e72bbbb8935cd3eaaaa6b25fd5bfabe9))

## Changelog
