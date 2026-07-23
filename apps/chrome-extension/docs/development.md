# Developer guide

Building, testing, packaging, and releasing the qURL File Upload extension.
If you just want to **use** the extension in Gmail, see the
[README](../README.md) instead.

## Prerequisites

- Node 18+ (the repo pins `22.21.0` in `.nvmrc`)
- Google Chrome, or another Chromium browser, with access to
  `chrome://extensions`

## Setup

```bash
cd apps/chrome-extension
npm install
```

## Build, lint, and test

```bash
npm run release   # build the unpacked extension into release/
npm run lint      # eslint, zero warnings allowed
npm test          # node --test unit suite
```

The `release/` directory holds the unpacked extension. Load it through **Load
unpacked** as described in [installation.md](./installation.md), which also
covers the build-time default server override (`QURL_API_BASE`).

For a full screenshot-based smoke test against live Gmail before a release,
follow [local-unpacked-testing.md](./local-unpacked-testing.md).

## Default qURL server

The built-in default server is centralized in `lib/qurl-config.js` (see the
[DESIGN.md configuration table](./DESIGN.md#configuration)). To point a
packaged build at a non-production or self-hosted server, set `QURL_API_BASE`
before building; the build regenerates `lib/qurl-config.js` and the manifest
host permission together. See
[installation.md](./installation.md#build-time-default-server-override) for the
exact steps and `.env.example` for the template.

## Icons

Regenerate the PNG icons from the shared `icons/logo.png` source:

```bash
npm run icons
```

## Packaging for the Chrome Web Store

For most release and local-validation workflows, one command bumps the
version, rebuilds `release/`, and writes a fresh upload ZIP to `dist/`:

```bash
./scripts/package-all.sh 1.0.0      # explicit version
./scripts/package-all.sh patch      # or patch | minor | major
```

The steps it runs are also available individually.

### 1. Bump the version

Keeps `package.json`, `manifest.json`, and the root version in
`package-lock.json` in lockstep:

```bash
npm run version:patch   # or version:minor | version:major
node scripts/bump-version.js 1.2.3   # explicit version
```

### 2. Build the release directory

```bash
npm run release
```

`release/` contains `manifest.json`, `background.js`, and the `content/`,
`popup/`, `lib/`, `icons/`, and `_locales/` directories.

### 3. Create the upload ZIP

```bash
npm run package:release
```

This rebuilds `release/` first, then writes a ZIP to `dist/`. On
macOS/Linux it uses `zip`; on Windows it uses PowerShell's `Compress-Archive`.
The ZIP places `manifest.json` at the root, as the Chrome Web Store requires.

```text
dist/qurl-chrome-extension-v1.0.0.zip
```

Upload the ZIP from `dist/` to the Chrome Web Store — not the repository
itself.

### Bump and package in one step

```bash
npm run publish:patch   # or publish:minor | publish:major
```

If packaging fails after the bump, `package.json`, `package-lock.json`, and
`manifest.json` may already carry the new version even though no ZIP was
produced.

### Versioning is owned by Release Please

Released versions are managed by Release Please monorepo mode. This app is
registered in `release-please-config.json` with a `node` release-type, and an
`extra-files` entry keeps `manifest.json`'s `$.version` in lockstep with
`package.json`. The seed in `.release-please-manifest.json` is **`1.0.2`** —
intentionally not the `0.1.0` other apps use — because the extension already
carried `1.0.2` as its pre-monorepo Chrome Web Store version; seeding lower
would make automated bumps appear to regress. The `bump-version.js` scripts
above remain a convenience for ad-hoc local Web Store ZIPs outside the release
flow.

## Architecture and API contract

[DESIGN.md](./DESIGN.md) documents the architecture (popup, background service
worker, Gmail content script, shared formatter), the internal message
protocol, the upload API contract (`POST {QURL_API_BASE}/api/upload` and its
response shapes), configuration variables, and security considerations.

## Chrome Web Store review

[chrome-web-store-review.md](./chrome-web-store-review.md) explains the
`optional_host_permissions: ["https://*/*"]` declaration for reviewers: it
exists only to let the extension request a single user-entered HTTPS server
origin at runtime, and grants no broad access on its own.
