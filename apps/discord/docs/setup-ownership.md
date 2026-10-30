# Discord setup ownership

OAuth setup requires a signed ID token with the correct issuer, audience,
expiry, flow nonce, and authentication time. Both setup entry points request
`max_age=0`. The callback permits five seconds of clock difference and rejects
authentication before the current redirect. Missing or invalid identity fails
before the bot mints a key.

The service uses the Auth0 subject as the JWT owner ID and as the owner of keys
minted by that JWT. `/v1/me` reports that same owner ID for API keys.

Each configured guild stores `qurl_owner_id`, the stable qURL owner identity.
It does not depend on the OAuth state secret or audit-log retention. Setup can
replace a key for the same owner. A different owner gets a refusal with guidance.
Pasted keys must pass the same owner check.

For a legacy row, the bot asks `/v1/me` to identify its saved key. It adopts the
owner only if the returned owner matches the new credential's owner. An invalid
saved key, an unavailable identity service, or an incomplete identity response
cannot authorize migration. The existing configuration stays in place.

The database write checks the owner and the exact encrypted key that was read.
Two concurrent setups cannot both replace that key. A losing OAuth setup tries
to delete its newly minted orphan key and asks the administrator to start again.

## Ownership transfer and recovery

`/qurl setup` is a same-owner operation. It is not an ownership-transfer door.
If the saved key cannot prove a legacy owner, the bot operator must verify the
existing account and guild administrator before recovery. An ownership transfer
also requires the current owner's approval and verification of the new account.
Keep the existing binding until both checks are complete. Record the approval,
revoke the old credential, reconcile its webhook subscription references, and
use a conditional update against the reviewed guild row. Never delete a row or
clear its owner as a way to make an unverified setup succeed. This process needs
operator action; there is no automatic or self-service owner reset.

Bot removal currently records an uninstall audit event and retains the guild row.
It cannot reset the owner. The raw row-delete helper has no production caller.
