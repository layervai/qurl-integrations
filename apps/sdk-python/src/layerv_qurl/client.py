"""QURL API client."""

from __future__ import annotations

import random
import time
from dataclasses import asdict
from typing import Any

import httpx

from layerv_qurl.errors import QURLError
from layerv_qurl.types import (
    AccessGrant,
    AccessPolicy,
    CreateInput,
    CreateOutput,
    ExtendInput,
    ListOutput,
    MintInput,
    MintOutput,
    QURL,
    Quota,
    ResolveInput,
    ResolveOutput,
    UpdateInput,
)

DEFAULT_BASE_URL = "https://api.layerv.ai"
DEFAULT_TIMEOUT = 30.0
DEFAULT_MAX_RETRIES = 3
DEFAULT_USER_AGENT = "qurl-python-sdk/0.1.0"

_RETRYABLE_STATUS = {429, 502, 503, 504}


class QURLClient:
    """Synchronous QURL API client."""

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = DEFAULT_MAX_RETRIES,
        user_agent: str = DEFAULT_USER_AGENT,
        http_client: httpx.Client | None = None,
    ) -> None:
        if not api_key or not api_key.strip():
            raise ValueError("api_key must not be empty")

        self._base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._max_retries = max_retries
        self._user_agent = user_agent
        self._client = http_client or httpx.Client(
            timeout=timeout,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
                "User-Agent": user_agent,
            },
        )
        self._owns_client = http_client is None

    def __repr__(self) -> str:
        masked = self._api_key[:4] + "***" + self._api_key[-4:] if len(self._api_key) > 8 else "***"
        return f"QURLClient(api_key='{masked}', base_url='{self._base_url}')"

    def close(self) -> None:
        """Close the underlying HTTP client (only if owned by this instance)."""
        if self._owns_client:
            self._client.close()

    def __enter__(self) -> QURLClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    # --- Public API ---

    def create(self, data: CreateInput) -> CreateOutput:
        """Create a new QURL."""
        resp = self._request("POST", "/v1/qurl", body=_serialize(data))
        return CreateOutput(
            resource_id=resp["resource_id"],
            qurl_link=resp["qurl_link"],
            qurl_site=resp["qurl_site"],
            expires_at=resp.get("expires_at"),
        )

    def get(self, resource_id: str) -> QURL:
        """Get a QURL by ID."""
        resp = self._request("GET", f"/v1/qurls/{resource_id}")
        return _parse_qurl(resp)

    def list(
        self,
        *,
        limit: int | None = None,
        cursor: str | None = None,
        status: str | None = None,
        q: str | None = None,
        sort: str | None = None,
    ) -> ListOutput:
        """List QURLs with optional filters."""
        params: dict[str, str] = {}
        if limit is not None:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        if status:
            params["status"] = status
        if q:
            params["q"] = q
        if sort:
            params["sort"] = sort

        resp_data, meta = self._raw_request("GET", "/v1/qurls", params=params)
        qurls = [_parse_qurl(q_data) for q_data in resp_data] if isinstance(resp_data, list) else []
        return ListOutput(
            qurls=qurls,
            next_cursor=meta.get("next_cursor") if meta else None,
            has_more=meta.get("has_more", False) if meta else False,
        )

    def delete(self, resource_id: str) -> None:
        """Delete (revoke) a QURL."""
        self._request("DELETE", f"/v1/qurls/{resource_id}")

    def extend(self, resource_id: str, data: ExtendInput) -> QURL:
        """Extend a QURL's expiration."""
        resp = self._request("PATCH", f"/v1/qurls/{resource_id}", body=_serialize(data))
        return _parse_qurl(resp)

    def update(self, resource_id: str, data: UpdateInput) -> QURL:
        """Update a QURL's mutable properties."""
        resp = self._request("PATCH", f"/v1/qurls/{resource_id}", body=_serialize(data))
        return _parse_qurl(resp)

    def mint_link(self, resource_id: str, data: MintInput | None = None) -> MintOutput:
        """Mint a new access link for a QURL."""
        body = _serialize(data) if data else None
        resp = self._request("POST", f"/v1/qurls/{resource_id}/mint_link", body=body)
        return MintOutput(qurl_link=resp["qurl_link"], expires_at=resp.get("expires_at"))

    def resolve(self, data: ResolveInput) -> ResolveOutput:
        """Resolve a QURL access token (headless).

        Triggers an NHP knock to open firewall access for the caller's IP.
        Requires ``qurl:resolve`` scope on the API key.
        """
        resp = self._request("POST", "/v1/resolve", body=_serialize(data))
        grant = None
        if resp.get("access_grant"):
            g = resp["access_grant"]
            grant = AccessGrant(
                expires_in=g["expires_in"], granted_at=g["granted_at"], src_ip=g["src_ip"]
            )
        return ResolveOutput(
            target_url=resp["target_url"],
            resource_id=resp["resource_id"],
            access_grant=grant,
        )

    def get_quota(self) -> Quota:
        """Get quota and usage information."""
        resp = self._request("GET", "/v1/quota")
        return Quota(
            plan=resp.get("plan", ""),
            period_start=resp.get("period_start", ""),
            period_end=resp.get("period_end", ""),
            rate_limits=resp.get("rate_limits"),
            usage=resp.get("usage"),
        )

    # --- Internal HTTP plumbing ---

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
    ) -> Any:
        data, _ = self._raw_request(method, path, body=body, params=params)
        return data

    def _raw_request(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
    ) -> tuple[Any, dict[str, Any] | None]:
        url = f"{self._base_url}{path}"
        last_error: Exception | None = None

        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "Content-Type": "application/json",
            "User-Agent": self._user_agent,
        }

        for attempt in range(self._max_retries + 1):
            if attempt > 0:
                delay = self._retry_delay(attempt, last_error)
                time.sleep(delay)

            response = self._client.request(
                method,
                url,
                json=body if body is not None else None,
                params=params,
                headers=headers,
            )

            if response.status_code < 400:
                if response.status_code == 204 or not response.content:
                    return None, None
                envelope = response.json()
                return envelope.get("data"), envelope.get("meta")

            err = self._parse_error(response)
            if response.status_code in _RETRYABLE_STATUS and attempt < self._max_retries:
                last_error = err
                continue
            raise err

        raise last_error or QURLError(
            status=0, code="unknown", title="Request failed", detail="Exhausted retries"
        )

    def _parse_error(self, response: httpx.Response) -> QURLError:
        retry_after = None
        if response.status_code == 429:
            ra = response.headers.get("Retry-After")
            if ra and ra.isdigit():
                retry_after = int(ra)

        try:
            envelope = response.json()
            err = envelope.get("error", {})
            return QURLError(
                status=err.get("status", response.status_code),
                code=err.get("code", "unknown"),
                title=err.get("title", response.reason_phrase or ""),
                detail=err.get("detail", ""),
                invalid_fields=err.get("invalid_fields"),
                request_id=envelope.get("meta", {}).get("request_id"),
                retry_after=retry_after,
            )
        except (ValueError, KeyError):
            return QURLError(
                status=response.status_code,
                code="unknown",
                title=response.reason_phrase or "",
                detail=response.text,
                retry_after=retry_after,
            )

    def _retry_delay(self, attempt: int, last_error: Exception | None) -> float:
        if isinstance(last_error, QURLError) and last_error.retry_after:
            return float(last_error.retry_after)
        base = 0.5 * (2 ** (attempt - 1))
        jitter = random.random() * base * 0.5  # noqa: S311
        return min(base + jitter, 30.0)


# --- Helpers ---


def _serialize(obj: Any) -> dict[str, Any] | None:
    if obj is None:
        return None
    d = asdict(obj)
    # Remove None values and serialize nested dataclasses
    result: dict[str, Any] = {}
    for k, v in d.items():
        if v is not None:
            result[k] = v
    return result


def _parse_qurl(data: dict[str, Any]) -> QURL:
    policy = None
    if data.get("access_policy"):
        p = data["access_policy"]
        policy = AccessPolicy(
            ip_allowlist=p.get("ip_allowlist"),
            ip_denylist=p.get("ip_denylist"),
            geo_allowlist=p.get("geo_allowlist"),
            geo_denylist=p.get("geo_denylist"),
            user_agent_allow_regex=p.get("user_agent_allow_regex"),
            user_agent_deny_regex=p.get("user_agent_deny_regex"),
        )
    return QURL(
        resource_id=data["resource_id"],
        target_url=data["target_url"],
        status=data["status"],
        created_at=data["created_at"],
        expires_at=data.get("expires_at"),
        one_time_use=data.get("one_time_use", False),
        max_sessions=data.get("max_sessions"),
        description=data.get("description"),
        qurl_site=data.get("qurl_site"),
        qurl_link=data.get("qurl_link"),
        access_policy=policy,
    )
