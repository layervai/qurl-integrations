# QURL Gmail Chrome Extension

Upload local files to QURL directly from Gmail's compose window. After upload, secure access links (with expiry times) are automatically inserted into your email draft.

---

## Features

- **Local file upload** — select files from your computer, no need to attach to Gmail first
- **Batch upload** — upload multiple files at once
- **Auto-insert links** — QURL access links + expiry times are appended to the active Gmail draft
- **Manual copy fallback** — if Gmail draft insertion fails, copy the generated content from the popup
- **Native Gmail integration** — links are inserted directly into the compose body as HTML
- **Clean popup UI** — simple, focused interface with upload progress and per-file status

---

## Prerequisites

- Google Chrome (or Chromium-based browser)
- A running QURL upload server (the same server used by the Gmail Apps Script add-on)

---

## Configuration

### Step 1 — Configure the QURL API URL

Open the extension popup and set the **QURL server** field.

- If you save a custom URL, uploads use that URL
- If you leave it empty, uploads fall back to the built-in default: `https://getqurllink.layerv.xyz/`
- You can paste either the server base URL or a full `/api/upload` URL; the client normalizes it automatically
- If you want packaged `release/` or `dist/` builds to default to a different server, set `QURL_API_BASE` in `.env` before running the build scripts

### Step 2 — Load the Extension in Chrome

Follow [docs/installation.md](docs/installation.md) for the unpacked install steps. For a full verification flow with screenshots, see [docs/local-unpacked-testing.md](docs/local-unpacked-testing.md).

If you plan to publish to the Chrome Web Store, see [docs/chrome-web-store-review.md](docs/chrome-web-store-review.md) for the reviewer note covering the custom-server permission model.

---

## Usage

1. Open **Gmail** (`mail.google.com`)
2. Click **Compose** to open a new email draft
3. Click the **QURL File Upload** extension icon in the Chrome toolbar
4. Click **Browse files** and select one or more files
5. Click **Upload to QURL**
6. Wait for the upload to complete — the QURL links will be automatically inserted at the bottom of your email draft
7. Continue composing your email and send as normal

---

## How It Works

```
User clicks extension icon
         │
         ▼
popup.html — user selects local files
         │
         ▼
popup.js — calls uploadFile() from lib/qurl-api.js
           → POST multipart/form-data to {QURL_API_BASE}/api/upload
           → Parse JSON response
         │
         ▼
popup.js — sends results to background.js via chrome.runtime.sendMessage
         │
         ▼
background.js — ensures Gmail tab + content script availability
         │
         ▼
gmail-compose.js — finds the Gmail compose body (.Am.Al.editable)
                   → insertHTML / Selection API / insertAdjacentHTML
         │
         ▼
Email draft now contains QURL access links near the end of the message.
```

---

## Draft Insert Format

Each uploaded file generates an entry like:

```
QURL File Access Links

- report.pdf: https://xxx.layerv.ai/q/abc123 (expires: 2026-05-01T12:00:00Z)
- screenshot.png: https://xxx.layerv.ai/q/def456 (expires: 2026-05-01T12:00:00Z)
```

Inserted as styled HTML so it looks clean in both HTML and plain-text email clients.

---

## Regenerating Icons

If you edit the SVG source files in `icons/`, regenerate PNG icons:

```bash
npm install
npm run icons
```

---

## Packaging for Release

The project includes scripts for bumping the extension version, building a clean release directory, and creating a Chrome Web Store upload ZIP.

### Recommended One-Command Packaging

For most release and local validation workflows, use:

```bash
./scripts/package-all.sh 1.0.0
```

You can also replace `1.0.0` with `patch`, `minor`, or `major`.

This script:

1. Updates the version when needed
2. Rebuilds `release/`
3. Creates a fresh upload ZIP in `dist/`
4. Applies `QURL_API_BASE` from `.env` or the shell environment if set

### Step 1 — Bump the Version

Before publishing an update, increase the version number used by both `manifest.json` and `package.json`.

Use one of these commands:

```bash
npm run version:patch
npm run version:minor
npm run version:major
```

What they do:

- Update `package.json`
- Update `manifest.json`
- Update the root version entry in `package-lock.json`

You can also set an explicit version manually:

```bash
node scripts/bump-version.js 1.2.3
```

### Step 2 — Build the Release Directory

Generate a clean `release/` directory containing only the files needed for publishing:

```bash
npm run release
```

The generated `release/` directory includes:

- `manifest.json`
- `background.js`
- `content/`
- `popup/`
- `lib/`
- `icons/`
- `_locales/`

### Step 3 — Create the Upload ZIP

Build a Chrome Web Store upload ZIP:

```bash
npm run package:release
```

This script automatically rebuilds `release/` first, then creates a ZIP in `dist/`.

On Windows, `scripts/package-release.js` uses PowerShell's `Compress-Archive`; on macOS/Linux it uses `zip`.

Example output:

```text
dist/qurl-gmail-chrome-extension-v1.0.0.zip
```

### One-Command Publish Packaging

If you want to bump the version and package the ZIP in one step, use:

```bash
npm run publish:patch
npm run publish:minor
npm run publish:major
```

These commands:

1. Increase the version
2. Rebuild `release/`
3. Generate a fresh upload ZIP in `dist/`

If packaging fails after the version bump step, `package.json`, `package-lock.json`, and `manifest.json` may already contain the new version even though no ZIP was produced.

### Important Notes

- Upload the ZIP file from `dist/` to the Chrome Web Store
- Do not upload the whole repository directly
- The ZIP is built so that `manifest.json` is at the ZIP root, which is required by the Chrome Web Store

---

## API Reference

### Endpoint

**POST** `{QURL_API_BASE}/api/upload`

### Request

```
Content-Type: multipart/form-data; boundary=----QurlBoundary<random>

Body (multipart):
  name="file"; filename="<filename>"
  Content-Type: <contentType>
  [file bytes]
```

### Expected Response

```json
{
  "success": true,
  "data": {
    "resource_id": "rkrdrn7o79c",
    "qurl_link": "https://xxx.layerv.ai/q/abc123",
    "qurl_site": "https://get.qurl.link",
    "expires_at": "2026-05-01T12:00:00Z"
  }
}
```

The client also accepts a flat response (no `data` wrapper).

---

## File Structure

```
qurl-gmail-chrome-extension/
├── manifest.json              # MV3 extension manifest
├── background.js             # Service worker
├── popup/
│   ├── popup.html            # Popup UI
│   ├── popup.css             # Popup styles
│   └── popup.js              # Popup logic + file handling
├── content/
│   └── gmail-compose.js      # Gmail content script (DOM manipulation)
├── lib/
│   ├── qurl-api.js           # QURL upload API client
│   └── qurl-compose-format.js # Shared draft/clipboard formatter
├── icons/
│   ├── icon16.svg / .png
│   ├── icon48.svg / .png
│   └── icon128.svg / .png
├── _locales/
│   └── en/
│       └── messages.json     # i18n strings
├── docs/
│   ├── installation.md       # Unpacked install + build-time override
│   ├── DESIGN.md             # Architecture and runtime behavior
│   └── local-unpacked-testing.md
├── scripts/
│   ├── build-release.js      # Build clean release directory
│   ├── bump-version.js       # Bump version for manifest/package files
│   ├── generate-icons.js     # SVG → PNG generator
│   ├── package-all.sh        # Rebuild release/ and package dist/ in one step
│   └── package-release.js    # Build Chrome Web Store upload ZIP
├── dist/                     # Generated upload ZIP files (gitignored)
├── release/                  # Generated clean release directory (gitignored)
├── .env.example              # Build-time QURL_API_BASE template
├── package.json
└── package-lock.json
```

---

## Local Testing

For local unpacked testing in Chrome, including screenshot-based validation of upload, draft insertion, copy fallback, and link verification, use [docs/local-unpacked-testing.md](docs/local-unpacked-testing.md).

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| "Could not find Gmail compose window" | No compose window open | Open a new compose window in Gmail first |
| All uploads fail with "Failed to fetch" | Wrong API URL or server unreachable | Verify the configured QURL server URL in the popup |
| Links inserted but not visible in sent email | Recipient uses plain-text client | HTML is always inserted; some clients strip formatting |
| Extension icon not showing | Not loaded in Chrome | Go to `chrome://extensions` and enable the extension |

## Permission Note

`optional_host_permissions` remains `https://*/*` because the popup accepts user-configured QURL servers and requests only the single saved origin at runtime. The broad declaration exists to enable a per-origin prompt when the user saves a custom server.

---

## License

MIT License.
