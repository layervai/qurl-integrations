# QURL Gmail Chrome Extension — Design Document

## Overview

The QURL Gmail Chrome Extension is a Chrome Manifest V3 (MV3) extension that lets users upload local files directly to a QURL upload server and automatically insert secure, expiring access links into an active Gmail compose draft.

The core goal: replace Gmail's built-in attachment flow (which uploads to Google's servers) with an upload to a self-controlled QURL server, inserting only a short link into the email — keeping emails small, bypass attachment limits, and enabling time-limited or permission-controlled file access.

---

## Architecture

The extension consists of four cooperating runtime pieces:

```
┌─────────────────────────────────────────────────────────────┐
│                    Chrome Extension (MV3)                    │
│                                                              │
│  ┌──────────────┐     chrome.runtime.sendMessage   ┌──────────────┐│
│  │  popup.js    │ ───────────────────────────────▶ │ background.js ││
│  │  (UI/Logic)  │                                   │ (MV3 worker) ││
│  └──────┬───────┘                                   └──────┬───────┘│
│         │ uploadFile()                                          │    │
│         ▼                                                       │    │
│  ┌─────────────────────┐                                        │    │
│  │   lib/qurl-api.js   │                                        │    │
│  │  + host permission  │                                        │    │
│  └──────────┬──────────┘                                        │    │
│             │ POST /api/upload                                  │    │
│             ▼                                                   │    │
│         QURL Server                              chrome.tabs.sendMessage /
│                                                  chrome.scripting.executeScript
│                                                                   │
│                                                                   ▼
│                                                     ┌────────────────────────┐
│                                                     │ content/gmail-compose.js│
│                                                     │ + lib/qurl-compose-    │
│                                                     │   format.js            │
│                                                     └────────────────────────┘
└─────────────────────────────────────────────────────────────┘
```

### Why this design?

1. **Popup owns user interaction and upload orchestration.** File selection, settings, progress UI, and copy fallback all live in `popup.js`.
2. **Background mediates Gmail tab access.** `background.js` validates that the active tab is Gmail, pings the content script, and reinjects scripts if Gmail lost them after navigation or refresh.
3. **Upload logic is isolated.** `lib/qurl-api.js` owns URL normalization, permission checks, multipart upload generation, and response parsing.
4. **Formatting is shared.** `lib/qurl-compose-format.js` keeps Gmail draft insertion and clipboard copy output consistent.

---

## File Structure

```
qurl-gmail-chrome-extension/
├── manifest.json              # MV3 extension manifest
├── background.js              # Service worker (relay + content script bootstrap)
├── popup/
│   ├── popup.html            # Popup DOM
│   ├── popup.css             # Styles (CSS custom properties)
│   └── popup.js              # UI logic, upload orchestration, copy fallback
├── content/
│   └── gmail-compose.js      # Gmail DOM manipulation content script
├── lib/
│   ├── qurl-api.js           # QURL upload API client
│   └── qurl-compose-format.js # Shared formatter for HTML/plain-text link output
├── icons/                    # SVG source + generated PNG icons
├── _locales/
│   └── en/
│       └── messages.json     # i18n message strings
├── docs/
│   ├── DESIGN.md
│   ├── installation.md
│   └── local-unpacked-testing.md
└── scripts/
    ├── build-release.js      # Rebuild release/ directory
    ├── bump-version.js       # Sync version across package files
    ├── generate-icons.js     # SVG → PNG icon generator
    ├── package-all.sh        # One-command rebuild + packaging flow
    └── package-release.js    # ZIP packaging for Chrome Web Store
```

---

## Components

### manifest.json

Declares the extension using Chrome Manifest V3. Key declarations:

- **`default_locale: "en"`** — English as the default and only locale.
- **`action.default_popup`** — Popup entry point.
- **`content_scripts`** — Preloads `lib/qurl-compose-format.js` and `gmail-compose.js` into `https://mail.google.com/*` pages at `document_start`.
- **`host_permissions`** — Always grants Gmail and the built-in default QURL server origin.
- **`optional_host_permissions`** — Declared as `https://*/*` so Chrome can show a per-origin runtime prompt for any user-entered HTTPS QURL server. The extension never auto-grants broad host access: `ensureQurlHostPermission()` requests only the single saved origin, and only after the user explicitly saves that server URL. This broad declaration exists to satisfy MV3 runtime permission mechanics and should be kept narrowly justified for Chrome Web Store review.
- **`permissions`** — `activeTab`, `scripting`, and `storage`.

### background.js

A lightweight MV3 service worker. Its responsibilities:

1. **Gmail tab validation** — Rejects requests when the active tab is not a Gmail tab.
2. **Content script bootstrap** — Sends a `QURL_PING`; if the receiving end is missing, reinjects `lib/qurl-compose-format.js` and `content/gmail-compose.js` using `chrome.scripting.executeScript`.
3. **Message relay** — Forwards `INSERT_LINKS` messages from the popup to the active Gmail tab and returns the content script response.

```js
// popup.js                        // background.js                  // gmail-compose.js
chrome.runtime.sendMessage     →  chrome.tabs.sendMessage         →  onMessage listener
  { type: 'INSERT_LINKS' }           (tabs[0].id, message)              (insertLinksIntoGmailDraft)
```

### popup.js

The popup is the user-facing interface. State: `selectedFiles[]`.

**User flow:**
1. User clicks "Browse files" → hidden `<input type="file">` triggers `change` event → files stored in `selectedFiles[]` → `renderFileList()` displays them.
2. User clicks "Upload to QURL" → for each file: read as `ArrayBuffer`, call `uploadFile()`, push result → `insertIntoGmailDraft(results)`.
3. `insertIntoGmailDraft()` sends successful results to the background relay.
4. `showResults()` renders success rows, upload failures, and Gmail insertion failures.
5. If Gmail insertion fails, the popup enables a manual **Copy inserted content** fallback using HTML and plain-text clipboard payloads.

**Key decisions:**
- Files are read as `ArrayBuffer` (not base64) — efficient for large files.
- Uploads run sequentially (not in parallel) — keeps UI state simple and avoids overwhelming the QURL server.
- Custom QURL server configuration is stored in `chrome.storage.local`.
- Fallback text is English; primary strings come from `chrome.i18n.getMessage()`.

### lib/qurl-api.js

The QURL API client. Exposes a single async function:

```js
async function uploadFile(fileBuffer, filename, contentType)
// → { success, resource_id, qurl_link, resource_url, expires_at, error }
```

**Host permission handling** — Before upload, `ensureQurlHostPermission()` checks whether the chosen QURL origin is already allowed. The built-in default origin is always permitted; custom HTTPS origins are requested dynamically when saved.

**Base URL normalization** — `normalizeQurlApiBase()` accepts either a bare server URL or a full `/api/upload` URL, strips the endpoint suffix, removes query/hash fragments, and enforces `https://`.

**Multipart body building** — The request body is built manually using a `Blob` with a custom boundary so the extension can upload raw file bytes with explicit control over multipart formatting.

**Payload extraction** — The API may return responses in two shapes:
```json
// Wrapped
{ "success": true, "data": { "qurl_link": "...", "expires_at": "..." } }
// Flat
{ "success": true, "qurl_link": "...", "expires_at": "..." }
```
`_extractPayload()` handles both.

**Expiry parsing** — `_parseExpiry()` accepts ISO strings, Unix timestamps (seconds), or milliseconds and normalizes to ISO strings.

**Filename sanitization** — `_sanitizeFilename()` strips `"`, `\`, `\r`, `\n` from filenames to prevent multipart header injection.

**Build-time default override** — `scripts/build-release.js` optionally reads `QURL_API_BASE` from the shell environment or `.env`, rewrites `release/lib/qurl-api.js`, and updates the release manifest's default host permission to match.
Quoted `.env` values are accepted, but only simple wrapping quotes are stripped; shell-style escaping is intentionally out of scope.

### content/gmail-compose.js

Injected into Gmail at `document_start` and also reinjected on demand by `background.js` when needed. Prevents double injection via `window.__QURL_COMPOSE_INJECTED__`.

**Compose body discovery** — `findComposeBody()` uses three strategies:

| Priority | Strategy | CSS Selector / Approach |
|---|---|---|
| 1 | Focused editable element | `.Am.Al.editable:not([contenteditable="false"]):focus` or semantic `[role="textbox"][contenteditable="true"]` fallbacks |
| 2 | Any visible editable | Gmail class selectors first, then semantic compose-body fallbacks with `isVisible()` |
| 3 | Compose iframe | `iframe[name="__frame"]` or `iframe[src*="compose"]` |

The semantic fallback reduces reliance on Gmail's obfuscated class names when a compose body exposes stable `role="textbox"` and `contenteditable="true"` attributes.

**Async wait strategy** — `findComposeBodyAsync()` first checks immediately, then uses a short-lived `MutationObserver` to react when Gmail mounts the compose body instead of silently polling for five seconds.

**HTML insertion** — Three-fallback strategy:

1. `document.execCommand('insertHTML', false, html)` — Works in Gmail's contenteditable context. Tried first as it respects cursor position.
2. **Selection API** — Creates a `Range` at the end of the editable div and inserts the fragment. Used when `execCommand` is not supported or throws.
3. **`insertAdjacentHTML('beforeend', ...)`** — Last resort append without reparsing the existing compose DOM.

**Notification** — `showGmailNotification()` creates a fixed-position toast that auto-dismisses after 4 seconds. Uses `role="alert"` and `aria-live="polite"` for accessibility.

---

## Communication Protocol

### Message: `QURL_PING`

Sent from `background.js` to the content script to verify that the Gmail runtime is ready.

**Response:**
```js
{ success: true }
```

### Message: `INSERT_LINKS`

Sent from popup → background → content script.

**Payload:**
```js
{
  type: 'INSERT_LINKS',
  results: [
    {
      filename: string,
      link: string,        // QURL access URL
      expiry: string|null  // ISO date string or null
    }
  ]
}
```

**Response:**
```js
{ success: boolean }
```

---

## API Contract

### Upload Endpoint

**POST** `{QURL_API_BASE}/api/upload`

**Request:**
```
Content-Type: multipart/form-data; boundary=----QurlBoundary<random>

Body:
  name="file"; filename="<filename>"
  Content-Type: <contentType>
  [file bytes]
```

**Expected response:**
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

The client accepts both wrapped (`{success, data: {...}}`) and flat (`{success, qurl_link: ...}`) response shapes.

---

## Configuration

| Variable | File | Default | Description |
|---|---|---|---|
| `DEFAULT_QURL_API_BASE` | `lib/qurl-api.js` | `https://getqurllink.layerv.xyz/` | Built-in fallback QURL server base URL |
| `qurlApiBase` | `chrome.storage.local` | unset | User-configured override for the QURL server base URL |
| `host_permissions` | `manifest.json` | Gmail + default QURL origin | Always-allowed origins bundled with the extension |
| `optional_host_permissions` | `manifest.json` | `https://*/*` | Additional HTTPS origins that may be requested one origin at a time for custom QURL servers |

---

## Security Considerations

1. **No sensitive data stored in extension.** API credentials (if any) should be handled server-side. The extension only sends file bytes and receives public URLs.
2. **HTML insertion is controlled.** Link labels and URLs are escaped before rendering. Shared formatting lives in `lib/qurl-compose-format.js`.
3. **Multipart header sanitization.** Filenames are stripped of `"`, `\r`, `\n` before inclusion in `Content-Disposition` headers.
4. **Custom server access is explicit.** Additional origins are requested only when the user saves a custom HTTPS QURL server.

---

## Browser Compatibility

Tested on:
- Google Chrome 88+ (MV3 required)
- Other Chromium-based browsers (Edge, Arc, Brave) — generally compatible

Not supported:
- Firefox (uses WebExtension manifest v2/v3 with different APIs)
- Safari (different extension format)

---

## Extension Icons

Icons are SVG source files (`icons/icon{16,48,128}.svg`) converted to PNG via `sharp`. The PNG files are what Chrome displays in the toolbar and extension list.

To regenerate after editing SVG sources:
```bash
npm install
npm run icons
```

The SVG source files are the canonical versions. Never edit PNG files directly — they will be overwritten.

---

## Local Verification

For a screenshot-based local unpacked validation flow, including loading `release/` in Chrome and verifying upload, draft insertion, copy fallback, and link access, see `docs/local-unpacked-testing.md`.
