# Changelog

## [0.1.1](https://github.com/layervai/qurl-integrations/compare/discord-v0.1.0...discord-v0.1.1) (2026-04-30)


### Features

* **discord:** async store contract + DynamoDB backend ([#116](https://github.com/layervai/qurl-integrations/issues/116)) ([8c0d52a](https://github.com/layervai/qurl-integrations/commit/8c0d52ab229d4cad75194b768bfd8ea9d4b2c78e))
* **discord:** gateway/HTTP process split via PROCESS_ROLE env var ([#117](https://github.com/layervai/qurl-integrations/issues/117)) ([d31aac1](https://github.com/layervai/qurl-integrations/commit/d31aac11ee413238e054658c4994c7370a82b196))
* **discord:** migrate Discord bot from qurl-integrations-infra ([#40](https://github.com/layervai/qurl-integrations/issues/40)) ([ede0c3a](https://github.com/layervai/qurl-integrations/commit/ede0c3a57958cb1138602420e2bd713df1774c95))
* **discord:** migrate to Node.js bot with /qurl send ([#45](https://github.com/layervai/qurl-integrations/issues/45)) ([350e796](https://github.com/layervai/qurl-integrations/commit/350e79613730fe60ed9b14a24667cece873f6924))
* **discord:** rebrand QURL to qURL, revise DM copy, sanitize sender alias ([#124](https://github.com/layervai/qurl-integrations/issues/124)) ([4ac456a](https://github.com/layervai/qurl-integrations/commit/4ac456a0e403dfdd774d628e327885f5e27f9040))
* **discord:** structured audit events + 🔗 Step Through DM button ([#142](https://github.com/layervai/qurl-integrations/issues/142)) ([fafbad9](https://github.com/layervai/qurl-integrations/commit/fafbad9aeae842962af1aa57cb4df794ee167124))
* **discord:** support multi-tenant mode when GUILD_ID is unset ([#82](https://github.com/layervai/qurl-integrations/issues/82)) ([fb9ab1c](https://github.com/layervai/qurl-integrations/commit/fb9ab1c99cf5ae5d6fa83df4100aea190898c6fe))
* scaffold qurl-integrations monorepo ([#1](https://github.com/layervai/qurl-integrations/issues/1)) ([071e7ff](https://github.com/layervai/qurl-integrations/commit/071e7ffd4dba4db4c0c6c9a2cc2b3e9fa2ea1cab))


### Bug Fixes

* **discord:** always append informational note after /qurl revoke ([#111](https://github.com/layervai/qurl-integrations/issues/111)) ([d20860e](https://github.com/layervai/qurl-integrations/commit/d20860e123b9a59c53134622f69c5b6efe265d24))
* **discord:** gate voice target on invoking channel type, not sender-in-voice ([#100](https://github.com/layervai/qurl-integrations/issues/100)) ([35df012](https://github.com/layervai/qurl-integrations/commit/35df012428dac6950de4b77d754c44b04d16ffb1))
* **discord:** label /qurl status as admin-only in description + help ([#102](https://github.com/layervai/qurl-integrations/issues/102)) ([70ff355](https://github.com/layervai/qurl-integrations/commit/70ff355abf3bcf88709cad3bb383d0924cd8ce69))
* **discord:** polish /qurl help — clickable link, expiry note, glossary, plain-language scaling ([#98](https://github.com/layervai/qurl-integrations/issues/98)) ([3a7b701](https://github.com/layervai/qurl-integrations/commit/3a7b701cd2dac48c1037cf6ad8ff5b1a23b2ca24))
* **discord:** rename /qurl send options + revoke dropdown filter + descriptive labels + smoke test ([#106](https://github.com/layervai/qurl-integrations/issues/106)) ([3ffc3b6](https://github.com/layervai/qurl-integrations/commit/3ffc3b61ea8dd65fc0608931f0e2240965c378e4))
* **discord:** restore channel notification + dynamic target autocomplete ([#96](https://github.com/layervai/qurl-integrations/issues/96)) ([c666a8b](https://github.com/layervai/qurl-integrations/commit/c666a8b85f9750c4e38312fdcb9ec996642332c0))
* **discord:** restore voice-channel viewer enumeration (regression from 4b57bb2) ([#103](https://github.com/layervai/qurl-integrations/issues/103)) ([2466d6f](https://github.com/layervai/qurl-integrations/commit/2466d6f8ceee9c372a90df59c3485cd3bbfe5a4c))


### Performance Improvements

* **discord:** add timing instrumentation to /qurl send ([#42](https://github.com/layervai/qurl-integrations/issues/42)) ([24f4f82](https://github.com/layervai/qurl-integrations/commit/24f4f821d3a0483bea5f9fae0f20f8e7f485b77a))
