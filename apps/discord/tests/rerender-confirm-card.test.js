const {
  handleConfirmExpirySelect,
  handleConfirmSelfDestructSelect,
  makeInteraction,
} = require('./helpers/qurl-send-fixtures');

describe('rerenderConfirmCard cache-miss recipient fallback', () => {
  const u1 = '100000000000000001';
  const persistedAlias = 'Alice (display)';

  function makeSelectInteraction({ value = '3d', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('renders persisted alias when member-cache is empty', async () => {
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: persistedAlias },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
    };
    const int = makeSelectInteraction({ value: '3d', guildMembers: {} });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Alice/);
    expect(lastEdit.content).not.toMatch(new RegExp(u1));
  });

  test('warningsBlock from payload carries across menu interactions', async () => {
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: 'Alice' },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
      warningsBlock: '⚠️ Skipped bots: 1\n\n',
    };
    const int = makeSelectInteraction({ value: '3d', guildMembers: { [u1]: {} } });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Skipped bots/);
  });

  test('off-EXPIRY_LABELS expiresIn in payload → renderer defaults the 24h option (no first-option misrepresentation)', async () => {
    const { StringSelectMenuBuilder } = require('discord.js');
    StringSelectMenuBuilder.mockClear();
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: ['100000000000000001'],
      recipientAliases: { '100000000000000001': 'Alice' },
      expiresIn: '999d',  // corrupted: not in EXPIRY_LABELS
      selfDestructSeconds: null,
      personalMessage: null,
    };
    const int = makeInteraction({ guildMembers: { '100000000000000001': {} } });
    int.values = ['30'];  // legit self-destruct preset
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    const expirySelectCalls = StringSelectMenuBuilder.mock.results
      .filter((r) => {
        const calls = r.value.setCustomId.mock.calls;
        return calls.length && calls[0][0] === 'qurl_confirm_expiry';
      });
    expect(expirySelectCalls.length).toBeGreaterThan(0);
    const expiryAddOptionsArgs = expirySelectCalls[expirySelectCalls.length - 1]
      .value.addOptions.mock.calls[0];
    const defaultedExpiryOptions = expiryAddOptionsArgs.filter((o) => o.default);
    expect(defaultedExpiryOptions).toHaveLength(1);
    expect(defaultedExpiryOptions[0].value).toBe('24h');
  });

  test('voice-mode + empty recipientIds → "0 users in #voice" + Send disabled (no auto-revert to picker)', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [],
      recipientAliases: {},
      recipientMode: 'voice',
      voiceChannelId: 'voice-empty',
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
    };
    const int = makeInteraction();
    int.values = ['3d'];
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/0 users in <#voice-empty>/);
    expect(lastEdit.content).not.toMatch(/you not included/);
    expect(lastEdit.content).not.toMatch(/Pick recipients below/);
    const sendBtn = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_send'
    );
    expect(sendBtn.value.setDisabled).toHaveBeenCalledWith(true);
    const customIds = ButtonBuilder.mock.results.map(
      (r) => r.value.setCustomId.mock.calls[0]?.[0]
    );
    expect(customIds).toContain('qurl_confirm_pick_manual');
  });
});
