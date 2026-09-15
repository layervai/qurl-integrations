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

A **direct message** (`D…` channel) carries no channel-policy rows in practice —
nothing in the protect flow binds resources to a DM — so its allow-set comes
back empty and `/qurl list` there returns the channel empty state, with **no
resources listed**. Running `/qurl list` in a 1:1 does *not* show tunnels
protected in #ops or any other channel. Group / multi-person DMs (`mpim`)
behave the same way for the same reason. This follows from the general
fail-closed rule above rather than a type-specific guard: the scope is keyed on
the channel's policy row, not on its kind, so any channel with no row yields an
empty allow-set. (If you remember a time when a 1:1 listed tunnels from other
channels, see [Past behavior](#past-behavior) below — that was an earlier,
since-reverted state.)

## The capability boundary (defense in depth)

Channel scoping bounds what `/qurl list` *discloses*. Independently, minting is
gated per channel at mint time, so even a token learned out-of-band cannot be
minted from a channel where it isn't allowed:

- Minting a token that `/qurl list` showed enforces the channel's allow-set for
  non-admins. A token pasted into `/qurl get` from a channel where it isn't
  allowed fails closed.
- Minting through a channel alias requires that alias to be bound in the current
  channel. An alias that isn't bound in your channel fails closed.
- `/qurl get` requires channel context and accepts only the tokens and aliases
  that `/qurl list` and `/qurl aliases` surface — never an internal resource
  identifier.

So disclosure (what `/qurl list` reveals) and capability (what `/qurl get` will
mint) are governed by the *same* per-channel allow-set, and capability is
re-checked at mint time regardless of how a token was obtained.

## Why this matters for operators

Because `/qurl list` is channel-scoped, the set of resource names, descriptions,
and tokens a member can see in a channel is bounded by that channel's allow-set.
Treat membership in a channel as the disclosure boundary for the resources
protected there: adding a resource to a channel (or binding a channel alias)
discloses its name, description, and token to that channel's members, and is
also what lets them mint it.

## Past behavior

`/qurl list` has not always been channel-scoped. An earlier version briefly
listed every resource in the workspace to any member, regardless of channel,
and release notes from that period describe that wider disclosure. That widening
has since been reverted: the channel-scoped, fail-closed model described above
is the current behavior, and list, aliases, and mint now share one
channel-scoped allow-set.

So if you are reading a release note that says `/qurl list` shows the full
workspace list to every member, it predates the revert and no longer matches how
the bot behaves today.
