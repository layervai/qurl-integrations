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
  rewriteDefaultApiBase(normalizedBase, resolvedReleaseRoot);
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

function rewriteDefaultApiBase(normalizedBase, targetReleaseRoot) {
  const apiClientPath = path.join(targetReleaseRoot || releaseRoot, 'lib', 'qurl-api.js');
  const source = fs.readFileSync(apiClientPath, 'utf8');
  const existingBase = normalizeBuildQurlApiBase(readDefaultQurlApiBase(apiClientPath));
  if (existingBase === normalizedBase) {
    return;
  }
  const replacement = `const DEFAULT_QURL_API_BASE = ${JSON.stringify(normalizedBase + '/')};`;
  const updated = source.replace(
    /^const DEFAULT_QURL_API_BASE\s*=\s*['"][^'"]+['"]\s*;\s*$/m,
    function () {
      return replacement;
    }
  );

  if (updated === source) {
    throw new Error('Could not rewrite DEFAULT_QURL_API_BASE in release/lib/qurl-api.js.');
  }

  fs.writeFileSync(apiClientPath, updated);
}

function rewriteManifestHostPermission(normalizedBase, targetReleaseRoot) {
  const resolvedReleaseRoot = targetReleaseRoot || releaseRoot;
  const manifestPath = path.join(resolvedReleaseRoot, 'manifest.json');
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));

  // Derive the expected pattern from the bundled DEFAULT_QURL_API_BASE in the PROJECT
  // source (not the release copy, which may have already been rewritten by rewriteDefaultApiBase).
  // This ensures we match against the original bundled default to avoid accidentally replacing
  // the wrong host_permissions entry if a third permission (e.g., telemetry endpoint) is added later.
  const projectApiClientPath = path.join(projectRoot, 'lib', 'qurl-api.js');
  const bundledDefaultApiBase = normalizeBuildQurlApiBase(readDefaultQurlApiBase(projectApiClientPath));
  const expectedCurrentPattern = `${new URL(bundledDefaultApiBase).origin}/*`;
  const overridePattern = `${new URL(normalizedBase).origin}/*`;

  const hostPermissions = manifest.host_permissions || [];
  const matchIndex = hostPermissions.indexOf(expectedCurrentPattern);

  if (matchIndex === -1) {
    throw new Error(
      `Could not locate the bundled QURL host permission in release/manifest.json. ` +
      `Expected "${expectedCurrentPattern}" but found: [${hostPermissions.join(', ')}]`
    );
  }

  manifest.host_permissions = hostPermissions.map(function (permission) {
    return permission === expectedCurrentPattern ? overridePattern : permission;
  });

  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + '\n');
}

function validateDefaultQurlHostPermission(manifest, targetReleaseRoot) {
  const bundledHostPermission = (manifest.host_permissions || []).find(function (permission) {
    return permission !== 'https://mail.google.com/*';
  });
  const apiClientPath = path.join(targetReleaseRoot, 'lib', 'qurl-api.js');
  const bundledDefaultApiBase = normalizeBuildQurlApiBase(readDefaultQurlApiBase(apiClientPath));
  validateFallbackQurlApiBase(apiClientPath);
  const expectedHostPermission = `${new URL(bundledDefaultApiBase).origin}/*`;

  if (bundledHostPermission !== expectedHostPermission) {
    throw new Error(
      `Release manifest host permission mismatch: expected ${expectedHostPermission} but found ${bundledHostPermission || '(missing)'}.`
    );
  }
}

function validateFallbackQurlApiBase(apiClientPath) {
  try {
    normalizeBuildQurlApiBase(readFallbackQurlApiBase(apiClientPath));
  } catch (err) {
    throw new Error(`Bundled fallback QURL API base is invalid: ${err.message}`);
  }
}

function readDefaultQurlApiBase(apiClientPath) {
  const source = fs.readFileSync(apiClientPath, 'utf8');
  const match = source.match(/^const DEFAULT_QURL_API_BASE\s*=\s*['"]([^'"]+)['"]\s*;\s*$/m);

  if (!match) {
    throw new Error('Could not read DEFAULT_QURL_API_BASE from lib/qurl-api.js.');
  }

  return match[1];
}

function readFallbackQurlApiBase(apiClientPath) {
  const source = fs.readFileSync(apiClientPath, 'utf8');
  const match = source.match(/^const DEFAULT_QURL_API_BASE_FALLBACK\s*=\s*['"]([^'"]+)['"]\s*;\s*$/m);

  if (!match) {
    throw new Error('Could not read DEFAULT_QURL_API_BASE_FALLBACK from lib/qurl-api.js.');
  }

  return match[1];
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
  loadBuildConfig,
  normalizeBuildQurlApiBase,
  parseDotEnv,
  readDefaultQurlApiBase,
  readFallbackQurlApiBase,
  stripWrappingQuotes,
  rewriteDefaultApiBase,
  rewriteManifestHostPermission,
  validateDefaultQurlHostPermission,
  validateReleaseManifest,
};
