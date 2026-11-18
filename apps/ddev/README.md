# Private DDEV review with qURL

This preview requires the graceful provider shutdown in
[DDEV PR 9139](https://github.com/ddev/ddev/pull/9139).

qURL™ checks the reviewer's identity before connecting them to the app. This
add-on uses DDEV's loopback HTTP port and prints a private request address.
The reviewer asks for access in their browser. Give access only after the
reviewer gives you their displayed code through a conversation you trust.

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
