# Disclosure model for `/qurl list`

This note documents what `/qurl list` discloses to a workspace member, and how
that differs from what they can actually *do*. It is reference material for
operators and workspace admins reasoning about confidentiality. If you're a
Slack user, the [README](../README.md) is the place to start.

## What `/qurl list` shows

Within a workspace, **every member sees the same `/qurl list` output** — the
full workspace master list of tunnels — regardless of channel or admin status.
Specifically, all members can see:

- Tunnel slugs (`$<slug>`) and their descriptions.
- The resource-id fallback token (`$r_<id>`), which is shown only for a
  slug-less / alias-less tunnel. This is surfaced to all members, **including
  IDs the caller cannot mint for in their current channel.**

### DM corollary

Running `/qurl list` from a direct message (a `D…` channel) returns the **full
workspace master list**, the same as any other channel. DMs have no
channel-policy rows of their own, so the listing is not narrowed there. If you
expect `/qurl list` in a 1:1 to be empty or DM-scoped, it is not — "I ran
`/qurl list` in a 1:1 and saw tunnels from #ops" is the expected behavior, not
a bug.

## What `/qurl list` does *not* change: the capability boundary

Seeing a tunnel in `/qurl list` does **not** grant the ability to mint a link
for it. Minting is gated separately and per channel:

- `/qurl get $r_<id>` enforces the channel's allowed resource-id set for
  non-admins. Pasting a seen-but-not-allowed `$r_<id>` into `/qurl get` from a
  channel where it isn't allowed still fails closed.
- `/qurl get $<alias>` requires an alias binding in the current channel. An
  alias you saw in the (workspace-wide) listing but that isn't bound in your
  channel still fails closed.

So `/qurl list` is a **confidentiality / disclosure** surface, not a
**capability** boundary. The set of links a member can actually mint is
unchanged by what `/qurl list` reveals.

## Why this matters for operators

`/qurl list` does not scope its output by channel or membership. If a workspace
has been treating the contents of `/qurl list` as a confidentiality boundary on
tunnel slugs and descriptions — assuming members only see tunnels for channels
they're in — that assumption does not hold. Slugs and descriptions are
workspace-wide.

This affects confidentiality of the *names and descriptions* of tunnels only.
It does not widen who can mint links, which remains governed per channel by the
allowed resource-id set and alias bindings.
