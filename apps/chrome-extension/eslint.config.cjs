const js = require('@eslint/js');
const globals = require('globals');

module.exports = [
  {
    files: ['**/*.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'script',
      globals: {
        ...globals.browser,
        ...globals.webextensions,
        ...globals.node,
        ...globals.es2022,
        "globalThis": "readonly",
        "importScripts": "readonly",
        "ClipboardItem": "readonly",
        "QURLConfig": "readonly",
        "QURLI18n": "readonly",
        "QURLComposeFormatter": "readonly",
        "uploadFile": "readonly",
        "getQurlApiBase": "readonly",
        "getStoredQurlApiBase": "readonly",
        "setStoredQurlApiBase": "readonly",
        "normalizeQurlApiBase": "readonly",
        "isDefaultQurlOrigin": "readonly",
        "ensureQurlHostPermission": "readonly",
        "requestQurlHostPermission": "readonly",
        "getQurlHostPermissionPattern": "readonly",
        "resolveDefaultQurlApiConfig": "readonly",
      }
    },
    rules: {
      ...js.configs.recommended.rules,
      // Shared browser scripts declare globals that other files consume.
      "no-redeclare": ["error", { "builtinGlobals": false }],
      "no-unused-vars": ["warn", {"argsIgnorePattern": "^_", "varsIgnorePattern": "^_"}],
      "no-console": ["error", {"allow": ["warn", "error"]}],
      "no-empty": ["error", {"allowEmptyCatch": true}],
      "no-prototype-builtins": "error",
      "no-case-declarations": "error",
      "eqeqeq": ["error", "smart"],
      "no-undef": "error",
    }
  },
  {
    files: ['test/**/*.js'],
    rules: { 'no-unused-vars': 'off', 'no-console': 'off' }
  },
  {
    files: ['scripts/**/*.js'],
    rules: { 'no-console': 'off' }
  }
];
