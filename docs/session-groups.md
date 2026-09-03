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
- **Per-share failure isolation.** If the platform reports a route's resource
  as gone (revoked or deleted), the daemon withdraws just that route and
  persists that share off; every other route keeps serving. A transient failure
  to bring a route up leaves it in the group and retrying, again without
  affecting siblings.
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

## Scale and limits

- **Up to 2000 local shares per machine.** The registry cap
  (`localSharesMaxItems`) matches the session group's `MaxGroupRoutes`, so the
  durable registry can hold exactly as many shares as one admission carries
  routes.
- **Registry file cap.** The registry file is bounded at 4 MiB. A full 2000-row
  registry of maximal rows (long base64url resource identities and CRIDs, long
  Connector, routing, and knock identities, and verbose IPv6 loopback targets)
  measures ~1.7 MiB, so the cap keeps roughly 2x headroom.
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
