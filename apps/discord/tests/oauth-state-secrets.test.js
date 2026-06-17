const {
  MIN_OAUTH_STATE_SECRET_LENGTH,
  normalizeSecretValue,
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
  });
});
