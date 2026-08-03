import { verifyDoubleSubmitCookie } from './cookies.js';
import { OAuthCoreError, isOAuthCoreError } from './errors.js';
import type {
  CallbackCompletion,
  ConfidentialTokenClient,
  IdTokenVerifier,
  Logger,
  OAuthStateConsumer,
  ProviderBinder,
} from './interfaces.js';
import { nullLogger } from './interfaces.js';

export interface OAuthCallbackInput {
  readonly state: string;
  readonly cookieState?: string;
  readonly code: string;
}

export interface OAuthCallbackCoreOptions {
  readonly state: OAuthStateConsumer;
  readonly tokenClient: ConfidentialTokenClient;
  readonly idTokenVerifier: IdTokenVerifier;
  readonly providerBinder: ProviderBinder;
  readonly logger?: Logger;
}

export class OAuthCallbackCore {
  readonly #state: OAuthStateConsumer;
  readonly #tokenClient: ConfidentialTokenClient;
  readonly #idTokenVerifier: IdTokenVerifier;
  readonly #providerBinder: ProviderBinder;
  readonly #logger: Logger;

  constructor(options: OAuthCallbackCoreOptions) {
    this.#state = options.state;
    this.#tokenClient = options.tokenClient;
    this.#idTokenVerifier = options.idTokenVerifier;
    this.#providerBinder = options.providerBinder;
    this.#logger = options.logger ?? nullLogger;
  }

  async complete(input: OAuthCallbackInput): Promise<CallbackCompletion> {
    verifyDoubleSubmitCookie(input.cookieState, input.state);
    if (!input.code || input.code.length > 4_096) {
      throw new OAuthCoreError('INVALID_INPUT', 'Authorization code is invalid.');
    }

    // One-shot state is deliberately burned before any token or bind side effect.
    const transaction = await this.#state.consume(input.state);
    const tokens = await this.#tokenClient.exchangeAuthorizationCode({
      code: input.code,
      codeVerifier: transaction.pkceVerifier,
    });
    const identity = await this.#idTokenVerifier.verify(tokens.idToken, {
      nonce: transaction.oidcNonce,
      normalizedEmail: transaction.setupEmail,
    });

    try {
      const binding = await this.#providerBinder.bind({
        teamsTenantId: transaction.teamsTenantId,
        actorAadObjectId: transaction.actorAadObjectId,
        actorDeliveryId: transaction.actorDeliveryId,
        setupMode: transaction.setupMode,
        providerSubject: identity.subject,
        providerEmail: identity.email,
        accessToken: tokens.accessToken,
      });
      return {
        teamsTenantId: transaction.teamsTenantId,
        actorAadObjectId: transaction.actorAadObjectId,
        actorDeliveryId: transaction.actorDeliveryId,
        setupMode: transaction.setupMode,
        identity,
        binding,
      };
    } catch (error) {
      const errorCode = isOAuthCoreError(error) ? error.code : 'BINDING_FAILED';
      this.#logger.error('Provider binding failed after verified OAuth callback.', { errorCode });
      throw new OAuthCoreError('BINDING_FAILED', 'Provider binding could not be completed.', { retryable: true });
    }
  }
}
