# qURL Edge Add-ons Submission Guide

Use this guide when preparing `apps/edge-extension` for Microsoft Edge
Add-ons.

## Package summary

- Directory: `apps/edge-extension`
- Package name: `qurl-gmail-edge-extension`
- Default version: `1.0.0`
- Extension name: `qURL File Upload`
- Default upload server: `https://getqurllink.layerv.ai/`

## Build

```bash
cd apps/edge-extension
npm test
npm run package:release
```

The upload ZIP is written to `dist/` and should be submitted from there.

## Reviewer note

Use [edge-add-ons-review.md](./edge-add-ons-review.md) as the reviewer note
for the wildcard `optional_host_permissions` declaration.

## Store listing

- Extension name: `qURL File Upload`
- Short description: `Upload files to qURL and insert secure access links into Gmail compose drafts.`
- Privacy policy: explain that the extension uploads only user-selected files
  to the configured qURL server and inserts links into Gmail drafts.

## Final check

- Confirm the package name and version in `package.json`
- Confirm the manifest version matches `package.json`
- Confirm the Edge permission prompt flow still works with a custom HTTPS
  server
