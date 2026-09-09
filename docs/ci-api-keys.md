# CLI automation credentials

CLI CI uses one dedicated qURL™ API key for trusted setup and cleanup. It never
requests an Auth0 client-credentials token. The customer processes receive only
the two run-specific child keys, which expire within 24 hours.

Before enabling the changed workflows:

1. Deploy qurl-service PR #1498 with the `qurl:keys` permission.
2. Use an unlimited dedicated CI account. Create its automation key through an
   authenticated account-management session whose JWT holds `qurl:keys` and
   `qurl:agent`, with these exact key scopes:
   `qurl:agent`, `qurl:keys`, `qurl:read`, `qurl:resolve`, `qurl:write`.
3. Store the key as GitHub environment secret `QURL_JOURNEY_API_KEY`. Set
   environment secret `QURL_JOURNEY_OWNER_ID` to the matching `/v1/me` owner.
   A finite key needs at least three hours remaining when setup starts.
4. Run all scheduled/release customer lanes and the cleanup workflow. Check that
   the run keys are distinct, device enrollment completes, and cleanup revokes
   only the run's keys and resources. The automation key must survive cleanup.
5. Remove `QURL_JOURNEY_AUTH_CLIENT_ID`, `QURL_JOURNEY_AUTH_CLIENT_SECRET`, and
   `QURL_JOURNEY_AUTH_TOKEN_ENDPOINT`, then revoke the old custom-API Auth0 grant.

Keep the existing CI owner when migrating so cleanup can reach old run records.
A key for a different account cannot clean up those records. If the old owner is
an Auth0 machine subject, account administrators must provision the replacement
under that same canonical owner through the approved administrative procedure;
do not bypass ownership by changing database rows.

The parent key never enters artifacts or the customer process environment.
Children cannot hold `qurl:keys`, exceed parent scopes, or outlive their parent.
Interactive Discord, Slack, and Teams login is unchanged.

The unlimited requirement is the account plan, not a recommendation to keep
credentials forever. Rotate the automation key at least every 90 days: create
a replacement under the same owner, update both protected environments, verify
setup and cleanup, then revoke the old key. For suspected compromise, revoke
it immediately and revoke its remaining children through account management.
