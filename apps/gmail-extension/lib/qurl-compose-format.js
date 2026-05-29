/**
 * Shared formatter for Gmail draft insertion and clipboard copy.
 */

(function (global) {
  'use strict';

  function getI18n() {
    if (global && global.QURLI18n) {
      return global.QURLI18n;
    }
    if (typeof module !== 'undefined' && module.exports) {
      return require('./qurl-i18n.js');
    }
    return null;
  }

  function escapeHtml(str) {
    if (!str) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function normalizeAllowedLink(link) {
    if (!link) return null;

    try {
      const parsed = new URL(String(link));
      if (parsed.protocol === 'https:') {
        return parsed.toString();
      }
    } catch (e) {
      return null;
    }

    return null;
  }

  function formatExpiry(isoString) {
    if (!isoString) return null;

    const d = new Date(isoString);
    if (Number.isNaN(d.getTime())) {
      return null;
    }

    const pad = function (n) { return ('0' + n).slice(-2); };
    // Render in the sender's local time, but append the UTC offset so the absolute instant is
    // unambiguous once the string is inserted into an email a recipient may read in another zone.
    const offsetMinutes = -d.getTimezoneOffset();
    const offsetSign = offsetMinutes >= 0 ? '+' : '-';
    const offsetAbs = Math.abs(offsetMinutes);
    const offset = offsetSign + pad(Math.floor(offsetAbs / 60)) + pad(offsetAbs % 60);
    return d.getFullYear() + '-'
      + pad(d.getMonth() + 1) + '-'
      + pad(d.getDate()) + ' '
      + pad(d.getHours()) + ':'
      + pad(d.getMinutes()) + ':'
      + pad(d.getSeconds()) + ' '
      + offset;
  }

  function getMessage(key, fallback, substitutions) {
    const QURLI18n = getI18n();
    if (QURLI18n && typeof QURLI18n.getMessage === 'function') {
      return QURLI18n.getMessage(key, fallback, substitutions);
    }
    return applyFallbackSubstitutions(fallback, substitutions);
  }

  function applyFallbackSubstitutions(template, substitutions) {
    const QURLI18n = getI18n();
    if (QURLI18n && typeof QURLI18n.applyFallbackSubstitutions === 'function') {
      return QURLI18n.applyFallbackSubstitutions(template, substitutions);
    }

    let result = template || '';
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

  function buildExpirySuffix(expiry) {
    const formattedExpiry = formatExpiry(expiry);
    return formattedExpiry
      ? getMessage('expiry_suffix', ' (Expires: $1)', [formattedExpiry])
      : '';
  }

  function getUnnamedFileLabel() {
    return getMessage('unnamed_file', 'Unnamed file');
  }

  function buildLinkHtml(results) {
    if (!results || results.length === 0) return '';

    // HTML inserted into Gmail relies on this formatter to escape labels and reject unsafe URLs.
    return results.map(function (result) {
      const filename = escapeHtml(result.filename || getUnnamedFileLabel());
      const safeLink = normalizeAllowedLink(result.link);
      if (!safeLink) {
        return filename + buildExpirySuffix(result.expiry);
      }
      return '<a href="' + escapeHtml(safeLink) + '" style="color:#1a73e8;text-decoration:none;">'
        + filename
        + '</a>'
        + buildExpirySuffix(result.expiry);
    }).join('<br>');
  }

  function buildLinkPlainText(results) {
    if (!results || results.length === 0) return '';

    // Apply the same https-only validation as buildLinkHtml so the clipboard fallback
    // cannot carry a non-https (e.g. javascript:) link the HTML path would have dropped.
    return results.map(function (result) {
      const filename = result.filename || getUnnamedFileLabel();
      const safeLink = normalizeAllowedLink(result.link);
      const suffix = buildExpirySuffix(result.expiry);
      return safeLink
        ? filename + ': ' + safeLink + suffix
        : filename + suffix;
    }).join('\n');
  }

  global.QURLComposeFormatter = {
    buildLinkHtml: buildLinkHtml,
    buildLinkPlainText: buildLinkPlainText,
    escapeHtml: escapeHtml,
    formatExpiry: formatExpiry,
    normalizeAllowedLink: normalizeAllowedLink,
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = global.QURLComposeFormatter;
  }
}(globalThis));
