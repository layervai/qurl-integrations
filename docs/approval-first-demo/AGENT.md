# Review the demo with your own AI agent

You can test an approved agent without finding a second person. Use a CLI-enabled
agent with its own qURL device identity, such as an agent on a second computer or
in its own environment. The agent must be able to run the qURL CLI and reach
LayerV's service. A cloud agent cannot publish an app on your computer, but it
can review the app you publish for its device.

This checks an HTTP response. Use the [browser demo](README.md) to check page
rendering and a browser action. Keep the app's own login and permissions.

## 1. Start the app and identify the reviewer

Use Python 3.7+ and qURL CLI 4.4.3+. Start the loopback server from step 1 of
the [browser demo](README.md#1-start-the-local-app). Serve only the included
non-sensitive pages. Keep its request log visible; do not make local HTTP
health checks during the observation.

Ask the reviewer agent to run:

```sh
qurl whoami -o json
```

Get only its `device_public_key_b64` through a trusted conversation. On the
publisher computer, run the same command and compare the public keys. They
must differ. Do not copy publisher credentials or state to the agent. If both
commands use the same device identity, this is an owner fetch, not this test.
Do not publish full command output, keys or access links in a public thread.

Before access, choose a non-sensitive string from the demo page for the agent
to find. Do not give that string to the agent in its request; ask it to return
the page's title. Check the response against the local file yourself.

## 2. Publish for that device

Replace `REVIEWER_PUBLIC_KEY` with the intended agent's public key. On
macOS/Linux, use a fresh local ID:

```sh
qurl publish http://127.0.0.1:8765 --id "agent-demo-$(date +%s)" --name "Private agent review" --allow-device-key 'REVIEWER_PUBLIC_KEY' --foreground -o json
```

On Windows, choose a new ID rather than using `$(date +%s)`. Keep the publisher
running. Check `private` is `true` and `found_existing` is `false`. Otherwise
stop and inspect; do not send a reused resource or delete an earlier share.
Use the returned `crid`, not the different `resource_id` field.

This command approves the key at publication. It is a different path from
browser request codes or `--access` policy modes; do not combine those modes
with this recipe. Check the app log before the agent fetches: it should have
no HTTP request lines. Record the actual window and any unexpected requests.

## 3. Ask the agent to fetch the actual app

Give the CRID to the intended agent through the trusted conversation. Ask it
to run this command and return the title from the response:

```sh
qurl get CRID --file -
```

Check the title against the local page and check that the app log records a
successful GET. A link, an access page, a tool listing or the agent saying it
is ready is not proof that it received the app. For your own app, choose an
agreed non-sensitive task and verify its result.

## 4. Remove access

Follow [step 4 of the browser demo](README.md#4-remove-access): delete only this
owned resource with `qurl delete CRID --yes` before stopping or reusing its port.
Deletion is permanent. Ask the same agent to repeat the fetch; it should fail
without returning app bytes. Then stop the publisher and Python server.
Do not report cleanup as confirmed if deletion fails.

The publisher computer and app must stay running during the review. This test
measures app HTTP requests, not every network packet, and qURL does not close
any other public route to your app.

[Share your actual app with a reviewer](https://qurl.link/share-without-deploying/?utm_source=approval-first-agent-demo&utm_medium=referral&utm_campaign=first-share-pilot)
