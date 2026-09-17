/**
 * Channel-alias grammar, shared by the command parser, the bot's alias-binding
 * path, and the connector install renderer.
 *
 * This is the single gate on what may be written to a channel-policy alias row:
 * the alias commands (`set-alias`, `unset-alias`) parse their argument through
 * the same grammar, so anything bound outside it can never be managed again.
 */
const CHANNEL_ALIAS_PATTERN = /^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function isChannelAlias(value: string): boolean {
  return CHANNEL_ALIAS_PATTERN.test(value);
}
