/**
 * qURL Extension — Centralized Configuration
 *
 * SINGLE SOURCE OF TRUTH for the default qURL server base URL. Every other module
 * (lib/qurl-api.js, the popup, the release build) reads the default from here.
 *
 * Build-time configurable: scripts/build-release.js rewrites the marked DEFAULT_QURL_API_BASE
 * declaration below (see the marker comment) from the QURL_API_BASE env var (or
 * apps/gmail-extension/.env) when packaging a release — e.g. a sandbox build sets
 * QURL_API_BASE=https://getqurllink.layerv.xyz. Confining the value to this tiny, dedicated
 * file keeps the rewrite low-risk (small surface, one marked line) and removes the duplicate
 * "fallback" constant the old in-place rewrite of the 600-line API client needed.
 *
 * Runtime configurable: users may override the server per-install via the popup
 * settings (persisted under chrome.storage.local "qurlApiBase"); see lib/qurl-api.js.
 *
 * The default points at the qURL production upload connector. Keep it in lockstep
 * with the qurl-s3-connector endpoint provisioned by nhp terraform
 * (qurl_s3_connector_domain) and with manifest.json host_permissions.
 */

'use strict';

(function (global) {
  // qurl-config:DEFAULT_QURL_API_BASE — build-release.js regenerates this declaration.
  const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.ai/';

  const QURLConfig = { DEFAULT_QURL_API_BASE };

  if (global) {
    global.QURLConfig = QURLConfig;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = QURLConfig;
  }
}(typeof globalThis !== 'undefined' ? globalThis : this));
