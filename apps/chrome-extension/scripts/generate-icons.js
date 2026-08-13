/**
 * Generates PNG icons from the shared logo source using sharp.
 * Run: node scripts/generate-icons.js
 *
 * The generated icons are committed, so they can drift from this script silently — a sharp upgrade
 * can change the PNG encoder and leave the committed bytes stale, which is what happened in #908.
 * `test/generate-icons.test.js` byte-compares them against a fresh run; #1046 tracks the tradeoff
 * that check accepts.
 */
const path = require('path');
const fs = require('fs');

const sizes = [16, 48, 128];

const projectRoot = path.resolve(__dirname, '..');
const defaultIconsDir = path.join(projectRoot, 'icons');
const defaultSourcePath = path.join(defaultIconsDir, 'logo.png');

function loadSharp() {
  try {
    return require('sharp');
  } catch (error) {
    // Keep the original failure as `cause`. Every require failure arrives here, but they do not
    // share a fix: a missing package needs `npm install`, an installed-but-unloadable native
    // binding needs `npm install --include=optional`. The cause is what tells them apart.
    throw new Error(
      'Missing dependency: "sharp". Run "npm install" in the project root before generating icons ' +
      '(if sharp is installed but its native binding failed to load, try "npm install --include=optional").',
      { cause: error }
    );
  }
}

/**
 * Renders every icon size into `outDir` and resolves with the written paths.
 *
 * `outDir` is required rather than defaulting to `icons/`: this module is exported so tests can
 * render somewhere disposable, and a caller that forgot the argument would silently overwrite the
 * committed icons.
 */
async function generateIcons(options) {
  const { outDir } = options || {};

  if (!outDir) {
    throw new Error('generateIcons requires an explicit outDir.');
  }

  // Checked before `loadSharp()`: it is free, and loading a native module is not. It also keeps
  // this reportable in an environment where sharp itself cannot load.
  if (!fs.existsSync(defaultSourcePath)) {
    throw new Error(`Missing icon source: ${defaultSourcePath}`);
  }

  const sharp = loadSharp();

  fs.mkdirSync(outDir, { recursive: true });

  const written = [];
  for (const size of sizes) {
    const pngPath = path.join(outDir, `icon${size}.png`);

    await sharp(defaultSourcePath)
      // The logo source is not exactly square (420x418), and sharp's default `fit: 'cover'`
      // would crop the overhanging edge to fill the square. `contain` keeps the whole mark and
      // pads with transparency instead, so the padding stays invisible on any logo background.
      .resize(size, size, { fit: 'contain', background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .png()
      .toFile(pngPath);

    written.push(pngPath);
  }

  return written;
}

async function main() {
  for (const pngPath of await generateIcons({ outDir: defaultIconsDir })) {
    console.log(`Generated: ${path.relative(projectRoot, pngPath)}`);
  }
}

if (require.main === module) {
  // `main` is async, so unlike the sibling scripts a bare call would surface failures as an
  // unhandled rejection. `console.error(error)` already prints the stack and the `[cause]` chain;
  // `process.exitCode` lets Node drain stderr rather than truncating it as `process.exit()` can.
  main().catch(function (error) {
    console.error(error);
    process.exitCode = 1;
  });
}

module.exports = {
  defaultIconsDir,
  generateIcons,
  main,
  projectRoot,
  sizes,
};
