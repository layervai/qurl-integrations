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

  it('preserves embedded and lone quotes in display names', () => {
    for (const text of ['My "cool" name', 'My "cool name']) {
      expect(parseCommand(`set-display-name $docs ${text}`)).toMatchObject({
        verb: 'set-display-name', resource: 'docs', text,
      });
    }
  });

  it('strips only surrounding display-name quotes with quoted resources and command prefixes', () => {
    for (const prefix of ['', 'qurl ', '/qurl ', 'qurl-admin ', '/qurl-admin ']) {
      for (const quote of ['"', "'"]) {
        expect(parseCommand(`${prefix}set-display-name "$docs" ${quote}  My "cool" name  ${quote}`)).toMatchObject({
          verb: 'set-display-name', resource: 'docs', text: 'My "cool" name',
        });
      }
    }
    expect(() => parseCommand('set-display-name $docs "  "')).toThrow('display name is required');
    expect(() => parseCommand(`set-display-name $docs "${'a'.repeat(201)}"`)).toThrow('display name is too long');
  });

  it('validates setup email before starting OAuth', () => {
    expect(parseCommand('setup admin@example.com')).toMatchObject({ verb: 'setup', email: 'admin@example.com' });
    expect(() => parseCommand('setup admin')).toThrow('setup email is invalid');
    const longestEmail = `${'a'.repeat(242)}@example.com`;
    expect(parseCommand(`setup ${longestEmail}`)).toMatchObject({ email: longestEmail });
    expect(() => parseCommand(`setup a${longestEmail}`)).toThrow('setup email is invalid');
  });

  it('parses and validates per-command flags', () => {
    expect(parseCommand('get $docs dm:true reason:"private docs"')).toMatchObject({ flags: { dm: 'true', reason: 'private docs' } });
    expect(parseCommand('get $docs DM:TRUE Reason:"On Call"')).toMatchObject({ flags: { dm: 'true', reason: 'On Call' } });
    expect(parseCommand('get $docs Dm:FALSE')).toMatchObject({ flags: { dm: 'false' } });
    expect(() => parseCommand('get $docs DM:yes')).toThrow('dm flag must be true or false');
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
