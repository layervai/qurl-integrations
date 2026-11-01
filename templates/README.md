# qURL edge templates

Each qURL™ template directory is self-contained: copy it to start a mint endpoint, or copy
its handler and exact SDK dependency into an existing project.

- [Cloudflare Worker](cloudflare-worker/README.md)
- [Netlify Edge Function](netlify-edge/README.md)
- [Vercel Edge Function](vercel-edge/README.md)

These examples use `createPortalForUrl()` from `@layervai/qurl` 3.1.1.
They mint on demand for a fixed configured target and require a separate
server-side bearer credential. Read the platform README before deploying.
The CLI scaffold integration requested in [#122](https://github.com/layervai/qurl-integrations/issues/122) is still tracked separately;
no `layerv protect` command is provided by these examples.

To run the local contract tests from the repository root:

```sh
for platform in cloudflare-worker netlify-edge vercel-edge; do
  (cd "templates/$platform" && npm ci --ignore-scripts --omit=optional --no-audit)
done
node --test templates/edge-templates.test.mjs
```

Tests invoke all three handlers with the real installed SDK and an intercepted
HTTP transport. They check authentication, fixed target and lifetime, no API
calls for rejected requests, upstream failures, and SDK identity validation.
They do not create live links or deploy a function.
