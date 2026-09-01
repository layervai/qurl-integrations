import { describe, expect, it } from 'vitest';
import { parseCommand, tokenize } from '../src/parser.js';

describe('Teams command parser', () => {
  it('accepts the verb allowlist and rejects unknown commands', () => {
    expect(parseCommand('list')).toMatchObject({ verb: 'list', args: [] });
    expect(() => parseCommand('not-a-command')).toThrow('Unknown qURL command');
  });

  it('handles quoted values and rejects unterminated quotes', () => {
    expect(tokenize('get $docs reason:"private docs"')).toEqual(['get', '$docs', 'reason:private docs']);
    expect(() => tokenize('get $docs reason:"private docs')).toThrow('unterminated quoted value');
  });

  it('validates setup email before starting OAuth', () => {
    expect(parseCommand('setup admin@example.com')).toMatchObject({ verb: 'setup', email: 'admin@example.com' });
    expect(() => parseCommand('setup admin')).toThrow('setup email is invalid');
  });

  it('parses and validates per-command flags', () => {
    expect(parseCommand('get $docs dm:true reason:"private docs"')).toMatchObject({ flags: { dm: 'true', reason: 'private docs' } });
    expect(parseCommand('protect-connector prod env:compose port:9090 alias:$web')).toMatchObject({ flags: { env: 'compose', port: '9090', alias: 'web' } });
    expect(() => parseCommand('get $docs dm:yes')).toThrow('dm flag must be true or false');
    expect(() => parseCommand('protect-connector prod port:0')).toThrow('connector port is invalid');
    expect(() => parseCommand('get dm:true')).toThrow('resource token is required');
  });

  it('requires safe HTTPS URL targets and valid mentions', () => {
    expect(parseCommand('protect-url url:https://example.com as:$docs')).toMatchObject({ verb: 'protect-url', flags: { as: 'docs' } });
    expect(() => parseCommand('protect-url url:http://example.com as:$docs')).toThrow('URL target must be HTTPS');
    expect(() => parseCommand('add not-a-mention')).toThrow('Teams user mention');
  });
});
