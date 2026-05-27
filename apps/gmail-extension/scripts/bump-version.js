const fs = require('fs');
const path = require('path');

const projectRoot = path.resolve(__dirname, '..');
const packageJsonPath = path.join(projectRoot, 'package.json');
const packageLockPath = path.join(projectRoot, 'package-lock.json');
const manifestPath = path.join(projectRoot, 'manifest.json');

function main() {
  const input = process.argv[2];
  if (!input) {
    throw new Error('Usage: node scripts/bump-version.js <patch|minor|major|x.y.z>');
  }

  const packageJson = readJson(packageJsonPath);
  const manifest = readJson(manifestPath);
  const currentVersion = packageJson.version;
  const nextVersion = resolveNextVersion(currentVersion, input);

  packageJson.version = nextVersion;
  manifest.version = nextVersion;

  writeJson(packageJsonPath, packageJson);
  writeJson(manifestPath, manifest);

  if (fs.existsSync(packageLockPath)) {
    const packageLock = readJson(packageLockPath);
    if (typeof packageLock.version === 'string') {
      packageLock.version = nextVersion;
    }
    if (packageLock.packages && packageLock.packages['']) {
      // npm lockfiles may omit this root package entry in some formats; update it only when present.
      packageLock.packages[''].version = nextVersion;
    }
    writeJson(packageLockPath, packageLock);
  }

  console.log(`Version bumped: ${currentVersion} -> ${nextVersion}`);
}

function readJson(filePath) {
  return JSON.parse(fs.readFileSync(filePath, 'utf8'));
}

function writeJson(filePath, data) {
  fs.writeFileSync(filePath, JSON.stringify(data, null, 2) + '\n');
}

function resolveNextVersion(currentVersion, input) {
  validateVersion(currentVersion);

  if (input === 'patch' || input === 'minor' || input === 'major') {
    return bumpVersion(currentVersion, input);
  }

  validateVersion(input);
  if (compareVersions(input, currentVersion) <= 0) {
    throw new Error(`Version must be greater than the current version (${currentVersion}).`);
  }
  return input;
}

function bumpVersion(version, level) {
  const parts = version.split('.').map(Number);

  if (level === 'major') {
    return `${parts[0] + 1}.0.0`;
  }
  if (level === 'minor') {
    return `${parts[0]}.${parts[1] + 1}.0`;
  }
  return `${parts[0]}.${parts[1]}.${parts[2] + 1}`;
}

function validateVersion(version) {
  if (!/^\d+\.\d+\.\d+$/.test(version)) {
    throw new Error(`Invalid version: ${version}. Expected x.y.z`);
  }
}

function compareVersions(a, b) {
  const aParts = a.split('.').map(Number);
  const bParts = b.split('.').map(Number);

  for (let i = 0; i < 3; i += 1) {
    if (aParts[i] > bParts[i]) {
      return 1;
    }
    if (aParts[i] < bParts[i]) {
      return -1;
    }
  }

  return 0;
}

if (require.main === module) {
  main();
}

module.exports = {
  bumpVersion,
  compareVersions,
  main,
  resolveNextVersion,
  validateVersion,
};
