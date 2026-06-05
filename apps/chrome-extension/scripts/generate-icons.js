/**
 * Generates PNG icons from SVG sources using sharp.
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
  for (const size of sizes) {
    const svgPath = path.join(__dirname, '..', 'icons', `icon${size}.svg`);
    const pngPath = path.join(__dirname, '..', 'icons', `icon${size}.png`);

    if (!fs.existsSync(svgPath)) {
      throw new Error(`Missing SVG source: ${svgPath}`);
    }

    await sharp(svgPath)
      .resize(size, size)
      .png()
      .toFile(pngPath);

    console.log(`Generated: icons/icon${size}.png`);
  }
}

generateIcons().catch(console.error);
