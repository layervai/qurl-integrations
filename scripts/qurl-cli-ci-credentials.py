#!/usr/bin/env python3
"""Create and revoke one short-lived qURL CLI CI credential."""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import pathlib
import re
import stat
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


MAX_RESPONSE = 64 * 1024
REQUIRED_M2M_SCOPES = frozenset({"qurl:agent", "qurl:read", "qurl:write"})
CUSTOMER_SCOPES = ["qurl:agent", "qurl:read", "qurl:resolve", "qurl:write"]
KEY_ID = re.compile(r"key_[A-Za-z0-9]{12}\Z")
POSITIVE_INTEGER = re.compile(r"[1-9][0-9]{0,19}\Z")
LANE = re.compile(r"(?:linux|macos|windows)\Z")
RUN_NAME = re.compile(r"qurl CLI CI [1-9][0-9]{0,19}/[1-9][0-9]{0,19}/(?:linux|macos|windows)\Z")
MAX_ATTEMPTS = 3
MAX_PAGES = 3
MAX_SWEEP_ITEMS = 250
RUN_KEY_PREFIX = "qurl CLI CI "
DEVICE_KEY_NAME = "qURL CLI registered device"
JOURNEY_DESCRIPTION = "qurl-integrations cli sandbox e2e journey (self-cleaning; safe to delete)"


class CredentialError(RuntimeError):
    """A credential operation failed closed."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001,ANN201
        return None


def private_value(path: pathlib.Path, label: str) -> str:
    before = path.lstat()
    unix_private = os.name == "nt" or (before.st_uid == os.getuid() and not before.st_mode & 0o077)
    if (
        not stat.S_ISREG(before.st_mode)
        or not unix_private
        or before.st_nlink != 1
        or before.st_size < 1
        or before.st_size > MAX_RESPONSE
    ):
        raise CredentialError(f"{label} is not one private regular file")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    with os.fdopen(descriptor, "rb") as handle:
        after = os.fstat(handle.fileno())
        if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
            raise CredentialError(f"{label} changed while opening")
        raw = handle.read(MAX_RESPONSE + 1)
    if len(raw) > MAX_RESPONSE:
        raise CredentialError(f"{label} exceeds its size limit")
    value = raw.decode("utf-8")
    if not value or value != value.strip() or "\r" in value or "\n" in value:
        raise CredentialError(f"{label} must contain one exact value")
    return value


def write_private(path: pathlib.Path, value: str) -> None:
    if not value or value != value.strip() or "\r" in value or "\n" in value:
        raise CredentialError("refusing to persist malformed credential material")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        handle.write(value)
        handle.flush()
        os.fsync(handle.fileno())


def https_origin(value: str, label: str) -> str:
    parsed = urllib.parse.urlsplit(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise CredentialError(f"{label} must be one HTTPS URL without credentials or fragments")
    return value.rstrip("/")


def request(
    url: str,
    method: str,
    bearer: str | None = None,
    body: bytes | None = None,
    content_type: str | None = None,
    extra_headers: dict[str, str] | None = None,
) -> tuple[int, bytes]:
    headers = {"Accept": "application/json"}
    if bearer:
        headers["Authorization"] = f"Bearer {bearer}"
    if content_type:
        headers["Content-Type"] = content_type
    if extra_headers:
        headers.update(extra_headers)
    operation = urllib.request.Request(url, data=body, method=method, headers=headers)
    try:
        response = urllib.request.build_opener(NoRedirect()).open(operation, timeout=20)
    except urllib.error.HTTPError as exc:
        response = exc
    except (OSError, urllib.error.URLError) as exc:
        raise CredentialError("credential HTTP request failed") from exc
    try:
        raw = response.read(MAX_RESPONSE + 1)
    finally:
        response.close()
    if len(raw) > MAX_RESPONSE:
        raise CredentialError("credential HTTP response exceeds its size limit")
    return response.status, raw


def json_object(raw: bytes, label: str) -> dict[str, Any]:
    def unique(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise CredentialError(f"{label} repeats a field")
            result[key] = value
        return result

    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=unique)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CredentialError(f"{label} is not valid JSON") from exc
    if not isinstance(value, dict):
        raise CredentialError(f"{label} is not a JSON object")
    return value


def jwt_claims(token: str) -> dict[str, Any]:
    parts = token.split(".")
    if len(parts) != 3:
        raise CredentialError("Auth0 access token is not a JWT")
    try:
        payload = base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4))
    except ValueError as exc:
        raise CredentialError("Auth0 access token payload is malformed") from exc
    return json_object(payload, "Auth0 access token payload")


def effective_scopes(claims: dict[str, Any]) -> frozenset[str]:
    values: list[str] = []
    scope = claims.get("scope", "")
    if not isinstance(scope, str):
        raise CredentialError("Auth0 access token scope is malformed")
    values.extend(filter(None, scope.split(" ")))
    for field in ("permissions", "https://layerv.ai/permissions"):
        permissions = claims.get(field, [])
        if not isinstance(permissions, list) or any(not isinstance(item, str) for item in permissions):
            raise CredentialError("Auth0 access token permissions are malformed")
        values.extend(permissions)
    return frozenset(values)


def auth0_token(args: argparse.Namespace) -> tuple[str, str]:
    client_id = private_value(args.client_id_file, "Auth0 client ID")
    client_secret = private_value(args.client_secret_file, "Auth0 client secret")
    form = urllib.parse.urlencode(
        {
            "audience": args.audience,
            "client_id": client_id,
            "client_secret": client_secret,
            "grant_type": "client_credentials",
            "scope": " ".join(sorted(REQUIRED_M2M_SCOPES)),
        }
    ).encode("ascii")
    status, raw = request(args.token_endpoint, "POST", body=form, content_type="application/x-www-form-urlencoded")
    if status != 200:
        raise CredentialError("Auth0 client credential was rejected")
    response = json_object(raw, "Auth0 token response")
    token = response.get("access_token")
    if response.get("token_type") != "Bearer" or not isinstance(token, str):
        raise CredentialError("Auth0 did not return one bearer token")
    claims = jwt_claims(token)
    now = int(time.time())
    parsed_endpoint = urllib.parse.urlsplit(args.token_endpoint)
    expected_issuer = f"{parsed_endpoint.scheme}://{parsed_endpoint.netloc}/"
    audience = claims.get("aud")
    if audience != args.audience and audience != [args.audience]:
        raise CredentialError("Auth0 token audience does not match the qURL API")
    if (
        claims.get("gty") != "client-credentials"
        or claims.get("sub") != f"{client_id}@clients"
        or claims.get("iss") != expected_issuer
    ):
        raise CredentialError("Auth0 token is not owned by the CI client")
    issued = claims.get("iat")
    expires = claims.get("exp")
    if (
        not isinstance(issued, int)
        or isinstance(issued, bool)
        or not isinstance(expires, int)
        or isinstance(expires, bool)
        or issued > now + 60
        or now - issued > 60
        or expires - now < 3600
        or expires - issued > 7200
    ):
        raise CredentialError("Auth0 token cannot cover the journey and cleanup")
    if effective_scopes(claims) != REQUIRED_M2M_SCOPES:
        raise CredentialError("Auth0 token does not have the exact CI scope set")
    return token, claims["sub"]


def qurl_json(
    endpoint: str,
    token: str,
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
    extra_headers: dict[str, str] | None = None,
) -> tuple[int, dict[str, Any]]:
    encoded = None
    content_type = None
    if body is not None:
        encoded = json.dumps(body, separators=(",", ":"), sort_keys=True).encode("utf-8")
        content_type = "application/json"
    status, raw = request(
        endpoint + path,
        method,
        bearer=token,
        body=encoded,
        content_type=content_type,
        extra_headers=extra_headers,
    )
    return status, json_object(raw, "qURL API response") if raw else {}


def identity(endpoint: str, token: str) -> dict[str, Any]:
    status, response = qurl_json(endpoint, token, "GET", "/v1/me")
    if status != 200 or not isinstance(response.get("data"), dict):
        raise CredentialError("qURL identity check failed")
    return response["data"]


def retry_revoke(endpoint: str, jwt: str, key_id: str) -> None:
    last_error: Exception | None = None
    for attempt in range(MAX_ATTEMPTS):
        try:
            status, _ = qurl_json(
                endpoint,
                jwt,
                "DELETE",
                "/v1/api-keys/" + urllib.parse.quote(key_id, safe=""),
            )
            if status in {200, 204, 404}:
                return
            if status != 429 and status < 500:
                raise CredentialError("qURL API-key revoke was rejected")
            last_error = CredentialError("qURL API-key revoke was temporarily rejected")
        except CredentialError as exc:
            last_error = exc
        if attempt + 1 < MAX_ATTEMPTS:
            time.sleep(attempt + 1)
    raise CredentialError("qURL API-key revoke did not converge after bounded retries") from last_error


def retry_resource_delete(endpoint: str, jwt: str, resource_id: str) -> None:
    if not resource_id or len(resource_id) > 512 or resource_id != resource_id.strip():
        raise CredentialError("resource cleanup ID is malformed")
    last_error: Exception | None = None
    for attempt in range(MAX_ATTEMPTS):
        try:
            status, _ = qurl_json(
                endpoint,
                jwt,
                "DELETE",
                "/v1/resources/" + urllib.parse.quote(resource_id, safe=""),
            )
            if status in {204, 404}:
                return
            if status != 429 and status < 500:
                raise CredentialError("qURL resource cleanup was rejected")
            last_error = CredentialError("qURL resource cleanup was temporarily rejected")
        except CredentialError as exc:
            last_error = exc
        if attempt + 1 < MAX_ATTEMPTS:
            time.sleep(attempt + 1)
    raise CredentialError("qURL resource cleanup did not converge after bounded retries") from last_error


def paged_rows(endpoint: str, jwt: str, path: str, label: str) -> list[dict[str, Any]]:
    cursor = ""
    seen: set[str] = set()
    result: list[dict[str, Any]] = []
    for _ in range(MAX_PAGES):
        query = {"limit": "100", "status": "active"}
        if cursor:
            query["cursor"] = cursor
        status, response = qurl_json(endpoint, jwt, "GET", path + "?" + urllib.parse.urlencode(query))
        rows = response.get("data")
        meta = response.get("meta")
        if status != 200 or not isinstance(rows, list) or not isinstance(meta, dict):
            raise CredentialError(f"{label} inventory was rejected")
        if any(not isinstance(row, dict) for row in rows):
            raise CredentialError(f"{label} inventory contains a malformed row")
        result.extend(rows)
        if len(result) > MAX_SWEEP_ITEMS:
            raise CredentialError(f"{label} inventory exceeded its item bound")
        has_more = meta.get("has_more", False)
        next_cursor = meta.get("next_cursor", "")
        if not isinstance(has_more, bool) or not isinstance(next_cursor, str) or has_more != bool(next_cursor):
            raise CredentialError(f"{label} inventory pagination is malformed")
        if not has_more:
            return result
        if next_cursor in seen:
            raise CredentialError(f"{label} inventory repeated a cursor")
        seen.add(next_cursor)
        cursor = next_cursor
    raise CredentialError(f"{label} inventory exceeded its page bound")


def sweep(args: argparse.Namespace) -> None:
    endpoint = https_origin(args.qurl_endpoint, "qURL endpoint")
    args.token_endpoint = https_origin(args.token_endpoint, "Auth0 token endpoint")
    jwt, expected_owner = auth0_token(args)
    m2m = identity(endpoint, jwt)
    if m2m.get("auth_type") != "jwt" or m2m.get("owner_id") != expected_owner:
        raise CredentialError("qURL rejected the dedicated CI owner")

    resources = paged_rows(endpoint, jwt, "/v1/resources", "qURL resource cleanup")
    resource_ids: list[str] = []
    for row in resources:
        target = row.get("target_url")
        description = row.get("description")
        owned_remote = (
            description == JOURNEY_DESCRIPTION
            and isinstance(target, str)
            and target.startswith("https://example.com/?qurl-private-sandbox-device-journey=")
        )
        # Management reads intentionally redact the connector slug. The M2M
        # owner is dedicated to this gate, so every active tunnel row under
        # that owner is disposable CI state.
        owned_connector = row.get("type") == "tunnel"
        if owned_remote or owned_connector:
            resource_id = row.get("resource_id")
            if not isinstance(resource_id, str):
                raise CredentialError("qURL resource cleanup row has no resource ID")
            resource_ids.append(resource_id)
    for resource_id in dict.fromkeys(resource_ids):
        retry_resource_delete(endpoint, jwt, resource_id)

    credentials = paged_rows(endpoint, jwt, "/v1/api-keys", "qURL credential cleanup")
    key_ids: list[str] = []
    for row in credentials:
        key_id = row.get("key_id")
        kind = row.get("kind")
        name = row.get("name")
        if not isinstance(key_id, str) or not KEY_ID.fullmatch(key_id):
            raise CredentialError("qURL credential cleanup row has a malformed key ID")
        if kind == "device" or (kind == "api_key" and isinstance(name, str) and name.startswith(RUN_KEY_PREFIX)):
            key_ids.append(key_id)
    for key_id in dict.fromkeys(key_ids):
        retry_revoke(endpoint, jwt, key_id)
    print(f"reconciled {len(resource_ids)} resources and {len(key_ids)} credentials")


def mint_ordinary_key(endpoint: str, jwt: str, name: str) -> tuple[str, str]:
    idempotency = "qurl-cli-ci-" + hashlib.sha256(name.encode("ascii")).hexdigest()
    body = {"kind": "api_key", "name": name, "scopes": CUSTOMER_SCOPES}
    last_error: Exception | None = None
    for attempt in range(MAX_ATTEMPTS):
        try:
            status, response = qurl_json(
                endpoint,
                jwt,
                "POST",
                "/v1/api-keys",
                body,
                {"Idempotency-Key": idempotency},
            )
            if status != 201:
                if status != 429 and status < 500:
                    raise CredentialError("qURL ordinary API-key creation was rejected")
                raise CredentialError("qURL ordinary API-key creation was temporarily rejected")
            data = response.get("data")
            if not isinstance(data, dict):
                raise CredentialError("qURL returned a malformed ordinary API key")
            key_id = data.get("key_id")
            api_key = data.get("api_key")
            if (
                not isinstance(key_id, str)
                or not KEY_ID.fullmatch(key_id)
                or not isinstance(api_key, str)
                or not api_key.startswith(("lv_test_", "lv_live_"))
                or data.get("kind") != "api_key"
                or data.get("name") != name
                or data.get("scopes") != CUSTOMER_SCOPES
                or data.get("status") != "active"
            ):
                raise CredentialError("qURL returned a malformed ordinary API key")
            return key_id, api_key
        except CredentialError as exc:
            last_error = exc
        if attempt + 1 < MAX_ATTEMPTS:
            time.sleep(attempt + 1)
    raise CredentialError("qURL ordinary API-key creation did not converge after bounded retries") from last_error


def prepare_output_directory(path: pathlib.Path) -> None:
    if not path.is_absolute() or path == pathlib.Path(path.anchor):
        raise CredentialError("credential directory must be an absolute non-root path")
    if path.exists():
        before = path.lstat()
        private = os.name == "nt" or (
            before.st_uid == os.getuid() and not before.st_mode & 0o077
        )
        if (
            not stat.S_ISDIR(before.st_mode)
            or stat.S_ISLNK(before.st_mode)
            or not private
            or any(path.iterdir())
        ):
            raise CredentialError("credential directory is not one empty private directory")
        return
    path.mkdir(mode=0o700)


def revoke_persisted(endpoint: str, directory: pathlib.Path) -> None:
    jwt = private_value(directory / "cleanup-jwt", "cleanup JWT")
    name = private_value(directory / "run-name", "run-scoped API-key name")
    if not RUN_NAME.fullmatch(name):
        raise CredentialError("run-scoped API-key name is malformed")
    key_id_path = directory / "api-key-id"
    if key_id_path.exists():
        key_id = private_value(key_id_path, "API key ID")
        if not KEY_ID.fullmatch(key_id):
            raise CredentialError("API key ID is malformed")
    else:
        # A create response can be lost after the service commits it. Repeat
        # the same idempotent POST to recover that exact key ID, then revoke it.
        key_id, _ = mint_ordinary_key(endpoint, jwt, name)
        write_private(key_id_path, key_id)
    retry_revoke(endpoint, jwt, key_id)


def create(args: argparse.Namespace) -> None:
    endpoint = https_origin(args.qurl_endpoint, "qURL endpoint")
    args.token_endpoint = https_origin(args.token_endpoint, "Auth0 token endpoint")
    if not POSITIVE_INTEGER.fullmatch(args.run_id) or not POSITIVE_INTEGER.fullmatch(args.run_attempt):
        raise CredentialError("run identity must use canonical positive integers")
    if not LANE.fullmatch(args.lane):
        raise CredentialError("platform lane is invalid")
    jwt, expected_owner = auth0_token(args)
    m2m = identity(endpoint, jwt)
    if m2m.get("auth_type") != "jwt" or m2m.get("owner_id") != expected_owner:
        raise CredentialError("qURL rejected the dedicated CI owner")

    name = f"qurl CLI CI {args.run_id}/{args.run_attempt}/{args.lane}"
    prepare_output_directory(args.output_dir)
    write_private(args.output_dir / "cleanup-jwt", jwt)
    write_private(args.output_dir / "run-name", name)
    try:
        key_id, api_key = mint_ordinary_key(endpoint, jwt, name)
        write_private(args.output_dir / "api-key-id", key_id)
        customer = identity(endpoint, api_key)
        customer_key = customer.get("api_key")
        if (
            customer.get("auth_type") != "api_key"
            or customer.get("owner_id") != expected_owner
            or not isinstance(customer_key, dict)
            or customer_key.get("key_id") != key_id
            or customer_key.get("kind") != "api_key"
            or customer_key.get("scopes") != CUSTOMER_SCOPES
        ):
            raise CredentialError("minted API key does not belong to the CI owner")
        write_private(args.output_dir / "api-key", api_key)
    except (OSError, CredentialError) as exc:
        try:
            revoke_persisted(endpoint, args.output_dir)
        except (OSError, CredentialError) as cleanup_exc:
            raise CredentialError(
                "credential creation failed and bounded revoke did not converge"
            ) from cleanup_exc
        raise CredentialError("credential creation failed; the exact key was revoked") from exc
    print("created one run-scoped customer API key")


def revoke(args: argparse.Namespace) -> None:
    endpoint = https_origin(args.qurl_endpoint, "qURL endpoint")
    if not args.credential_dir.exists():
        return
    jwt_path = args.credential_dir / "cleanup-jwt"
    name_path = args.credential_dir / "run-name"
    if not jwt_path.exists() and not name_path.exists():
        return
    if not jwt_path.exists() or not name_path.exists():
        raise CredentialError("credential cleanup state is incomplete")
    revoke_persisted(endpoint, args.credential_dir)
    for path in args.credential_dir.iterdir():
        if path.is_file() and not path.is_symlink():
            path.unlink()
    args.credential_dir.rmdir()
    print("revoked the run-scoped customer API key")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser()
    commands = result.add_subparsers(dest="command", required=True)
    create_parser = commands.add_parser("create")
    for current in (create_parser, commands.add_parser("sweep")):
        current.add_argument("--token-endpoint", required=True)
        current.add_argument("--audience", required=True)
        current.add_argument("--qurl-endpoint", required=True)
        current.add_argument("--client-id-file", type=pathlib.Path, required=True)
        current.add_argument("--client-secret-file", type=pathlib.Path, required=True)
    sweep_parser = commands.choices["sweep"]
    sweep_parser.set_defaults(handler=sweep)
    create_parser.add_argument("--output-dir", type=pathlib.Path, required=True)
    create_parser.add_argument("--run-id", required=True)
    create_parser.add_argument("--run-attempt", required=True)
    create_parser.add_argument("--lane", required=True)
    create_parser.set_defaults(handler=create)
    revoke_parser = commands.add_parser("revoke")
    revoke_parser.add_argument("--qurl-endpoint", required=True)
    revoke_parser.add_argument("--credential-dir", type=pathlib.Path, required=True)
    revoke_parser.set_defaults(handler=revoke)
    return result


def main() -> int:
    try:
        args = parser().parse_args()
        args.handler(args)
    except (CredentialError, OSError, UnicodeError) as exc:
        print(f"credential operation failed: {exc}", file=os.sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
