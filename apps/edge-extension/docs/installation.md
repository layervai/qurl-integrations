# Installation

## Local Unpacked Install

1. From `apps/edge-extension/`, build the unpacked extension:

```bash
npm run release
```

2. Open `edge://extensions`.
3. Enable **Developer mode**.
4. Click **Load unpacked**.
5. Select the generated `release/` directory.

For a full validation flow with screenshots, see [local-unpacked-testing.md](./local-unpacked-testing.md).

## Build-Time Default Server Override

The extension supports two qURL server configuration paths:

- Runtime override in the popup settings, stored in `chrome.storage.local`
- Build-time default override for `release/` and `dist/`

To change the build-time default:

1. Copy `.env.example` to `.env`.
2. Set `QURL_API_BASE=https://your-qurl-server.com` (the value must start with
   `https://`).
3. Rebuild with `npm run release` or `npm run package:release`.

`QURL_API_BASE` is applied only by the build scripts. The extension runtime does not read `.env` directly.

## Runtime Custom Server Permissions

When you save a custom qURL server in the popup settings, the extension first asks for confirmation naming the exact origin, then Edge shows its per-origin host-permission prompt.

`optional_host_permissions` remains `https://*/*` only to enable this one-origin-at-a-time flow for user-specified HTTPS qURL servers.
