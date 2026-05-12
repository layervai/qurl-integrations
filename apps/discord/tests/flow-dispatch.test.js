/**
 * Unit tests for src/flow-dispatch.js — the customId → handler
 * routing layer that replaces in-process `awaitMessageComponent` for
 * component / modal interactions.
 *
 * Trust-model surface: flow_id is derived from interaction context
 * (user.id + channelId + shard), NEVER from customId. Tests pin that
 * a hostile customId cannot reach a sibling user's flow row even if
 * the routing prefix matches.
 *
 * Stage-mismatch surface: the dispatcher's primary guard is
 * `row.stage === expectedStage`. A flow that has advanced past its
 * registered stage (e.g. PR 6's setup modal having already moved to
 * `awaiting_complete`) must yield the same "superseded" reply as a
 * missing row — both signal "this customId can't act now."
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('../src/flow-state', () => ({
  loadFlow: jest.fn(),
}));

jest.mock('../src/config', () => ({
  SHARD_ID: '0:1',
}));

const { loadFlow } = require('../src/flow-state');
const {
  registerFlow,
  flowIdForInteraction,
  handleFlowInteraction,
  SUPERSEDED_MSG,
} = require('../src/flow-dispatch');

function makeInteraction(overrides = {}) {
  return {
    customId: 'test_prefix',
    user: { id: 'user-123' },
    channelId: 'channel-456',
    guildId: 'guild-789',
    replied: false,
    deferred: false,
    reply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    update: jest.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

describe('flowIdForInteraction', () => {
  it('builds canonical flow_id from interaction context (guild)', () => {
    const flow_id = flowIdForInteraction(makeInteraction());
    expect(flow_id).toBe('0:1#guild-789#channel-456#user-123');
  });

  it('namespaces DM context under dm:<user_id>', () => {
    const flow_id = flowIdForInteraction(makeInteraction({ guildId: null }));
    expect(flow_id).toBe('0:1#dm:user-123#channel-456#user-123');
  });

  it('uses interaction.user.id, NOT anything from customId', () => {
    // Security pin: a hostile customId encoding "user-VICTIM" must
    // not affect routing — the trusted identity is the interaction
    // context. (This is implicit since flowIdForInteraction doesn't
    // read customId at all, but pin it so a future refactor that
    // adds customId-derived disambiguation surfaces in review.)
    const flow_id = flowIdForInteraction(makeInteraction({
      customId: 'qurl_revoke_select:user-VICTIM',
      user: { id: 'user-ATTACKER' },
    }));
    expect(flow_id).toContain('user-ATTACKER');
    expect(flow_id).not.toContain('user-VICTIM');
  });
});

describe('registerFlow', () => {
  // registerFlow mutates module-private state, which leaks across
  // tests in this describe. Use unique prefixes per case so we don't
  // need a reset hook.
  it('rejects duplicate registration', () => {
    registerFlow('dup_prefix', { expectedStage: 's', handler: jest.fn() });
    expect(() => registerFlow('dup_prefix', { expectedStage: 's', handler: jest.fn() }))
      .toThrow(/already registered/);
  });

  it('rejects non-string customId', () => {
    expect(() => registerFlow('', { expectedStage: 's', handler: jest.fn() }))
      .toThrow(/customId must be a non-empty string/);
  });

  it('rejects non-string expectedStage', () => {
    expect(() => registerFlow('bad_stage_prefix', { expectedStage: '', handler: jest.fn() }))
      .toThrow(/expectedStage must be a non-empty string/);
  });

  it('rejects non-function handler', () => {
    expect(() => registerFlow('bad_handler_prefix', { expectedStage: 's', handler: null }))
      .toThrow(/handler must be a function/);
  });
});

describe('handleFlowInteraction', () => {
  // Tests use a per-test prefix to avoid the global registry leaking
  // state between describe blocks. Each test registers its own
  // unique prefix.
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it('routes to the registered handler when stage matches', async () => {
    const handler = jest.fn().mockResolvedValue(undefined);
    registerFlow('route_match', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue({
      flow_id: '0:1#g#c#u',
      stage: 'awaiting',
      version: 1,
    });
    const interaction = makeInteraction({
      customId: 'route_match',
      user: { id: 'u' },
      channelId: 'c',
      guildId: 'g',
    });

    await handleFlowInteraction(interaction);

    expect(loadFlow).toHaveBeenCalledWith('0:1#g#c#u');
    expect(handler).toHaveBeenCalledTimes(1);
    const [passedInteraction, ctx] = handler.mock.calls[0];
    expect(passedInteraction).toBe(interaction);
    expect(ctx.flow_id).toBe('0:1#g#c#u');
    expect(ctx.row.stage).toBe('awaiting');
  });

  it('replies superseded when row is missing', async () => {
    const handler = jest.fn();
    registerFlow('route_missing', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({ customId: 'route_missing' });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      ephemeral: true,
    });
  });

  it('replies superseded when row stage does not match expectedStage', async () => {
    const handler = jest.fn();
    registerFlow('route_stage_mismatch', { expectedStage: 'awaiting_select', handler });
    loadFlow.mockResolvedValue({
      flow_id: '0:1#g#c#u',
      stage: 'awaiting_modal', // different stage
      version: 1,
    });
    const interaction = makeInteraction({ customId: 'route_stage_mismatch' });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      ephemeral: true,
    });
  });

  it('silently drops unknown customId without loading flow', async () => {
    const interaction = makeInteraction({ customId: 'never_registered_prefix' });

    await handleFlowInteraction(interaction);

    expect(loadFlow).not.toHaveBeenCalled();
    expect(interaction.reply).not.toHaveBeenCalled();
    expect(interaction.update).not.toHaveBeenCalled();
  });

  it('silently drops when customId is empty/missing', async () => {
    const interaction = makeInteraction({ customId: '' });

    await handleFlowInteraction(interaction);

    expect(loadFlow).not.toHaveBeenCalled();
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('replies superseded when loadFlow throws', async () => {
    const handler = jest.fn();
    registerFlow('route_load_throws', { expectedStage: 'awaiting', handler });
    loadFlow.mockRejectedValue(new Error('DDB outage'));
    const interaction = makeInteraction({ customId: 'route_load_throws' });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      ephemeral: true,
    });
  });

  it('uses followUp instead of reply when interaction is already replied', async () => {
    const handler = jest.fn();
    registerFlow('route_followup', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({
      customId: 'route_followup',
      replied: true,
    });

    await handleFlowInteraction(interaction);

    expect(interaction.reply).not.toHaveBeenCalled();
    expect(interaction.followUp).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      ephemeral: true,
    });
  });

  it('swallows reply failures so a stale interaction does not throw', async () => {
    registerFlow('route_reply_throws', {
      expectedStage: 'awaiting',
      handler: jest.fn(),
    });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({
      customId: 'route_reply_throws',
      reply: jest.fn().mockRejectedValue(new Error('Unknown interaction')),
    });

    // Must not throw — silent swallow with a warn log is the contract.
    await expect(handleFlowInteraction(interaction)).resolves.toBeUndefined();
  });

  it('catches handler throws and replies a generic error', async () => {
    // Safety-net pin: a handler that throws after committing an
    // irreversible flow_state side effect (e.g. the deleteFlow-first
    // ordering in handleRevokeSelect) must not produce an
    // unhandledRejection. The user gets a recoverable message.
    const handler = jest.fn().mockRejectedValue(new Error('downstream API died'));
    registerFlow('route_throws', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue({
      flow_id: '0:1#g#c#u',
      stage: 'awaiting',
      version: 1,
    });
    const interaction = makeInteraction({ customId: 'route_throws' });

    await handleFlowInteraction(interaction);

    expect(handler).toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/Something went wrong/),
      }),
    );
  });

  it('flow_id passed to handler is derived from interaction context', async () => {
    // Anti-spoofing pin: even if the customId contained user/channel
    // hints, the handler receives the flow_id built from
    // interaction.user.id + interaction.channelId.
    const handler = jest.fn().mockResolvedValue(undefined);
    registerFlow('route_spoof_check', { expectedStage: 's', handler });
    loadFlow.mockResolvedValue({ flow_id: '0:1#g#c#actual-user', stage: 's', version: 1 });
    const interaction = makeInteraction({
      customId: 'route_spoof_check',
      user: { id: 'actual-user' },
      channelId: 'c',
      guildId: 'g',
    });

    await handleFlowInteraction(interaction);

    const ctx = handler.mock.calls[0][1];
    expect(ctx.flow_id).toBe('0:1#g#c#actual-user');
  });
});
