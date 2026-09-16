# Local shares on one Connector session

The qURL™ CLI daemon serves **every local share on one Connector session**. A
machine that publishes many CRIDs knocks once, logs in once, and holds one
heartbeat stream for the whole set; each share is one route on that single
session rather than a session of its own.

## Why

Before this model, the daemon ran one session per share: each CRID had its own
NHP knock, its own login, its own per-proxy authorization, and its own
registration heartbeat stream, rotated make-before-break every admission
window. Every share therefore cost one of each of those on the qURL platform,
and the per-owner platform budgets for sessions and heartbeat streams capped a
single machine near ~300 shares.

Every local share on a machine is a route of the *same* Connector, protected by
the *same* knock resource, so one admission legitimately authorizes every
route; the platform only gates each proxy registration on the admission still
being inside its open window. Serving the whole set from one session amortizes
the knock, login, authorization, and heartbeat stream across every share. Only
the per-route proxy registrations still scale with the share count, and the
platform accepts those in batches, so 1000 shares on one machine become cheap.

## The model

- **One session, many routes.** The daemon reconciles the durable local-share
  registry into one `SessionGroupRunner`. Each desired-on share is one route on
  that runner. Bringing the whole set up is one knock and one login.
- **Hot add and remove.** Publishing a new share adds its route to the live
  session with no second knock. Stopping or deleting a share drops its route.
  Neither disturbs the routes that are already serving.
- **`restart` re-registers one route.** `qurl restart <CRID>` advances that
  share's serving epoch and re-registers only its route under a fresh proxy
  name, so a stale session cannot keep serving it. Its siblings and the shared
  admission are untouched — restarting one share is not a whole-session
  rotation.
- **Per-share failure isolation.** If the platform refuses a route's proxy
  registration (a `resource_not_found` answer to that one proxy), the daemon
  withdraws just that route, reports it on `/status` as `retrying` with failure
  category `platform_denied`, and re-adds it under a fresh proxy name with
  bounded backoff for as long as its row stays desired-on; it never silently
  turns the share off, so `qurl publish` and `qurl inspect` show what happened.
  Every other route keeps serving. A transient failure to bring a route up
  leaves it in the group and retrying, again without affecting siblings. The
  only permanent case is a denial of the group's single knock (the Connector's
  own resource unknown to the platform), which persists every share off.
- **Make-before-break rotation.** The one admission is renewed before it
  expires: a replacement session registers every route the old session was
  serving before the old one is drained. The rotation lead scales with the
  route count (registering N routes on a fresh session is sequential on the
  platform), so a large set starts its replacement earlier. When the admission
  window is too short for the lead the route count needs, the daemon logs a
  warning with the numbers (`routes`, `needed`, `lead`) so an operator can see
  the window is undersized.
- **Diagnostics unchanged.** `qurl status` and `qurl inspect` still report each
  share's own redacted state — serving, retrying with a failure category and
  code, or failed — derived from that route's phase on the session. The IPC
  `/status` contract is unchanged.

## Modes

The single-session model above is the target and the default. The daemon also
carries a compatibility mode, selected by `QURL_SHARE_GROUP_MODE` (config key
`share_group_mode`, flag `--share-group-mode` on `qurl daemon run`), for a
platform that has not yet authorized every route of a Connector on one
session:

- **`single`** — one `SessionGroupRunner` over every desired-on share, as
  described above. On a platform whose tunnel edge admits a session's proxies
  only for the one resource the NHP session was signed for, every share beyond
  the first is refused (`resource_not_found`) and stays visible as
  `retrying` / `platform_denied` until the platform authorizes it.
- **`per-share`** — one `SessionGroupRunner` per desired-on share, each with
  `Routes=[that share]`, `ResourceID` its own resource, and `KnockResourceID`
  from its own row. The native admitter is still shared (knocks serialize on
  it), but every share spends its own admission, login, and journal, and is
  rotated on its own. This is what such a platform accepts today, and what the
  daemon did before the single-session model. Above `PerShareSoftCap` (300)
  desired-on shares the daemon logs a warning on every reconcile: the platform
  is still the authority, but a fleet that size is expected to run into the
  per-owner session and heartbeat-stream budgets, and the warning lets an
  operator attribute the retrying excess to the budget rather than to the
  shares.

A removed group that outlives the daemon's stop bound is tracked as
*retiring*, and its resource is not re-admitted until that group has finished,
so two live sessions are never signed for one resource; a reconcile that had
to wait for one is re-run shortly.

Reconcile semantics are identical in both modes: `publish` adds a group,
`stop` removes exactly that group (retiring its admission), `restart` is a
`RestartRoute` on that share's own group, a refused route retires and
re-knocks only its own group after its backoff, and a knock-level denial
persists only that share off. `/status` reports the union of every group, so
the IPC contract and `qurl inspect` output are unchanged. One cost follows
from the group having a single route: a refusal withdraws the group's only
route, so the session ends and the retry is a fresh knock rather than a
re-registration on a live session. The refusal backoff carries across that
rebuild, so a share the platform keeps refusing costs one knock per five
minutes at steady state — the same cadence as `single` mode's one
registration attempt, with a knock in front of it.

The mode is folded into the daemon's job version (`3/<version>` for `single`,
`3/<version>/per-share` otherwise) and written into the per-user job as an
explicit `--share-group-mode`, so a resident daemon always runs in the mode
its job was installed for. Changing the mode is therefore a job-definition
change: the next `qurl start`, `restart`, or `publish` replaces the resident
daemon in the new mode, the same path a binary-version change takes.

## Scale and limits

- **Up to 2000 local shares per machine.** The registry cap
  (`localSharesMaxItems`) matches the session group's `MaxGroupRoutes`, so the
  durable registry can hold exactly as many shares as one admission carries
  routes. That figure is for `single` mode; `per-share` pays one session per
  share and is bounded by the per-owner platform session and heartbeat-stream
  budgets instead (`PerShareSoftCap`, ~300 shares, carries the
  `TODO(upstream-contract)` marker for that budget).
- **Registry file cap.** The registry file is bounded at 4 MiB. A full 2000-row
  registry of maximal rows (long base64url resource identities and CRIDs, long
  Connector, routing, and knock identities, and verbose IPv6 loopback targets)
  measures ~1.66 MiB, so the cap keeps roughly 2.5x headroom.
- **Bounded status payload.** The owner-only `/status` payload is bounded so a
  full 2000-route fleet stays well within the reader limit: each route appears
  once in the running map and once in the redacted diagnostics map, together
  well under 1 KiB per route.

## Server-side dependencies

Correctness needs no platform change — one machine served this many shares by
knocking per share before, just expensively. What makes 1000 routes *cheap* on
the platform is batched registration heartbeats, a per-owner authorization
limiter sized for a whole set's rotation, and a longer Connector admission
window so the session is not forced through a fresh knock every few minutes.
Those land on the platform independently of this CLI change.
