# Changelog

## [2.3.2](https://github.com/layervai/qurl-integrations/compare/v2.3.1...v2.3.2) (2026-09-05)


### Bug Fixes

* **ci:** keep macOS grant probe on direct egress ([#1307](https://github.com/layervai/qurl-integrations/issues/1307)) ([e730dbf](https://github.com/layervai/qurl-integrations/commit/e730dbf6c66f2168d813999709a7c4a6a4db8dbd))

## [2.3.1](https://github.com/layervai/qurl-integrations/compare/v2.3.0...v2.3.1) (2026-09-05)


### Bug Fixes

* **cli:** recover reauthorized headless sharing state ([#1388](https://github.com/layervai/qurl-integrations/issues/1388)) ([100e8ad](https://github.com/layervai/qurl-integrations/commit/100e8adee4b4d3966adc142f878b0b05f0a592ea))

## [2.3.0](https://github.com/layervai/qurl-integrations/compare/v2.2.0...v2.3.0) (2026-09-05)


### Features

* **cli:** add a per-share session group mode for the local daemon ([#1331](https://github.com/layervai/qurl-integrations/issues/1331)) ([ed2c047](https://github.com/layervai/qurl-integrations/commit/ed2c0475612f91bf4949e756af815bbf8698db8b))
* **cli:** serve every local share on one Connector session ([#1326](https://github.com/layervai/qurl-integrations/issues/1326)) ([946d0f5](https://github.com/layervai/qurl-integrations/commit/946d0f59db6bfe6e0ac51bbe0b0b8724bce84a2e))


### Bug Fixes

* **cli:** allow Connector name reuse after deletion ([#1384](https://github.com/layervai/qurl-integrations/issues/1384)) ([e7b8345](https://github.com/layervai/qurl-integrations/commit/e7b83451ee65678d734724c9e7773423a8e860a9))
* **cli:** bound retired and pending Connector resource memories ([#1334](https://github.com/layervai/qurl-integrations/issues/1334)) ([a92fbe3](https://github.com/layervai/qurl-integrations/commit/a92fbe3418648711ba52e91fa9aa31a704fd646b))
* **cli:** converge a deleted share's row when its Connector ID was rebound ([#1341](https://github.com/layervai/qurl-integrations/issues/1341)) ([5dbd819](https://github.com/layervai/qurl-integrations/commit/5dbd81920a9860580f1cbc4898847b71956318ee))
* **cli:** cut saturated live-tailed chains at the link before the share ([#1336](https://github.com/layervai/qurl-integrations/issues/1336)) ([549bff4](https://github.com/layervai/qurl-integrations/commit/549bff4fd50f861699ad6c676031952ebefbfbdf))
* **cli:** keep a platform-refused route retrying instead of turning it off ([#1330](https://github.com/layervai/qurl-integrations/issues/1330)) ([cc2d6df](https://github.com/layervai/qurl-integrations/commit/cc2d6df25700a21ae745d5a544cd30457298c694))
* **cli:** keep live default-ID chains intact under retired eviction ([#1335](https://github.com/layervai/qurl-integrations/issues/1335)) ([fa5b5aa](https://github.com/layervai/qurl-integrations/commit/fa5b5aa2c71f7fdcec14821a701335031eb49cd1))
* **cli:** reauthorize headless resources before serving ([#1387](https://github.com/layervai/qurl-integrations/issues/1387)) ([cdac5cd](https://github.com/layervai/qurl-integrations/commit/cdac5cd78531805b4e33d0bc6a7ee71651fd11ef))

## [2.2.0](https://github.com/layervai/qurl-integrations/compare/v2.1.1...v2.2.0) (2026-09-02)


### ⚠ BREAKING CHANGES

* **cli:** rename the resolve command to share (hard cutover) ([#1323](https://github.com/layervai/qurl-integrations/issues/1323))

### Features

* **cli:** rename the resolve command to share (hard cutover) ([#1323](https://github.com/layervai/qurl-integrations/issues/1323)) ([ae6a567](https://github.com/layervai/qurl-integrations/commit/ae6a567a4828da75bf3332fae39d31fb908eaddb))


### Bug Fixes

* **ci:** validate generated Homebrew archive templates ([#1317](https://github.com/layervai/qurl-integrations/issues/1317)) ([e60cb25](https://github.com/layervai/qurl-integrations/commit/e60cb25d89bbbf5623bf2ec875b8cc2ac339ae21))

## [2.1.1](https://github.com/layervai/qurl-integrations/compare/v2.1.0...v2.1.1) (2026-09-01)


### Bug Fixes

* **cli:** use UDP-only Connector control plane ([#1312](https://github.com/layervai/qurl-integrations/issues/1312)) ([748f80c](https://github.com/layervai/qurl-integrations/commit/748f80c0a3f57298cf2449410a75423a88028a42))

## [2.1.0](https://github.com/layervai/qurl-integrations/compare/v2.0.3...v2.1.0) (2026-09-01)


### Features

* **cli:** ship registered CRID lifecycle ([#1279](https://github.com/layervai/qurl-integrations/issues/1279)) ([528695b](https://github.com/layervai/qurl-integrations/commit/528695bb8fe2e8418abb4f3c566719473bda3192))


### Bug Fixes

* **cli:** carry qv2 session bearer to content ([#1303](https://github.com/layervai/qurl-integrations/issues/1303)) ([502a0a3](https://github.com/layervai/qurl-integrations/commit/502a0a3cfe5f8cc43aff04924bb1670b54d4cb95))
* **cli:** classify transient access denials ([#1301](https://github.com/layervai/qurl-integrations/issues/1301)) ([f88c71f](https://github.com/layervai/qurl-integrations/commit/f88c71f1e7874c7300306a4c90c6a9ae3f6fce20))
* **cli:** complete packaged lifecycle journey ([#1296](https://github.com/layervai/qurl-integrations/issues/1296)) ([265d79b](https://github.com/layervai/qurl-integrations/commit/265d79b9b8fcbfa3b6835271a318936e62796009))
* **cli:** harden packaged lifecycle journey ([#1294](https://github.com/layervai/qurl-integrations/issues/1294)) ([dba10e9](https://github.com/layervai/qurl-integrations/commit/dba10e947eb3fc8b51ffd9a84f28069da134b796))
* **cli:** permit fail-closed dark releases ([#1300](https://github.com/layervai/qurl-integrations/issues/1300)) ([2c728e1](https://github.com/layervai/qurl-integrations/commit/2c728e1d7f6cf6f8d147167d97ba907c0e2fecaa))
* **cli:** preserve actionable lifecycle failures ([#1298](https://github.com/layervai/qurl-integrations/issues/1298)) ([ffd119b](https://github.com/layervai/qurl-integrations/commit/ffd119b018eeaf53cf957afa1e9b6dc7b6aeb59f))
* **cli:** repair packaged customer journey ([#1293](https://github.com/layervai/qurl-integrations/issues/1293)) ([8787051](https://github.com/layervai/qurl-integrations/commit/8787051bd37bf245e00adba1c149d67674200060))
* **cli:** use native UDP for Connector sessions ([#1308](https://github.com/layervai/qurl-integrations/issues/1308)) ([bb67fac](https://github.com/layervai/qurl-integrations/commit/bb67fac42dbc66d8780e2de317e9f108b25df601))
* **cli:** stabilize packaged CRID customer journey ([#1297](https://github.com/layervai/qurl-integrations/issues/1297)) ([db24f49](https://github.com/layervai/qurl-integrations/commit/db24f494a38d6bd4689a5e4dbbfd65f15cb9f66f))

## [2.0.3](https://github.com/layervai/qurl-integrations/compare/v2.0.2...v2.0.3) (2026-08-26)


### Bug Fixes

* **cli:** recover revoked Connector identity ([#1276](https://github.com/layervai/qurl-integrations/issues/1276)) ([aeb1eea](https://github.com/layervai/qurl-integrations/commit/aeb1eea0b696ab4bb19a7d3da7c65b98b1ca5c2c))
* **cli:** report status for remote URL resources ([#1274](https://github.com/layervai/qurl-integrations/issues/1274)) ([21c2474](https://github.com/layervai/qurl-integrations/commit/21c2474160e054a2f64651eb3d30351524aaac7b))

## [2.0.2](https://github.com/layervai/qurl-integrations/compare/v2.0.1...v2.0.2) (2026-08-26)


### Bug Fixes

* **cli:** bind and validate CRID lifecycle continuity ([#1272](https://github.com/layervai/qurl-integrations/issues/1272)) ([6d25194](https://github.com/layervai/qurl-integrations/commit/6d25194d824a9dcf8e1b7d5748d30909dd47dd9e))

## [2.0.1](https://github.com/layervai/qurl-integrations/compare/v2.0.0...v2.0.1) (2026-08-26)


### Bug Fixes

* **cli:** pin qurl-go and stabilize release smoke ([#1270](https://github.com/layervai/qurl-integrations/issues/1270)) ([8243c04](https://github.com/layervai/qurl-integrations/commit/8243c04cd967a112c2883d1e5b23ead93491fb21))

## [2.0.0](https://github.com/layervai/qurl-integrations/compare/v1.8.0...v2.0.0) (2026-08-26)


### ⚠ BREAKING CHANGES

* **cli:** ship CRID lifecycle commands ([#1266](https://github.com/layervai/qurl-integrations/issues/1266))

### Features

* **cli:** ship CRID lifecycle commands ([#1266](https://github.com/layervai/qurl-integrations/issues/1266)) ([16916c9](https://github.com/layervai/qurl-integrations/commit/16916c94937b36a63a8022946d64e7ce47053016))

## [1.8.0](https://github.com/layervai/qurl-integrations/compare/v1.7.0...v1.8.0) (2026-08-25)


### Features

* **cli:** add isolated lifecycle validation authority ([#1256](https://github.com/layervai/qurl-integrations/issues/1256)) ([7a1b061](https://github.com/layervai/qurl-integrations/commit/7a1b0613d527de8924c59f72aed91fc8a08fb190))
* **cli:** stabilize lifecycle validation with fixed test identities ([#1259](https://github.com/layervai/qurl-integrations/issues/1259)) ([78b0254](https://github.com/layervai/qurl-integrations/commit/78b025404be1d52e9fe9144a28812b71dcd80b96))

## [1.7.0](https://github.com/layervai/qurl-integrations/compare/v1.6.2...v1.7.0) (2026-08-21)


### Features

* **cli:** resolve Connector resources through the qURL platform ([79c7358](https://github.com/layervai/qurl-integrations/commit/79c735821c64dd7ce2f0590a8258ada8cc2e5402))


### Bug Fixes

* **cli:** bind Connector validation to its deployment ([#1242](https://github.com/layervai/qurl-integrations/issues/1242)) ([13cde81](https://github.com/layervai/qurl-integrations/commit/13cde8130a06fd0e212db1b345559daec8bb3d5e))
* **cli:** pin Connector validation to the trusted platform endpoint ([#1243](https://github.com/layervai/qurl-integrations/issues/1243)) ([f39ac2e](https://github.com/layervai/qurl-integrations/commit/f39ac2ed477e9f33dc83b075447e6e5f4742655e))

## [1.6.2](https://github.com/layervai/qurl-integrations/compare/v1.6.1...v1.6.2) (2026-08-20)


### Bug Fixes

* **cli:** route qv2t1 links through secure opener ([#1238](https://github.com/layervai/qurl-integrations/issues/1238)) ([e24091e](https://github.com/layervai/qurl-integrations/commit/e24091e076b543b2b6dcb7ec56e36dc8d5b3ac30))

## [1.6.1](https://github.com/layervai/qurl-integrations/compare/v1.6.0...v1.6.1) (2026-08-20)


### Bug Fixes

* **cli:** recover orphaned assignment refresh state ([#1236](https://github.com/layervai/qurl-integrations/issues/1236)) ([1ae4e5e](https://github.com/layervai/qurl-integrations/commit/1ae4e5e9e0434fcc137b75ee608716020b5eac3a))

## [1.6.0](https://github.com/layervai/qurl-integrations/compare/v1.5.0...v1.6.0) (2026-08-20)


### Features

* **cli:** publish local apps with one command ([#1192](https://github.com/layervai/qurl-integrations/issues/1192)) ([46ec229](https://github.com/layervai/qurl-integrations/commit/46ec229b200f2afc089b449367ab404acacf2351))


### Bug Fixes

* **cli:** attach the redial guards to the method that actually dials ([#1186](https://github.com/layervai/qurl-integrations/issues/1186)) ([f961226](https://github.com/layervai/qurl-integrations/commit/f9612261f85cf983832af6b332f58f8928f49f7d))
* **cli:** bound and explain a Connector tunnel that cannot stay up ([#1172](https://github.com/layervai/qurl-integrations/issues/1172)) ([6b6a5b0](https://github.com/layervai/qurl-integrations/commit/6b6a5b0356e1fbbb2881af3c7a186c3e4b31b595))
* **cli:** match the fork's QUIC casing in TLSEnabled, and mark three mirrors ([#1226](https://github.com/layervai/qurl-integrations/issues/1226)) ([e050a82](https://github.com/layervai/qurl-integrations/commit/e050a828cf7b61e013f78f12bf5d8a8c2f0c664b))

## [1.5.0](https://github.com/layervai/qurl-integrations/compare/v1.4.0...v1.5.0) (2026-08-19)


### Features

* **cli:** surface list-row description, type and tags in JSON output ([#1165](https://github.com/layervai/qurl-integrations/issues/1165)) ([76f33bd](https://github.com/layervai/qurl-integrations/commit/76f33bd09423df1a1c14827049f8e2ff56d0dab9))

## [1.4.0](https://github.com/layervai/qurl-integrations/compare/v1.3.0...v1.4.0) (2026-08-19)


### Features

* **cli:** log the Connector's CRID as a structured event ([#1163](https://github.com/layervai/qurl-integrations/issues/1163)) ([a3fed6a](https://github.com/layervai/qurl-integrations/commit/a3fed6a7f564094f5b57cb441aa5d9f8b97e4794))

## [1.3.0](https://github.com/layervai/qurl-integrations/compare/v1.2.0...v1.3.0) (2026-08-19)


### Features

* **cli:** print the Connector's CRID when it starts serving ([#1160](https://github.com/layervai/qurl-integrations/issues/1160)) ([383dc63](https://github.com/layervai/qurl-integrations/commit/383dc638c5eee313bfe2a976ae04eb5ac5759246))

## [1.2.0](https://github.com/layervai/qurl-integrations/compare/v1.1.0...v1.2.0) (2026-08-19)


### Features

* **cli:** adopt the customer term ID for the Connector run surface ([#1139](https://github.com/layervai/qurl-integrations/issues/1139)) ([d41b1fb](https://github.com/layervai/qurl-integrations/commit/d41b1fb8dc53ebf90a0a941e7dba99672a51a5d2))


### Bug Fixes

* **cli:** download target content through the platform access flow ([#1159](https://github.com/layervai/qurl-integrations/issues/1159)) ([f99bb13](https://github.com/layervai/qurl-integrations/commit/f99bb13af5db44aa7a331db3de583850e97ac40b))
* **cli:** speak customer language for Connector enrollment failures ([#1153](https://github.com/layervai/qurl-integrations/issues/1153)) ([37a38bf](https://github.com/layervai/qurl-integrations/commit/37a38bfdaee2baba4acd7e841361a2f54d357738))
* **cli:** tell the truth about republishing an already-published URL ([#1150](https://github.com/layervai/qurl-integrations/issues/1150)) ([be5f228](https://github.com/layervai/qurl-integrations/commit/be5f2288fd3d3d5ca29f72f78747c88f65dc0e79))

## [1.1.0](https://github.com/layervai/qurl-integrations/compare/v1.0.0...v1.1.0) (2026-08-18)


### Features

* **cli:** add the local Connector session supervisor ([#1127](https://github.com/layervai/qurl-integrations/issues/1127)) ([047e1e5](https://github.com/layervai/qurl-integrations/commit/047e1e512f19f5f7fd76ab46a7d30de3359facd7))
* **cli:** qurl connector run — serve a local app through the qURL platform ([#1130](https://github.com/layervai/qurl-integrations/issues/1130)) ([dcc40ee](https://github.com/layervai/qurl-integrations/commit/dcc40eeb3ef1006ec99814dc3c0dfedb0cfcf859))


### Bug Fixes

* **cli:** render the invalid refresh-mode sentinel in customer language ([#1131](https://github.com/layervai/qurl-integrations/issues/1131)) ([a06abef](https://github.com/layervai/qurl-integrations/commit/a06abef5c31a68d1fa0ea146d3fcc22d8fd28d25))

## [1.0.0](https://github.com/layervai/qurl-integrations/compare/v0.1.0...v1.0.0) (2026-08-18)


### ⚠ BREAKING CHANGES

* **cli:** crid-native v2 command tree replacing the token-era cli ([#1118](https://github.com/layervai/qurl-integrations/issues/1118))

### Features

* **cli:** add enter command for qv2 portal links ([#914](https://github.com/layervai/qurl-integrations/issues/914)) ([76b320c](https://github.com/layervai/qurl-integrations/commit/76b320ce2f5f034fbc5c95a379cc77359f8bae7d))
* **cli:** crid-native v2 command tree replacing the token-era cli ([#1118](https://github.com/layervai/qurl-integrations/issues/1118)) ([46be7d7](https://github.com/layervai/qurl-integrations/commit/46be7d7b5f0cb317946f861ef535f22ae3312f05))
* **cli:** os-keyring credential storage and the real login, logout, and whoami ([#1121](https://github.com/layervai/qurl-integrations/issues/1121)) ([44a2683](https://github.com/layervai/qurl-integrations/commit/44a26837db2ba7dbad8ae8c6cfa1f4eed7b7e29c))
* **cli:** port qURL Connector glue into internal connector packages ([#1124](https://github.com/layervai/qurl-integrations/issues/1124)) ([12f5c49](https://github.com/layervai/qurl-integrations/commit/12f5c49d2af086e4d4df356d6ec561d6fbd49ed6))
* **cli:** the real qurl get — verified browser-open and file download ([#1123](https://github.com/layervai/qurl-integrations/issues/1123)) ([93978c9](https://github.com/layervai/qurl-integrations/commit/93978c936488cd53c0767391c4f8a78d1b02875d))


### Bug Fixes

* **shared:** client.Create uses /v1/qurls (plural), not /v1/qurl ([#176](https://github.com/layervai/qurl-integrations/issues/176)) ([1538f16](https://github.com/layervai/qurl-integrations/commit/1538f169458c4f3435b5dc4299041ac5a4d86bcd))
* **shared:** send the qURL label field on create (was description) ([#632](https://github.com/layervai/qurl-integrations/issues/632)) ([2c78a81](https://github.com/layervai/qurl-integrations/commit/2c78a81924de4f158b37a197a2727889a3ae2608))
* **slack:** support public resource IDs ([#954](https://github.com/layervai/qurl-integrations/issues/954)) ([fc11fb5](https://github.com/layervai/qurl-integrations/commit/fc11fb58e1f0703dcaf923f4a6cc83d1049a2836))
