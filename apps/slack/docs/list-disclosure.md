# Disclosure model for `/qurl list`

This note documents what `/qurl list` discloses to a workspace member, and how
that disclosure is bounded. It is reference material for operators and workspace
admins reasoning about confidentiality. If you're a Slack user, the
[README](../README.md) is the place to start.

## What `/qurl list` shows

`/qurl list` is **channel-scoped**. A member running it in a channel sees only
the resources protected in *that* channel — the channel's allow-set, the same
set `/qurl get` mints against and `/qurl aliases` lists. A resource protected in
another channel does not appear until an admin makes it available in this one
(via the Edit modal or a channel alias binding). The scope applies to **admins
too**: there is no admin bypass that surfaces every workspace resource from any
channel.

This means `/qurl list`, `/qurl aliases`, and `/qurl get` all agree on the same
per-channel set — what you can see listed is what you can mint here, and nothing
beyond it.

### Fail-closed behavior (including DMs)

The channel scope is enforced fail-closed. If the scope cannot be computed, the
listing refuses rather than falling back to a workspace-wide view:

- **No channel context** (a payload with an empty `channel_id`) is refused, not
  fanned out workspace-wide.
- **Admin store unavailable** (a deployment without the backing store) refuses,
  because the scope can't be computed.
- **A scope read error** fails closed — it never falls back to an unscoped list.
- **Nothing protected in this channel** returns the channel empty state, with no
  resources listed.

A **direct message** (`D…` channel) has no channel-policy rows of its own, so
its allow-set is empty and `/qurl list` there returns the channel empty state —
**no resources are listed**. Running `/qurl list` in a 1:1 does *not* show
tunnels protected in #ops or any other channel. (If you remember a time when it
did, see [History](#history) below — that was an earlier, since-reverted state.)

## The capability boundary (defense in depth)

Channel scoping bounds what `/qurl list` *discloses*. Independently, minting is
gated per channel at mint time, so even a token learned out-of-band cannot be
minted from a channel where it isn't allowed:

- `/qurl get $<id>` enforces the channel's allowed resource-id set for
  non-admins. A `$<id>` pasted into `/qurl get` from a channel where it isn't
  allowed fails closed.
- `/qurl get $<alias>` requires an alias binding in the current channel. An
  alias that isn't bound in your channel fails closed.
- `/qurl get` also requires channel context and rejects raw internal
  `r_...` identifiers.

So disclosure (what `/qurl list` reveals) and capability (what `/qurl get` will
mint) are governed by the *same* per-channel allow-set, and capability is
re-checked at mint time regardless of how a token was obtained.

## Why this matters for operators

Because `/qurl list` is channel-scoped, the set of resource names, descriptions,
and tokens a member can see in a channel is bounded by that channel's allow-set.
Treat membership in a channel as the disclosure boundary for the resources
protected there: adding a resource to a channel (or binding a channel alias)
discloses its slug/description/token to that channel's members, and is also what
lets them mint it.

## History

This model has changed over time; the order matters if you're reconciling an old
release note:

- Per-channel scoping for `/qurl list` was added in #234.
- It was **reverted** in #459, which widened `/qurl list` to show the full
  workspace list to every member regardless of channel. Release notes from that
  window describe the wider disclosure.
- Channel scoping was **re-introduced** in #589 and is the behavior described
  above. List, aliases, and mint now share one channel-scoped allow-set. (The
  code comments cite #590 — a later paging-completeness fix within the #589
  channel-scope work, not a separate disclosure change.)

If you are reading a release note that says `/qurl list` shows the full
workspace list to every member, it refers to the #459 window and no longer
matches current behavior.
