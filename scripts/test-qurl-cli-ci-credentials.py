#!/usr/bin/env python3
"""Hermetic tests for the trusted CLI customer-credential controller."""

from __future__ import annotations

import argparse
import base64
import importlib.util
import json
import pathlib
import sys
import tempfile
import time
import urllib.parse
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("qurl-cli-ci-credentials.py")
sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("qurl_cli_ci_credentials", SCRIPT)
assert SPEC and SPEC.loader
credentials = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(credentials)


def encoded(value: dict[str, object]) -> str:
    raw = json.dumps(value, separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


class FakeAPI:
    def __init__(self) -> None:
        now = int(time.time())
        self.owner = "ci-client@clients"
        self.jwt = "header." + encoded(
            {
                "aud": "https://sandbox.example",
                "exp": now + 4000,
                "gty": "client-credentials",
                "iat": now,
                "iss": "https://auth.example/",
                "scope": "qurl:agent qurl:read qurl:write",
                "sub": self.owner,
            }
        ) + ".signature"
        self.key_id = "key_AbCdEf123456"
        self.api_key = "lv_test_customer-key"
        self.keys: dict[str, dict[str, object]] = {}
        self.resources = [
            {
                "description": "qurl CLI journey v2 resource 1231/2/host",
                "resource_id": "r_customer_ci",
                "status": "active",
                "target_url": "https://example.com/?qurl-private-sandbox-device-journey=1",
            },
            {
                "description": "customer data",
                "resource_id": "r_keep",
                "status": "active",
                "target_url": "https://example.com/keep",
                "type": "url",
            },
            {
                "resource_id": "r_redacted_tunnel",
                "status": "active",
                "type": "tunnel",
            },
        ]
        self.connector_resources = {
            "connector-cli-journey-v2-415907f85f12d5ffd69c6a62": {
                "resource_id": "r_connector_smoke",
                "slug": "connector-cli-journey-v2-415907f85f12d5ffd69c6a62",
                "status": "active",
                "type": "tunnel",
            },
            "connector-cli-journey-v2-87e091ca6623843507b5863b": {
                "resource_id": "r_connector_failure",
                "slug": "connector-cli-journey-v2-87e091ca6623843507b5863b",
                "status": "active",
                "type": "tunnel",
            },
        }
        self.deleted_keys: list[str] = []
        self.deleted_resources: list[str] = []
        self.operations: list[str] = []
        self.transient_key_delete = 0

    def __call__(
        self,
        url: str,
        method: str,
        bearer: str | None = None,
        body: bytes | None = None,
        content_type: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        parsed = urllib.parse.urlsplit(url)
        if parsed.netloc == "auth.example":
            form = urllib.parse.parse_qs((body or b"").decode())
            assert method == "POST"
            assert content_type == "application/x-www-form-urlencoded"
            assert form["scope"] == ["qurl:agent qurl:read qurl:write"]
            return 200, json.dumps({"access_token": self.jwt, "token_type": "Bearer"}).encode()
        if parsed.path == "/v1/me":
            if bearer == self.jwt:
                data = {"auth_type": "jwt", "owner_id": self.owner}
            elif bearer == self.api_key:
                data = {
                    "api_key": {
                        "key_id": self.key_id,
                        "kind": "api_key",
                        "scopes": credentials.CUSTOMER_SCOPES,
                    },
                    "auth_type": "api_key",
                    "owner_id": self.owner,
                }
            else:
                return 401, b'{}'
            return 200, json.dumps({"data": data}).encode()
        if parsed.path == "/v1/api-keys" and method == "POST":
            request = json.loads(body or b"{}")
            assert request == {
                "kind": "api_key",
                "name": request["name"],
                "scopes": credentials.CUSTOMER_SCOPES,
            }
            assert extra_headers and extra_headers["Idempotency-Key"].startswith("qurl-cli-ci-")
            row = {
                "api_key": self.api_key,
                "key_id": self.key_id,
                "kind": "api_key",
                "name": request["name"],
                "scopes": credentials.CUSTOMER_SCOPES,
                "status": "active",
            }
            self.keys[self.key_id] = row
            return 201, json.dumps({"data": row}).encode()
        if parsed.path == "/v1/api-keys" and method == "GET":
            rows = list(self.keys.values()) + [
                {
                    "key_id": "key_Device123456",
                    "kind": "device",
                    "name": "qURL CLI registered device",
                    "status": "active",
                },
                {
                    "key_id": "key_Unrelated123",
                    "kind": "api_key",
                    "name": "customer key",
                    "status": "active",
                },
            ]
            return 200, json.dumps({"data": rows, "meta": {"has_more": False}}).encode()
        if parsed.path.startswith("/v1/api-keys/") and method == "DELETE":
            if self.transient_key_delete:
                self.transient_key_delete -= 1
                return 503, b'{}'
            key_id = urllib.parse.unquote(parsed.path.rsplit("/", 1)[1])
            self.deleted_keys.append(key_id)
            self.operations.append("revoke:" + key_id)
            self.keys.pop(key_id, None)
            return 204, b""
        if parsed.path == "/v1/resources" and method == "GET":
            query = urllib.parse.parse_qs(parsed.query)
            if "slug" in query:
                assert set(query) == {"slug"}
                assert len(query["slug"]) == 1
                row = self.connector_resources.get(query["slug"][0])
                return 200, json.dumps({"data": [] if row is None else [row]}).encode()
            assert query == {"limit": ["100"]}
            return 200, json.dumps({"data": self.resources, "meta": {"has_more": False}}).encode()
        if parsed.path.startswith("/v1/resources/") and method == "DELETE":
            resource_id = urllib.parse.unquote(parsed.path.rsplit("/", 1)[1])
            assert not resource_id.startswith("connector-cli-journey-v2-")
            self.deleted_resources.append(resource_id)
            self.operations.append("delete:" + resource_id)
            for connector_id, row in list(self.connector_resources.items()):
                if row["resource_id"] == resource_id:
                    del self.connector_resources[connector_id]
            return 204, b""
        raise AssertionError((method, url, bearer))


def private_file(root: pathlib.Path, name: str, value: str) -> pathlib.Path:
    path = root / name
    path.write_text(value, encoding="utf-8")
    path.chmod(0o600)
    return path


def auth_args(root: pathlib.Path) -> argparse.Namespace:
    return argparse.Namespace(
        audience="https://sandbox.example",
        client_id_file=private_file(root, "client-id", "ci-client"),
        client_secret_file=private_file(root, "client-secret", "client-secret"),
        qurl_endpoint="https://sandbox.example",
        token_endpoint="https://auth.example/oauth/token",
    )


def test_unbounded_valid_pagination() -> None:
    pages = 5

    def fake_request(
        url: str,
        method: str,
        bearer: str | None = None,
        body: bytes | None = None,
        content_type: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        del bearer, body, content_type, extra_headers
        assert method == "GET"
        parsed = urllib.parse.urlsplit(url)
        query = urllib.parse.parse_qs(parsed.query)
        assert "status" not in query
        page = int(query.get("cursor", ["0"])[0])
        response = {
            "data": [{"resource_id": f"r_{page}"}],
            "meta": {
                "has_more": page + 1 < pages,
                "next_cursor": str(page + 1) if page + 1 < pages else "",
            },
        }
        return 200, json.dumps(response).encode()

    with mock.patch.object(credentials, "request", fake_request):
        rows = credentials.paged_rows(
            "https://sandbox.example", "jwt", "/v1/resources", "test", status_filter=None
        )
    assert [row["resource_id"] for row in rows] == [f"r_{page}" for page in range(pages)]


def test_connector_cleanup_lookup_fails_closed() -> None:
    connector_id = "connector-cli-journey-v2-415907f85f12d5ffd69c6a62"
    valid = {
        "resource_id": "r_connector_smoke",
        "slug": connector_id,
        "status": "active",
        "type": "tunnel",
    }
    for response in (
        {},
        {"data": [valid, valid]},
        {"data": [{**valid, "slug": "connector-cli-journey-v2-000000000000000000000000"}]},
        {"data": [{**valid, "type": "url"}]},
        {"data": [{**valid, "status": "revoked"}]},
    ):
        methods: list[str] = []

        def fake_request(
            url: str,
            method: str,
            bearer: str | None = None,
            body: bytes | None = None,
            content_type: str | None = None,
            extra_headers: dict[str, str] | None = None,
        ) -> tuple[int, bytes]:
            del bearer, body, content_type, extra_headers
            parsed = urllib.parse.urlsplit(url)
            assert parsed.path == "/v1/resources"
            assert urllib.parse.parse_qs(parsed.query) == {"slug": [connector_id]}
            methods.append(method)
            return 200, json.dumps(response).encode()

        with mock.patch.object(credentials, "request", fake_request), mock.patch.object(
            credentials.time, "sleep", lambda _: None
        ):
            try:
                credentials.retry_connector_resource_delete("https://sandbox.example", "jwt", connector_id)
            except credentials.CredentialError:
                pass
            else:
                raise AssertionError(f"malformed Connector lookup succeeded: {response}")
        assert methods == ["GET"] * credentials.MAX_ATTEMPTS

    def empty_request(
        url: str,
        method: str,
        bearer: str | None = None,
        body: bytes | None = None,
        content_type: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        del url, bearer, body, content_type, extra_headers
        assert method == "GET"
        return 200, b'{"data":[]}'

    with mock.patch.object(credentials, "request", empty_request):
        assert not credentials.retry_connector_resource_delete(
            "https://sandbox.example", "jwt", connector_id
        )


def main() -> None:
    test_unbounded_valid_pagination()
    test_connector_cleanup_lookup_fails_closed()
    assert credentials.run_device_key_names(
        argparse.Namespace(run_id="1231", run_attempt="2", runtime="host")
    ) == {"agent:qurl-journey-v2-r1231-a2-hs", "agent:qurl-journey-v2-r1231-a2-hf"}
    assert credentials.run_device_key_names(
        argparse.Namespace(run_id="1231", run_attempt="2", runtime="hardened_container")
    ) == {"agent:qurl-journey-v2-r1231-a2-cs", "agent:qurl-journey-v2-r1231-a2-cf"}
    assert credentials.run_connector_ids(
        argparse.Namespace(run_id="1231", run_attempt="2", runtime="host", lane="linux")
    ) == {
        "connector-cli-journey-v2-415907f85f12d5ffd69c6a62",
        "connector-cli-journey-v2-87e091ca6623843507b5863b",
    }
    lane_identities = [
        argparse.Namespace(run_id="9001" + str(index), run_attempt="3", runtime="host", lane=lane)
        for index, lane in enumerate(("linux", "macos", "windows"), start=1)
    ]
    assert len({credentials.run_description(item) for item in lane_identities}) == 3
    assert credentials.run_credential_names(lane_identities[0]) == {
        "qurl CLI journey v2 90011/3/linux/primary",
        "qurl CLI journey v2 90011/3/linux/failure",
    }
    assert len(set().union(*(credentials.run_credential_names(item) for item in lane_identities))) == 6
    assert len(set().union(*(credentials.run_device_key_names(item) for item in lane_identities))) == 6
    assert len(set().union(*(credentials.run_connector_ids(item) for item in lane_identities))) == 6
    fake = FakeAPI()
    with tempfile.TemporaryDirectory() as raw_root, mock.patch.object(credentials, "request", fake), mock.patch.object(
        credentials.time, "sleep", lambda _: None
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        owner_output = root / "owner-id"
        credentials.identify(argparse.Namespace(**vars(args), output_file=owner_output))
        assert owner_output.read_text(encoding="utf-8") == fake.owner
        output = root / "credential"
        output.mkdir(mode=0o700)
        create_args = argparse.Namespace(
            **vars(args), output_dir=output, run_id="1231", run_attempt="2", lane="linux", purpose="primary"
        )
        credentials.create(create_args)
        assert {path.name for path in output.iterdir()} == {
            "api-key", "api-key-id", "cleanup-jwt", "owner-id", "run-name"
        }
        assert (output / "owner-id").read_text(encoding="utf-8") == fake.owner
        assert (output / "run-name").read_text(encoding="utf-8") == "qurl CLI journey v2 1231/2/linux/primary"
        fake.transient_key_delete = 1
        credentials.revoke(
            argparse.Namespace(qurl_endpoint=args.qurl_endpoint, credential_dir=output)
        )
        assert not output.exists()
        assert fake.deleted_keys.count(fake.key_id) == 1

        recovery = root / "recovery"
        recovery.mkdir(mode=0o700)
        credentials.write_private(recovery / "cleanup-jwt", fake.jwt)
        credentials.write_private(recovery / "run-name", "qurl CLI journey v2 1232/2/macos/failure")
        credentials.revoke_persisted(args.qurl_endpoint, recovery)
        assert (recovery / "api-key-id").read_text(encoding="utf-8") == fake.key_id

        run_key_id = "key_RunKey123456"
        failure_key_id = "key_FailKey12345"
        device_key_id = "key_DevRun123456"
        unrecorded_device_key_id = "key_DevFail12345"
        unrelated_key_id = "key_OtherKey1234"
        unrelated_device_key_id = "key_OtherDev1234"
        fake.keys[run_key_id] = {
            "key_id": run_key_id,
            "kind": "api_key",
            "name": "qurl CLI journey v2 1231/2/linux/primary",
            "scopes": credentials.CUSTOMER_SCOPES,
            "status": "active",
        }
        fake.keys[failure_key_id] = {
            "key_id": failure_key_id,
            "kind": "api_key",
            "name": "qurl CLI journey v2 1231/2/linux/failure",
            "scopes": credentials.CUSTOMER_SCOPES,
            "status": "active",
        }
        fake.keys[device_key_id] = {
            "key_id": device_key_id,
            "kind": "device",
            "name": "agent:qurl-journey-v2-r1231-a2-hs",
            "scopes": credentials.DEVICE_SCOPES,
            "status": "active",
        }
        fake.keys[unrecorded_device_key_id] = {
            "key_id": unrecorded_device_key_id,
            "kind": "device",
            "name": "agent:qurl-journey-v2-r1231-a2-hf",
            "scopes": credentials.DEVICE_SCOPES,
            "status": "active",
        }
        fake.keys[unrelated_key_id] = {
            "key_id": unrelated_key_id,
            "kind": "api_key",
            "name": "qurl CLI journey v2 9999/1/linux",
            "scopes": credentials.CUSTOMER_SCOPES,
            "status": "active",
        }
        fake.keys[unrelated_device_key_id] = {
            "key_id": unrelated_device_key_id,
            "kind": "device",
            "name": "agent:qurl-share-r9999-a1-hs",
            "scopes": credentials.DEVICE_SCOPES,
            "status": "active",
        }
        cleanup_ids = root / "cleanup-ids"
        cleanup_ids.mkdir(mode=0o700)
        device_digest = credentials.hashlib.sha256(device_key_id.encode("ascii")).hexdigest()
        private_file(cleanup_ids, "device-key-" + device_digest, device_key_id)
        fake.operations.clear()
        credentials.reconcile_run(
            argparse.Namespace(
                **vars(args),
                cleanup_id_dir=cleanup_ids,
                lane="linux",
                run_attempt="2",
                run_id="1231",
                runtime="host",
            )
        )
        expected_connector_resource_ids = {"r_connector_smoke", "r_connector_failure"}
        assert set(fake.deleted_resources) == expected_connector_resource_ids | {"r_customer_ci"}
        assert "r_keep" not in fake.deleted_resources
        assert "r_redacted_tunnel" not in fake.deleted_resources
        assert {run_key_id, failure_key_id, device_key_id, unrecorded_device_key_id}.issubset(fake.deleted_keys)
        assert unrelated_key_id not in fake.deleted_keys
        assert unrelated_device_key_id not in fake.deleted_keys
        first_revoke = next(index for index, operation in enumerate(fake.operations) if operation.startswith("revoke:"))
        assert all(
            fake.operations.index("delete:" + resource_id) < first_revoke
            for resource_id in expected_connector_resource_ids | {"r_customer_ci"}
        )

        malformed_device_key_id = "key_BadDevice123"
        fake.keys[malformed_device_key_id] = {
            "key_id": malformed_device_key_id,
            "kind": "api_key",
            "name": "agent:qurl-journey-v2-r1231-a2-hs",
            "scopes": credentials.DEVICE_SCOPES,
            "status": "active",
        }
        try:
            credentials.reconcile_run(
                argparse.Namespace(
                    **vars(args),
                    cleanup_id_dir=None,
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                )
            )
        except credentials.CredentialError as exc:
            assert "unexpected authority shape" in str(exc)
        else:
            raise AssertionError("malformed exact-name device credential was not rejected")
        assert malformed_device_key_id not in fake.deleted_keys

    print("qurl CLI journey credential tests passed")


if __name__ == "__main__":
    main()
