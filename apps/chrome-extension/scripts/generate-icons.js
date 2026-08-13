/**
 * Generates PNG icons from the shared logo source using sharp.
 * Run: node scripts/generate-icons.js
 */
const path = require('path');
const fs = require('fs');

const sizes = [16, 48, 128];
let sharp;

try {
  sharp = require('sharp');
} catch (error) {
  console.error('Missing dependency: "sharp". Run "npm install" in the project root before generating icons.');
  process.exit(1);
}

async function generateIcons() {
  const sourcePath = path.join(__dirname, '..', 'icons', 'logo.png');

  if (!fs.existsSync(sourcePath)) {
    throw new Error(`Missing icon source: ${sourcePath}`);
  }

  for (const size of sizes) {
    const pngPath = path.join(__dirname, '..', 'icons', `icon${size}.png`);

    await sharp(sourcePath)
      // The logo source is not exactly square (420x418), and sharp's default `fit: 'cover'`
      // would crop the overhanging edge to fill the square. `contain` keeps the whole mark and
      // pads with transparency instead, so the padding stays invisible on any logo background.
      .resize(size, size, { fit: 'contain', background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .png()
      .toFile(pngPath);

    console.log(`Generated: icons/icon${size}.png`);
  }
}

generateIcons().catch(console.error);
