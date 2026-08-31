#!/usr/bin/env python3
"""Hermetic tests for the trusted CLI customer-credential controller."""

from __future__ import annotations

import argparse
import base64
import importlib.util
import json
import os
import pathlib
import tempfile
import time
import urllib.parse
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("qurl-cli-ci-credentials.py")
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
                "description": credentials.JOURNEY_DESCRIPTION,
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
        self.deleted_keys: list[str] = []
        self.deleted_resources: list[str] = []
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
                    "name": credentials.DEVICE_KEY_NAME,
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
            self.keys.pop(key_id, None)
            return 204, b""
        if parsed.path == "/v1/resources" and method == "GET":
            return 200, json.dumps({"data": self.resources, "meta": {"has_more": False}}).encode()
        if parsed.path.startswith("/v1/resources/") and method == "DELETE":
            self.deleted_resources.append(urllib.parse.unquote(parsed.path.rsplit("/", 1)[1]))
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


def main() -> None:
    fake = FakeAPI()
    with tempfile.TemporaryDirectory() as raw_root, mock.patch.object(credentials, "request", fake), mock.patch.object(
        credentials.time, "sleep", lambda _: None
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        output = root / "credential"
        output.mkdir(mode=0o700)
        create_args = argparse.Namespace(
            **vars(args), output_dir=output, run_id="1231", run_attempt="2", lane="linux"
        )
        credentials.create(create_args)
        assert {path.name for path in output.iterdir()} == {
            "api-key", "api-key-id", "cleanup-jwt", "run-name"
        }
        fake.transient_key_delete = 1
        credentials.revoke(
            argparse.Namespace(qurl_endpoint=args.qurl_endpoint, credential_dir=output)
        )
        assert not output.exists()
        assert fake.deleted_keys.count(fake.key_id) == 1

        recovery = root / "recovery"
        recovery.mkdir(mode=0o700)
        credentials.write_private(recovery / "cleanup-jwt", fake.jwt)
        credentials.write_private(recovery / "run-name", "qurl CLI CI 1232/2/macos")
        credentials.revoke_persisted(args.qurl_endpoint, recovery)
        assert (recovery / "api-key-id").read_text(encoding="utf-8") == fake.key_id

        fake.keys[fake.key_id] = {
            "key_id": fake.key_id,
            "kind": "api_key",
            "name": "qurl CLI CI 1233/2/windows",
            "status": "active",
        }
        credentials.sweep(args)
        assert "r_customer_ci" in fake.deleted_resources
        assert "r_redacted_tunnel" in fake.deleted_resources
        assert "r_keep" not in fake.deleted_resources
        assert "key_Device123456" in fake.deleted_keys
        assert "key_Unrelated123" not in fake.deleted_keys

    print("qurl CLI CI credential tests passed")


if __name__ == "__main__":
    main()
