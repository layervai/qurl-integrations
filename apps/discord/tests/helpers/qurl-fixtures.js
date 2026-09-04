// TODO(upstream-contract): Real-shaped IDs copied from qurl-service's CRID v1
// known-answer test. These fixtures cover accepted shapes only; this repo does
// not verify the CRID's derivation from the public key. Keep their shapes
// aligned with internal/crid/crid_test.go when its accepted forms change.
const PUBLIC_KEY_RESOURCE_ID = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEcOtuxu2qhc3gt1E7BiEU0CLqEDlXDwzZq0JnESgMAwERX6y_XXF5Cn5SKITWIZQmUhCZ0pHHlVn7SmFUTAnTGQ';
const CRID_RESOURCE_ID = 'qe4jqpd7eaoslq7jinmjv4yikgzmcxgpjfsuobiniqnko32lpw742pueoujq';

module.exports = { PUBLIC_KEY_RESOURCE_ID, CRID_RESOURCE_ID };
