import type { TeamsActivity } from './activity.js';

export interface TeamsMessagePoster {
  reply(activity: TeamsActivity, text: string, signal?: AbortSignal): Promise<void>;
  sendText(serviceUrl: string, conversationId: string, text: string, signal?: AbortSignal): Promise<void>;
  resolveMemberAadObjectId?(activity: TeamsActivity, memberId: string, signal?: AbortSignal): Promise<string | undefined>;
}
