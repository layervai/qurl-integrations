import type { App } from '@microsoft/teams.apps';
import { toActivityParams, type ActivityLike } from '@microsoft/teams.api';
import type { TeamsActivity } from './activity.js';
import type { TeamsMessagePoster } from './connector.js';

const TRUSTED_SERVICE_HOSTS = new Set([
  'smba.trafficmanager.net',
  'smba.infra.gcc.teams.microsoft.com',
  'smba.infra.gov.teams.microsoft.us',
  'smba.infra.dod.teams.microsoft.us',
]);

export function validateTeamsServiceUrl(value: string): string {
  const url = new URL(value);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.search || url.hash
    || !TRUSTED_SERVICE_HOSTS.has(url.hostname.toLowerCase())) {
    throw new Error('Teams service URL is not trusted');
  }
  return url.toString().replace(/\/$/, '');
}

/**
 * Teams SDK-backed outbound adapter. The SDK owns Bot Framework credentials,
 * service-token handling, and Connector REST calls; qURL business logic only
 * sees the existing TeamsMessagePoster seam.
 *
 * TODO(upstream-contract): keep this allowlist aligned with Microsoft's
 * documented regional Bot Framework service hosts.
 */
export class TeamsSdkMessagePoster implements TeamsMessagePoster {
  readonly #app: App;

  constructor(app: App) {
    this.#app = app;
  }

  async reply(activity: TeamsActivity, text: string, signal?: AbortSignal): Promise<void> {
    await this.sendActivity(activity.serviceUrl ?? '', activity.conversation?.id ?? '', { type: 'message', text, ...(activity.id === undefined ? {} : { replyToId: activity.id }) }, signal);
  }

  async sendText(serviceUrl: string, conversationId: string, text: string, signal?: AbortSignal): Promise<void> {
    await this.sendActivity(serviceUrl, conversationId, { type: 'message' as const, text }, signal);
  }

  private async sendActivity(serviceUrl: string, conversationId: string, activity: ActivityLike, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) throw new Error('Teams message delivery was cancelled');
    const baseUrl = validateTeamsServiceUrl(serviceUrl);
    const params = toActivityParams(activity);
    // The Teams API activity helper does not expose Axios request options.
    // Use the SDK's authenticated HTTP client directly so cancellation and
    // the outbound timeout remain part of this adapter's contract.
    await this.#app.api.http.post(`${baseUrl}/v3/conversations/${encodeURIComponent(conversationId)}/activities`, params, {
      timeout: 15_000,
      ...(signal === undefined ? {} : { signal }),
    });
  }
}
