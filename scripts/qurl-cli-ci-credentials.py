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
# TODO(upstream-contract): the CI Auth0 tenant issues one-hour M2M tokens.
AUTH0_M2M_TOKEN_LIFETIME_SECONDS = 60 * 60
AUTH0_ISSUANCE_SKEW_SECONDS = 60
# An M2M token is consumed only by one trusted setup or reconciliation command.
# The setup command removes it before the customer journey starts. A cleanup
# command reuses one token for its bounded batch and never exposes that token to
# a customer process. The lifetime is independent of the 110-minute soak lane.
M2M_MANAGEMENT_HEADROOM_SECONDS = 40 * 60
JOURNEY_CLEANUP_MARGIN_SECONDS = 15 * 60
MIN_M2M_TOKEN_REMAINING_SECONDS = (
    M2M_MANAGEMENT_HEADROOM_SECONDS + JOURNEY_CLEANUP_MARGIN_SECONDS
)
CUSTOMER_SCOPES = ["qurl:agent", "qurl:read", "qurl:resolve", "qurl:write"]
DEVICE_SCOPES = ["qurl:read", "qurl:resolve", "qurl:write"]
KEY_ID = re.compile(r"key_[A-Za-z0-9]{12}\Z")
POSITIVE_INTEGER = re.compile(r"[1-9][0-9]{0,19}\Z")
LANE = re.compile(r"(?:linux|macos|windows)\Z")
RUN_NAME = re.compile(
    r"qurl CLI journey v2 [1-9][0-9]{0,19}/[1-9][0-9]{0,19}/"
    r"(?:linux|macos|windows)/(?:primary|failure)\Z"
)
CLEANUP_ID_FILE = re.compile(r"device-key-[0-9a-f]{64}\Z")
RUN_CONNECTOR_ID = re.compile(r"connector-cli-journey-v2-[0-9a-f]{24}\Z")
RUN_AGENT_ID = re.compile(
    r"qurl-journey-v2-r[1-9][0-9]{0,19}-a[1-9][0-9]{0,19}-[hc][sf]\Z"
)
RUN_SPEC = re.compile(
    r"([1-9][0-9]{0,19}):([1-9][0-9]{0,19}):"
    r"(linux|macos|windows):(host|hardened_container)\Z"
)
MAX_RECONCILE_RUNS = 12
MAX_ATTEMPTS = 3
# Bound the trusted cleanup inventory independently of per-request timeouts so
# malformed pagination cannot hold a runner or grow its in-memory result set.
# TODO(upstream-contract): qurl-service currently honors a 100-row maximum,
# retains revoked API keys for 30 days, and hard-deletes resources. Recalibrate
# this dedicated-CI-owner bound if any of those service contracts change.
INVENTORY_PAGE_SIZE = 100
INVENTORY_MAX_PAGES = 20
INVENTORY_MAX_ROWS = INVENTORY_PAGE_SIZE * INVENTORY_MAX_PAGES
# One absolute deadline covers every inventory scan in one reconciliation. The
# primary cleanup batches four runs and the fallback batches at most twelve.
# Two minutes per run leaves bounded time for cleanup writes in both jobs.
RECONCILE_INVENTORY_BUDGET_SECONDS = 2 * 60
# A slow credential inventory must not consume the whole shared deadline before
# the final resource inventory can attempt cleanup. This is a reservation inside
# the two-minute total, not a second independent budget.
RESOURCE_INVENTORY_RESERVE_SECONDS = 30


class CredentialError(RuntimeError):
    """A credential operation failed closed."""


class InventoryBoundError(CredentialError):
    """A trusted cleanup inventory exceeded a runner-safety bound."""


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
        or issued > now + AUTH0_ISSUANCE_SKEW_SECONDS
        or now - issued > AUTH0_ISSUANCE_SKEW_SECONDS
        or expires - now < MIN_M2M_TOKEN_REMAINING_SECONDS
        or expires - issued > 2 * AUTH0_M2M_TOKEN_LIFETIME_SECONDS
    ):
        raise CredentialError("Auth0 token does not have the required CI management lifetime")
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


def retry_assignment_retire(endpoint: str, jwt: str, agent_id: str) -> None:
    if not RUN_AGENT_ID.fullmatch(agent_id):
        raise CredentialError("run Connector assignment ID is malformed")
    last_error: Exception | None = None
    for attempt in range(MAX_ATTEMPTS):
        try:
            status, _ = qurl_json(
                endpoint,
                jwt,
                "DELETE",
                "/v1/connectors/agents/"
                + urllib.parse.quote(agent_id, safe="")
                + "/assignment",
            )
        except CredentialError as exc:
            last_error = exc
        else:
            if status == 204:
                return
            # qurl-service reserves 500 for a durable invariant failure and
            # 503 for a retryable store failure. Do not retry durable faults.
            if status not in {429, 502, 503, 504}:
                raise CredentialError("qURL Connector assignment retirement was rejected")
            last_error = CredentialError(
                "qURL Connector assignment retirement was temporarily rejected"
            )
        if attempt + 1 < MAX_ATTEMPTS:
            time.sleep(attempt + 1)
    raise CredentialError(
        "qURL Connector assignment retirement did not converge after bounded retries"
    ) from last_error


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


def retry_connector_resource_delete(endpoint: str, jwt: str, connector_id: str) -> bool:
    if not RUN_CONNECTOR_ID.fullmatch(connector_id):
        raise CredentialError("run Connector cleanup ID is malformed")
    last_error: Exception | None = None
    resource_id = ""
    for attempt in range(MAX_ATTEMPTS):
        try:
            query = urllib.parse.urlencode({"slug": connector_id})
            status, response = qurl_json(endpoint, jwt, "GET", "/v1/resources?" + query)
            if status != 200:
                if status != 429 and status < 500:
                    raise CredentialError("qURL Connector cleanup lookup was rejected")
                raise CredentialError("qURL Connector cleanup lookup was temporarily rejected")
            rows = response.get("data")
            if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows):
                raise CredentialError("qURL Connector cleanup lookup is malformed")
            if not rows:
                return False
            if len(rows) != 1:
                raise CredentialError("qURL Connector cleanup lookup is ambiguous")
            row = rows[0]
            resource_id = row.get("resource_id", "")
            if (
                not isinstance(resource_id, str)
                or not resource_id
                or resource_id != resource_id.strip()
                or len(resource_id) > 512
                or row.get("slug") != connector_id
                or row.get("type") != "tunnel"
                or row.get("status") != "active"
            ):
                raise CredentialError("qURL Connector cleanup row has an unexpected shape")
            break
        except CredentialError as exc:
            last_error = exc
        if attempt + 1 < MAX_ATTEMPTS:
            time.sleep(attempt + 1)
    else:
        raise CredentialError("qURL Connector cleanup lookup did not converge after bounded retries") from last_error
    retry_resource_delete(endpoint, jwt, resource_id)
    return True


def paged_rows(
    endpoint: str,
    jwt: str,
    path: str,
    label: str,
    status_filter: str | None = "active",
    *,
    deadline: float,
) -> list[dict[str, Any]]:
    cursor = ""
    seen: set[str] = set()
    result: list[dict[str, Any]] = []
    pages = 0
    while True:
        if pages >= INVENTORY_MAX_PAGES:
            raise InventoryBoundError(f"{label} inventory exceeded its page limit")
        if time.monotonic() >= deadline:
            raise InventoryBoundError(f"{label} inventory exceeded its time limit")
        query = {"limit": str(INVENTORY_PAGE_SIZE)}
        if status_filter is not None:
            query["status"] = status_filter
        if cursor:
            query["cursor"] = cursor
        status, response = qurl_json(endpoint, jwt, "GET", path + "?" + urllib.parse.urlencode(query))
        pages += 1
        if time.monotonic() >= deadline:
            raise InventoryBoundError(f"{label} inventory exceeded its time limit")
        rows = response.get("data")
        meta = response.get("meta")
        if status != 200 or not isinstance(rows, list) or not isinstance(meta, dict):
            raise CredentialError(f"{label} inventory was rejected")
        if any(not isinstance(row, dict) for row in rows):
            raise CredentialError(f"{label} inventory contains a malformed row")
        if len(result) + len(rows) > INVENTORY_MAX_ROWS:
            raise InventoryBoundError(f"{label} inventory exceeded its row limit")
        result.extend(rows)
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


def authenticated_owner(args: argparse.Namespace) -> tuple[str, str, str]:
    endpoint = https_origin(args.qurl_endpoint, "qURL endpoint")
    args.token_endpoint = https_origin(args.token_endpoint, "Auth0 token endpoint")
    jwt, expected_owner = auth0_token(args)
    m2m = identity(endpoint, jwt)
    if m2m.get("auth_type") != "jwt" or m2m.get("owner_id") != expected_owner:
        raise CredentialError("qURL rejected the dedicated CI owner")
    return endpoint, jwt, expected_owner


def identify(args: argparse.Namespace) -> None:
    _, _, expected_owner = authenticated_owner(args)
    if not args.output_file.is_absolute() or args.output_file == pathlib.Path(args.output_file.anchor):
        raise CredentialError("owner output must be an absolute non-root path")
    write_private(args.output_file, expected_owner)
    print("verified one dedicated CI owner")


def run_description(args: argparse.Namespace) -> str:
    if not POSITIVE_INTEGER.fullmatch(args.run_id) or not POSITIVE_INTEGER.fullmatch(args.run_attempt):
        raise CredentialError("run identity must use canonical positive integers")
    if not LANE.fullmatch(args.lane):
        raise CredentialError("platform lane is invalid")
    if args.runtime not in {"host", "hardened_container"}:
        raise CredentialError("journey runtime is invalid")
    return f"qurl CLI journey v2 resource {args.run_id}/{args.run_attempt}/{args.runtime}"


def run_credential_names(args: argparse.Namespace) -> set[str]:
    run_description(args)
    return {
        run_credential_name(args.run_id, args.run_attempt, args.lane, purpose)
        for purpose in ("primary", "failure")
    }


def run_credential_name(run_id: str, run_attempt: str, lane: str, purpose: str) -> str:
    return f"qurl CLI journey v2 {run_id}/{run_attempt}/{lane}/{purpose}"


def run_agent_ids(args: argparse.Namespace) -> set[str]:
    # TODO(upstream-contract): qurl-service stores each native device key as
    # "agent:" + AgentID, and the tagged harness derives AgentID from this
    # exact run/attempt/runtime plus its smoke or controlled-failure phase.
    # Assignment retirement returns 204 for both a committed release and a
    # missing assignment; 404 means the required route is not deployed.
    # Keep both sides in lockstep so a lost runner remains exactly cleanable
    # without moving a bearer credential between jobs.
    runtime_code = {"host": "h", "hardened_container": "c"}[args.runtime]
    return {
        f"qurl-journey-v2-r{args.run_id}-a{args.run_attempt}-{runtime_code}{label_code}"
        for label_code in ("s", "f")
    }


def run_device_key_names(args: argparse.Namespace) -> set[str]:
    return {"agent:" + agent_id for agent_id in run_agent_ids(args)}


def run_connector_ids(args: argparse.Namespace) -> set[str]:
    # The protected journey creates exactly one normal and one controlled-
    # failure Connector. Derive their IDs from the same public test inputs as
    # the Go harness so the trusted controller can remove either resource even
    # if the runner stops before it records a CRID or resource ID.
    run_description(args)
    result: set[str] = set()
    for label in ("smoke", "failure"):
        material = "\x00".join(
            ("qurl-cli-journey-v2", args.run_id, args.run_attempt, args.runtime, label)
        ).encode("utf-8")
        result.add("connector-cli-journey-v2-" + hashlib.sha256(material).hexdigest()[:24])
    return result


def cleanup_device_key_ids(path: pathlib.Path | None) -> set[str]:
    if path is None or not path.exists():
        return set()
    if not path.is_absolute() or path == pathlib.Path(path.anchor):
        raise CredentialError("cleanup ID directory must be an absolute non-root path")
    before = path.lstat()
    private = os.name == "nt" or (before.st_uid == os.getuid() and not before.st_mode & 0o077)
    if not stat.S_ISDIR(before.st_mode) or stat.S_ISLNK(before.st_mode) or not private:
        raise CredentialError("cleanup ID directory is not one private directory")
    entries = list(path.iterdir())
    if len(entries) > 16:
        raise CredentialError("cleanup ID directory exceeds its file limit")
    result: set[str] = set()
    for entry in entries:
        if not CLEANUP_ID_FILE.fullmatch(entry.name):
            raise CredentialError("cleanup ID directory contains an unexpected entry")
        key_id = private_value(entry, "device API key ID")
        if not KEY_ID.fullmatch(key_id):
            raise CredentialError("cleanup device API key ID is malformed")
        digest = hashlib.sha256(key_id.encode("ascii")).hexdigest()
        if entry.name != "device-key-" + digest:
            raise CredentialError("cleanup device API key ID filename does not match its value")
        result.add(key_id)
    return result


def reconcile_run(
    args: argparse.Namespace,
    authenticated: tuple[str, str] | None = None,
) -> None:
    description = run_description(args)
    credential_names = run_credential_names(args)
    device_key_names = run_device_key_names(args)
    connector_ids = run_connector_ids(args)
    if authenticated is None:
        endpoint, jwt, _ = authenticated_owner(args)
    else:
        endpoint, jwt = authenticated
    inventory_deadline = time.monotonic() + RECONCILE_INVENTORY_BUDGET_SECONDS
    credential_inventory_deadline = (
        inventory_deadline - RESOURCE_INVENTORY_RESERVE_SECONDS
    )

    failures: dict[str, int] = {}

    def record_failure(category: str) -> None:
        failures[category] = failures.get(category, 0) + 1

    # Revoke run credentials before resource cleanup. The trusted M2M token,
    # not a customer or device key, authorizes every resource deletion below.
    # Validate each exact target before deletion, attempt every valid target,
    # and retain only redacted failure categories for the final error.
    key_ids: list[str] = []
    try:
        device_key_ids = cleanup_device_key_ids(args.cleanup_id_dir)
        credentials = paged_rows(
            endpoint,
            jwt,
            "/v1/api-keys",
            "qURL credential cleanup",
            deadline=credential_inventory_deadline,
        )
        seen_device_ids: set[str] = set()
        for row in credentials:
            row_name = row.get("name")
            key_id = row.get("key_id")
            if (
                not isinstance(key_id, str)
                or not KEY_ID.fullmatch(key_id)
                or not isinstance(row_name, str)
            ):
                record_failure("credential_shape")
                continue
            is_customer = row_name in credential_names
            is_device = key_id in device_key_ids or row_name in device_key_names
            if not is_customer and not is_device:
                continue
            if is_customer:
                if row.get("kind") != "api_key" or row.get("scopes") != CUSTOMER_SCOPES:
                    record_failure("credential_shape")
                else:
                    key_ids.append(key_id)
            if is_device:
                if (
                    row.get("kind") != "device"
                    or row_name not in device_key_names
                    or row.get("scopes") != DEVICE_SCOPES
                ):
                    record_failure("credential_shape")
                else:
                    seen_device_ids.add(key_id)
                    key_ids.append(key_id)
        # A device key that the harness already revoked is correctly absent
        # from the active inventory. Any still-active recorded key must be
        # visible and have the exact device scope set before deletion.
        missing_device_ids = device_key_ids - seen_device_ids
        if missing_device_ids:
            all_credentials = paged_rows(
                endpoint,
                jwt,
                "/v1/api-keys",
                "qURL revoked credential cleanup",
                status_filter=None,
                deadline=credential_inventory_deadline,
            )
            revoked_ids: set[str] = set()
            for row in all_credentials:
                key_id = row.get("key_id")
                row_name = row.get("name")
                is_recorded = isinstance(key_id, str) and key_id in missing_device_ids
                is_named_device = isinstance(row_name, str) and row_name in device_key_names
                if not is_recorded and not is_named_device:
                    continue
                if (
                    not isinstance(key_id, str)
                    or not KEY_ID.fullmatch(key_id)
                    or not isinstance(row_name, str)
                    or row.get("kind") != "device"
                    or row.get("scopes") != DEVICE_SCOPES
                ):
                    record_failure("credential_shape")
                    continue
                if row.get("status") == "revoked":
                    revoked_ids.add(key_id)
            if not missing_device_ids.issubset(revoked_ids):
                record_failure("credential_inventory")
    except InventoryBoundError:
        record_failure("credential_inventory_bound")
    except (CredentialError, OSError, UnicodeError):
        record_failure("credential_inventory")

    unique_key_ids = sorted(set(key_ids))
    for key_id in unique_key_ids:
        try:
            retry_revoke(endpoint, jwt, key_id)
        except CredentialError:
            record_failure("credential_revoke")

    # Revocation returns the device-key quota. Retirement then removes the
    # exact owner-scoped assignment and returns its separate assignment slot.
    # Both identities are deterministic, so cancellation cleanup can finish
    # even when a runner stopped before it recorded local state.
    for agent_id in sorted(run_agent_ids(args)):
        try:
            retry_assignment_retire(endpoint, jwt, agent_id)
        except CredentialError:
            record_failure("assignment_retire")

    # Resource cleanup still runs after every credential outcome. Resource
    # inventory intentionally redacts Connector descriptions and tags, so the
    # deterministic Connector IDs recover them after abrupt runner loss.
    connector_resources = 0
    for connector_id in sorted(connector_ids):
        try:
            if retry_connector_resource_delete(endpoint, jwt, connector_id):
                connector_resources += 1
        except CredentialError:
            record_failure("connector_resource")

    # Remote-URL resources remain identifiable by the exact run description.
    resource_ids: list[str] = []
    try:
        resources = paged_rows(
            endpoint,
            jwt,
            "/v1/resources",
            "qURL resource cleanup",
            status_filter=None,
            deadline=inventory_deadline,
        )
        for row in resources:
            if row.get("description") != description:
                continue
            resource_id = row.get("resource_id")
            if not isinstance(resource_id, str):
                record_failure("resource_shape")
                continue
            resource_ids.append(resource_id)
    except InventoryBoundError:
        record_failure("resource_inventory_bound")
    except CredentialError:
        record_failure("resource_inventory")
    unique_resource_ids = list(dict.fromkeys(resource_ids))
    for resource_id in unique_resource_ids:
        try:
            retry_resource_delete(endpoint, jwt, resource_id)
        except CredentialError:
            record_failure("resource_delete")

    if failures:
        summary = ", ".join(f"{category}={count}" for category, count in sorted(failures.items()))
        raise CredentialError(f"run cleanup did not converge ({summary})")

    # Cancellation cleanup can start before zero, one, or both ordinary keys
    # exist. Reconcile every matching key, but do not require a fixed count.
    print(
        f"reconciled {connector_resources + len(unique_resource_ids)} run resources "
        f"and {len(unique_key_ids)} run credentials"
    )


def reconcile_batch(args: argparse.Namespace) -> None:
    if not 1 <= len(args.run_spec) <= MAX_RECONCILE_RUNS:
        raise CredentialError("reconciliation batch exceeds its run limit")
    if len(set(args.run_spec)) != len(args.run_spec):
        raise CredentialError("reconciliation batch contains a duplicate run")

    parsed: list[argparse.Namespace] = []
    for value in args.run_spec:
        match = RUN_SPEC.fullmatch(value)
        if match is None:
            raise CredentialError("reconciliation run specification is malformed")
        run_id, run_attempt, lane, runtime = match.groups()
        parsed.append(
            argparse.Namespace(
                run_id=run_id,
                run_attempt=run_attempt,
                lane=lane,
                runtime=runtime,
                cleanup_id_dir=None,
            )
        )

    endpoint, jwt, _ = authenticated_owner(args)
    failures = 0
    for run in parsed:
        try:
            reconcile_run(run, authenticated=(endpoint, jwt))
        except CredentialError:
            failures += 1
    if failures:
        raise CredentialError(
            f"batch cleanup did not converge for {failures} of {len(parsed)} runs"
        )
    print(f"reconciled {len(parsed)} runs with one management token")


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


def revoke_named_credential(
    endpoint: str,
    jwt: str,
    name: str,
    key_id_path: pathlib.Path,
) -> None:
    if not RUN_NAME.fullmatch(name):
        raise CredentialError("run-scoped API-key name is malformed")
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


def revoke_persisted(endpoint: str, directory: pathlib.Path) -> None:
    jwt = private_value(directory / "cleanup-jwt", "cleanup JWT")
    name = private_value(directory / "run-name", "run-scoped API-key name")
    revoke_named_credential(endpoint, jwt, name, directory / "api-key-id")


def validate_create_args(args: argparse.Namespace) -> None:
    if not POSITIVE_INTEGER.fullmatch(args.run_id) or not POSITIVE_INTEGER.fullmatch(args.run_attempt):
        raise CredentialError("run identity must use canonical positive integers")
    if not LANE.fullmatch(args.lane):
        raise CredentialError("platform lane is invalid")
    if args.purpose not in {"primary", "failure"}:
        raise CredentialError("customer credential purpose is invalid")


def create_with_auth(
    args: argparse.Namespace,
    endpoint: str,
    jwt: str,
    expected_owner: str,
) -> None:
    validate_create_args(args)
    name = run_credential_name(args.run_id, args.run_attempt, args.lane, args.purpose)
    prepare_output_directory(args.output_dir)
    write_private(args.output_dir / "cleanup-jwt", jwt)
    write_private(args.output_dir / "run-name", name)
    write_private(args.output_dir / "owner-id", expected_owner)
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


def create(args: argparse.Namespace) -> None:
    validate_create_args(args)
    endpoint, jwt, expected_owner = authenticated_owner(args)
    create_with_auth(args, endpoint, jwt, expected_owner)


def create_pair(args: argparse.Namespace) -> None:
    for purpose in ("primary", "failure"):
        validate_create_args(
            argparse.Namespace(
                run_id=args.run_id,
                run_attempt=args.run_attempt,
                lane=args.lane,
                purpose=purpose,
            )
        )
    for path in (args.primary_output_dir, args.failure_output_dir):
        if not path.is_absolute() or path == pathlib.Path(path.anchor):
            raise CredentialError("credential directory must be an absolute non-root path")
    primary_path = args.primary_output_dir.resolve()
    failure_path = args.failure_output_dir.resolve()
    if (
        primary_path == failure_path
        or primary_path in failure_path.parents
        or failure_path in primary_path.parents
    ):
        raise CredentialError("customer credential directories must be distinct")
    endpoint, jwt, expected_owner = authenticated_owner(args)
    directories = {
        "primary": args.primary_output_dir,
        "failure": args.failure_output_dir,
    }
    try:
        for purpose, output_dir in directories.items():
            create_with_auth(
                argparse.Namespace(
                    run_id=args.run_id,
                    run_attempt=args.run_attempt,
                    lane=args.lane,
                    purpose=purpose,
                    output_dir=output_dir,
                ),
                endpoint,
                jwt,
                expected_owner,
            )
        primary_id = private_value(
            args.primary_output_dir / "api-key-id", "primary API key ID"
        )
        failure_id = private_value(
            args.failure_output_dir / "api-key-id", "failure API key ID"
        )
        primary_key = private_value(
            args.primary_output_dir / "api-key", "primary API key"
        )
        failure_key = private_value(
            args.failure_output_dir / "api-key", "failure API key"
        )
        if primary_id == failure_id or primary_key == failure_key:
            raise CredentialError("customer credentials are not isolated")
    except (OSError, CredentialError) as exc:
        cleanup_failed = False
        for purpose, directory in directories.items():
            try:
                name = run_credential_name(
                    args.run_id, args.run_attempt, args.lane, purpose
                )
                revoke_named_credential(
                    endpoint, jwt, name, directory / "api-key-id"
                )
            except (OSError, CredentialError):
                cleanup_failed = True
        if cleanup_failed:
            raise CredentialError(
                "credential-pair creation failed and bounded revoke did not converge"
            ) from exc
        raise CredentialError(
            "credential-pair creation failed; every exact key was revoked"
        ) from exc
    print("created two isolated run-scoped customer API keys with one management token")


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
    create_pair_parser = commands.add_parser("create-pair")
    identify_parser = commands.add_parser("identify")
    reconcile_parser = commands.add_parser("reconcile-run")
    reconcile_batch_parser = commands.add_parser("reconcile-batch")
    for current in (
        create_parser,
        create_pair_parser,
        identify_parser,
        reconcile_parser,
        reconcile_batch_parser,
    ):
        current.add_argument("--token-endpoint", required=True)
        current.add_argument("--audience", required=True)
        current.add_argument("--qurl-endpoint", required=True)
        current.add_argument("--client-id-file", type=pathlib.Path, required=True)
        current.add_argument("--client-secret-file", type=pathlib.Path, required=True)
    identify_parser.add_argument("--output-file", type=pathlib.Path, required=True)
    identify_parser.set_defaults(handler=identify)
    create_parser.add_argument("--output-dir", type=pathlib.Path, required=True)
    create_parser.add_argument("--run-id", required=True)
    create_parser.add_argument("--run-attempt", required=True)
    create_parser.add_argument("--lane", required=True)
    create_parser.add_argument("--purpose", choices=("primary", "failure"), required=True)
    create_parser.set_defaults(handler=create)
    create_pair_parser.add_argument(
        "--primary-output-dir", type=pathlib.Path, required=True
    )
    create_pair_parser.add_argument(
        "--failure-output-dir", type=pathlib.Path, required=True
    )
    create_pair_parser.add_argument("--run-id", required=True)
    create_pair_parser.add_argument("--run-attempt", required=True)
    create_pair_parser.add_argument("--lane", required=True)
    create_pair_parser.set_defaults(handler=create_pair)
    reconcile_parser.add_argument("--run-id", required=True)
    reconcile_parser.add_argument("--run-attempt", required=True)
    reconcile_parser.add_argument("--lane", required=True)
    reconcile_parser.add_argument("--runtime", required=True)
    reconcile_parser.add_argument("--cleanup-id-dir", type=pathlib.Path)
    reconcile_parser.set_defaults(handler=reconcile_run)
    reconcile_batch_parser.add_argument(
        "--run-spec", action="append", required=True
    )
    reconcile_batch_parser.set_defaults(handler=reconcile_batch)
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
