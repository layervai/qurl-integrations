#!/usr/bin/env python3
"""Hermetic tests for the trusted CLI customer-credential controller."""

from __future__ import annotations

import argparse
import base64
import contextlib
import importlib.util
import io
import json
import pathlib
import re
import sys
import tempfile
import time
import urllib.parse
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("qurl-cli-ci-credentials.py")
CLI_WORKFLOW = SCRIPT.parent.parent / ".github" / "workflows" / "cli.yml"
CUSTOMER_CLEANUP_WORKFLOW = (
    SCRIPT.parent.parent / ".github" / "workflows" / "qurl-cli-customer-cleanup.yml"
)
RELEASE_GATE = SCRIPT.with_name("check-exact-cli-release-gate.sh")
RELEASE_WORKFLOW = SCRIPT.parent.parent / ".github" / "workflows" / "release-please.yml"
sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("qurl_cli_ci_credentials", SCRIPT)
assert SPEC and SPEC.loader
credentials = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = credentials
SPEC.loader.exec_module(credentials)


def encoded(value: dict[str, object]) -> str:
    raw = json.dumps(value, separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


class FakeAPI:
    def __init__(self) -> None:
        now = int(time.time())
        self.owner = "ci-client@clients"
        self.jwt = (
            "header."
            + encoded(
                {
                    "aud": "https://sandbox.example",
                    "exp": now + 4000,
                    "gty": "client-credentials",
                    "iat": now,
                    "iss": "https://auth.example/",
                    "scope": "qurl:agent qurl:read qurl:write",
                    "sub": self.owner,
                }
            )
            + ".signature"
        )
        self.key_id = "key_AbCdEf123456"
        self.api_key = "lv_test_customer-key"
        self.auth_token_requests = 0
        self.api_key_inventory_requests = 0
        self.resource_inventory_requests = 0
        self.retain_revoked_keys = False
        self.issued_api_keys: dict[str, tuple[str, str]] = {}
        self.keys: dict[str, dict[str, object]] = {}
        self.extra_credentials: list[dict[str, object]] = []
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
        self.assignments = {
            "qurl-journey-v2-r1231-a2-hs",
            "qurl-journey-v2-r1231-a2-hf",
        }
        self.retired_assignments: list[str] = []
        self.deleted_resources: list[str] = []
        self.operations: list[str] = []
        self.transient_key_delete = 0
        self.failed_key_deletes: set[str] = set()
        self.failed_assignment_retires: set[str] = set()
        self.assignment_retire_statuses: dict[str, int] = {}
        self.failed_resource_deletes: set[str] = set()
        self.key_delete_attempts: list[str] = []
        self.assignment_retire_attempts: list[str] = []
        self.resource_delete_attempts: list[str] = []

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
            self.auth_token_requests += 1
            form = urllib.parse.parse_qs((body or b"").decode())
            assert method == "POST"
            assert content_type == "application/x-www-form-urlencoded"
            assert form["scope"] == ["qurl:agent qurl:read qurl:write"]
            return 200, json.dumps(
                {"access_token": self.jwt, "token_type": "Bearer"}
            ).encode()
        if parsed.path == "/v1/me":
            if bearer == self.jwt:
                data = {"auth_type": "jwt", "owner_id": self.owner}
            elif bearer == self.api_key or bearer in self.issued_api_keys:
                key_id, api_key = self.issued_api_keys.get(
                    bearer, (self.key_id, self.api_key)
                )
                data = {
                    "api_key": {
                        "key_id": key_id,
                        "kind": "api_key",
                        "scopes": credentials.CUSTOMER_SCOPES,
                    },
                    "auth_type": "api_key",
                    "owner_id": self.owner,
                }
            else:
                return 401, b"{}"
            return 200, json.dumps({"data": data}).encode()
        if parsed.path == "/v1/api-keys" and method == "POST":
            request = json.loads(body or b"{}")
            assert request == {
                "kind": "api_key",
                "name": request["name"],
                "scopes": credentials.CUSTOMER_SCOPES,
            }
            assert extra_headers and extra_headers["Idempotency-Key"].startswith(
                "qurl-cli-ci-"
            )
            issued = len(self.issued_api_keys)
            key_id = self.key_id if issued == 0 else f"key_Paired{issued:06d}"
            api_key = self.api_key if issued == 0 else f"lv_test_customer-key-{issued}"
            self.issued_api_keys[api_key] = (key_id, api_key)
            row = {
                "api_key": api_key,
                "key_id": key_id,
                "kind": "api_key",
                "name": request["name"],
                "scopes": credentials.CUSTOMER_SCOPES,
                "status": "active",
            }
            self.keys[self.key_id] = row
            return 201, json.dumps({"data": row}).encode()
        if parsed.path == "/v1/api-keys" and method == "GET":
            self.api_key_inventory_requests += 1
            rows = (
                list(self.keys.values())
                + self.extra_credentials
                + [
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
            )
            status_filter = urllib.parse.parse_qs(parsed.query).get("status", [])
            if status_filter:
                assert len(status_filter) == 1
                rows = [row for row in rows if row.get("status") == status_filter[0]]
            return 200, json.dumps({"data": rows, "meta": {"has_more": False}}).encode()
        if parsed.path.startswith("/v1/api-keys/") and method == "DELETE":
            key_id = urllib.parse.unquote(parsed.path.rsplit("/", 1)[1])
            self.key_delete_attempts.append(key_id)
            if self.transient_key_delete:
                self.transient_key_delete -= 1
                return 503, b"{}"
            if key_id in self.failed_key_deletes:
                return 503, b"{}"
            self.deleted_keys.append(key_id)
            self.operations.append("revoke:" + key_id)
            if self.retain_revoked_keys and key_id in self.keys:
                self.keys[key_id]["status"] = "revoked"
            else:
                self.keys.pop(key_id, None)
            return 204, b""
        if (
            parsed.path.startswith("/v1/connectors/agents/")
            and parsed.path.endswith("/assignment")
            and method == "DELETE"
        ):
            agent_id = urllib.parse.unquote(
                parsed.path.removeprefix("/v1/connectors/agents/").removesuffix(
                    "/assignment"
                )
            )
            assert credentials.RUN_AGENT_ID.fullmatch(agent_id)
            self.assignment_retire_attempts.append(agent_id)
            if agent_id in self.assignment_retire_statuses:
                return self.assignment_retire_statuses[agent_id], b"{}"
            if agent_id in self.failed_assignment_retires:
                return 503, b"{}"
            if agent_id in self.assignments:
                self.retired_assignments.append(agent_id)
                self.operations.append("retire:" + agent_id)
                self.assignments.remove(agent_id)
            return 204, b""
        if parsed.path == "/v1/resources" and method == "GET":
            query = urllib.parse.parse_qs(parsed.query)
            if "slug" in query:
                assert set(query) == {"slug"}
                assert len(query["slug"]) == 1
                row = self.connector_resources.get(query["slug"][0])
                return 200, json.dumps({"data": [] if row is None else [row]}).encode()
            self.resource_inventory_requests += 1
            assert query == {"limit": [str(credentials.INVENTORY_PAGE_SIZE)]}
            return 200, json.dumps(
                {"data": self.resources, "meta": {"has_more": False}}
            ).encode()
        if parsed.path.startswith("/v1/resources/") and method == "DELETE":
            resource_id = urllib.parse.unquote(parsed.path.rsplit("/", 1)[1])
            assert not resource_id.startswith("connector-cli-journey-v2-")
            self.resource_delete_attempts.append(resource_id)
            if resource_id in self.failed_resource_deletes:
                return 503, b"{}"
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


def workflow_timeout_minutes(workflow: pathlib.Path, job_name: str) -> int:
    """Read one fixed timeout from the standard two-/four-space job shape."""
    lines = workflow.read_text(encoding="utf-8").splitlines()
    header = f"  {job_name}:"
    try:
        start = lines.index(header) + 1
    except ValueError as exc:
        raise AssertionError(f"workflow job {job_name!r} is missing") from exc
    end = next(
        (
            index
            for index in range(start, len(lines))
            if re.fullmatch(r"  [A-Za-z0-9_-]+:", lines[index])
        ),
        len(lines),
    )
    values = [
        int(match.group(1))
        for line in lines[start:end]
        if (match := re.fullmatch(r"    timeout-minutes: ([1-9][0-9]*)", line))
    ]
    assert len(values) == 1, f"workflow job {job_name!r} must have one fixed timeout"
    return values[0]


def sticky_clock(*values: float):
    """Return each value once, then repeat the final value."""
    assert values
    remaining = iter(values)
    last = values[-1]

    def read() -> float:
        nonlocal last
        last = next(remaining, last)
        return last

    return read


def test_scheduled_soak_workflow_contract() -> None:
    workflow = CLI_WORKFLOW.read_text(encoding="utf-8")
    assert 'cron: "17 6 * * *"' in workflow
    assert (
        "contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && 'true'"
        in workflow
    )

    base_line = next(
        line.strip()
        for line in workflow.splitlines()
        if line.strip().startswith("matrix='{")
    )
    base_matrix = json.loads(base_line.removeprefix("matrix='").removesuffix("'"))
    assert base_matrix == {
        "include": [
            {
                "lane": "linux",
                "lane_id": 1,
                "os": "ubuntu-latest",
                "test_name": "TestSandboxLinuxDefaultDaemonLifecycle",
                "timeout_minutes": 35,
                "soak": False,
            },
            {
                "lane": "macos",
                "lane_id": 2,
                "os": "macos-latest",
                "test_name": "TestSandboxMacOSDefaultDaemonLifecycle",
                "timeout_minutes": 35,
                "soak": False,
            },
            {
                "lane": "windows",
                "lane_id": 3,
                "os": "windows-latest",
                "test_name": "TestSandboxWindowsDefaultDaemonFullCustomerLifecycle",
                "timeout_minutes": 35,
                "soak": False,
            },
        ]
    }
    soak_line = next(line for line in workflow.splitlines() if ".include += [" in line)
    soak_row = json.loads(soak_line.split(".include += [", 1)[1].split("]' <<<", 1)[0])
    assert soak_row == {
        "lane": "linux",
        "lane_id": 4,
        "os": "ubuntu-latest",
        "test_name": "TestSandboxLocalPublishSoak",
        "timeout_minutes": 110,
        "soak": True,
    }
    assert "matrix: ${{ fromJSON(needs.changes.outputs.journey_matrix) }}" in workflow
    assert "timeout-minutes: ${{ matrix.timeout_minutes }}" in workflow
    assert "-tags=clisandbox,clisoak" in workflow
    assert (
        "QURL_CLI_SANDBOX_LOCAL_PUBLISH_SOAK: ${{ matrix.soak && 'enabled' || '' }}"
        in workflow
    )
    assert (
        "QURL_CLI_SANDBOX_SOAK_DURATION: ${{ matrix.soak && '80m' || '' }}" in workflow
    )
    lane_count = len(base_matrix["include"]) + 1
    base_specs = [
        f"{row['lane']}:{row['lane_id']}:full" for row in base_matrix["include"]
    ]
    soak_spec = f"{soak_row['lane']}:{soak_row['lane_id']}:soak"
    assert f"lane_specs=({' '.join([*base_specs, soak_spec])})" in workflow
    assert credentials.MAX_RECONCILE_RUNS == lane_count * 3
    assert "(.journeys | length) == $journey_count" in RELEASE_GATE.read_text(
        encoding="utf-8"
    )
    assert (
        f'"$CLI_RUN_ID" "$CLI_RUN_ATTEMPT" {lane_count})'
        in RELEASE_WORKFLOW.read_text(encoding="utf-8")
    )
    assert "needs: [required, journey]" in workflow
    assert "github.ref == 'refs/heads/main'" in workflow
    assert "github.event_name != 'pull_request'" not in workflow
    assert (
        'contains(fromJson(\'["push","schedule","workflow_dispatch"]\'), github.event_name)'
        not in workflow
    )
    assert (
        workflow.count(
            'contains(fromJson(\'["schedule","workflow_dispatch"]\'), github.event_name)'
        )
        >= 5
    )
    assert workflow.count("qurl-cli-ci-credentials.py create-pair") == 2
    assert "qurl-cli-ci-credentials.py create " not in workflow
    assert "cleanup-jwt" not in SCRIPT.read_text(encoding="utf-8")
    assert "qurl-cli-ci-credentials.py reconcile-run" not in workflow
    assert workflow.count("qurl-cli-ci-credentials.py reconcile-batch") == 1
    assert "--require-device-keys" not in workflow
    assert "needs.customer-artifacts.result == 'success'" in workflow
    assert "QURL_CLI_CI_CLEANUP_ID_DIR" not in workflow
    assert 'sudo loginctl disable-linger "$(id -un)" 2>/dev/null || true' in workflow
    assert "inputs.release_source_sha != ''" in workflow
    assert '"$GITHUB_SHA" == "$RELEASE_SOURCE_SHA"' in workflow
    assert "needs.required.result == 'success'" in workflow
    assert "needs.journey.result == 'success'" in workflow
    assert "notify-soak-manual-failure:" in workflow
    assert "SOAK_STATUS: failure" in workflow
    assert "SOAK_DURATION: 80m" in workflow
    assert "run: scripts/notify-qurl-cli-soak-status.sh" in workflow

    cleanup = CUSTOMER_CLEANUP_WORKFLOW.read_text(encoding="utf-8")
    assert "schedule|workflow_dispatch) include_soak=true" in cleanup
    assert (
        '(.event == "push" or .event == "schedule" or .event == "workflow_dispatch")'
        in cleanup
    )
    assert f"lane_specs=({' '.join(base_specs)})" in cleanup
    assert f"lane_specs+=({soak_spec})" in cleanup
    assert "qurl-cli-ci-credentials.py reconcile-run" not in cleanup
    assert cleanup.count("qurl-cli-ci-credentials.py reconcile-batch") == 1
    assert '.name == "cli / customer journey cleanup"' in cleanup
    assert '.conclusion == "success"' in cleanup


def test_pair_and_batch_each_request_one_auth0_token() -> None:
    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        primary = root / "primary"
        failure = root / "failure"
        primary.mkdir(mode=0o700)
        failure.mkdir(mode=0o700)
        credentials.create_pair(
            argparse.Namespace(
                **vars(args),
                primary_output_dir=primary,
                failure_output_dir=failure,
                lane="linux",
                run_attempt="2",
                run_id="1231",
            )
        )
        assert fake.auth_token_requests == 1
        assert (primary / "api-key-id").read_text(encoding="utf-8") != (
            failure / "api-key-id"
        ).read_text(encoding="utf-8")
        assert (primary / "api-key").read_text(encoding="utf-8") != (
            failure / "api-key"
        ).read_text(encoding="utf-8")
        assert not (primary / "cleanup-jwt").exists()
        assert not (failure / "cleanup-jwt").exists()

        fake.auth_token_requests = 0
        credentials.reconcile_batch(
            argparse.Namespace(
                **vars(args),
                run_spec=(
                    "1231:2:linux:host:full",
                    "1232:2:macos:host:full",
                ),
            )
        )
        assert fake.auth_token_requests == 1


def test_batch_rejects_invalid_input_before_auth0_and_attempts_every_run() -> None:
    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        args = auth_args(pathlib.Path(raw_root))
        for run_specs in (
            ("malformed",),
            tuple(f"{1000 + index}:1:linux:host:full" for index in range(13)),
        ):
            try:
                credentials.reconcile_batch(
                    argparse.Namespace(**vars(args), run_spec=run_specs)
                )
            except credentials.CredentialError:
                pass
            else:
                raise AssertionError("invalid reconciliation batch was accepted")
        assert fake.auth_token_requests == 0

        attempted: list[str] = []

        def reconcile(
            run: argparse.Namespace, authenticated=None, inventory=None
        ) -> None:
            assert authenticated == ("https://sandbox.example", fake.jwt)
            assert isinstance(inventory, credentials.ReconciliationInventory)
            attempted.append(run.run_id)
            if run.run_id == "1231":
                raise credentials.CredentialError("first run failed")

        with mock.patch.object(credentials, "reconcile_run", reconcile):
            credentials.reconcile_batch(
                argparse.Namespace(
                    **vars(args),
                    run_spec=(
                        "1232:2:macos:host:full",
                        "1232:2:macos:host:full",
                    ),
                )
            )
        assert attempted == ["1232"]
        assert fake.auth_token_requests == 1

        attempted.clear()
        fake.auth_token_requests = 0
        diagnostics = io.StringIO()
        with (
            mock.patch.object(credentials, "reconcile_run", reconcile),
            contextlib.redirect_stderr(diagnostics),
        ):
            try:
                credentials.reconcile_batch(
                    argparse.Namespace(
                        **vars(args),
                        run_spec=(
                            "1231:2:linux:host:full",
                            "1232:2:macos:host:full",
                        ),
                    )
                )
            except credentials.CredentialError as exc:
                assert str(exc) == "batch cleanup did not converge for 1 of 2 runs"
            else:
                raise AssertionError("failed reconciliation batch was accepted")
        assert attempted == ["1231", "1232"]
        assert fake.auth_token_requests == 1
        assert (
            "::error::run cleanup failed for linux lane run 1231/2: first run failed"
            in diagnostics.getvalue()
        )

        attempted.clear()
        fake.auth_token_requests = 0
        fake.api_key_inventory_requests = 0
        fake.resource_inventory_requests = 0
        maximum = tuple(
            f"{2000 + index}:1:{('linux', 'macos', 'windows')[index % 3]}:host:full"
            for index in range(credentials.MAX_RECONCILE_RUNS)
        )
        with mock.patch.object(credentials, "reconcile_run", reconcile):
            credentials.reconcile_batch(
                argparse.Namespace(**vars(args), run_spec=maximum)
            )
        assert len(attempted) == credentials.MAX_RECONCILE_RUNS
        assert fake.auth_token_requests == 1
        assert fake.api_key_inventory_requests == 1
        assert fake.resource_inventory_requests == 1


def test_create_rejects_invalid_input_before_auth0() -> None:
    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        root = pathlib.Path(raw_root)
        auth = auth_args(root)
        for field, value in (
            ("run_id", "0"),
            ("run_attempt", "01"),
            ("lane", "solaris"),
            ("purpose", "admin"),
        ):
            values = {
                **vars(auth),
                "output_dir": root / "credential",
                "run_id": "1231",
                "run_attempt": "2",
                "lane": "linux",
                "purpose": "primary",
            }
            values[field] = value
            try:
                credentials.create(argparse.Namespace(**values))
            except credentials.CredentialError:
                pass
            else:
                raise AssertionError(f"invalid create field {field} was accepted")
    assert fake.auth_token_requests == 0


def test_pair_failure_revokes_both_exact_keys_with_the_same_token() -> None:
    fake = FakeAPI()

    def reject_second_identity(
        url: str,
        method: str,
        bearer: str | None = None,
        body: bytes | None = None,
        content_type: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        if method == "GET" and bearer == "lv_test_customer-key-1":
            return 401, b"{}"
        return fake(url, method, bearer, body, content_type, extra_headers)

    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", reject_second_identity),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        primary = root / "primary"
        failure = root / "failure"
        primary.mkdir(mode=0o700)
        failure.mkdir(mode=0o700)
        try:
            credentials.create_pair(
                argparse.Namespace(
                    **vars(args),
                    primary_output_dir=primary,
                    failure_output_dir=failure,
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == (
                "credential-pair creation failed; every created key was revoked"
            )
        else:
            raise AssertionError("invalid second customer identity was accepted")
    assert fake.auth_token_requests == 1
    assert set(fake.deleted_keys) == {"key_AbCdEf123456", "key_Paired000001"}
    assert fake.keys == {}


def test_pair_first_create_failure_never_mints_a_recovery_key() -> None:
    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(
            credentials,
            "prepare_output_directory",
            side_effect=credentials.CredentialError("output directory rejected"),
        ),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        try:
            credentials.create_pair(
                argparse.Namespace(
                    **vars(args),
                    primary_output_dir=root / "primary",
                    failure_output_dir=root / "failure",
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == (
                "credential-pair creation failed; every created key was revoked"
            )
        else:
            raise AssertionError("rejected first credential unexpectedly succeeded")
    assert fake.auth_token_requests == 1
    assert fake.issued_api_keys == {}
    assert fake.deleted_keys == []

    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(
            credentials,
            "create_with_auth",
            side_effect=credentials.CleanupConvergenceError("cleanup failed"),
        ),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        try:
            credentials.create_pair(
                argparse.Namespace(
                    **vars(args),
                    primary_output_dir=root / "primary",
                    failure_output_dir=root / "failure",
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == (
                "credential-pair creation failed and bounded revoke did not converge"
            )
        else:
            raise AssertionError("failed inner cleanup was masked")


def test_auth0_token_remaining_lifetime_matches_workflow_budget() -> None:
    fixed_now = 2_000_000_000
    cleanup_minutes = workflow_timeout_minutes(CLI_WORKFLOW, "journey-cleanup")
    fallback_cleanup_minutes = workflow_timeout_minutes(
        CUSTOMER_CLEANUP_WORKFLOW, "cleanup"
    )
    assert fallback_cleanup_minutes == 45
    assert credentials.FALLBACK_CLEANUP_BUDGET_SECONDS == fallback_cleanup_minutes * 60
    assert credentials.M2M_EXPIRY_MARGIN_SECONDS == 5 * 60
    assert credentials.MIN_M2M_TOKEN_REMAINING_SECONDS == 3000, (
        "journey credential budget changed; confirm the M2M lifetime still covers it"
    )
    assert (
        credentials.MIN_M2M_TOKEN_REMAINING_SECONDS
        + credentials.AUTH0_ISSUANCE_SKEW_SECONDS
        <= credentials.AUTH0_M2M_TOKEN_LIFETIME_SECONDS
    ), "journey budget no longer fits inside the CI Auth0 M2M token lifetime"
    assert (
        fallback_cleanup_minutes * 60 <= credentials.MIN_M2M_TOKEN_REMAINING_SECONDS
    ), "fallback cleanup timeout no longer fits inside the shared management token"
    # Scheduled/manual runs add the Linux soak lane. The fallback accepts at
    # most three source runs, for twelve total reconciliations in the largest
    # mixed recovery request.
    assert credentials.RECONCILE_INVENTORY_BUDGET_SECONDS * 4 < cleanup_minutes * 60, (
        "primary inventory budgets no longer leave room for cleanup writes"
    )
    assert (
        credentials.RECONCILE_INVENTORY_BUDGET_SECONDS * 12
        < fallback_cleanup_minutes * 60
    ), "fallback inventory budgets no longer leave room for cleanup writes"
    assert (
        0
        < credentials.RESOURCE_INVENTORY_RESERVE_SECONDS
        < (credentials.RECONCILE_INVENTORY_BUDGET_SECONDS)
    )

    def token(remaining_seconds: int, issued_ago: int = 0) -> str:
        return (
            "header."
            + encoded(
                {
                    "aud": "https://sandbox.example",
                    "exp": fixed_now + remaining_seconds,
                    "gty": "client-credentials",
                    "iat": fixed_now - issued_ago,
                    "iss": "https://auth.example/",
                    "scope": "qurl:agent qurl:read qurl:write",
                    "sub": "ci-client@clients",
                }
            )
            + ".signature"
        )

    def token_response(value: str) -> tuple[int, bytes]:
        return 200, json.dumps({"access_token": value, "token_type": "Bearer"}).encode()

    with tempfile.TemporaryDirectory() as raw_root:
        args = auth_args(pathlib.Path(raw_root))
        for remaining_seconds, issued_ago in ((3599, 1), (3000, 0)):
            value = token(remaining_seconds, issued_ago)
            with (
                mock.patch.object(
                    credentials, "request", return_value=token_response(value)
                ),
                mock.patch.object(credentials.time, "time", return_value=fixed_now),
            ):
                assert credentials.auth0_token(args) == (value, "ci-client@clients")

        with (
            mock.patch.object(
                credentials, "request", return_value=token_response(token(2999))
            ),
            mock.patch.object(credentials.time, "time", return_value=fixed_now),
        ):
            try:
                credentials.auth0_token(args)
            except credentials.CredentialError as exc:
                assert str(exc) == (
                    "Auth0 token does not have the required CI management lifetime"
                )
            else:
                raise AssertionError("2999-second Auth0 token was accepted")


def add_run_credentials(fake: FakeAPI) -> tuple[str, str, str, str]:
    run_key_id = "key_RunKey123456"
    failure_key_id = "key_FailKey12345"
    device_key_id = "key_DevRun123456"
    unrecorded_device_key_id = "key_DevFail12345"
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
    return run_key_id, failure_key_id, device_key_id, unrecorded_device_key_id


def test_bounded_valid_pagination() -> None:
    pages = credentials.INVENTORY_MAX_PAGES

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
            "data": [
                {"resource_id": f"r_{page}_{row}"}
                for row in range(credentials.INVENTORY_PAGE_SIZE)
            ],
            "meta": {
                "has_more": page + 1 < pages,
                "next_cursor": str(page + 1) if page + 1 < pages else "",
            },
        }
        return 200, json.dumps(response).encode()

    with mock.patch.object(credentials, "request", fake_request):
        rows = credentials.paged_rows(
            "https://sandbox.example",
            "jwt",
            "/v1/resources",
            "test",
            status_filter=None,
            deadline=time.monotonic() + 60,
        )
    assert len(rows) == credentials.INVENTORY_MAX_ROWS
    assert rows[0]["resource_id"] == "r_0_0"
    assert rows[-1]["resource_id"] == (
        f"r_{credentials.INVENTORY_MAX_PAGES - 1}_{credentials.INVENTORY_PAGE_SIZE - 1}"
    )


def test_pagination_safety_limits_fail_closed() -> None:
    calls = 0

    def endless_request(
        url: str,
        method: str,
        bearer: str | None = None,
        body: bytes | None = None,
        content_type: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        nonlocal calls
        del url, bearer, body, content_type, extra_headers
        assert method == "GET"
        calls += 1
        return 200, json.dumps(
            {
                "data": [],
                "meta": {"has_more": True, "next_cursor": f"cursor-{calls}"},
            }
        ).encode()

    with mock.patch.object(credentials, "request", endless_request):
        try:
            credentials.paged_rows(
                "https://sandbox.example",
                "jwt",
                "/v1/resources",
                "test",
                status_filter=None,
                deadline=time.monotonic() + 60,
            )
        except credentials.InventoryBoundError as exc:
            assert str(exc) == "test inventory exceeded its page limit"
        else:
            raise AssertionError("inventory page limit was not enforced")
    assert calls == credentials.INVENTORY_MAX_PAGES

    rows = [
        {"resource_id": f"r_{index}"}
        for index in range(credentials.INVENTORY_MAX_ROWS + 1)
    ]

    def oversized_request(*_args: object, **_kwargs: object) -> tuple[int, bytes]:
        return 200, json.dumps({"data": rows, "meta": {"has_more": False}}).encode()

    with mock.patch.object(credentials, "request", oversized_request):
        try:
            credentials.paged_rows(
                "https://sandbox.example",
                "jwt",
                "/v1/resources",
                "test",
                status_filter=None,
                deadline=time.monotonic() + 60,
            )
        except credentials.InventoryBoundError as exc:
            assert str(exc) == "test inventory exceeded its row limit"
        else:
            raise AssertionError("inventory row limit was not enforced")

    def one_page_request(*_args: object, **_kwargs: object) -> tuple[int, bytes]:
        return 200, b'{"data":[],"meta":{"has_more":false}}'

    with (
        mock.patch.object(credentials, "request", one_page_request),
        mock.patch.object(
            credentials.time,
            "monotonic",
            side_effect=sticky_clock(
                0.0, float(credentials.RECONCILE_INVENTORY_BUDGET_SECONDS)
            ),
        ),
    ):
        try:
            credentials.paged_rows(
                "https://sandbox.example",
                "jwt",
                "/v1/resources",
                "test",
                status_filter=None,
                deadline=float(credentials.RECONCILE_INVENTORY_BUDGET_SECONDS),
            )
        except credentials.InventoryBoundError as exc:
            assert str(exc) == "test inventory exceeded its time limit"
        else:
            raise AssertionError("inventory time limit was not enforced")

    calls = 0

    def first_page_request(*_args: object, **_kwargs: object) -> tuple[int, bytes]:
        nonlocal calls
        calls += 1
        return 200, json.dumps(
            {
                "data": [{"resource_id": "r_1"}],
                "meta": {"has_more": True, "next_cursor": "next"},
            }
        ).encode()

    with (
        mock.patch.object(credentials, "request", first_page_request),
        mock.patch.object(
            credentials.time,
            "monotonic",
            side_effect=sticky_clock(
                0.0, 0.0, float(credentials.RECONCILE_INVENTORY_BUDGET_SECONDS)
            ),
        ),
    ):
        try:
            credentials.paged_rows(
                "https://sandbox.example",
                "jwt",
                "/v1/resources",
                "test",
                status_filter=None,
                deadline=float(credentials.RECONCILE_INVENTORY_BUDGET_SECONDS),
            )
        except credentials.InventoryBoundError as exc:
            assert str(exc) == "test inventory exceeded its time limit"
        else:
            raise AssertionError("pre-request inventory deadline was not enforced")
    assert calls == 1


def test_reconciliation_reserves_time_for_resource_inventory() -> None:
    fake = FakeAPI()
    deadlines: list[tuple[str, float]] = []
    real_paged_rows = credentials.paged_rows

    def capture_deadline(*args: object, **kwargs: object) -> list[dict[str, object]]:
        deadline = kwargs.get("deadline")
        assert isinstance(deadline, float)
        path = args[2]
        assert isinstance(path, str)
        deadlines.append((path, deadline))
        return real_paged_rows(*args, **kwargs)  # type: ignore[arg-type]

    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(credentials, "paged_rows", capture_deadline),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        credentials.reconcile_run(
            argparse.Namespace(
                **vars(args),
                lane="linux",
                run_attempt="2",
                run_id="1231",
                runtime="host",
                profile="full",
            )
        )
    assert len(deadlines) == 2
    assert deadlines[0][0] == "/v1/api-keys"
    assert deadlines[1][0] == "/v1/resources"
    assert deadlines[1][1] - deadlines[0][1] == (
        credentials.RESOURCE_INVENTORY_RESERVE_SECONDS
    )


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
        {
            "data": [
                {**valid, "slug": "connector-cli-journey-v2-000000000000000000000000"}
            ]
        },
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

        with (
            mock.patch.object(credentials, "request", fake_request),
            mock.patch.object(credentials.time, "sleep", lambda _: None),
        ):
            try:
                credentials.retry_connector_resource_delete(
                    "https://sandbox.example", "jwt", connector_id
                )
            except credentials.CredentialError:
                pass
            else:
                raise AssertionError(
                    f"malformed Connector lookup succeeded: {response}"
                )
        assert methods == ["GET"]

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


def test_cleanup_hard_rejections_are_not_retried() -> None:
    connector_id = "connector-cli-journey-v2-415907f85f12d5ffd69c6a62"
    operations = (
        lambda: credentials.retry_revoke(
            "https://sandbox.example", "jwt", "key_AbCdEf123456"
        ),
        lambda: credentials.retry_resource_delete(
            "https://sandbox.example", "jwt", "r_customer_ci"
        ),
        lambda: credentials.retry_connector_resource_delete(
            "https://sandbox.example", "jwt", connector_id
        ),
    )
    for operation in operations:
        calls = 0

        def reject_request(*args: object, **kwargs: object) -> tuple[int, bytes]:
            nonlocal calls
            del args, kwargs
            calls += 1
            return 400, b"{}"

        with mock.patch.object(credentials, "request", reject_request):
            try:
                operation()
            except credentials.CredentialError as exc:
                assert str(exc).endswith("was rejected")
            else:
                raise AssertionError("durable cleanup rejection was accepted")
        assert calls == 1


def test_resource_failure_still_revokes_every_target_credential() -> None:
    fake = FakeAPI()
    target_key_ids = set(add_run_credentials(fake))
    fake.failed_resource_deletes.add("r_connector_smoke")
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        args = auth_args(pathlib.Path(raw_root))
        try:
            credentials.reconcile_run(
                argparse.Namespace(
                    **vars(args),
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                    profile="full",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == "run cleanup did not converge (connector_resource=1)"
        else:
            raise AssertionError("resource cleanup failure did not fail reconciliation")
    assert target_key_ids.issubset(fake.deleted_keys)
    assert (
        fake.resource_delete_attempts.count("r_connector_smoke")
        == credentials.MAX_ATTEMPTS
    )
    assert {"r_connector_failure", "r_customer_ci"}.issubset(fake.deleted_resources)


def test_credential_failure_still_attempts_every_target_and_resources() -> None:
    fake = FakeAPI()
    run_key_id, failure_key_id, device_key_id, unrecorded_device_key_id = (
        add_run_credentials(fake)
    )
    fake.failed_key_deletes.add(failure_key_id)
    fake.failed_resource_deletes.add("r_connector_smoke")
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        args = auth_args(pathlib.Path(raw_root))
        try:
            credentials.reconcile_run(
                argparse.Namespace(
                    **vars(args),
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                    profile="full",
                )
            )
        except credentials.CredentialError as exc:
            assert (
                str(exc)
                == "run cleanup did not converge (connector_resource=1, credential_revoke=1)"
            )
        else:
            raise AssertionError(
                "credential cleanup failure did not fail reconciliation"
            )
    assert fake.key_delete_attempts.count(failure_key_id) == credentials.MAX_ATTEMPTS
    assert {run_key_id, device_key_id, unrecorded_device_key_id}.issubset(
        fake.deleted_keys
    )
    assert (
        fake.resource_delete_attempts.count("r_connector_smoke")
        == credentials.MAX_ATTEMPTS
    )
    assert {"r_connector_failure", "r_customer_ci"}.issubset(fake.deleted_resources)


def test_assignment_failure_still_attempts_every_target_and_resources() -> None:
    fake = FakeAPI()
    add_run_credentials(fake)
    failed_agent = "qurl-journey-v2-r1231-a2-hs"
    other_agent = "qurl-journey-v2-r1231-a2-hf"
    fake.failed_assignment_retires.add(failed_agent)
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        args = auth_args(pathlib.Path(raw_root))
        try:
            credentials.reconcile_run(
                argparse.Namespace(
                    **vars(args),
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                    profile="full",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == "run cleanup did not converge (assignment_retire=1)"
        else:
            raise AssertionError(
                "assignment retirement failure did not fail reconciliation"
            )
    assert (
        fake.assignment_retire_attempts.count(failed_agent) == credentials.MAX_ATTEMPTS
    )
    assert other_agent in fake.retired_assignments
    assert {"r_connector_smoke", "r_connector_failure", "r_customer_ci"}.issubset(
        fake.deleted_resources
    )


def test_assignment_absence_is_idempotent_and_permanent_failures_are_fatal() -> None:
    absent_agent = "qurl-journey-v2-r1231-a2-hs"
    absent = FakeAPI()
    absent.assignments.clear()
    with mock.patch.object(credentials, "request", absent):
        credentials.retry_assignment_retire(
            "https://sandbox.example", absent.jwt, absent_agent
        )
    assert absent.assignment_retire_attempts == [absent_agent]
    assert absent.retired_assignments == []

    for status in (404, 500):
        rejected = FakeAPI()
        rejected.assignment_retire_statuses[absent_agent] = status
        with mock.patch.object(credentials, "request", rejected):
            try:
                credentials.retry_assignment_retire(
                    "https://sandbox.example", rejected.jwt, absent_agent
                )
            except credentials.CredentialError as exc:
                assert str(exc) == "qURL Connector assignment retirement was rejected"
            else:
                raise AssertionError(
                    f"permanent assignment retirement status {status} was accepted"
                )
        assert rejected.assignment_retire_attempts == [absent_agent]


def test_empty_run_reconciliation_is_idempotent() -> None:
    fake = FakeAPI()
    fake.assignments.clear()
    fake.connector_resources.clear()
    fake.resources = [
        resource
        for resource in fake.resources
        if resource.get("description") == "customer data"
    ]
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        args = auth_args(pathlib.Path(raw_root))
        credentials.reconcile_run(
            argparse.Namespace(
                **vars(args),
                lane="linux",
                run_attempt="2",
                run_id="1231",
                runtime="host",
                profile="full",
            )
        )
    assert set(fake.assignment_retire_attempts) == {
        "qurl-journey-v2-r1231-a2-hs",
        "qurl-journey-v2-r1231-a2-hf",
    }
    assert fake.deleted_keys == []
    assert fake.retired_assignments == []
    assert fake.deleted_resources == []


def test_successful_cleanup_contract_is_idempotent_after_revocation() -> None:
    fake = FakeAPI()
    fake.retain_revoked_keys = True
    add_run_credentials(fake)
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        args = auth_args(pathlib.Path(raw_root))
        run = argparse.Namespace(
            **vars(args),
            lane="linux",
            run_attempt="2",
            run_id="1231",
            runtime="host",
            profile="full",
        )
        credentials.reconcile_run(run)
        first_revocations = list(fake.deleted_keys)
        credentials.reconcile_run(run)
    assert fake.deleted_keys == first_revocations
    assert {
        row["status"]
        for row in fake.keys.values()
        if row.get("name") in credentials.run_device_key_names(run)
    } == {"revoked"}


def test_soak_cleanup_requires_and_removes_the_soak_device() -> None:
    fake = FakeAPI()
    soak_key_id = "key_SoakDev12345"
    fake.keys[soak_key_id] = {
        "key_id": soak_key_id,
        "kind": "device",
        "name": "agent:qurl-journey-v2-r1231-a2-hk",
        "scopes": credentials.DEVICE_SCOPES,
        "status": "active",
    }
    fake.assignments = {"qurl-journey-v2-r1231-a2-hk"}
    fake.connector_resources.clear()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
    ):
        args = auth_args(pathlib.Path(raw_root))
        run = argparse.Namespace(
            **vars(args),
            lane="linux",
            run_attempt="2",
            run_id="1231",
            runtime="host",
            profile="soak",
        )
        credentials.reconcile_run(run)
    assert credentials.run_agent_ids(run) == {"qurl-journey-v2-r1231-a2-hk"}
    assert fake.deleted_keys == [soak_key_id]
    assert fake.retired_assignments == ["qurl-journey-v2-r1231-a2-hk"]


def test_unhashable_inventory_fields_remain_bounded() -> None:
    active_fake = FakeAPI()
    run_key_id, failure_key_id, device_key_id, unrecorded_device_key_id = (
        add_run_credentials(active_fake)
    )
    active_fake.extra_credentials.extend(
        (
            {
                "key_id": {"unexpected": "object"},
                "kind": "api_key",
                "name": "qurl CLI journey v2 1231/2/linux/primary",
                "scopes": credentials.CUSTOMER_SCOPES,
                "status": "active",
            },
            {
                "key_id": device_key_id,
                "kind": "device",
                "name": ["unexpected", "array"],
                "scopes": credentials.DEVICE_SCOPES,
                "status": "active",
            },
        )
    )
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", active_fake),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        try:
            credentials.reconcile_run(
                argparse.Namespace(
                    **vars(args),
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                    profile="full",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == "run cleanup did not converge (credential_shape=2)"
        else:
            raise AssertionError(
                "malformed active credential inventory did not fail closed"
            )
    assert {
        run_key_id,
        failure_key_id,
        device_key_id,
        unrecorded_device_key_id,
    }.issubset(active_fake.deleted_keys)
    assert {"r_connector_smoke", "r_connector_failure", "r_customer_ci"}.issubset(
        active_fake.deleted_resources
    )


def main() -> None:
    test_scheduled_soak_workflow_contract()
    test_pair_and_batch_each_request_one_auth0_token()
    test_batch_rejects_invalid_input_before_auth0_and_attempts_every_run()
    test_create_rejects_invalid_input_before_auth0()
    test_pair_failure_revokes_both_exact_keys_with_the_same_token()
    test_pair_first_create_failure_never_mints_a_recovery_key()
    test_auth0_token_remaining_lifetime_matches_workflow_budget()
    test_bounded_valid_pagination()
    test_pagination_safety_limits_fail_closed()
    test_reconciliation_reserves_time_for_resource_inventory()
    test_connector_cleanup_lookup_fails_closed()
    test_cleanup_hard_rejections_are_not_retried()
    test_resource_failure_still_revokes_every_target_credential()
    test_credential_failure_still_attempts_every_target_and_resources()
    test_assignment_failure_still_attempts_every_target_and_resources()
    test_assignment_absence_is_idempotent_and_permanent_failures_are_fatal()
    test_empty_run_reconciliation_is_idempotent()
    test_successful_cleanup_contract_is_idempotent_after_revocation()
    test_soak_cleanup_requires_and_removes_the_soak_device()
    test_unhashable_inventory_fields_remain_bounded()
    assert credentials.run_device_key_names(
        argparse.Namespace(
            run_id="1231", run_attempt="2", runtime="host", profile="full"
        )
    ) == {"agent:qurl-journey-v2-r1231-a2-hs", "agent:qurl-journey-v2-r1231-a2-hf"}
    assert credentials.run_device_key_names(
        argparse.Namespace(
            run_id="1231",
            run_attempt="2",
            runtime="hardened_container",
            profile="full",
        )
    ) == {"agent:qurl-journey-v2-r1231-a2-cs", "agent:qurl-journey-v2-r1231-a2-cf"}
    assert credentials.run_agent_ids(
        argparse.Namespace(
            run_id="1231", run_attempt="2", runtime="host", profile="full"
        )
    ) == {"qurl-journey-v2-r1231-a2-hs", "qurl-journey-v2-r1231-a2-hf"}
    assert credentials.run_connector_ids(
        argparse.Namespace(
            run_id="1231",
            run_attempt="2",
            runtime="host",
            lane="linux",
            profile="full",
        )
    ) == {
        "connector-cli-journey-v2-415907f85f12d5ffd69c6a62",
        "connector-cli-journey-v2-87e091ca6623843507b5863b",
    }
    lane_identities = [
        argparse.Namespace(
            run_id="9001" + str(index),
            run_attempt="3",
            runtime="host",
            lane=lane,
            profile="full",
        )
        for index, lane in enumerate(("linux", "macos", "windows"), start=1)
    ]
    assert len({credentials.run_description(item) for item in lane_identities}) == 3
    assert credentials.run_credential_names(lane_identities[0]) == {
        "qurl CLI journey v2 90011/3/linux/primary",
        "qurl CLI journey v2 90011/3/linux/failure",
    }
    assert (
        len(
            set().union(
                *(credentials.run_credential_names(item) for item in lane_identities)
            )
        )
        == 6
    )
    assert (
        len(
            set().union(
                *(credentials.run_device_key_names(item) for item in lane_identities)
            )
        )
        == 6
    )
    assert (
        len(
            set().union(
                *(credentials.run_connector_ids(item) for item in lane_identities)
            )
        )
        == 6
    )
    fake = FakeAPI()
    with (
        tempfile.TemporaryDirectory() as raw_root,
        mock.patch.object(credentials, "request", fake),
        mock.patch.object(credentials.time, "sleep", lambda _: None),
    ):
        root = pathlib.Path(raw_root)
        args = auth_args(root)
        owner_output = root / "owner-id"
        credentials.identify(argparse.Namespace(**vars(args), output_file=owner_output))
        assert owner_output.read_text(encoding="utf-8") == fake.owner
        output = root / "credential"
        output.mkdir(mode=0o700)
        create_args = argparse.Namespace(
            **vars(args),
            output_dir=output,
            run_id="1231",
            run_attempt="2",
            lane="linux",
            purpose="primary",
        )
        credentials.create(create_args)
        assert {path.name for path in output.iterdir()} == {
            "api-key",
            "api-key-id",
            "owner-id",
            "run-name",
        }
        assert (output / "owner-id").read_text(encoding="utf-8") == fake.owner
        assert (output / "run-name").read_text(
            encoding="utf-8"
        ) == "qurl CLI journey v2 1231/2/linux/primary"
        run_key_id, failure_key_id, device_key_id, unrecorded_device_key_id = (
            add_run_credentials(fake)
        )
        unrelated_key_id = "key_OtherKey1234"
        unrelated_device_key_id = "key_OtherDev1234"
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
        fake.operations.clear()
        credentials.reconcile_run(
            argparse.Namespace(
                **vars(args),
                lane="linux",
                run_attempt="2",
                run_id="1231",
                runtime="host",
                profile="full",
            )
        )
        expected_connector_resource_ids = {"r_connector_smoke", "r_connector_failure"}
        assert set(fake.deleted_resources) == expected_connector_resource_ids | {
            "r_customer_ci"
        }
        assert "r_keep" not in fake.deleted_resources
        assert "r_redacted_tunnel" not in fake.deleted_resources
        assert {
            run_key_id,
            failure_key_id,
            device_key_id,
            unrecorded_device_key_id,
        }.issubset(fake.deleted_keys)
        assert set(fake.retired_assignments) == {
            "qurl-journey-v2-r1231-a2-hs",
            "qurl-journey-v2-r1231-a2-hf",
        }
        assert unrelated_key_id not in fake.deleted_keys
        assert unrelated_device_key_id not in fake.deleted_keys
        last_revoke = max(
            index
            for index, operation in enumerate(fake.operations)
            if operation.startswith("revoke:")
        )
        first_retire = min(
            index
            for index, operation in enumerate(fake.operations)
            if operation.startswith("retire:")
        )
        last_retire = max(
            index
            for index, operation in enumerate(fake.operations)
            if operation.startswith("retire:")
        )
        assert last_revoke < first_retire
        assert all(
            last_retire < fake.operations.index("delete:" + resource_id)
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
                    lane="linux",
                    run_attempt="2",
                    run_id="1231",
                    runtime="host",
                    profile="full",
                )
            )
        except credentials.CredentialError as exc:
            assert str(exc) == "run cleanup did not converge (credential_shape=1)"
        else:
            raise AssertionError(
                "malformed exact-name device credential was not rejected"
            )
        assert malformed_device_key_id not in fake.deleted_keys

    print("qurl CLI journey credential tests passed")


if __name__ == "__main__":
    main()
