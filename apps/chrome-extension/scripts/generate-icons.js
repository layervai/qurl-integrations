/**
 * Generates PNG icons from the shared logo source using sharp.
 * Run: node scripts/generate-icons.js
 *
 * The generated icons are committed, so they can silently drift from this script:
 * a sharp upgrade can change the PNG encoder and leave the committed bytes stale
 * (that is exactly what happened in #908, where merging main moved sharp
 * ^0.34.5 -> ^0.35.0). `test/generate-icons.test.js` guards against that by
 * regenerating into a temp directory and byte-comparing against `icons/`.
 * (Issue #1046 tracks the tradeoff that comparison makes: it is byte-exact, so
 * any upstream sharp encoder change turns CI red until the icons are regenerated.)
 */
const path = require('path');
const fs = require('fs');

const sizes = [16, 48, 128];

const extensionRoot = path.join(__dirname, '..');
const defaultIconsDir = path.join(extensionRoot, 'icons');
const defaultSourcePath = path.join(defaultIconsDir, 'logo.png');

function loadSharp() {
  try {
    return require('sharp');
  } catch (error) {
    // Keep the original failure as `cause`. Every require failure surfaces through this one
    // message, but they do not share a fix: a missing package needs `npm install`, while an
    // installed-but-unloadable native binding needs `npm install --include=optional`. The
    // cause is the only thing that tells them apart, and this message now also reaches
    // whoever is reading a red `npm test`.
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
 * committed icons. `main()` is the only caller that passes the real directory.
 *
 * `onGenerated` is called after each file lands, so the CLI can report progress — and so a run
 * that dies partway still says which icons it had already overwritten.
 */
async function generateIcons(options) {
  const { sourcePath = defaultSourcePath, outDir, onGenerated } = options || {};

  if (!outDir) {
    throw new Error('generateIcons requires an explicit outDir.');
  }

  const sharp = loadSharp();

  if (!fs.existsSync(sourcePath)) {
    throw new Error(`Missing icon source: ${sourcePath}`);
  }

  fs.mkdirSync(outDir, { recursive: true });

  const written = [];
  for (const size of sizes) {
    const pngPath = path.join(outDir, `icon${size}.png`);

    await sharp(sourcePath)
      // The logo source is not exactly square (420x418), and sharp's default `fit: 'cover'`
      // would crop the overhanging edge to fill the square. `contain` keeps the whole mark and
      // pads with transparency instead, so the padding stays invisible on any logo background.
      .resize(size, size, { fit: 'contain', background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .png()
      .toFile(pngPath);

    written.push(pngPath);

    if (onGenerated) {
      onGenerated(pngPath);
    }
  }

  return written;
}

async function main() {
  await generateIcons({
    outDir: defaultIconsDir,
    onGenerated: function (pngPath) {
      console.log(`Generated: ${path.relative(extensionRoot, pngPath)}`);
    },
  });
}

if (require.main === module) {
  main().catch(function (error) {
    // Prefer the stack; a non-Error rejection has no `.message` and would print as undefined.
    console.error(error && error.stack ? error.stack : error);

    if (error && error.cause) {
      console.error(`Caused by: ${error.cause.stack || error.cause}`);
    }

    // Not `process.exit()`: stdio to a pipe is async on macOS, so exiting here can discard the
    // diagnostic above under `npm run icons | tee build.log`. Setting the code lets Node drain
    // and exit 1 on its own.
    process.exitCode = 1;
  });
}

module.exports = {
  defaultIconsDir,
  defaultSourcePath,
  generateIcons,
  main,
  sizes,
};
