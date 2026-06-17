const {
  enforceProductionOAuthStateSecrets,
  MIN_OAUTH_STATE_SECRET_LENGTH,
  normalizeSecretValue,
  productionOAuthStateSecretWarnings,
  validateProductionOAuthStateSecrets,
} = require('../src/utils/oauth-state-secrets');

describe('oauth-state-secrets helpers', () => {
  it('normalizes whitespace and ignores the SSM placeholder sentinel', () => {
    expect(normalizeSecretValue(undefined)).toBeUndefined();
    expect(normalizeSecretValue('')).toBeUndefined();
    expect(normalizeSecretValue('   ')).toBeUndefined();
    expect(normalizeSecretValue('PLACEHOLDER')).toBeUndefined();
    expect(normalizeSecretValue(`  ${'g'.repeat(64)}  `)).toBe('g'.repeat(64));
  });

  describe('validateProductionOAuthStateSecrets', () => {
    it('accepts legacy OAUTH_STATE_SECRET during the GitHub migration window', () => {
      const { errors, secrets } = validateProductionOAuthStateSecrets({
        OAUTH_STATE_SECRET: 's'.repeat(64),
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: false,
      });

      expect(errors).toEqual([]);
      expect(secrets.legacy).toBe('s'.repeat(64));
    });

    it('rejects missing or placeholder GitHub OAuth state secrets in OpenNHP mode', () => {
      const { errors } = validateProductionOAuthStateSecrets({
        GITHUB_OAUTH_STATE_SECRET: 'PLACEHOLDER',
        OAUTH_STATE_SECRET: ' ',
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: false,
      });

      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('GITHUB_OAUTH_STATE_SECRET must be set');
    });

    it('rejects a short dedicated qURL/Auth0 state secret at boot', () => {
      const { errors } = validateProductionOAuthStateSecrets({
        QURL_OAUTH_STATE_SECRET: 'q'.repeat(MIN_OAUTH_STATE_SECRET_LENGTH - 1),
        OAUTH_STATE_SECRET: 's'.repeat(64),
      }, {
        isOpenNHPActive: false,
        isQurlOAuthConfigured: true,
      });

      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('QURL_OAUTH_STATE_SECRET must be at least 32 chars');
    });

    it('does not fail a valid dedicated secret because a legacy secret is short', () => {
      const { errors } = validateProductionOAuthStateSecrets({
        QURL_OAUTH_STATE_SECRET: 'q'.repeat(64),
        OAUTH_STATE_SECRET: 'short',
      }, {
        isOpenNHPActive: false,
        isQurlOAuthConfigured: true,
      });

      expect(errors).toEqual([]);
    });

    it('rejects a short legacy secret when it is the only configured state secret', () => {
      const { errors } = validateProductionOAuthStateSecrets({
        OAUTH_STATE_SECRET: 'short',
      }, {
        isOpenNHPActive: false,
        isQurlOAuthConfigured: true,
      });

      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('OAUTH_STATE_SECRET must be at least 32 chars');
    });

    it('dedupes a short legacy secret error when both flows depend on it', () => {
      const { errors } = validateProductionOAuthStateSecrets({
        OAUTH_STATE_SECRET: 'short',
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: true,
      });

      expect(errors).toHaveLength(1);
      expect(errors[0]).toContain('OAUTH_STATE_SECRET must be at least 32 chars');
    });
  });

  describe('productionOAuthStateSecretWarnings', () => {
    it('warns when a configured flow is still using the legacy state secret', () => {
      const warnings = productionOAuthStateSecretWarnings({
        legacy: 's'.repeat(64),
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: true,
      });

      expect(warnings).toEqual([
        expect.stringContaining('GitHub OAuth state is using legacy OAUTH_STATE_SECRET'),
        expect.stringContaining('qURL OAuth state is using legacy OAUTH_STATE_SECRET'),
      ]);
    });

    it('warns when dedicated secrets match each other or the legacy secret', () => {
      const shared = 's'.repeat(64);
      const warnings = productionOAuthStateSecretWarnings({
        legacy: shared,
        github: shared,
        qurl: shared,
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: true,
      });

      expect(warnings).toEqual([
        expect.stringContaining('GITHUB_OAUTH_STATE_SECRET and QURL_OAUTH_STATE_SECRET are identical'),
        expect.stringContaining('GITHUB_OAUTH_STATE_SECRET matches legacy OAUTH_STATE_SECRET'),
        expect.stringContaining('QURL_OAUTH_STATE_SECRET matches legacy OAUTH_STATE_SECRET'),
      ]);
    });

    it('warns when a short legacy secret is ignored behind a dedicated secret', () => {
      const warnings = productionOAuthStateSecretWarnings({
        legacy: 'short',
        github: 'g'.repeat(64),
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: false,
      });

      expect(warnings).toEqual([
        expect.stringContaining('OAUTH_STATE_SECRET is shorter than 32 chars and will be ignored'),
      ]);
    });
  });

  describe('enforceProductionOAuthStateSecrets', () => {
    it('logs validation errors and exits before emitting warnings', () => {
      const logger = { error: jest.fn(), warn: jest.fn() };
      const exit = jest.fn();

      const result = enforceProductionOAuthStateSecrets({
        GITHUB_OAUTH_STATE_SECRET: 'short',
        OAUTH_STATE_SECRET: 's'.repeat(64),
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: false,
      }, {
        logger,
        exit,
      });

      expect(result.ok).toBe(false);
      expect(logger.error).toHaveBeenCalledWith(expect.stringContaining('GITHUB_OAUTH_STATE_SECRET must be at least 32 chars'));
      expect(logger.warn).not.toHaveBeenCalled();
      expect(exit).toHaveBeenCalledWith(1);
    });

    it('logs production warnings without exiting after validation succeeds', () => {
      const logger = { error: jest.fn(), warn: jest.fn() };
      const exit = jest.fn();

      const result = enforceProductionOAuthStateSecrets({
        GITHUB_OAUTH_STATE_SECRET: 'g'.repeat(64),
        OAUTH_STATE_SECRET: 'short',
      }, {
        isOpenNHPActive: true,
        isQurlOAuthConfigured: false,
      }, {
        logger,
        exit,
      });

      expect(result.ok).toBe(true);
      expect(logger.error).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(expect.stringContaining('OAUTH_STATE_SECRET is shorter than 32 chars'));
      expect(exit).not.toHaveBeenCalled();
    });
  });
});
