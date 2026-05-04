// Defense-in-depth helpers for parsing untrusted req.query values.
//
// Express parses repeated query params (`?code=a&code=b`) as arrays;
// the route code's habitual `String(req.query.x || '')` then
// stringifies the array as comma-joined ("a,b"). Auth0 + Discord
// would reject the resulting token-exchange request, so this isn't a
// known exploit — but rejecting up-front is cleaner posture than
// passing a smuggled-comma payload through to upstream services.

/**
 * Returns `value` if it's a single string param, otherwise empty string.
 * Use for any req.query field that the route logic treats as scalar
 * (code, state, error, guild_id).
 */
function singleStringParam(value) {
  return typeof value === 'string' ? value : '';
}

module.exports = { singleStringParam };
