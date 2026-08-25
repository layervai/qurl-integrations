export interface TeamsAccount {
  readonly id?: string;
  readonly name?: string;
  readonly aadObjectId?: string;
}

export interface TeamsConversation {
  readonly id?: string;
  readonly conversationType?: string;
  readonly tenantId?: string;
  readonly isGroup?: boolean;
}

export interface TeamsEntity {
  readonly type?: string;
  readonly mentioned?: TeamsAccount;
  readonly text?: string;
}

export interface TeamsChannelData {
  readonly tenant?: { readonly id?: string };
  readonly channel?: { readonly id?: string };
}

export interface TeamsActivity {
  readonly id?: string;
  readonly type?: string;
  readonly text?: string;
  readonly serviceUrl?: string;
  readonly from?: TeamsAccount;
  readonly recipient?: TeamsAccount;
  readonly conversation?: TeamsConversation;
  readonly channelData?: TeamsChannelData;
  readonly entities?: readonly TeamsEntity[];
}

export interface TeamsScope {
  readonly tenantId: string;
  readonly scopeId: string;
  readonly channel: boolean;
}

export function normalizeActivityText(activity: TeamsActivity): string {
  let text = activity.text ?? '';
  for (const entity of activity.entities ?? []) {
    if (entity.type !== 'mention' || !entity.mentioned?.id || !entity.text) continue;
    const mention = entity.text.replace(/^<at>|<\/at>$/gi, '').trim();
    const botId = activity.recipient?.id;
    text = text.replace(entity.text, entity.mentioned.id === botId ? '' : `<@${entity.mentioned.id}>`);
    if (mention && entity.mentioned.id !== botId) text = text.replace(mention, `<@${entity.mentioned.id}>`);
  }
  return text.replace(/<at>[^<]*<\/at>/gi, '').replace(/\s+/g, ' ').trim();
}

export function deriveScope(activity: TeamsActivity): TeamsScope {
  const tenantId = (activity.channelData?.tenant?.id ?? activity.conversation?.tenantId ?? '').trim().toLowerCase();
  const conversationId = (activity.conversation?.id ?? '').trim();
  const channel = activity.conversation?.conversationType === 'channel'
    || Boolean(activity.channelData?.channel?.id);
  // A channel conversation id identifies a thread, not the durable channel
  // policy scope. Prefer Teams' channel id so policies apply to every thread.
  const channelId = (activity.channelData?.channel?.id ?? '').trim();
  return { tenantId, scopeId: channel ? (channelId || conversationId) : 'personal', channel };
}
