import { isChannelAlias } from './alias.js';
import type { SetupMode } from './interfaces.js';
import { normalizeTunnelEnvironment, validateTunnelSlug } from './tunnel.js';
import { UserFacingError } from './user-facing-error.js';

export const BOT_VERBS = Object.freeze([
  'help', 'setup', 'get', 'list', 'aliases', 'protect-url', 'protect-connector',
  'set-alias', 'unset-alias', 'set-display-name', 'unset-display-name',
  'add', 'remove', 'admins', 'revoke', 'uninstall', 'feedback',
] as const);

export interface TeamsCommand {
  readonly raw: string;
  readonly verb: string;
  readonly resource?: string;
  readonly alias?: string;
  readonly target?: string;
  readonly userId?: string;
  readonly email?: string;
  readonly setupMode?: SetupMode;
  readonly text?: string;
  readonly flags: Readonly<Record<string, string>>;
  readonly args: readonly string[];
}

const mentionPattern = /^<@([A-Za-z0-9._:-]{1,200})>$/;
const MAX_FEEDBACK_LENGTH = 2_000;
const MAX_DISPLAY_NAME_LENGTH = 200;
const MAX_REASON_LENGTH = 200;
const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function tokenize(text: string): string[] {
  const tokens: string[] = [];
  let current = '';
  let quoted = false;
  for (const char of text.trim()) {
    if (char === '"') { quoted = !quoted; continue; }
    if (/\s/.test(char) && !quoted) {
      if (current) { tokens.push(current); current = ''; }
    } else current += char;
  }
  if (quoted) throw new UserFacingError('unterminated quoted value');
  if (current) tokens.push(current);
  return tokens;
}

function lookup(value: string): string {
  const result = value.trim().replace(/^\$/, '');
  if (!result) throw new UserFacingError('missing resource token');
  return result;
}

function alias(value: string): string {
  const result = value.trim().replace(/^\$/, '');
  if (!isChannelAlias(result)) throw new UserFacingError('invalid alias');
  return result;
}

export function parseCommand(input: string): TeamsCommand {
  const raw = input.trim();
  const tokens = tokenize(raw);
  if (tokens[0]?.toLowerCase() === 'qurl' || tokens[0]?.toLowerCase() === '/qurl'
    || tokens[0]?.toLowerCase() === 'qurl-admin' || tokens[0]?.toLowerCase() === '/qurl-admin') tokens.shift();
  const verb = (tokens.shift() ?? 'help').toLowerCase();
  if (!(BOT_VERBS as readonly string[]).includes(verb)) throw new UserFacingError('Unknown qURL command.');
  const args = [...tokens];
  if (['help', 'list', 'aliases', 'admins', 'uninstall'].includes(verb) && args.length) throw new UserFacingError('unexpected argument');
  if (verb === 'setup') {
    if (!args[0]) throw new UserFacingError('setup email is required');
    if (args.length !== 1) throw new UserFacingError('OAuth setup supports only the bind flow');
    if (!emailPattern.test(args[0])) throw new UserFacingError('setup email is invalid');
    return { raw, verb, email: args[0], setupMode: 'bind', flags: {}, args };
  }
  if (verb === 'feedback') {
    if (!args.length) throw new UserFacingError('feedback message is required');
    const text = args.join(' ');
    if (text.length > MAX_FEEDBACK_LENGTH) throw new UserFacingError('feedback message is too long');
    return { raw, verb, text, flags: {}, args };
  }
  if (verb === 'get') {
    if (!args[0]) throw new UserFacingError('resource token is required');
    if (/^(?:dm|reason):/i.test(args[0])) throw new UserFacingError('resource token is required');
    const flags: Record<string, string> = {};
    for (const token of args.slice(1)) {
      const match = /^([a-z][a-z0-9_]*):(.*)$/.exec(token);
      const key = match?.[1];
      const value = match?.[2]?.trim();
      if (!key || value === undefined || !['dm', 'reason'].includes(key)) throw new UserFacingError('invalid get flag');
      if (key === 'dm' && value !== 'true' && value !== 'false') throw new UserFacingError('dm flag must be true or false');
      if (key === 'reason' && value.length > MAX_REASON_LENGTH) throw new UserFacingError('reason is too long');
      flags[key] = value;
    }
    return { raw, verb, resource: lookup(args[0]), flags, args };
  }
  if (verb === 'protect-url') {
    if (args.length < 1 || args.length > 2) throw new UserFacingError('protect-url requires a resource or URL and optional as:$alias');
    const target = args[0];
    if (!target) throw new UserFacingError('protect-url target is required');
    const flags: Record<string, string> = {};
    for (const token of args.slice(1)) {
      const match = /^as:(.*)$/i.exec(token);
      if (!match?.[1]) throw new UserFacingError('protect-url supports only as:$alias');
      flags.as = alias(match[1]);
    }
    if (target.toLowerCase().startsWith('url:')) {
      let url: URL;
      try { url = new URL(target.slice(4)); } catch { throw new UserFacingError('URL target is invalid'); }
      if (url.protocol !== 'https:' || url.username || url.password) throw new UserFacingError('URL target must be HTTPS without credentials');
      if (!flags.as) throw new UserFacingError('a channel alias is required for a new URL resource');
    }
    return { raw, verb, resource: lookup(target), flags, args };
  }
  if (verb === 'protect-connector') {
    if (!args[0]) throw new UserFacingError('connector id is required');
    const slug = lookup(args[0]);
    validateTunnelSlug(slug);
    const flags: Record<string, string> = {};
    for (const token of args.slice(1)) {
      const match = /^([a-z][a-z0-9_-]*):(.*)$/i.exec(token);
      const key = match?.[1]?.toLowerCase();
      const value = match?.[2]?.trim();
      if (!key || !value || !['env', 'port', 'alias'].includes(key)) throw new UserFacingError('invalid connector option');
      if (key === 'env') flags.env = normalizeTunnelEnvironment(value);
      else if (key === 'alias') flags.alias = alias(value);
      else {
        if (!/^\d+$/.test(value)) throw new UserFacingError('connector port is invalid');
        const port = Number(value);
        if (!Number.isInteger(port) || port < 1 || port > 65_535) throw new UserFacingError('connector port is invalid');
        flags.port = String(port);
      }
    }
    return { raw, verb, resource: slug, flags, args };
  }
  if (verb === 'set-alias') {
    if (args.length !== 2) throw new UserFacingError('set-alias requires alias and resource');
    const aliasArg = args[0]; const targetArg = args[1];
    if (!aliasArg || !targetArg) throw new UserFacingError('set-alias requires alias and resource');
    return { raw, verb, alias: alias(aliasArg), target: lookup(targetArg), flags: {}, args };
  }
  if (verb === 'set-display-name') {
    if (args.length < 2) throw new UserFacingError('display name is required');
    const resource = args[0];
    if (!resource) throw new UserFacingError('resource token is required');
    const text = args.slice(1).join(' ');
    if (text.length > MAX_DISPLAY_NAME_LENGTH) throw new UserFacingError('display name is too long');
    return { raw, verb, resource: lookup(resource), text, flags: {}, args };
  }
  if (['unset-alias', 'unset-display-name', 'revoke'].includes(verb)) {
    if (args.length !== 1) throw new UserFacingError('resource token is required');
    const resource = args[0];
    if (!resource) throw new UserFacingError('resource token is required');
    return { raw, verb, resource: verb === 'unset-alias' ? alias(resource) : lookup(resource), flags: {}, args };
  }
  if (['add', 'remove'].includes(verb)) {
    const match = mentionPattern.exec(args[0] ?? '');
    if (!match || args.length !== 1) throw new UserFacingError('a Teams user mention is required');
    const userId = match[1];
    if (!userId) throw new UserFacingError('a Teams user mention is required');
    return { raw, verb, userId, flags: {}, args };
  }
  return { raw, verb, args, flags: {} };
}
