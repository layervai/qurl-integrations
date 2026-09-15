const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const { resolveTarget } = require('./build-release');

const projectRoot = path.resolve(__dirname, '..');
const releaseRoot = path.join(projectRoot, 'release');

function main(targetName = process.argv[2] || 'chrome') {
  const target = resolveTarget(targetName);
  const targetRoot = target.root;
  const targetReleaseRoot = target.releaseRoot;
  const targetDistRoot = path.join(targetRoot, 'dist');
  const pkg = JSON.parse(fs.readFileSync(path.join(targetRoot, 'package.json'), 'utf8'));
  rebuildRelease(targetName);
  const zipName = `${pkg.name}-v${pkg.version}.zip`;
  const zipPath = path.join(targetDistRoot, zipName);

  fs.mkdirSync(targetDistRoot, { recursive: true });
  fs.rmSync(zipPath, { force: true });

  createZipFromRelease(zipPath, targetReleaseRoot);

  console.log('Packaged ZIP created at:', zipPath);
  console.log(`Upload this ZIP to ${targetName === 'edge' ? 'Microsoft Edge Add-ons' : 'the Chrome Web Store'}.`);
}

function rebuildRelease(targetName = 'chrome') {
  execFileSync(process.execPath, [path.join(projectRoot, 'scripts', 'build-release.js'), targetName], {
    cwd: projectRoot,
    stdio: 'inherit',
  });
}

function createZipFromRelease(zipPath, targetReleaseRoot = releaseRoot) {
  if (process.platform === 'win32') {
    createZipWithPowerShell(zipPath, targetReleaseRoot);
    return;
  }

  createZipWithZipCommand(zipPath, targetReleaseRoot);
}

function createZipWithZipCommand(zipPath, targetReleaseRoot = releaseRoot) {
  const relativeOutput = path.relative(targetReleaseRoot, zipPath);

  try {
    execFileSync('zip', ['-r', relativeOutput, '.', '-x', '*.DS_Store', '*/.DS_Store'], {
      cwd: targetReleaseRoot,
      stdio: 'inherit',
    });
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new Error('The "zip" command is not available. Install zip or create the ZIP manually from release/.');
    }
    throw err;
  }
}

function createZipWithPowerShell(zipPath, targetReleaseRoot = releaseRoot) {
  const sourcePattern = path.join(targetReleaseRoot, '*');

  try {
    execFileSync('powershell.exe', [
      '-NoProfile',
      '-Command',
      `Compress-Archive -Path '${escapePowerShell(sourcePattern)}' -DestinationPath '${escapePowerShell(zipPath)}' -Force`,
    ], {
      cwd: targetReleaseRoot,
      stdio: 'inherit',
    });
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new Error('PowerShell is required to package the extension on Windows.');
    }
    throw err;
  }
}

function escapePowerShell(value) {
  // The command wraps paths in single quotes, so doubling embedded single quotes is sufficient.
  return String(value).replace(/'/g, "''");
}

if (require.main === module) {
  main();
}

module.exports = {
  createZipFromRelease,
  createZipWithPowerShell,
  createZipWithZipCommand,
  escapePowerShell,
  main,
  rebuildRelease,
};
