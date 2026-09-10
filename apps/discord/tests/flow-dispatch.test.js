
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
const logger = require('../src/logger');
const {
  registerFlow,
  siblingMessageForStage,
  flowIdForInteraction,
  handleFlowInteraction,
  SUPERSEDED_MSG,
} = require('../src/flow-dispatch');
const { UNSUPPORTED_CONTEXT_MSG } = require('../src/interaction-context');

function makeInteraction(overrides = {}) {
  return {
    customId: 'test_prefix',
    user: { id: 'user-123' },
    channelId: 'channel-456',
    guildId: 'guild-789',
    replied: false,
    deferred: false,
    isMessageComponent: () => true,
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
    const flow_id = flowIdForInteraction(makeInteraction({
      customId: 'qurl_revoke_select:user-VICTIM',
      user: { id: 'user-ATTACKER' },
    }));
    expect(flow_id).toContain('user-ATTACKER');
    expect(flow_id).not.toContain('user-VICTIM');
  });
});

describe('registerFlow', () => {
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

  it('rejects non-string siblingMessage when provided', () => {
    expect(() => registerFlow('sm_bad_type', {
      expectedStage: 's_bad_type', handler: jest.fn(), siblingMessage: 42,
    })).toThrow(/siblingMessage must be a non-empty string when provided/);
    expect(() => registerFlow('sm_bad_empty', {
      expectedStage: 's_bad_empty', handler: jest.fn(), siblingMessage: '',
    })).toThrow(/siblingMessage must be a non-empty string when provided/);
  });

  it('rejects re-registering a stage with a DIFFERENT siblingMessage', () => {
    registerFlow('sm_first_prefix', {
      expectedStage: 'sm_consistent_stage',
      handler: jest.fn(),
      siblingMessage: 'first message',
    });
    expect(() => registerFlow('sm_second_prefix', {
      expectedStage: 'sm_consistent_stage',
      handler: jest.fn(),
      siblingMessage: 'second message',
    })).toThrow(/already has a different siblingMessage/);
  });

  it('allows omitting siblingMessage entirely (no entry in the lookup)', () => {
    registerFlow('sm_optional_prefix', {
      expectedStage: 'sm_optional_stage',
      handler: jest.fn(),
    });
    expect(siblingMessageForStage('sm_optional_stage')).toBeNull();
  });
});

describe('siblingMessageForStage', () => {
  it('returns the message registered alongside a customId for that stage', () => {
    registerFlow('sm_lookup_prefix', {
      expectedStage: 'sm_lookup_stage',
      handler: jest.fn(),
      siblingMessage: 'You have an X in progress — finish it first.',
    });
    expect(siblingMessageForStage('sm_lookup_stage'))
      .toBe('You have an X in progress — finish it first.');
  });

  it('returns null for an unregistered stage', () => {
    expect(siblingMessageForStage('never_registered_stage')).toBeNull();
  });

  it('returns null for non-string input (defensive against caller bugs)', () => {
    expect(siblingMessageForStage(undefined)).toBeNull();
    expect(siblingMessageForStage(null)).toBeNull();
    expect(siblingMessageForStage(42)).toBeNull();
    expect(siblingMessageForStage('')).toBeNull();
  });
});

describe('handleFlowInteraction', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it('rejects a component resumed from a DM before loading flow state', async () => {
    const handler = jest.fn();
    registerFlow('route_dm_component', { expectedStage: 'awaiting', handler });
    const interaction = makeInteraction({
      customId: 'route_dm_component',
      guildId: null,
    });

    await handleFlowInteraction(interaction);

    expect(loadFlow).not.toHaveBeenCalled();
    expect(handler).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'flow-dispatch: unsupported interaction context, rejecting',
      { customId: 'route_dm_component', has_guild: false },
    );
    expect(interaction.update).toHaveBeenCalledWith({
      content: UNSUPPORTED_CONTEXT_MSG,
      components: [],
    });
  });

  it('rejects a modal resumed from a user-only install inside a guild', async () => {
    const handler = jest.fn();
    registerFlow('route_user_install_modal', { expectedStage: 'awaiting', handler });
    const interaction = makeInteraction({
      customId: 'route_user_install_modal',
      authorizingIntegrationOwners: { 1: 'user-123' },
      isMessageComponent: () => false,
    });

    await handleFlowInteraction(interaction);

    expect(loadFlow).not.toHaveBeenCalled();
    expect(handler).not.toHaveBeenCalled();
    expect(interaction.update).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith({
      content: UNSUPPORTED_CONTEXT_MSG,
      ephemeral: true,
    });
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

  it('allows a dual install when the guild authorized the resumed flow', async () => {
    const handler = jest.fn().mockResolvedValue(undefined);
    registerFlow('route_dual_install', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue({ stage: 'awaiting', version: 1 });
    const interaction = makeInteraction({
      customId: 'route_dual_install',
      authorizingIntegrationOwners: {
        0: 'guild-789',
        1: 'user-123',
      },
    });

    await handleFlowInteraction(interaction);

    expect(loadFlow).toHaveBeenCalledWith('0:1#guild-789#channel-456#user-123');
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('updates the source card when row is missing (component path)', async () => {
    const handler = jest.fn();
    registerFlow('route_missing', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({ customId: 'route_missing' });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.update).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      components: [],
    });
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('updates the source card when row stage does not match (component path)', async () => {
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
    expect(interaction.update).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      components: [],
    });
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('replies (does not update) for modal submits with no source card', async () => {
    const handler = jest.fn();
    registerFlow('route_modal_missing', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({
      customId: 'route_modal_missing',
      isMessageComponent: () => false,
    });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.update).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      ephemeral: true,
    });
  });

  it('falls back to reply when update fails (e.g. source ephemeral dismissed)', async () => {
    const handler = jest.fn();
    registerFlow('route_update_fails', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({
      customId: 'route_update_fails',
      update: jest.fn().mockRejectedValue(new Error('Unknown Message')),
    });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.update).toHaveBeenCalled();
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

  it('updates the source card when loadFlow throws (component path)', async () => {
    const handler = jest.fn();
    registerFlow('route_load_throws', { expectedStage: 'awaiting', handler });
    loadFlow.mockRejectedValue(new Error('DDB outage'));
    const interaction = makeInteraction({ customId: 'route_load_throws' });

    await handleFlowInteraction(interaction);

    expect(handler).not.toHaveBeenCalled();
    expect(interaction.update).toHaveBeenCalledWith({
      content: SUPERSEDED_MSG,
      components: [],
    });
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('uses followUp when interaction is already acked (no source-card update)', async () => {
    const handler = jest.fn();
    registerFlow('route_followup', { expectedStage: 'awaiting', handler });
    loadFlow.mockResolvedValue(null);
    const interaction = makeInteraction({
      customId: 'route_followup',
      replied: true,
    });

    await handleFlowInteraction(interaction);

    expect(interaction.update).not.toHaveBeenCalled();
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
      update: jest.fn().mockRejectedValue(new Error('Unknown Message')),
      reply: jest.fn().mockRejectedValue(new Error('Unknown interaction')),
    });

    await expect(handleFlowInteraction(interaction)).resolves.toBeUndefined();
  });

  it('catches handler throws and replies a generic error', async () => {
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
