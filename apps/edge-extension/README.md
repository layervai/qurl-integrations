# qURL for Microsoft Edge installation and releases

Use the qURL™ browser extension in Microsoft Edge to share files from Gmail
through expiring access links. Follow the [shared Gmail usage guide](../chrome-extension/README.md)
for uploading files and inserting links into a draft. This directory contains
the Edge packaging and store-submission instructions.

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
