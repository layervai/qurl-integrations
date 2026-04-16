/**
 * Discord HTTP API client for E2E testing.
 * Uses bot token to read/write messages and verify bot behavior.
 */

const API = 'https://discord.com/api/v9';

export interface DiscordMessage {
  id: string;
  channel_id: string;
  author: { id: string; username: string; bot?: boolean };
  content: string;
  embeds: Array<{
    title?: string;
    description?: string;
    author?: { name: string };
    fields?: Array<{ name: string; value: string; inline?: boolean }>;
    footer?: { text: string };
    color?: number;
  }>;
  components?: Array<{
    type: number;
    components: Array<{
      type: number;
      label?: string;
      custom_id?: string;
      style?: number;
      disabled?: boolean;
    }>;
  }>;
  flags?: number;
  timestamp: string;
}

async function api(token: string, method: string, path: string, body?: unknown): Promise<any> {
  const headers: Record<string, string> = {
    Authorization: `Bot ${token}`,
  };
  const opts: RequestInit = { method, headers };
  if (body) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(`${API}${path}`, opts);
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`Discord API ${method} ${path}: ${res.status} ${text}`);
  }
  if (res.status === 204) return null;
  return res.json();
}

/** Send a message to a channel */
export async function sendMessage(token: string, channelId: string, content: string): Promise<DiscordMessage> {
  return api(token, 'POST', `/channels/${channelId}/messages`, { content });
}

/** Get recent messages from a channel */
export async function getMessages(token: string, channelId: string, limit = 25): Promise<DiscordMessage[]> {
  return api(token, 'GET', `/channels/${channelId}/messages?limit=${limit}`);
}

/** Get messages after a specific message ID */
export async function getMessagesAfter(token: string, channelId: string, afterId: string, limit = 25): Promise<DiscordMessage[]> {
  return api(token, 'GET', `/channels/${channelId}/messages?after=${afterId}&limit=${limit}`);
}

/** Get DM channel with another user */
export async function getDMChannel(token: string, userId: string): Promise<{ id: string }> {
  return api(token, 'POST', '/users/@me/channels', { recipient_id: userId });
}

/** Get messages from a DM channel */
export async function getDMMessages(token: string, userId: string, limit = 10): Promise<DiscordMessage[]> {
  const dm = await getDMChannel(token, userId);
  return getMessages(token, dm.id, limit);
}

/** Get the bot's own user info */
export async function getMe(token: string): Promise<{ id: string; username: string }> {
  return api(token, 'GET', '/users/@me');
}

/** Poll for a new message from a specific author in a channel */
export async function waitForMessage(
  token: string,
  channelId: string,
  opts: { fromAuthorId?: string; afterMessageId?: string; containsText?: string; timeoutMs?: number; pollIntervalMs?: number },
): Promise<DiscordMessage> {
  const timeout = opts.timeoutMs ?? 60_000;
  const interval = opts.pollIntervalMs ?? 2_000;
  const deadline = Date.now() + timeout;

  while (Date.now() < deadline) {
    const messages = opts.afterMessageId
      ? await getMessagesAfter(token, channelId, opts.afterMessageId)
      : await getMessages(token, channelId, 10);

    for (const msg of messages) {
      if (opts.fromAuthorId && msg.author.id !== opts.fromAuthorId) continue;
      if (opts.containsText && !msg.content.includes(opts.containsText) &&
          !msg.embeds.some(e => JSON.stringify(e).includes(opts.containsText!))) continue;
      return msg;
    }

    await new Promise((r) => setTimeout(r, interval));
  }
  throw new Error(`No matching message in ${channelId} within ${timeout}ms`);
}

/** Extract QURL link from an embed */
export function extractQurlLink(msg: DiscordMessage): string | null {
  const allText = msg.embeds.map(e =>
    [e.description, ...(e.fields?.map(f => f.value) ?? [])].join(' ')
  ).join(' ');
  const match = allText.match(/https:\/\/qurl\.(link|io|dev|site)\/[A-Za-z0-9_-]+/);
  return match ? match[0] : null;
}

/** Extract all button labels from a message */
export function extractButtons(msg: DiscordMessage): string[] {
  return (msg.components ?? []).flatMap(row =>
    row.components.filter(c => c.type === 2).map(c => c.label ?? c.custom_id ?? '')
  );
}
