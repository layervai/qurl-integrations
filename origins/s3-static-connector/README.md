# s3-static-connector

Reusable origin image for a **private S3 static site behind qURL™ Connector**.
It serves a private S3 bucket over plain HTTP on **loopback only**, so the qURL
Connector (running as a sidecar in the same network namespace) is the sole
reachable path and applies access control before any traffic reaches the
origin.

Published image:

```sh
ghcr.io/layervai/qurl-integrations/s3-static-connector
```

The publish workflow pushes `:main` and `:<git-sha>` tags after merges to
`qurl-integrations/main`. Deployments should pin the resolved image digest once
the image has been published and soaked.

The image packages two pinned processes:

- **nginx** owns the website surface — clean URLs, the security header set,
  `proxy_cache` (+ stale-on-error), compression, range/HEAD — and listens on
  `LISTEN_ADDR` (loopback). On a cache miss it proxies to Envoy.
- **Envoy** is an internal **SigV4 signer** on `ENVOY_LISTEN_ADDR` (loopback,
  not reachable outside the container). It signs the final hop to private S3
  with the workload's AWS credentials and forwards over TLS.

Signing happens **after** nginx resolves clean URLs and strips the query string,
so the canonical request Envoy signs is exactly what S3 receives — avoiding the
`SignatureDoesNotMatch` failure class. nginx sets the `Host` to the bucket vhost,
Envoy signs that same Host, and the cluster SNI matches it.

## Environment contract

This contract is frozen — additive only once published.

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `S3_BUCKET` | Yes | none | Private bucket to serve from. Must be a non-dotted, DNS-compatible name using lowercase letters, numbers, and hyphens. |
| `S3_PREFIX` | No | empty | Key prefix; normalized to a single leading slash, no trailing slash. |
| `LISTEN_ADDR` | No | `127.0.0.1:8080` | nginx listen address; matches `protect-connector`'s default `port:8080`. Must be loopback unless `ALLOW_NON_LOOPBACK_LISTEN=true`. |
| `ENVOY_LISTEN_ADDR` | No | `127.0.0.1:9090` | Internal signer listener; never exposed outside the container. Must be loopback unless `ALLOW_NON_LOOPBACK_LISTEN=true`. |
| `ALLOW_NON_LOOPBACK_LISTEN` | No | `false` | Explicit local-test/diagnostic escape hatch for binding either listener off-loopback and for plaintext S3 endpoint diagnostics. Do not set in protected deployments. |
| `INDEX_DOCUMENT` | No | `index.html` | Powers clean URLs; empty is invalid when explicitly set. |
| `AWS_REGION` | Usually | env / IMDS | Region for the S3 endpoint host and SigV4. Resolved from IMDSv2 when unset; validated as lowercase letters, numbers, and hyphen-separated segments. |
| `S3_ENDPOINT_ADDR` | No | bucket S3 vhost | Test/diagnostic override for the Envoy cluster address. Leave unset for real S3. Must be a DNS name, IPv4 address, or unbracketed IPv6 address. |
| `S3_ENDPOINT_PORT` | No | `443` | Test/diagnostic override for the Envoy cluster port. Must be numeric and in `1..65535`. |
| `S3_TLS` | No | `true` | Keep `true` for real S3. `false` is accepted only with `ALLOW_NON_LOOPBACK_LISTEN=true` for plaintext local tests or diagnostics. |
| `CACHE_MAX_SIZE` | No | `1g` | nginx `proxy_cache_path` max size. |
| `CACHE_DEFAULT_TTL` | No | (unset) | Unset = cache per the object's `Cache-Control` / nginx default. Set it to force a fallback TTL for objects S3 returns without cache metadata. Must be a non-zero nginx time literal such as `60s`, `5m`, or `1h30m`. |
| `CACHE_CONNECTOR_ID` | No | `QURL_CONNECTOR_ID`, then empty | Logical connector/site label used by `qurl-origin-cachectl purge-connector` as a fail-closed deployment guard. Set it to the stable customer-provided connector ID/slug used by your deploy automation. |
| `CACHE_REPLICA_ID` | No | container `HOSTNAME` when set | Physical origin/cache replica label emitted in cache-control JSON so fan-out jobs can account for every replica they touched. |

Credentials come from Envoy's default AWS credential provider chain (EC2 instance
role via IMDSv2, ECS task role, web identity, or env) — no static IAM key is
required in the happy path.

This image targets standard AWS commercial-partition S3 virtual-hosted
endpoints (`<bucket>.s3.<region>.amazonaws.com`). China, GovCloud, FIPS,
dualstack, and VPC endpoint hostnames need a future signed-host/SNI override;
`S3_ENDPOINT_ADDR` changes only the dial target, not the Host Envoy signs.

## Behavior contract

Only `GET` and `HEAD` are served (anything else → `405`). Object resolution uses
the path only; the query string never participates (lookup or cache key), so
`?_t=` cache-busters are transparently tolerated.

Clean-URL rewrite mirrors the CloudFront viewer-request function this origin
replaces, rule for rule:

```js
if (uri.endsWith('/'))       uri += 'index.html';   // INDEX_DOCUMENT
else if (!uri.includes('.')) uri += '/index.html';  // dot anywhere suppresses
```

nginx applies its own safe URI normalization (merge_slashes, dot-segment
removal) before these rules — a strict superset of CloudFront for the static
paths served here; it differs only on pathological inputs (e.g. `//a`, `/../a`)
that the protected dashboards never emit.

The CI-backed key contract is for simple static-site object paths using
letters, numbers, dots, underscores, hyphens, and slashes. Object keys that
need percent-encoding (spaces, non-ASCII bytes, literal `%2F`, etc.) stay in
the staging-soak bucket before this image is reused for such keyspaces.

Successful responses pass through the object's `Content-Type` and `Cache-Control`
verbatim. These security headers are set on **every** response (200/404/405/5xx):

- `Strict-Transport-Security: max-age=31536000; includeSubDomains`
- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: no-referrer`
- `X-Robots-Tag: noindex, nofollow, noarchive, nosnippet, noimageindex`

## Error mapping

| Condition | Client status | Body | Observability |
| --- | ---: | --- | --- |
| Missing key (S3 `404`) | 404 | `Not Found` | access log `upstream_status:404` |
| Signing / auth failure (S3 `403`) | 404 | `Not Found` | access log `upstream_status:403` (drives the SigV4-denied alarm) |
| Upstream 5xx / Envoy down | 502 | `Bad Gateway` | access log `status:502` (drives the origin-5xx alarm) |
| Method other than GET/HEAD | 405 | (nginx default) | access log only |

S3 error bodies and the 403-vs-404 distinction are never leaked to clients; the
distinction is preserved in the access log for alarming.

Range serving is intended for uncompressed objects. nginx gzip can take
precedence for compressible content types such as CSS, JS, JSON, SVG, and XML,
so byte-range behavior for those assets is not part of the contract. Viewer
headers are stripped before the Envoy/S3 hop, so a cold range request can fetch
and cache the full object; subsequent ranges can be served from nginx's cache.

## Logging

Both processes log JSON to the container's stdout/stderr, tagged with a `layer`
field (`nginx` / `envoy` / `origin`) so a shared log stream stays filterable:

- nginx access lines: `{"layer":"nginx","status":<num>,"upstream_status":"<str>","cache":"<HIT|MISS|...>",…}`.
- `entrypoint.sh` emits `{"layer":"origin","msg":"origin_started"}` once per start
  (the OriginRestart metric filter keys on it).
- `qurl-origin-cachectl purge` emits
  `{"layer":"origin","msg":"cache_purged",...}` when deploy automation clears
  the local proxy cache.

## Cache control

The image includes a local cache-control command for deployment pipelines that
need the same operational shape as CloudFront invalidations:

```sh
docker exec s3-static-connector qurl-origin-cachectl purge
docker exec s3-static-connector qurl-origin-cachectl purge /index.html /website
docker exec s3-static-connector qurl-origin-cachectl purge-connector stats-connector /index.html /website
```

With no path arguments, the command removes all entries from nginx's local proxy
cache and leaves the cache directory in place. With path arguments, it removes
the matching `GET` and `HEAD` cache entries. Paths are viewer paths, but
object-style index paths such as `/index.html` and `/website/index.html` also
purge their clean-URL aliases (`/`, `/website`, `/website/`) so deploy
automation can mirror the current stats invalidation list.

`purge-connector` is the connector-scoped form for replica-aware deployments. It
does the same local cache deletion as `purge`, but first requires the requested
connector to match `CACHE_CONNECTOR_ID`; a mismatch or unlabeled replica exits
non-zero without deleting anything. The command still acts on one origin
container's local nginx cache. Fan-out is intentionally owned by deployment or
orchestration. If a protected S3 website runs multiple connector/origin
replicas behind the same connector ID, the deployment or orchestrator-level
default should be to run the same
`purge-connector <connector-id> ...` command on every active origin replica that
serves that connector and count the emitted `replica_id`s. Targeting one
replica should be an explicit diagnostic or maintenance override. FRP load
balancing fans viewer requests across registered replicas; it does not purge
peer nginx cache directories.

It is intentionally **not** exposed as an HTTP endpoint, because the connector
forwards viewer traffic to the origin port and cache administration must stay
local to the host/container automation path. A CI/CD deployment can run it
through SSM or the host supervisor after syncing new objects to S3.

Deployment order matters: sync new/changed S3 objects first, then purge the
matching local cache entries. Missing-key 404s are intentionally not cached:
OSS nginx file-deletion purges cannot reliably invalidate intercepted 404s from
the shared cache zone, so a newly synced S3 key must not be hidden behind an
unpurgeable local negative cache.

## Process model

`entrypoint.sh` renders both configs from the environment, then runs Envoy and
nginx and exits as soon as **either** exits — so a crash of either process takes
the container down and produces a clean restart/alarm signal. `tini` is PID 1.

## Deploy requirements

- The instance/task role needs **`s3:GetObject`** on the objects **and**
  scoped **`s3:ListBucket`** on the bucket — without ListBucket, a missing key
  returns `AccessDenied` (403) instead of `NoSuchKey` (404), muddying the
  signing-failure signal.
- IMDSv2 from inside a container requires **hop-limit 2** on the host.
- The bucket stays private; this image needs no bucket-policy change.

## Out of scope

Conditional GET/304 semantics beyond nginx defaults, S3-style configurable error
documents, directory listing, IPv6-only S3 egress, and the deploy-time bucket
sync (serve-local mode) are intentionally not implemented here. Fully static
sites can run serve-local (any loopback static server + a deploy-time sync) and
skip this image entirely.

## Local development

```sh
bash test/render_test.sh                      # render configs, diff goldens
bash test/cachectl_test.sh                    # cache-control path/safety tests
bash test/behavior_test.sh                    # run the host-arch image against a stub S3
PLATFORM=linux/arm64 bash test/behavior_test.sh # run the t4g target image under QEMU
```
