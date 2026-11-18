# Private DDEV review with qURL

qURL™ checks the reviewer's identity before connecting them to the app. This
workflow uses DDEV's loopback HTTP port and a private request address.
The reviewer asks for access in their browser. Give access only after the
reviewer gives you their displayed code through a conversation you trust.

## Review an app with the released DDEV

Install the [qURL CLI](../cli/README.md#install), version 4.4.3 or later.
Start your DDEV project, then run `ddev describe`. Find its web service's
loopback HTTP address, such as `http://127.0.0.1:32781`.

Publish that address in another terminal. Use a new local ID for this review:

```sh
qurl publish http://127.0.0.1:32781 --id ddev-review-1 --name "DDEV review" --allow-requests --foreground -o json
```

Keep the command running. Check `private: true`, `found_existing: false`,
and `access_requests: true` in the JSON. Send `resource_url` to the reviewer.
When they give you the code from their screen, approve it in another terminal:

```sh
qurl approve <CRID> <code>
```

Ask them to press **Open** and confirm that the app loads. End the review
with `qurl delete <CRID>`, then stop the publishing command with Ctrl+C.
Do this before stopping or restarting the DDEV project.

This direct CLI workflow does not use `ddev share` or need its shutdown fix.

## Add-on preview

The `ddev share --provider=qurl` add-on requires the graceful shutdown in
[DDEV PR 9139](https://github.com/ddev/ddev/pull/9139).

Requirements: Linux or macOS, Python 3, qURL CLI 4.4.3 or later, and a DDEV
build with PR 9139. The add-on does not change the default share provider.

Install the [qURL CLI](../cli/README.md#install), then check out this
repository's preview branch:

```sh
git clone --depth 1 --branch feat/ddev-private-review https://github.com/layervai/qurl-integrations.git
```

From a running DDEV project:

```sh
ddev add-on get /path/to/qurl-integrations/apps/ddev
ddev share --provider=qurl
```

Send the printed address to your reviewer. In another terminal, run the
approval command printed by the provider, using the code they give you.
Ask the reviewer to open the app and confirm that it loads.

Press Ctrl+C in the sharing terminal to end access. The provider deletes
the resource it created and stops its foreground Connector. If deletion
needs a retry, it prints the exact cleanup command. DDEV and the local app
stay running.

End sharing before stopping or restarting the DDEV project, so its host port
cannot be reused while a reviewer still has access. To remove the add-on,
end sharing, then run `ddev add-on remove qurl`.

[Private app review walkthrough](https://qurl.link/share-without-deploying?utm_source=ddev-addon&utm_medium=referral&utm_campaign=first-share-pilot)

## Development checks

```sh
python3 -m unittest discover -s apps/ddev -v
shellcheck apps/ddev/share-providers/qurl.sh
```
