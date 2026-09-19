import { describe, expect, it, vi } from 'vitest';
import { normalizeActivityText } from '../src/activity.js';

describe('Teams activity text normalization', () => {
  it('strips the bot self-mention and surrounding markup', () => {
    const text = '<at>qURL</at> list';
    expect(normalizeActivityText({
      text,
      recipient: { id: 'bot' },
      entities: [{ type: 'mention', text: '<at>qURL</at>', offset: 0, length: 13, mentioned: { id: 'bot' } }],
    })).toBe('list');
  });

  it('does not apply overlapping mention replacements twice', () => {
    const text = '<at>qURL</at> list';
    expect(normalizeActivityText({
      text,
      recipient: { id: 'bot' },
      entities: [
        { type: 'mention', text: '<at>qURL</at>', offset: 0, length: 13, mentioned: { id: 'bot' } },
        { type: 'mention', text: '<at>qURL</at>', offset: 0, length: 13, mentioned: { id: 'other' } },
      ],
    })).toBe('list');
  });

  it('falls back to the mention text when offsets do not match', () => {
    expect(normalizeActivityText({
      text: '<at>qURL</at> list',
      recipient: { id: 'bot' },
      entities: [{ type: 'mention', text: '<at>qURL</at>', offset: 999, length: 13, mentioned: { id: 'bot' } }],
    })).toBe('list');
  });

  it('rejects excessive mention work before scanning text', () => {
    const entity = { type: 'mention', text: '<at>qURL</at>', mentioned: { id: 'bot' } };
    expect(() => normalizeActivityText({ text: 'list', entities: Array.from({ length: 257 }, () => entity) })).toThrow('too many mentions');
    expect(normalizeActivityText({ text: 'list', entities: Array.from({ length: 256 }, () => entity) })).toBe('list');
  });

  it('skips a whole occupied mention range when finding a fallback mention', () => {
    const large = 'a'.repeat(100_000);
    const indexOf = vi.spyOn(String.prototype, 'indexOf');
    try {
      expect(normalizeActivityText({
        text: large + 'a', recipient: { id: 'bot' },
        entities: [
          { type: 'mention', text: large, offset: 0, length: large.length, mentioned: { id: 'bot' } },
          { type: 'mention', text: 'a', mentioned: { id: 'member' } },
        ],
      })).toBe('<@member>');
      expect(indexOf.mock.calls.filter(([text]) => text === 'a')).toHaveLength(2);
    } finally { indexOf.mockRestore(); }
  });

  it('scrubs residual Teams mention tags', () => {
    expect(normalizeActivityText({ text: '<at>unmodeled</at> list' })).toBe('list');
  });
});
