const fs = require('fs');
const path = require('path');

const projectRoot = path.resolve(__dirname, '..');
const releaseRoot = path.join(projectRoot, 'release');
const includePaths = [
  'manifest.json',
  'background.js',
  'content',
  'popup',
  'lib',
  'icons',
  '_locales',
];

function main() {
  const buildConfig = loadBuildConfig();
  recreateDirectory(releaseRoot);

  for (const relativePath of includePaths) {
    const source = path.join(projectRoot, relativePath);
    const target = path.join(releaseRoot, relativePath);

    if (!fs.existsSync(source)) {
      throw new Error(`Missing required release path: ${relativePath}`);
    }

    copyRecursive(source, target);
  }

  applyBuildOverrides(buildConfig);
  validateReleaseManifest();

  console.log('Release directory generated at:', releaseRoot);
  console.log('Next step: zip the contents of release/ so manifest.json is at the ZIP root.');
}

function recreateDirectory(dirPath) {
  fs.rmSync(dirPath, { recursive: true, force: true });
  fs.mkdirSync(dirPath, { recursive: true });
}

function copyRecursive(source, target) {
  const stat = fs.statSync(source);

  if (stat.isDirectory()) {
    fs.mkdirSync(target, { recursive: true });
    for (const entry of fs.readdirSync(source)) {
      copyRecursive(path.join(source, entry), path.join(target, entry));
    }
    return;
  }

  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.copyFileSync(source, target);
}

function validateReleaseManifest(targetReleaseRoot) {
  const resolvedReleaseRoot = targetReleaseRoot || releaseRoot;
  const manifestPath = path.join(resolvedReleaseRoot, 'manifest.json');
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));

  if (manifest.manifest_version !== 3) {
    throw new Error('Release manifest must use Manifest V3.');
  }

  if (!manifest.action || !manifest.action.default_popup) {
    throw new Error('Release manifest is missing action.default_popup.');
  }

  const localeMessagesPath = path.join(resolvedReleaseRoot, '_locales', 'en', 'messages.json');
  if (!fs.existsSync(localeMessagesPath)) {
    throw new Error('Release bundle is missing _locales/en/messages.json.');
  }

  const requiredManifestPaths = collectManifestAssetPaths(manifest);
  for (const relativePath of requiredManifestPaths) {
    const resolvedPath = path.join(resolvedReleaseRoot, relativePath);
    if (!fs.existsSync(resolvedPath)) {
      throw new Error(`Release bundle is missing manifest asset: ${relativePath}`);
    }
  }

  // The bundled default API base and manifest host permission must always move together.
  validateDefaultQurlHostPermission(manifest, resolvedReleaseRoot);
}

function loadBuildConfig() {
  const dotEnvValues = parseDotEnv(path.join(projectRoot, '.env'));
  return {
    qurlApiBase: process.env.QURL_API_BASE || dotEnvValues.QURL_API_BASE || null,
  };
}

function parseDotEnv(dotEnvPath) {
  if (!fs.existsSync(dotEnvPath)) {
    return {};
  }

  const result = {};
  const lines = fs.readFileSync(dotEnvPath, 'utf8').split(/\r?\n/);
  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) {
      continue;
    }

    const separatorIndex = trimmed.indexOf('=');
    if (separatorIndex === -1) {
      continue;
    }

    const key = trimmed.slice(0, separatorIndex).trim();
    const value = trimmed.slice(separatorIndex + 1).trim();
    result[key] = stripWrappingQuotes(value);
  }

  return result;
}

function stripWrappingQuotes(value) {
  const trimmed = String(value).trim();
  if ((trimmed.startsWith('"') && trimmed.endsWith('"')) || (trimmed.startsWith("'") && trimmed.endsWith("'"))) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}

function applyBuildOverrides(buildConfig, targetReleaseRoot) {
  if (!buildConfig.qurlApiBase) {
    return;
  }

  const normalizedBase = normalizeBuildQurlApiBase(buildConfig.qurlApiBase);
  const resolvedReleaseRoot = targetReleaseRoot || releaseRoot;
  writeDefaultApiBaseConfig(normalizedBase, resolvedReleaseRoot);
  rewriteManifestHostPermission(normalizedBase, resolvedReleaseRoot);
  console.log('Applied QURL_API_BASE override for release build:', normalizedBase);
}

// Keep this normalization logic in lockstep with lib/qurl-api.js:normalizeQurlApiBase.
// The build runs in Node without the extension runtime, so the helper is duplicated here.
function normalizeBuildQurlApiBase(value) {
  const parsed = new URL(String(value).trim());

  if (parsed.protocol !== 'https:') {
    throw new Error('QURL_API_BASE must start with https://');
  }

  const pathname = parsed.pathname.replace(/\/+$/, '').replace(/\/api\/upload$/i, '');
  parsed.pathname = pathname || '/';
  parsed.search = '';
  parsed.hash = '';

  return parsed.toString().replace(/\/$/, '');
}

// Chrome match patterns reject a port in the host, so derive the host permission from the
// hostname only. Mirrors lib/qurl-api.js:getQurlHostPermissionPattern — kept as a small local
// copy rather than imported, because requiring qurl-api.js would run its import-time
// resolveDefaultQurlApiConfig() and pull in qurl-i18n.js, coupling the build to the extension
// runtime. Keep the two in lockstep.
function hostPermissionPattern(base) {
  const parsed = new URL(base);
  return `${parsed.protocol}//${parsed.hostname}/*`;
}

function qurlConfigPath(targetReleaseRoot) {
  return path.join(targetReleaseRoot || releaseRoot, 'lib', 'qurl-config.js');
}

// Point the single source of truth (lib/qurl-config.js) at the override by rewriting its one
// marked DEFAULT_QURL_API_BASE declaration. Still an in-place regex rewrite, but confined to a
// tiny purpose-built module (one marked line) instead of the 600-line API client, and with no
// second "fallback" constant to keep in sync. The marker comment in qurl-config.js must stay.
function writeDefaultApiBaseConfig(normalizedBase, targetReleaseRoot) {
  const configPath = qurlConfigPath(targetReleaseRoot);
  const source = fs.readFileSync(configPath, 'utf8');
  // Anchor to a real declaration LINE (multiline ^…$, capturing leading indent) so we never
  // match a `const DEFAULT_QURL_API_BASE = '...'` that appears inside a comment or string.
  const declarationPattern = /^([ \t]*)const DEFAULT_QURL_API_BASE\s*=\s*['"][^'"]+['"]\s*;[ \t]*$/m;

  // Fail loudly if the marked declaration is gone (someone reshaped qurl-config.js); otherwise
  // the rewrite is unconditional — rewriting to the same value just writes identical content.
  if (!declarationPattern.test(source)) {
    throw new Error('Could not rewrite DEFAULT_QURL_API_BASE in release/lib/qurl-config.js.');
  }

  const declaration = `const DEFAULT_QURL_API_BASE = ${JSON.stringify(normalizedBase + '/')};`;
  fs.writeFileSync(configPath, source.replace(declarationPattern, function (_match, indent) {
    return indent + declaration;
  }));
}

function rewriteManifestHostPermission(normalizedBase, targetReleaseRoot) {
  const resolvedReleaseRoot = targetReleaseRoot || releaseRoot;
  const manifestPath = path.join(resolvedReleaseRoot, 'manifest.json');
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));

  // Derive the expected pattern from the bundled default in the PROJECT source (not the
  // release copy, which may already have been regenerated by writeDefaultApiBaseConfig).
  // Matching the exact current pattern avoids touching mail.google.com or any future
  // host_permission entry.
  const bundledDefaultApiBase = normalizeBuildQurlApiBase(readDefaultQurlApiBase(qurlConfigPath(projectRoot)));
  const expectedCurrentPattern = hostPermissionPattern(bundledDefaultApiBase);
  const overridePattern = hostPermissionPattern(normalizedBase);

  const hostPermissions = manifest.host_permissions || [];
  const matchIndex = hostPermissions.indexOf(expectedCurrentPattern);

  if (matchIndex === -1) {
    throw new Error(
      `Could not locate the bundled qURL host permission in release/manifest.json. ` +
      `Expected "${expectedCurrentPattern}" but found: [${hostPermissions.join(', ')}]`
    );
  }

  manifest.host_permissions = hostPermissions.map(function (permission) {
    return permission === expectedCurrentPattern ? overridePattern : permission;
  });

  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + '\n');
}

function validateDefaultQurlHostPermission(manifest, targetReleaseRoot) {
  // Derive the expected pattern from the bundled default rather than position-guessing the
  // manifest entry, so adding a third host permission later cannot break this check.
  const bundledDefaultApiBase = normalizeBuildQurlApiBase(readDefaultQurlApiBase(qurlConfigPath(targetReleaseRoot)));
  const expectedHostPermission = hostPermissionPattern(bundledDefaultApiBase);
  const hostPermissions = manifest.host_permissions || [];

  if (!hostPermissions.includes(expectedHostPermission)) {
    throw new Error(
      `Release manifest host permission mismatch: expected ${expectedHostPermission} but found [${hostPermissions.join(', ')}].`
    );
  }
}

// Reads the default base URL from the centralized config module. Requiring (rather than
// regex-scraping) is robust to formatting and survives full-file regeneration.
function readDefaultQurlApiBase(configPath) {
  const resolved = require.resolve(path.resolve(configPath));
  delete require.cache[resolved];
  const config = require(resolved);

  if (!config || typeof config.DEFAULT_QURL_API_BASE !== 'string' || !config.DEFAULT_QURL_API_BASE) {
    throw new Error('Could not read DEFAULT_QURL_API_BASE from lib/qurl-config.js.');
  }

  return config.DEFAULT_QURL_API_BASE;
}

function collectManifestAssetPaths(manifest) {
  const paths = new Set();

  if (manifest.background && manifest.background.service_worker) {
    paths.add(manifest.background.service_worker);
  }

  if (manifest.action && manifest.action.default_popup) {
    paths.add(manifest.action.default_popup);
  }

  if (manifest.content_scripts) {
    manifest.content_scripts.forEach(function (entry) {
      (entry.js || []).forEach(function (scriptPath) {
        paths.add(scriptPath);
      });
    });
  }

  if (manifest.action && manifest.action.default_icon) {
    Object.values(manifest.action.default_icon).forEach(function (iconPath) {
      paths.add(iconPath);
    });
  }

  if (manifest.icons) {
    Object.values(manifest.icons).forEach(function (iconPath) {
      paths.add(iconPath);
    });
  }

  return Array.from(paths);
}

if (require.main === module) {
  main();
}

module.exports = {
  applyBuildOverrides,
  collectManifestAssetPaths,
  hostPermissionPattern,
  loadBuildConfig,
  normalizeBuildQurlApiBase,
  parseDotEnv,
  qurlConfigPath,
  readDefaultQurlApiBase,
  stripWrappingQuotes,
  writeDefaultApiBaseConfig,
  rewriteManifestHostPermission,
  validateDefaultQurlHostPermission,
  validateReleaseManifest,
};
