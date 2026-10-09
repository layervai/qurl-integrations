# qURL for Microsoft Edge

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
