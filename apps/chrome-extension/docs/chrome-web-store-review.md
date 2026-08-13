# Chrome Web Store Review Notes

## `optional_host_permissions` rationale

The manifest declares:

```json
"optional_host_permissions": ["https://*/*"]
```

This broad declaration exists only because the extension allows a user to save an arbitrary HTTPS qURL server at runtime. It is intentionally broader than the extension's actual runtime access pattern and should be called out explicitly in any reviewer note or store-listing justification.

It cannot be narrowed without removing the custom-server feature: MV3's `chrome.permissions.request()` can only request origins already covered by a pattern in `optional_host_permissions`. Because the user may type any HTTPS origin, this wildcard is the only declaration that lets the extension request that exact origin at runtime. The wildcard *declares what may be requested* — it is not a grant. No origin is accessible until the user enters it and approves Chrome's own per-origin prompt.

Chrome Web Store reviewers commonly scrutinize `https://*/*`, so the reviewer note should explicitly tie this wildcard to the user-driven custom-server feature and the exact-origin prompt flow below.

The extension does not auto-grant access to all HTTPS origins:

- The built-in default server is covered by a fixed `host_permissions` entry.
- A custom origin must be entered manually by the user in the popup settings.
- Before requesting origin access, the popup shows an inline confirmation naming the exact origin that will be requested.
- The popup then calls `chrome.permissions.request(...)` for that exact origin from the confirmation click handler, preserving the user gesture for Chrome's own prompt.
- Uploads fail closed if host permission is unavailable for a non-default origin.
- Grants do not accumulate. When the saved server changes to a different host, or is cleared back to the bundled default, the previous custom origin's host permission is removed via `chrome.permissions.remove` (see `revokeStaleCustomOrigin` in `lib/qurl-api.js`). Revocation runs only after the new value is persisted, so a failed save leaves the working permission in place, and it is skipped when the host is unchanged (a path-only edit on the same server keeps its grant). At any moment the extension therefore holds at most one custom-origin grant: the server currently configured.

The extension does not crawl arbitrary browsing origins. Additional access is used only for the single qURL upload origin that the user explicitly chooses and approves.
