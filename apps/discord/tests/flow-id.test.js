/**
 * Unit tests for src/flow-id.js — the canonical parse/build pair
 * for the shard-aware flow_state.flow_id composite key.
 *
 * Roundtrip and validation surface. Same-format parity is enforced
 * here so a handler producing flow_ids and a worker consuming them
 * can't silently drift on the separator convention.
 */
const { buildFlowId, parseFlowId } = require('../src/flow-id');

describe('flow-id', () => {
  describe('buildFlowId', () => {
    test('joins components with # in canonical order', () => {
      expect(buildFlowId({
        shard_id: '0:1',
        guild_id: 'g',
        channel_id: 'c',
        user_id: 'u',
      })).toBe('0:1#g#c#u');
    });

    test('allows ":" inside shard_id (canonical k:n shape)', () => {
      expect(buildFlowId({
        shard_id: '3:8',
        guild_id: '1234',
        channel_id: '5678',
        user_id: '9012',
      })).toBe('3:8#1234#5678#9012');
    });

    test('rejects "#" inside shard_id', () => {
      expect(() => buildFlowId({
        shard_id: '0#1',
        guild_id: 'g',
        channel_id: 'c',
        user_id: 'u',
      })).toThrow(/shard_id must not contain '#'/);
    });

    test('rejects "#" inside guild_id', () => {
      expect(() => buildFlowId({
        shard_id: '0:1',
        guild_id: 'evil#guild',
        channel_id: 'c',
        user_id: 'u',
      })).toThrow(/guild_id must not contain '#'/);
    });

    test('rejects "#" inside channel_id', () => {
      expect(() => buildFlowId({
        shard_id: '0:1',
        guild_id: 'g',
        channel_id: 'evil#chan',
        user_id: 'u',
      })).toThrow(/channel_id must not contain '#'/);
    });

    test('rejects "#" inside user_id', () => {
      expect(() => buildFlowId({
        shard_id: '0:1',
        guild_id: 'g',
        channel_id: 'c',
        user_id: 'evil#user',
      })).toThrow(/user_id must not contain '#'/);
    });

    test.each([
      ['shard_id', { shard_id: '', guild_id: 'g', channel_id: 'c', user_id: 'u' }],
      ['guild_id', { shard_id: '0:1', guild_id: '', channel_id: 'c', user_id: 'u' }],
      ['channel_id', { shard_id: '0:1', guild_id: 'g', channel_id: '', user_id: 'u' }],
      ['user_id', { shard_id: '0:1', guild_id: 'g', channel_id: 'c', user_id: '' }],
    ])('rejects empty %s', (field, args) => {
      expect(() => buildFlowId(args)).toThrow(new RegExp(`${field} must be a non-empty string`));
    });

    test.each([
      ['shard_id', { shard_id: undefined, guild_id: 'g', channel_id: 'c', user_id: 'u' }],
      ['guild_id', { shard_id: '0:1', guild_id: null, channel_id: 'c', user_id: 'u' }],
      ['channel_id', { shard_id: '0:1', guild_id: 'g', channel_id: 123, user_id: 'u' }],
    ])('rejects non-string %s', (field, args) => {
      expect(() => buildFlowId(args)).toThrow(new RegExp(`${field} must be a non-empty string`));
    });
  });

  describe('parseFlowId', () => {
    test('inverts buildFlowId for the canonical case', () => {
      const built = buildFlowId({
        shard_id: '0:1',
        guild_id: '1234',
        channel_id: '5678',
        user_id: '9012',
      });
      expect(parseFlowId(built)).toEqual({
        shard_id: '0:1',
        guild_id: '1234',
        channel_id: '5678',
        user_id: '9012',
      });
    });

    test('preserves ":" inside shard_id on parse', () => {
      expect(parseFlowId('3:8#g#c#u')).toEqual({
        shard_id: '3:8',
        guild_id: 'g',
        channel_id: 'c',
        user_id: 'u',
      });
    });

    test('returns null for too few separators', () => {
      expect(parseFlowId('0:1#g#c')).toBeNull();
    });

    test('returns null for too many separators', () => {
      expect(parseFlowId('0:1#g#c#u#extra')).toBeNull();
    });

    test('returns null for empty component (leading sep)', () => {
      expect(parseFlowId('#g#c#u')).toBeNull();
    });

    test('returns null for empty component (trailing sep)', () => {
      expect(parseFlowId('0:1#g#c#')).toBeNull();
    });

    test('returns null for empty component (mid)', () => {
      expect(parseFlowId('0:1##c#u')).toBeNull();
    });

    test.each([
      ['empty string', ''],
      ['null', null],
      ['undefined', undefined],
      ['number', 12345],
      ['object', {}],
    ])('returns null for non-string input: %s', (_label, input) => {
      expect(parseFlowId(input)).toBeNull();
    });
  });

  describe('roundtrip property', () => {
    // Sample of realistic-shape inputs — the assertion is that
    // parseFlowId(buildFlowId(x)) deep-equals x for every legal x.
    const cases = [
      { shard_id: '0:1', guild_id: '1', channel_id: '2', user_id: '3' },
      { shard_id: '0:1', guild_id: '111111111111111111', channel_id: '222222222222222222', user_id: '333333333333333333' },
      { shard_id: '7:16', guild_id: 'g', channel_id: 'c', user_id: 'u' },
    ];
    test.each(cases)('roundtrips %o', (input) => {
      expect(parseFlowId(buildFlowId(input))).toEqual(input);
    });
  });
});
