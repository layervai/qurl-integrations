# Changelog

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

* **cli:** port the knock-only connector supervisor and link the FRP fork ([#1127](https://github.com/layervai/qurl-integrations/issues/1127)) ([047e1e5](https://github.com/layervai/qurl-integrations/commit/047e1e512f19f5f7fd76ab46a7d30de3359facd7))
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
