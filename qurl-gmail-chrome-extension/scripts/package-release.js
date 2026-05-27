const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const projectRoot = path.resolve(__dirname, '..');
const releaseRoot = path.join(projectRoot, 'release');
const distRoot = path.join(projectRoot, 'dist');
const packageJsonPath = path.join(projectRoot, 'package.json');

function main() {
  rebuildRelease();
  const pkg = JSON.parse(fs.readFileSync(packageJsonPath, 'utf8'));
  const zipName = `${pkg.name}-v${pkg.version}.zip`;
  const zipPath = path.join(distRoot, zipName);

  fs.mkdirSync(distRoot, { recursive: true });
  fs.rmSync(zipPath, { force: true });

  createZipFromRelease(zipPath);

  console.log('Packaged ZIP created at:', zipPath);
  console.log('Upload this ZIP to the Chrome Web Store.');
}

function rebuildRelease() {
  execFileSync(process.execPath, [path.join(projectRoot, 'scripts', 'build-release.js')], {
    cwd: projectRoot,
    stdio: 'inherit',
  });
}

function createZipFromRelease(zipPath) {
  if (process.platform === 'win32') {
    createZipWithPowerShell(zipPath);
    return;
  }

  createZipWithZipCommand(zipPath);
}

function createZipWithZipCommand(zipPath) {
  const relativeOutput = path.relative(releaseRoot, zipPath);

  try {
    execFileSync('zip', ['-r', relativeOutput, '.', '-x', '*.DS_Store', '*/.DS_Store'], {
      cwd: releaseRoot,
      stdio: 'inherit',
    });
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new Error('The "zip" command is not available. Install zip or create the ZIP manually from release/.');
    }
    throw err;
  }
}

function createZipWithPowerShell(zipPath) {
  const sourcePattern = path.join(releaseRoot, '*');

  try {
    execFileSync('powershell.exe', [
      '-NoProfile',
      '-Command',
      `Compress-Archive -Path '${escapePowerShell(sourcePattern)}' -DestinationPath '${escapePowerShell(zipPath)}' -Force`,
    ], {
      cwd: releaseRoot,
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
