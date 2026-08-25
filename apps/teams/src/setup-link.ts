import type { ConfidentialTokenClient, SetupMode } from './interfaces.js';
import type { OAuthStateManager } from './state.js';
import { pkceChallengeForVerifier } from './pkce.js';

export interface TeamsSetupLinkBuilderOptions { readonly state: OAuthStateManager; readonly tokenClient: ConfidentialTokenClient; }
export class TeamsSetupLinkBuilder {
  readonly #state: OAuthStateManager; readonly #tokenClient: ConfidentialTokenClient;
  readonly #setupBaseUrl: URL;
  constructor(options: TeamsSetupLinkBuilderOptions & { readonly setupBaseUrl: string }) {
    this.#state = options.state;
    this.#tokenClient = options.tokenClient;
    this.#setupBaseUrl = new URL(options.setupBaseUrl);
    if (this.#setupBaseUrl.protocol !== 'https:' || this.#setupBaseUrl.username !== ''
      || this.#setupBaseUrl.password !== '' || this.#setupBaseUrl.search !== ''
      || this.#setupBaseUrl.hash !== '' || this.#setupBaseUrl.pathname !== '/') {
      throw new Error('Teams setup base URL must be an HTTPS origin without credentials.');
    }
  }
  async build(tenantId: string, actorId: string, deliveryId: string, email: string, setupMode: SetupMode = 'bind'): Promise<{ readonly url: URL; readonly handle: string }> {
    const minted = await this.#state.mint({ teamsTenantId: tenantId, actorAadObjectId: actorId, actorDeliveryId: deliveryId, setupEmail: email, setupMode });
    const url = new URL('/oauth/qurl/start', this.#setupBaseUrl);
    url.searchParams.set('state', minted.handle);
    url.searchParams.set('code_challenge', pkceChallengeForVerifier(minted.transaction.pkceVerifier));
    url.searchParams.set('nonce', minted.transaction.oidcNonce);
    url.searchParams.set('login_hint', minted.transaction.setupEmail);
    // Keep the token client in this boundary so a malformed authorization
    // request is rejected before a link is sent to Teams. The URL itself is
    // intentionally the local start route, which is where the CSRF cookie is
    // created before the browser leaves for the provider.
    this.#tokenClient.createAuthorizationUrl({
      state: minted.handle,
      codeChallenge: pkceChallengeForVerifier(minted.transaction.pkceVerifier),
      nonce: minted.transaction.oidcNonce,
      loginHint: minted.transaction.setupEmail,
    });
    return { handle: minted.handle, url };
  }
}
