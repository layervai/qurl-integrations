# qURL Edge Add-ons Submission Guide

Use this guide when preparing `apps/edge-extension` for Microsoft Edge
Add-ons.

## Package summary

- Directory: `apps/edge-extension`
- Package name: `qurl-gmail-edge-extension`
- Version: see `package.json`
- Extension name: `qURL File Upload for Edge` (the localized `ext_name`, which is what the store listing shows)
- Default upload server: `https://getqurllink.layerv.ai/`

## Build

```bash
cd apps/chrome-extension
npm ci
npm test
cd ../edge-extension
npm run package:release
```

Both browser packages read `QURL_API_BASE` from
`apps/chrome-extension/.env`; see its `.env.example` before building.
The upload ZIP is written to `dist/` and should be submitted from there.

## Reviewer note

Use [edge-add-ons-review.md](./edge-add-ons-review.md) as the reviewer note
for the wildcard `optional_host_permissions` declaration.

## Store listing

- Extension name: `qURL File Upload for Edge` (the localized `ext_name`, which is what the store listing shows)
- Short description: `Upload files to qURL and insert secure access links into Gmail compose drafts.`
- Privacy policy: explain that the extension uploads only user-selected files
  to the configured qURL server and inserts links into Gmail drafts.

## Final check

- Confirm the package name and version in `package.json`
- Confirm the built manifest version matches `package.json`
- Confirm the Edge permission prompt flow still works with a custom HTTPS
  server
