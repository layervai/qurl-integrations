'use strict';

// TODO(infra-sentinel-sync): qurl-integrations-infra seeds optional SSM
// parameters with this literal in qurl-bot-discord/terraform/main.tf
// (`value = "PLACEHOLDER"` and `value_wo = "PLACEHOLDER"`). Update this
// application-side constant in lockstep. If they drift, a seeded sentinel can
// pass as configured and crash-loop a task or defer failure to OAuth.
const SSM_PLACEHOLDER_SENTINEL = 'PLACEHOLDER';

module.exports = { SSM_PLACEHOLDER_SENTINEL };
