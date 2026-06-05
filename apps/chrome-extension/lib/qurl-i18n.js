(function (global) {
  'use strict';

  function applyFallbackSubstitutions(template, substitutions) {
    const result = template || '';
    if (!substitutions || substitutions.length === 0) {
      return result;
    }

    return result.replace(/\$(\d+)/g, function (match, rawIndex) {
      const substitutionIndex = Number(rawIndex) - 1;
      if (substitutionIndex < 0 || substitutionIndex >= substitutions.length) {
        return match;
      }
      return String(substitutions[substitutionIndex]);
    });
  }

  function getMessage(key, fallback, substitutions) {
    if (typeof chrome !== 'undefined' && chrome.i18n && typeof chrome.i18n.getMessage === 'function') {
      return chrome.i18n.getMessage(key, substitutions) || applyFallbackSubstitutions(fallback, substitutions);
    }
    return applyFallbackSubstitutions(fallback, substitutions);
  }

  global.QURLI18n = {
    applyFallbackSubstitutions: applyFallbackSubstitutions,
    getMessage: getMessage,
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = global.QURLI18n;
  }
}(globalThis));
