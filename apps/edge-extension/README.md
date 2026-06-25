# qURL File Upload for Gmail

Upload files straight from Gmail's compose window and drop secure, expiring
links into your draft — no need to attach them to the email itself. Your
files go to qURL™, and only a short link travels in the message.

A **qURL** is a secure access link to an uploaded file. Links carry an
expiry, so a file you share today won't stay reachable forever.

## Quickstart

1. **Install** the extension (see [Installing](#installing) below).
2. Open **Gmail** and click **Compose**.
3. Click the **qURL File Upload** icon in the Edge toolbar.
4. Click **Browse files**, pick one or more, and click **Upload to qURL**.
5. The secure links appear at the bottom of your draft. Keep writing and send
   as normal.

## Installing

The extension is distributed by your qURL operator. Install the build they
give you:

1. Download or build the extension folder your operator provides (the
   unpacked `release/` directory).
2. Open `edge://extensions` in Microsoft Edge.
3. Turn on **Developer mode** (top-right toggle).
4. Click **Load unpacked** and select the extension folder.
5. The **qURL File Upload** icon appears in your toolbar. Pin it for quick
   access.

Building the extension yourself, or publishing it to Microsoft Edge Add-ons,
is covered in the [developer guide](docs/development.md).

## Using the extension

1. Open Gmail and start a **Compose** window — keep it open while you upload.
2. Click the **qURL File Upload** toolbar icon to open the popup.
3. Click **Browse files** and choose one or more files. Selected files are
   listed in the popup; remove any you didn't mean to add before uploading.
4. Click **Upload to qURL**. Each file shows its own progress, and the links
   are appended to the **end** of your draft once every file finishes.
5. Continue composing and send your email as usual. Each recipient gets a
   secure link plus, when available, the time the link expires.

A few things worth knowing:

- **Keep the popup open while uploading.** Clicking elsewhere or switching
  windows closes the popup and cancels any upload still in progress.
- **Gmail must be the active tab** in the focused window when you open the
  popup. A Gmail tab in a different window won't be picked up.
- **Files are capped at 100 MB each.** Larger files are reported individually
  instead of failing the whole batch. Your qURL server may set a lower limit,
  in which case a smaller file can still be rejected after upload.
- **Manual copy fallback.** If the links can't be inserted automatically (for
  example, no compose window is open), the popup keeps a **Copy inserted
  content** button so you can paste them into your draft yourself.

## Pointing at a different qURL server

By default the extension uploads to qURL's hosted server, so most people never
need to change anything. If your organization runs its own qURL server:

1. Open the popup and click the **settings** (gear) icon.
2. Enter your server's address in the **qURL server** field — either the base
   URL (`https://files.example.com`) or a full upload URL; the extension
   tidies it up for you. The address must use `https://`.
3. Click **Save**. The extension first asks you to confirm the exact address,
   then Edge shows its own permission prompt for that server. Approve both to
   start uploading there.
4. Click **Use default** at any time to switch back to the built-in server.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| "Could not find compose window" | No Gmail compose window is open | Open a compose window in Gmail, then upload again |
| "Active tab is not Gmail" | Gmail isn't the focused tab | Switch to your Gmail compose tab and reopen the popup |
| Uploads fail with a connection error | Wrong server address, or the server is unreachable | Check the **qURL server** field in settings, or clear it with **Use default** |
| A single file is rejected as too large | File exceeds the 100 MB cap, or your server's own limit | Share a smaller file, or ask your operator about server limits |
| Links inserted but missing in the sent email | Recipient's mail client strips formatting | Links are always inserted; some plain-text clients hide the styled version |
| Toolbar icon missing | Extension not enabled | Open `edge://extensions` and make sure it's turned on |

## Privacy

- Your files are uploaded only to the qURL server you're configured to use —
  the default hosted server, or the custom one you set in settings.
- The only thing the extension uploads is the files you choose — it never
  sends the contents of the pages you browse.
- A custom server is used only after you enter it and approve Edge's
  per-server permission prompt.

## For developers

Building, packaging, releasing, architecture, and the upload API contract are
documented in the [developer guide](docs/development.md).

## License

[MIT](../../LICENSE) — Copyright (c) 2025-present LayerV, Inc.
