'use strict';

// TODO(infra-sentinel-sync): qurl-integrations-infra seeds optional SSM
// parameters with this literal value. If infra changes the sentinel, update
// this single application-side constant in lockstep.
const SSM_PLACEHOLDER_SENTINEL = 'PLACEHOLDER';

module.exports = { SSM_PLACEHOLDER_SENTINEL };
