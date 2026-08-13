// The icon set the extension ships, kept in one place so the suites asserting on it cannot drift
// apart from each other.
//
// Spelled out here rather than derived from `require('../scripts/generate-icons.js').sizes`: an
// assertion built from the value under test goes vacuously green if that value ever becomes empty.

const EXPECTED_ICON_SIZES = [16, 48, 128];

// Sorted, because the suites compare against a sorted `fs.readdirSync(...)`.
const EXPECTED_ICON_FILES = EXPECTED_ICON_SIZES
  .map(function (size) { return `icon${size}.png`; })
  .sort();

module.exports = { EXPECTED_ICON_SIZES, EXPECTED_ICON_FILES };
