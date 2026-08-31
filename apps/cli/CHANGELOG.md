# Changelog

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
