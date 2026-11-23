# qURL for Microsoft Edge

**Send files from Gmail in Microsoft Edge as expiring access links instead of attachments.**
This is the Edge build of the [qURL™ extension for Gmail](../chrome-extension/README.md).
It uploads files from Gmail's compose window and drops a link for each one
into your draft, and your recipient just clicks. It is built from the same
source as the Chrome extension and works the same way.

## Common tasks

| You want to… | How |
| --- | --- |
| Build the Edge package | `npm run package:release` in `apps/edge-extension` |
| Try it in Edge | `edge://extensions` → **Developer mode** → **Load unpacked** → `release/` |
| Submit it to Microsoft Edge Add-ons | Upload the ZIP from `dist/` ([submission guide](docs/edge-add-ons-submission-guide.md)) |
| Build it for your own qURL server | Set `QURL_API_BASE` in `../chrome-extension/.env` before building |

Once it is loaded, use it exactly as the
[Gmail extension quickstart](../chrome-extension/README.md#quickstart) shows:
**Compose** → the **qURL File Upload for Edge** toolbar icon → **Browse files** → **Upload to qURL**.

### For AI agents and tools

- Package: `qurl-gmail-edge-extension`; extension name in Edge:
  `qURL File Upload for Edge`
- Source: `apps/chrome-extension` (shared); this directory holds the Edge
  version, changelog, and store documents
- Build: `cd apps/chrome-extension && npm ci`, then
  `cd ../edge-extension && npm run package:release` → `release/` and `dist/*.zip`
- The Edge and Chrome versions stay equal; a release of either bumps both
- Other ways to share: [qURL CLI](../cli/README.md) ·
  [Slack](../slack/README.md) · [Discord](../discord/README.md) ·
  [MCP server](https://github.com/layervai/qurl-mcp)

## Build and release

The Edge release uses the shared browser-extension source in
`../chrome-extension`. Release Please keeps its package version equal to the
Chrome package version. This directory owns the Edge release output, changelog,
and Microsoft Edge Add-ons documents.

Build configuration is shared too. Copy `../chrome-extension/.env.example` to
`../chrome-extension/.env` to set `QURL_API_BASE` for either browser package.

```sh
cd apps/edge-extension
npm run package:release
```

Load `release/` at `edge://extensions`, or submit the ZIP in `dist/` to
Microsoft Edge Add-ons. See [review notes](docs/edge-add-ons-review.md) and the
[submission guide](docs/edge-add-ons-submission-guide.md).
