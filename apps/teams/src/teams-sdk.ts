import type { App } from '@microsoft/teams.apps';
import { Client, toActivityParams, type ActivityLike } from '@microsoft/teams.api';
import type { TeamsActivity } from './activity.js';
import type { TeamsMessagePoster } from './connector.js';

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

  async reply(activity: TeamsActivity, text: string): Promise<void> {
    await this.sendActivity(activity.serviceUrl ?? '', activity.conversation?.id ?? '', { type: 'message', text, ...(activity.id === undefined ? {} : { replyToId: activity.id }) });
  }

  async sendText(serviceUrl: string, conversationId: string, text: string): Promise<void> {
    await this.sendActivity(serviceUrl, conversationId, { type: 'message' as const, text });
  }

  private async sendActivity(serviceUrl: string, conversationId: string, activity: ActivityLike): Promise<void> {
    const base = new URL(serviceUrl);
    if (base.protocol !== 'https:' || base.username || base.password || base.port || base.search || base.hash
      || !['smba.trafficmanager.net', 'smba.infra.gcc.teams.microsoft.com', 'smba.infra.gov.teams.microsoft.us', 'smba.infra.dod.teams.microsoft.us'].includes(base.hostname.toLowerCase())) {
      throw new Error('Teams service URL is not trusted');
    }
    const client = new Client(serviceUrl.replace(/\/$/, ''), this.#app.api.http);
    const params = toActivityParams(activity);
    await client.conversations.createActivity(conversationId, params);
  }
}
