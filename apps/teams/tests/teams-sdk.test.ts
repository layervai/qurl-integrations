import { describe, expect, it } from 'vitest';
import { TeamsSdkMessagePoster, validateTeamsServiceUrl } from '../src/teams-sdk.js';

describe('TeamsSdkMessagePoster', () => {
  it('validates configured Teams service URLs while preserving their region path', () => {
    expect(validateTeamsServiceUrl('https://smba.trafficmanager.net/teams')).toBe('https://smba.trafficmanager.net/teams');
    expect(() => validateTeamsServiceUrl('https://attacker.example/teams')).toThrow('not trusted');
    expect(() => validateTeamsServiceUrl('http://smba.trafficmanager.net/teams')).toThrow('not trusted');
  });

  it('rejects outbound messages to untrusted service URLs', async () => {
    const poster = new TeamsSdkMessagePoster({ api: { http: {} } } as never);
    await expect(poster.sendText('https://attacker.example', 'conversation', 'hello')).rejects.toThrow('not trusted');
  });

  it('uses the SDK client for an allowlisted Teams service URL', async () => {
    const calls: Array<{ readonly url: string; readonly body: unknown }> = [];
    const poster = new TeamsSdkMessagePoster({
      id: 'bot-id',
      api: { http: {
        request: async () => ({ data: {} }),
        post: async (url: string, body: unknown) => { calls.push({ url, body }); return { data: {} }; },
      } },
    } as never);
    await poster.sendText('https://smba.trafficmanager.net/teams', 'conversation', 'hello');
    expect(calls[0]?.url).toContain('/v3/conversations/conversation/activities');
  });
});
