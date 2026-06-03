# Chrome Web Store Review Notes

## `optional_host_permissions` rationale

The manifest declares:

```json
"optional_host_permissions": ["https://*/*"]
```

This broad declaration exists only because the extension allows a user to save an arbitrary HTTPS QURL server at runtime. It is intentionally broader than the extension's actual runtime access pattern and should be called out explicitly in any reviewer note or store-listing justification.

Chrome Web Store reviewers commonly scrutinize `https://*/*`, so the reviewer note should explicitly tie this wildcard to the user-driven custom-server feature and the exact-origin prompt flow below.

The extension does not auto-grant access to all HTTPS origins:

- The built-in default server is covered by a fixed `host_permissions` entry.
- A custom origin must be entered manually by the user in the popup settings.
- Before requesting origin access, the popup shows an inline confirmation naming the exact origin that will be requested.
- The popup then calls `chrome.permissions.request(...)` for that exact origin from the confirmation click handler, preserving the user gesture for Chrome's own prompt.
- Uploads fail closed if host permission is unavailable for a non-default origin.

The extension does not crawl arbitrary browsing origins. Additional access is used only for the single QURL upload origin that the user explicitly chooses and approves.
