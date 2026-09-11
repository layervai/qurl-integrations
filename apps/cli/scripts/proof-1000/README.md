# proof-1000 — N concurrent local shares from one machine

`proof-1000` is the client-side harness for the headline scaling proof: **N
Connector CRIDs published from one machine through the real `qurl` CLI, all
serving at once**. It publishes N loopback shares against a single local
origin, watches the resident share daemon until every share serves, verifies
a sample end to end through the qURL™ platform, holds at steady state while
re-sampling, and writes a report. N is a parameter; the report is designed to
show a platform limit rather than hide it.

Every mutation goes through the CLI (`qurl publish`, `qurl delete`). Every
observation comes from the daemon's owner-only IPC `/status` socket, the CLI's
local share registry (read through the CLI's own reader), or the platform
itself. The harness never edits daemon state, never stops the daemon, and only
ever deletes shares whose Connector ID it minted.

## What one run does

1. Starts one loopback HTTP origin that answers every path with a small JSON
   body: a per-process `server_nonce`, a per-request `request_id`, and the
   request `Host`. It counts requests per `Host`.
2. Publishes N shares with Connector IDs `proof-<run>-0001 … -N`, all pointing
   at that origin, through `qurl -v publish … --id … -o json`. Concurrency
   starts at `--concurrency` (default 4) and adapts: any rate-limit signal
   (exit 9, or a 429 the CLI retried) halves it and pauses for `Retry-After`;
   a streak of clean publishes grows it back by one. The `-v` diagnostics are
   parsed to count **API calls and 429s per publish**.
3. Samples the daemon `/status` every 5 s until every proof resource is
   `serving` or `--serving-deadline` passes, recording the serving curve.
4. Fetches a sample end to end with `qurl get <CRID> --file -` (all N when
   N ≤ 100, otherwise 100 seeded-random plus the first and last 10). Each
   fetch must return this origin's nonce, a `request_id` the origin actually
   logged for that `Host`, and a `Host` of `<connector routing id>.qurl.site.…`
   taken from the local registry. It also records the daemon's RSS, threads,
   open FDs, TCP sessions by remote port, and the machine's established TCP
   count.
5. Holds for `--hold` (default 5 min), re-sampling `/status` every 5 s and
   fetching one sampled share every `--fetch-interval`, counting every
   non-serving sample and failed fetch.
6. Writes `report.md` + `report.json` (plus `run.json`, `run.log`) into the
   run directory and prints a summary table. Failures carry the daemon log
   lines that mention that share.

A run is **resumable**: `run.json` records every share as soon as it is
published, so re-running with the same `--run` skips what is already
published and re-publishes only what failed. The origin port is pinned per
run because publishing the same ID with a different target restarts the
share. It is **safe to interrupt**: Ctrl-C still writes the report (exit 130).

## Requirements

- The installed release CLI (`--qurl`, default `qurl` on `PATH`) and a
  registered device (`qurl whoami` works). Its `publish` manages the per-user
  daemon, so **never point `--qurl` at a development build** — a binary
  version change re-points the resident daemon.
- The daemon's environment. The harness resolves the state dir exactly as
  the CLI does (`QURL_CONNECTOR_STATE_DIR`, else the platform user-state
  directory) and, on macOS, reads the resident LaunchAgent's own arguments to
  cross-check the state dir and endpoint and to supply the Hub trust triple
  (`QURL_CONNECTOR_HUB_HOST` / `_PORT` / `_SERVER_PUBLIC_KEY_B64`) to every
  CLI call when the process environment does not already set it. A release
  build without a pinned production Hub key needs that triple for every
  foreground command; the harness's preflight (`version`, `whoami`, `list`,
  daemon `/status`) fails before publishing anything if it is still missing.
- `QURL_DEPLOYMENT` pointing at the deployment settings file for the target
  environment (issuer keys plus relay allowlist), or `--skip-verify`. The
  SDK ships no issuers, so `qurl get --file` cannot run without it.
- A `get`-capable binary for `--consume-qurl` (default: same as `--qurl`).
  The consume path only mints links and downloads; it never touches the
  daemon, so a from-source build is safe there when the installed release
  cannot mint links against the target environment.

## Run

From the repository root:

```bash
export QURL_DEPLOYMENT=/path/to/sandbox-deployment.json

# validate the harness
go run ./apps/cli/scripts/proof-1000 --run r3 --n 3 --hold 1m
go run ./apps/cli/scripts/proof-1000 --run r25 --n 25

# the headline run
go run ./apps/cli/scripts/proof-1000 --run k1 --n 1000 --hold 15m --serving-deadline 30m

# tear down exactly one run's shares and verify they are gone
go run ./apps/cli/scripts/proof-1000 --run k1 --teardown
```

Reports land in `proof-1000-runs/<run>/` (override with `--out`); the
directory is git-ignored. `go run ./apps/cli/scripts/proof-1000 -h` lists
every flag.

Exit codes: `0` proof passed, `1` a proof assertion failed (report written),
`2` usage or preflight failure, `130` interrupted.

### Attributing failures to a known platform event

A sandbox deploy or API rollover during a run shows up as failed fetches and
degraded status samples. Declare it and the report attributes instead of
guessing:

```bash
go run ./apps/cli/scripts/proof-1000 --run k1 --n 1000 \
  --window 'api-rollover=2026-09-03T01:37:00Z/2026-09-03T01:46:00Z'

# or after the fact, from the run's report.json, without touching the platform
go run ./apps/cli/scripts/proof-1000 --run r25 --rerender \
  --window 'api-rollover=2026-09-03T01:37:00Z/2026-09-03T01:46:00Z'
```

Every failure and status sample is stamped with the window it falls in, the
hold and verify counts are split into inside/outside, and the verdict says
whether anything happened outside the declared window(s). The pass/fail
verdict itself stays strict — a window explains a failure, it does not erase
it — and no tunnel or daemon conclusion is drawn from failures inside one.

## Teardown

`--teardown` deletes the union of the run manifest and every local registry
row whose Connector ID matches `^proof-<run>-[0-9]{4,5}$`, and nothing else.
It then walks the whole active tunnel listing (following `has_more`) and the
local registry to prove none remain, writing `teardown.json`. Deleting an
already-deleted share is idempotent.

## Classifying a failed fetch

Each failed fetch is classified in the report: `api-rollover` (inside a
declared window), `nhp-ac-deny:<code>` (a platform access-control deny on
the visitor path — the readiness codes `52005` or `52028`, or an overload
signal; the CLI maps exactly these to "the service is busy"), `content-404`
(access was granted but the origin answered 404), or `other`. During a run, a failed verify/hold fetch is
followed immediately by an SDK-level probe (`qurl.EnterPortal` on a fresh
share link) that records the raw deny code the CLI keeps private, so the class
is grounded in what the platform actually said.

`--probe <CRID>` runs that same SDK probe once and prints the JSON
(`deny_code`, `granted`, `content_http_status`). **Caveat:** run it *during*
the run's life, not after — once the run ends its loopback origin is gone, so
a probe then reports `granted:true` with `content_http_status:404` for every
share (the tunnel is up but nothing listens locally). After a run, only the
`deny_code`/`granted` fields are meaningful; the content status is not.

## Reading the report

- **Publish**: counts, wall time, publishes/min, per-publish p50/p95, API
  calls per publish, 429s, throttle events, and the concurrency range, plus
  the full event timeline.
- **Serving curve**: serving/starting/retrying/failed/stopped/absent/missing
  counts over time with the daemon's failure category/code histogram.
- **End-to-end sample**: per fetch, the `Host` seen, and the three
  assertions.
- **Daemon resource usage** before, after publish, at steady state, and
  after the hold.
- **Implied cost of N=1000**: the API-budget bound (calls per publish ÷ the
  assumed per-owner budget) and the measured-throughput bound.
- **Failures** with the daemon log lines that explain them.

Owner ids, key prefixes, bearer tokens, in-link credentials, the Hub trust
values, the hostname, and the home directory are redacted from everything
written to the run directory (`run.json`, `report.json`, `report.md`,
`teardown.json`, and both logs go through the same redactor). CRIDs, routing
hosts, resource ids and the API endpoint are kept on purpose — they are the
evidence — so a run directory shared *before* teardown names live resources.
