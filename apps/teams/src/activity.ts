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
  readonly offset?: number;
  readonly length?: number;
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

function record(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}

function stringValue(value: unknown): string | undefined { return typeof value === 'string' ? value : undefined; }

function account(value: unknown): TeamsAccount | undefined {
  const source = record(value);
  if (!source) return undefined;
  const id = stringValue(source.id);
  const name = stringValue(source.name);
  const aadObjectId = stringValue(source.aadObjectId);
  return { ...(id === undefined ? {} : { id }), ...(name === undefined ? {} : { name }), ...(aadObjectId === undefined ? {} : { aadObjectId }) };
}

/** Copy only the small, untrusted activity subset consumed by the bot. */
export function toTeamsActivity(value: unknown): TeamsActivity | undefined {
  const source = record(value);
  const id = stringValue(source?.id);
  const type = stringValue(source?.type);
  const text = stringValue(source?.text);
  const serviceUrl = stringValue(source?.serviceUrl);
  if (!source) return undefined;
  const from = account(source.from);
  const recipient = account(source.recipient);
  const conversationSource = record(source.conversation);
  const conversationId = stringValue(conversationSource?.id);
  const conversationType = stringValue(conversationSource?.conversationType);
  const conversationTenantId = stringValue(conversationSource?.tenantId);
  const conversation = conversationSource ? {
    ...(conversationId === undefined ? {} : { id: conversationId }),
    ...(conversationType === undefined ? {} : { conversationType }),
    ...(conversationTenantId === undefined ? {} : { tenantId: conversationTenantId }),
    ...(typeof conversationSource.isGroup === 'boolean' ? { isGroup: conversationSource.isGroup } : {}),
  } : undefined;
  const channelDataSource = record(source.channelData);
  const tenantSource = record(channelDataSource?.tenant);
  const channelSource = record(channelDataSource?.channel);
  const tenantId = stringValue(tenantSource?.id);
  const channelId = stringValue(channelSource?.id);
  const channelData = channelDataSource ? {
    ...(tenantSource ? { tenant: { ...(tenantId === undefined ? {} : { id: tenantId }) } } : {}),
    ...(channelSource ? { channel: { ...(channelId === undefined ? {} : { id: channelId }) } } : {}),
  } : undefined;
  const entities = Array.isArray(source.entities) ? source.entities.map(entityValue => {
    const entity = record(entityValue);
    if (!entity) return undefined;
    const mentioned = account(entity.mentioned);
    return {
      ...(stringValue(entity.type) === undefined ? {} : { type: stringValue(entity.type) }),
      ...(mentioned === undefined ? {} : { mentioned }),
      ...(stringValue(entity.text) === undefined ? {} : { text: stringValue(entity.text) }),
      ...(typeof entity.offset === 'number' ? { offset: entity.offset } : {}),
      ...(typeof entity.length === 'number' ? { length: entity.length } : {}),
    };
  }).filter((entity): entity is TeamsEntity => entity !== undefined) : undefined;
  return {
    ...(id === undefined ? {} : { id }),
    ...(type === undefined ? {} : { type }),
    ...(text === undefined ? {} : { text }),
    ...(serviceUrl === undefined ? {} : { serviceUrl }),
    ...(from === undefined ? {} : { from }),
    ...(recipient === undefined ? {} : { recipient }),
    ...(conversation === undefined ? {} : { conversation }),
    ...(channelData === undefined ? {} : { channelData }),
    ...(entities === undefined ? {} : { entities }),
  };
}

export function normalizeActivityText(activity: TeamsActivity): string {
  let text = activity.text ?? '';
  const replacements: Array<{ readonly start: number; readonly end: number; readonly value: string }> = [];
  const entities = [...(activity.entities ?? [])];
  const validEntities = entities
    .map((entity, index) => ({ entity, index }))
    .filter(({ entity }) => {
      const start = entity.offset;
      const length = entity.length;
      return typeof start === 'number' && Number.isInteger(start) && start >= 0
        && typeof length === 'number' && Number.isInteger(length) && length > 0
        && entity.type === 'mention' && Boolean(entity.text) && Boolean(entity.mentioned?.id) && start + length <= text.length
        && text.slice(start, start + length) === entity.text;
    })
    .sort((left, right) => (left.entity.offset ?? 0) - (right.entity.offset ?? 0) || left.index - right.index);
  const usedRanges: Array<{ readonly start: number; readonly end: number }> = [];
  const overlaps = (start: number, end: number): boolean => usedRanges.some(range => start < range.end && end > range.start);
  const addReplacement = (entity: TeamsEntity, start: number, end: number): void => {
    if (!entity.text || overlaps(start, end)) return;
    const botId = activity.recipient?.id;
    const value = entity.mentioned?.id === botId ? '' : `<@${entity.mentioned?.id ?? ''}>`;
    replacements.push({ start, end, value });
    usedRanges.push({ start, end });
  };
  for (const { entity } of validEntities) {
    addReplacement(entity, entity.offset ?? 0, (entity.offset ?? 0) + (entity.length ?? 0));
  }
  for (const entity of entities) {
    if (entity.type !== 'mention' || !entity.mentioned?.id || !entity.text) continue;
    const start = entity.offset;
    const length = entity.length;
    const end = start === undefined || length === undefined ? -1 : start + length;
    if (typeof start === 'number' && Number.isInteger(start) && start >= 0 && typeof length === 'number' && Number.isInteger(length) && length > 0 && end <= text.length && text.slice(start, end) === entity.text) continue;
    let fallbackStart = text.indexOf(entity.text);
    while (fallbackStart >= 0 && overlaps(fallbackStart, fallbackStart + entity.text.length)) fallbackStart = text.indexOf(entity.text, fallbackStart + 1);
    if (fallbackStart >= 0) addReplacement(entity, fallbackStart, fallbackStart + entity.text.length);
  }
  for (const replacement of replacements.sort((left, right) => right.start - left.start)) {
    text = text.slice(0, replacement.start) + replacement.value + text.slice(replacement.end);
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
