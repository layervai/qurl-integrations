# Slack Marketplace reviewer test path

Use this runbook as the source for the reviewer instructions in the Slack App
Dashboard. It keeps the install, onboarding, user, admin, and Secure Access
Agent journeys reproducible without exposing an internal LayerV qURL™ resource.

Do not submit the app while any owner field below is blank or while the seeded
resource reaches a production system.

## Submission owner fields

Fill these fields in a private submission record. Do not commit credentials,
tokens, or reviewer personal data to this repository.

- Production install URL:
- Reviewer workspace:
- Reviewer Slack user:
- Reviewer qURL account email:
- Workspace owner/admin Slack user:
- Review channel:
- Seeded resource bare ID or alias (without `$`):
- Seeded resource description:
- Safe demo destination:
- Support contact:
- Production app version or commit:
- Exported manifest version and capture date:
- Committed production manifest bot scope set:
- Exported live production manifest bot scope set:
- Existing-install requested/granted bot scope set:
- Clean-install requested/granted bot scope set:
- Existing-install reauthorization capture date:

The seeded resource must:

- exist only for Marketplace review;
- be available in the review channel;
- resolve to non-sensitive demo content;
- accept a one-time qURL without changing external state; and
- be removable after review.

## Owner preflight

Complete this preflight before giving Slack the reviewer instructions.

1. Export the live production manifest and compare its URLs, scopes, events,
   interactivity, and Agent or Assistant feature with the committed manifest.
2. Record the exact committed and live bot scope sets above. Stop if the sets
   differ.
3. If the review workspace has an existing qURL install, rerun the production
   install flow to reauthorize it. Confirm that the requested and granted bot
   scopes exactly match the committed manifest set. Record the requested and
   granted sets above without retaining the bot token.
4. After retaining any reauthorization capture, have the owner remove the app,
   if one is installed, so the designated review workspace is clean. Open the
   production install URL, complete OAuth, and confirm that the requested and
   granted scopes match the committed manifest set. Record both sets above
   without retaining the bot token, and confirm that the success page directs
   the reviewer to `/qurl setup <email>`.
5. Prepare the dedicated qURL account named above, but do not run
   `/qurl setup`; the reviewer establishes the workspace connection in
   section 1.
6. Add the reviewer and owner/admin to the review channel.
7. Protect the safe demo destination in that channel. Record its bare ID or
   alias above without the leading `$`.
8. Confirm `/qurl help`, `/qurl-admin help`, App Home, private delivery, and
   the selected Agent or Assistant surface work in the production app.
9. Confirm the privacy policy, support page, AI disclosure, and paid Slack
   plan disclosure match the live Marketplace listing.

If the production manifest does not enable the reviewer-facing AI surface, do
not remove any AI steps. Fix or apply the manifest first so the instructions
match the submitted app.

## Reviewer journey

Run these steps in order in the designated review channel.
Leave the clean preflight install in place. The reviewer reopens the install
URL in section 1 to capture the reviewer-visible consent prompt and
reauthorizes the install before connecting it to qURL.

### 1. Install and connect

1. Open the production install URL, compare the consent prompt with the
   committed production manifest bot scope set recorded by the owner, and
   authorize it only when they match.
2. Verify that Slack returns to the qURL install-success page.
3. Run `/qurl setup <reviewer-email>`.
4. Complete the passwordless qURL sign-in.
5. Return to Slack and verify that setup succeeds without private operator
   instructions.

Expected result: the workspace is connected, the first connected user is the
workspace owner, and Slack provides the next command to run.

### 2. Check discoverability

1. Run `/qurl help`.
2. Run `/qurl-admin help`.
3. Open the qURL App Home.

Expected result: user and admin commands are distinct, setup guidance is
actionable, and App Home includes the AI disclosure, privacy link, and support
link.

### 3. Request private access

1. Run `/qurl list`.
2. Locate the seeded demo resource.
3. Run `/qurl get $<seeded-bare-id-or-alias> dm:true reason:"Slack Marketplace review"`.
4. Open the private message from qURL, but do not share its one-time link.

Expected result: only review-channel resources are listed, the access link is
delivered privately to the requester, its expiry is clear, and no secret is
posted to the channel.

### 4. Exercise the Secure Access Agent

1. Open the app's selected Agent or Assistant messaging surface.
2. Confirm that the first-use disclosure names Anthropic Claude, warns that AI
   output can be inaccurate, requires human review, links the privacy policy,
   and notes the paid Slack plan requirement.
3. Send `help`.
4. Ask for access to the seeded demo resource.
5. Review the proposal card and choose Reject.
6. Ask again and approve only if the owner marked the demo action safe for
   reviewer execution.
7. Open App Home and confirm the completed action appears.

Expected result: `help` is deterministic, the agent stays within the current
Slack context, every action requires an explicit human decision, Reject makes
no change, and any approved demo action records an audit event.

### 5. Check boundaries and recovery

1. Run `/qurl-admin admins` as the owner/admin.
2. Run the same command as a non-admin reviewer.
3. Send one unsupported file or image to the Agent or Assistant surface.
4. Use the Agent feedback controls or run `/qurl feedback`.
5. Ask the Slack workspace owner/admin to remove and reinstall the app.
6. Have the same first-connected reviewer from section 1 run `/qurl help` to
   confirm that Slack restored slash-command registration.
7. Have that reviewer run `/qurl setup <reviewer-email>` with the same email
   and complete passwordless sign-in to re-establish qURL workspace ownership.
8. Have that reviewer run `/qurl list` and confirm the seeded demo resource is
   listed.

Expected result: admin authorization is enforced, unsupported media receives a
clear limitation, feedback has a safe route, and reinstall requires an explicit
reconnect before connected commands return to a usable state. A successful
`/qurl help` response alone is not reconnect evidence.

## Evidence to retain

Keep reviewer evidence outside this repository:

- the exported production manifest;
- the exact committed and live bot scope sets;
- the exact existing-install and clean-install requested and granted scope
  sets;
- existing-install reauthorization, clean OAuth consent, and install-success
  captures;
- command, App Home, AI disclosure, proposal, reject, private-delivery, and
  unsupported-media captures;
- the post-reinstall setup and `/qurl list` capture;
- the production app version or commit used for the test;
- the audit record for the approved demo action, if approval was exercised;
  and
- the install, test, and cleanup timestamps.

Remove the seeded resource and reviewer workspace connection when review is
complete, unless the submission owner explicitly retains them for a follow-up
review.
