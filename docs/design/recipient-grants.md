# Recipient grants

Status: proposal for [issue #936](https://github.com/layervai/qurl-integrations/issues/936).
The commands and recipient-grant routes below are not implemented. This document
is a design input, not an API specification or an installation guide.

## User result

A publisher uses a qURL™ credential to grant access to one CRID and notify the
recipient. The service verifies the publisher's resource authority and the
recipient's identity. Delivery alone never proves identity or grants admission.

Proposed command inputs:

```text
qurl grant <CRID> --user email:ana@example.com
qurl grant <CRID> --user slack:<workspace-id>/<user-id>
qurl grant <CRID> --group slack:<workspace-id>/<channel-id>
```

Require one selector. Slack selectors include their workspace; do not infer it
from a local bot token or silently choose among installations. The CLI uses only
its configured qURL credential. It must not request a Slack bot token.

## Existing behavior to preserve

| Current command | Purpose |
| --- | --- |
| `qurl access invite <CRID> --email <address>` | Invite a consumer into the existing private-access enrollment flow. An invitation is not an approved grant. |
| `qurl access approve` / `qurl approve` | Approve an existing request through its supported decision contract. |
| `qurl grants <CRID>` | Inspect or change existing device/public-key access. It does not resolve email or Slack recipients. |

Reuse the existing verified enrollment, publisher-decision, delivery and cleanup
primitives where their contracts fit. Do not implement the new command as an
alias for generic link minting, an invitation-only call, or the device allowlist.
Do not publish a second independent enrollment or session-revocation system.

## Authorization and provider context

An email grant binds to the identity proven by the service's email-verification
flow. Reuse its address validation and normalization. Mailbox control does not
prove legal identity. Forwarding an invitation must not transfer the grant.

A Slack user grant binds to a verified workspace/user pair. A channel grant binds
to current eligible membership in that workspace/channel, rather than a frozen
recipient list. Membership changes must affect later admissions within an
explicit, tested freshness bound. Existing sessions follow the documented
revocation/lease contract. An unverified or stale membership result is not an
allow decision.

The service needs an authorized, managed provider installation and a trusted
identity/membership source for the target workspace. Installation inventory alone
is not that authority. Resolve Slack Connect and Enterprise Grid context without
crossing owners or choosing an unrelated workspace. Missing scopes, missing
installation and unavailable provider state must produce distinct errors.

## Proposed service surface

Use a separate recipient-grant family, provisionally
`/v1/crids/{crid}/recipient-grants`. The producer must publish its supported
contract before the SDK or CLI exposes it.

- Create with the typed recipient and an `Idempotency-Key`. The same key and
  intent return the same grant; a changed intent conflicts. A lost reply must
  be recoverable without sending duplicate invitations or creating new grants.
- Return an opaque grant ID, version, grant state and delivery state. Keep
  `queued`, provider-accepted delivery and failed delivery distinct. A successful
  API acceptance must not be described as a delivered message or a recipient view.
- Read and list grants with bounded pagination. Return effective expiry and
  session policy selected by the service. Omission uses the existing resource
  policy; do not invent a second set of CLI defaults or silently widen access.
- Update or revoke with the current `expected_version`, following the existing
  publisher-decision conflict convention. A stale decision must not overwrite a
  newer grant. Revoking an invitation and revoking an active grant are distinct.
- Revoke only this grant's authority. Report pending session cleanup as pending;
  do not claim completion from a timeout, generic 404, or missing observation.
  Preserve the replay/decision facts needed to retry the same operation safely.

Authorization and delivery have separate outcomes. If delivery fails after the
grant commits, retain its ID and state so the owner can inspect, retry delivery
or revoke it. Do not silently roll back or leave the caller with an unknown grant.
Owner-facing output must not expose invitation secrets, provider tokens or live
access links as diagnostic fields. Audit records need bounded event names,
operation outcomes and safe correlation, not recipient credentials or raw provider
responses.

## Implementation and release gates

1. Publish the producer's permission, identity, provider, state, expiry,
   idempotency, pagination and error contracts. Include unsupported installation,
   missing scope, wrong owner, ambiguous workspace and provider-unavailable cases.
2. Prove recipient-specific admission and revocation through the existing native
   resource/session lifecycle. A grant must not expose the origin publicly or
   remove another grant's access. Forwarded invitations and the wrong recipient
   must fail without consuming the intended recipient's access.
3. Add typed SDK support, then the CLI command and status/revoke operations. Use
   exact-wire tests and shared contract inputs; do not ship a client-only promise.
4. Run approved Sandbox email and Slack user/channel journeys, including member
   removal, concurrent decisions, a lost create reply, failed delivery and pending
   cleanup. Verify authorization, provider delivery and native access separately.
5. Keep provider resolution and membership refresh in bounded control-plane work.
   No provider lookup, grant fan-out, global lock, qURL mint or resolve belongs in
   steady-state application traffic. Large groups must not require an unbounded
   request or whole-history scan.

Issue #936 stays open until the producer, client and real recipient journeys meet
these gates. This proposal creates no route, command, grant, message or release.
