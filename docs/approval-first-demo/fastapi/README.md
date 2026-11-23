# Check whether an unauthorized client can reach a private FastAPI app

This disposable app logs `APP_REQUEST` for every HTTP request it receives.
It contains no secrets, database, application login or real user data.
Use it to check admission before sharing a real API. Keep the real API's own
authentication, authorization and Host/Origin checks.

## Run the local fixture

From this directory (`docs/approval-first-demo/fastapi`), run the commands below.
Install the [qURL CLI](../../../../apps/cli/README.md#install) before the access test.

Use Python 3.13 and a separate virtual environment. This example was tested with
FastAPI 0.163.1, Uvicorn 0.54.0 and qURL CLI 4.4.4 on macOS.

```sh
python3 -m venv .venv
.venv/bin/python -m pip install fastapi==0.163.1 uvicorn==0.54.0
.venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8766 --no-access-log
```

Keep this terminal visible. Do not open the app locally or send HTTP health
checks during the observation; those also reach the app and change the count.
TCP readiness alone does not send an HTTP request.

## Test private access

Use three distinct device identities: the publisher, an authorized person or
agent, and an unapproved test device. They can be separate machines. For a
controlled test on one machine, each identity needs its own state directory;
do not copy a publisher's identity or credentials to another participant.

The intended recipient runs `qurl whoami -o json` on their own device and gives
the publisher its `device_public_key_b64` through a trusted conversation. This
is a public key, not their private identity state. Verify whom the key belongs
to before allowing it.

The publisher replaces `PUBLIC_DEVICE_KEY` below and chooses a new `DEMO_ID`:

```sh
qurl publish http://127.0.0.1:8766 --id DEMO_ID --name "Private FastAPI test" --allow-device-key PUBLIC_DEVICE_KEY --foreground -o json
```

Before sharing, require `status: serving`, `private: true`, and
`found_existing: false` in the JSON. Note its full `crid` as `CRID` below.
If those checks fail, stop and reconcile the result. Do not delete an existing
resource as though this test created it. Keep the foreground publisher running.

1. On the unapproved device, run `qurl get CRID --file -`. It must fail without
   returning app bytes. The app terminal must show no additional `APP_REQUEST`.
2. On the approved device, run the same command. Require this actual JSON:
   `{"message":"This request reached the private FastAPI app."}`.
   The controlled single-fetch test must add one `APP_REQUEST`.
3. On the publisher, run `qurl delete CRID --yes -o json`. This permanently
   deletes only this fresh disposable resource. Require `deleted: true` and
   an `id` equal to that exact CRID; a zero exit status alone is insufficient.
4. Retry the fetch on the previously approved device. It must report revoked
   access, return no app bytes and add no app request.

After confirmed deletion, stop the foreground publisher and Uvicorn. Do not
reuse the app port while resource deletion is unconfirmed. Preserve the exact
recovery identity privately if cleanup fails; do not delete resources by name.

## What this establishes

The device key is authorized **at publication**. This is not a browser-code or
passkey demonstration, and it does not measure requests during a pending
approval. qURL's public access service remains reachable. The protected app
must have no separate public route. Configured ngrok authentication and
Cloudflare Access can also reject requests before an origin; compare the
selected access workflow rather than assuming their controls are absent.

The controlled November 23 test observed zero app requests initially, zero
additional requests from the unapproved device, one from the approved device,
and zero after deletion. The disposable resource and owned processes were
removed. This was LayerV synthetic QA, not independent product adoption.
