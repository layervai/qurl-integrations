# Watch a private app review

Identity first, then connection. qURL™ uses OpenNHP for cryptographic
invisibility: an unapproved visitor cannot reach this app through its private
share. This small demo lets you check actual app requests before and after
approval. It runs two static pages with Python's built-in HTTP server.

Have a CLI-enabled AI agent with its own device identity? You can use the
[agent review](agent-review.md) instead; it does not require a second person.

## Before you start

Use Python 3.7 or later and the [qURL CLI](../../apps/cli/README.md#install), version 4.4.3
or later, on the publisher computer. The reviewer needs a browser and a passkey.
No qURL publisher account, API key, deployment or DNS setup is needed.

Use the included non-sensitive pages. Do not point the server at your project
root: that could serve source files or credentials. Keep any real app's own
security controls when you later share it. This fixture has no application login.

## 1. Start the local app

From the repository root, run:

```sh
python3 -u -m http.server 8765 --bind 127.0.0.1 --directory docs/approval-first-demo/site
```

On Windows, use `py -3` if `python3` is not available.

Keep this terminal visible. It logs each HTTP request. Do not open the app
locally or run an HTTP health check during the observation: those requests would
also appear. A TCP connection alone is not an HTTP request.

## 2. Publish privately

In a second terminal, run the following macOS/Linux command. Its ID changes for
each run; wait at least a second before repeating it:

```sh
qurl publish http://127.0.0.1:8765 --id "approval-demo-$(date +%s)" --name "Private app review" --allow-requests --foreground -o json
```

On Windows, choose a new local ID instead of the `$(date +%s)` expression.

Keep this terminal running. Note the returned JSON `crid` value: use it for
`CRID` in the approval and deletion commands below. Before sending its
`resource_url`, check the JSON:
`private` must be `true`, `found_existing` must be `false`, and
`access_requests` must be `true`. Otherwise stop and inspect the resource;
do not send the link or delete a resource from an earlier review.

The reviewer opens `resource_url`, chooses **Request access**, saves a passkey
and gives you the displayed code through a conversation you trust. Record the
observation window and check the app terminal: no HTTP request lines should
appear before approval, including while the request is pending. If any appear,
record them and investigate; do not call that a zero-request result.

## 3. Approve and check the app

In a third terminal, replace `CRID` and `CODE` with this resource's values:

```sh
qurl approve CRID CODE
```

Approve only a code given by the intended reviewer through that trusted
conversation. A typed name on a request is not proof of identity.

The reviewer chooses **Open**, then **Confirm the review** in the app. Ask them
to confirm the second page. The app terminal should now show successful GETs
for `/` and `/review-confirmed.html`. Extra browser requests can occur; retain
the real log rather than assuming there must be exactly two.

## 4. Remove access

When the review ends, delete this demo's resource before stopping or reusing the
app port. Replace `CRID` with the resource created above:

`--yes` confirms permanent deletion of this owned resource without another prompt.
Deletion cannot be undone.

```sh
qurl delete CRID --yes
```

Check deletion succeeds. `qurl get CRID --file -` should then fail rather than
return the app; `Resource Revoked` is the expected error for this demo. Then stop the publisher and Python
server with Ctrl+C. If deletion fails, report cleanup as unconfirmed and resolve it.

This checks HTTP requests for these two pages, rather than every app or network
packet. Python serves only loopback here; qURL does not close other tunnels you
may have opened. The publisher computer and app must stay running during the review.

[Share your actual app with a reviewer](https://qurl.link/share-without-deploying/?utm_source=approval-first-demo&utm_medium=referral&utm_campaign=first-share-pilot)
