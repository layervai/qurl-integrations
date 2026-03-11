"""Tests for the QURL Python client."""

from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest
import respx

from layerv_qurl import QURLClient, QURLError
from layerv_qurl.types import (
    CreateInput,
    ExtendInput,
    MintInput,
    ResolveInput,
    UpdateInput,
)


BASE_URL = "https://api.test.layerv.ai"


@pytest.fixture
def client() -> QURLClient:
    return QURLClient(api_key="lv_live_test", base_url=BASE_URL, max_retries=0)


@pytest.fixture
def retry_client() -> QURLClient:
    return QURLClient(api_key="lv_live_test", base_url=BASE_URL, max_retries=2)


# --- Constructor tests ---


def test_empty_api_key_raises() -> None:
    with pytest.raises(ValueError, match="api_key must not be empty"):
        QURLClient(api_key="")


def test_whitespace_api_key_raises() -> None:
    with pytest.raises(ValueError, match="api_key must not be empty"):
        QURLClient(api_key="   ")


def test_repr_masks_api_key() -> None:
    c = QURLClient(api_key="lv_live_abcdefghij", base_url=BASE_URL)
    r = repr(c)
    assert "lv_l" in r
    assert "ghij" in r
    assert "abcdefghij" not in r
    assert "QURLClient(" in r
    c.close()


def test_repr_short_api_key() -> None:
    c = QURLClient(api_key="short123", base_url=BASE_URL)
    r = repr(c)
    assert "***" in r
    assert "short123" not in r
    c.close()


# --- CRUD tests ---


@respx.mock
def test_create(client: QURLClient) -> None:
    respx.post(f"{BASE_URL}/v1/qurl").mock(
        return_value=httpx.Response(
            201,
            json={
                "data": {
                    "resource_id": "r_abc123def45",
                    "qurl_link": "https://qurl.link/#at_test",
                    "qurl_site": "https://r_abc123def45.qurl.site",
                    "expires_at": "2026-03-15T10:00:00Z",
                },
                "meta": {"request_id": "req_1"},
            },
        )
    )

    result = client.create(CreateInput(target_url="https://example.com", expires_in="24h"))
    assert result.resource_id == "r_abc123def45"
    assert result.qurl_link == "https://qurl.link/#at_test"
    assert result.qurl_site == "https://r_abc123def45.qurl.site"


@respx.mock
def test_get(client: QURLClient) -> None:
    respx.get(f"{BASE_URL}/v1/qurls/r_abc123def45").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "resource_id": "r_abc123def45",
                    "target_url": "https://example.com",
                    "status": "active",
                    "created_at": "2026-03-10T10:00:00Z",
                    "expires_at": "2026-03-15T10:00:00Z",
                    "one_time_use": False,
                },
                "meta": {"request_id": "req_2"},
            },
        )
    )

    result = client.get("r_abc123def45")
    assert result.resource_id == "r_abc123def45"
    assert result.status == "active"
    assert result.target_url == "https://example.com"


@respx.mock
def test_list(client: QURLClient) -> None:
    respx.get(f"{BASE_URL}/v1/qurls").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {
                        "resource_id": "r_abc123def45",
                        "target_url": "https://example.com",
                        "status": "active",
                        "created_at": "2026-03-10T10:00:00Z",
                    }
                ],
                "meta": {"has_more": False, "page_size": 20},
            },
        )
    )

    result = client.list(status="active", limit=10)
    assert len(result.qurls) == 1
    assert result.qurls[0].resource_id == "r_abc123def45"
    assert result.has_more is False


@respx.mock
def test_delete(client: QURLClient) -> None:
    respx.delete(f"{BASE_URL}/v1/qurls/r_abc123def45").mock(
        return_value=httpx.Response(204)
    )

    client.delete("r_abc123def45")  # Should not raise


@respx.mock
def test_extend(client: QURLClient) -> None:
    respx.patch(f"{BASE_URL}/v1/qurls/r_abc123def45").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "resource_id": "r_abc123def45",
                    "target_url": "https://example.com",
                    "status": "active",
                    "created_at": "2026-03-10T10:00:00Z",
                    "expires_at": "2026-03-20T10:00:00Z",
                },
            },
        )
    )

    result = client.extend("r_abc123def45", ExtendInput(extend_by="7d"))
    assert result.expires_at == "2026-03-20T10:00:00Z"


@respx.mock
def test_update(client: QURLClient) -> None:
    respx.patch(f"{BASE_URL}/v1/qurls/r_abc123def45").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "resource_id": "r_abc123def45",
                    "target_url": "https://example.com",
                    "status": "active",
                    "created_at": "2026-03-10T10:00:00Z",
                    "description": "Updated description",
                },
            },
        )
    )

    result = client.update("r_abc123def45", UpdateInput(description="Updated description"))
    assert result.resource_id == "r_abc123def45"
    assert result.description == "Updated description"


@respx.mock
def test_mint_link(client: QURLClient) -> None:
    respx.post(f"{BASE_URL}/v1/qurls/r_abc123def45/mint_link").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "qurl_link": "https://qurl.link/#at_newtoken",
                    "expires_at": "2026-03-20T10:00:00Z",
                },
            },
        )
    )

    result = client.mint_link("r_abc123def45", MintInput(expires_at="2026-03-20T10:00:00Z"))
    assert result.qurl_link == "https://qurl.link/#at_newtoken"
    assert result.expires_at == "2026-03-20T10:00:00Z"


@respx.mock
def test_mint_link_no_input(client: QURLClient) -> None:
    respx.post(f"{BASE_URL}/v1/qurls/r_abc123def45/mint_link").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "qurl_link": "https://qurl.link/#at_default",
                },
            },
        )
    )

    result = client.mint_link("r_abc123def45")
    assert result.qurl_link == "https://qurl.link/#at_default"
    assert result.expires_at is None


@respx.mock
def test_resolve(client: QURLClient) -> None:
    respx.post(f"{BASE_URL}/v1/resolve").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "target_url": "https://api.example.com/data",
                    "resource_id": "r_abc123def45",
                    "access_grant": {
                        "expires_in": 305,
                        "granted_at": "2026-03-10T15:30:00Z",
                        "src_ip": "203.0.113.42",
                    },
                },
            },
        )
    )

    result = client.resolve(ResolveInput(access_token="at_k8xqp9h2sj9lx7r4a"))
    assert result.target_url == "https://api.example.com/data"
    assert result.access_grant is not None
    assert result.access_grant.expires_in == 305
    assert result.access_grant.src_ip == "203.0.113.42"


@respx.mock
def test_error_handling(client: QURLClient) -> None:
    respx.get(f"{BASE_URL}/v1/qurls/r_notfound0000").mock(
        return_value=httpx.Response(
            404,
            json={
                "error": {
                    "type": "https://api.qurl.link/problems/not_found",
                    "title": "Not Found",
                    "status": 404,
                    "detail": "QURL not found",
                    "code": "not_found",
                },
                "meta": {"request_id": "req_err"},
            },
        )
    )

    with pytest.raises(QURLError) as exc_info:
        client.get("r_notfound0000")

    err = exc_info.value
    assert err.status == 404
    assert err.code == "not_found"
    assert err.request_id == "req_err"


@respx.mock
def test_quota(client: QURLClient) -> None:
    respx.get(f"{BASE_URL}/v1/quota").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "plan": "growth",
                    "period_start": "2026-03-01T00:00:00Z",
                    "period_end": "2026-04-01T00:00:00Z",
                    "usage": {"active_qurls": 5, "qurls_created": 10},
                },
            },
        )
    )

    result = client.get_quota()
    assert result.plan == "growth"
    assert result.usage is not None
    assert result.usage["active_qurls"] == 5


# --- Injected http_client gets auth headers ---


@respx.mock
def test_injected_http_client_gets_auth_headers() -> None:
    """When user passes http_client, auth headers should still be set per-request."""
    custom_client = httpx.Client(timeout=10)
    qurl = QURLClient(api_key="lv_live_custom", base_url=BASE_URL, http_client=custom_client)

    route = respx.get(f"{BASE_URL}/v1/quota").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "plan": "free",
                    "period_start": "2026-03-01T00:00:00Z",
                    "period_end": "2026-04-01T00:00:00Z",
                },
            },
        )
    )

    qurl.get_quota()
    assert route.called
    req = route.calls[0].request
    assert req.headers["authorization"] == "Bearer lv_live_custom"
    assert req.headers["content-type"] == "application/json"

    custom_client.close()


# --- Retry logic tests ---


@respx.mock
def test_retry_success_after_429(retry_client: QURLClient) -> None:
    """Successful retry after receiving a 429."""
    route = respx.get(f"{BASE_URL}/v1/quota")
    route.side_effect = [
        httpx.Response(429, json={"error": {"status": 429, "code": "rate_limited", "title": "Rate Limited", "detail": "Slow down"}}),
        httpx.Response(
            200,
            json={
                "data": {
                    "plan": "growth",
                    "period_start": "2026-03-01T00:00:00Z",
                    "period_end": "2026-04-01T00:00:00Z",
                },
            },
        ),
    ]

    with patch("layerv_qurl.client.time.sleep"):
        result = retry_client.get_quota()

    assert result.plan == "growth"
    assert route.call_count == 2


@respx.mock
def test_retry_exhausted_raises_last_error(retry_client: QURLClient) -> None:
    """Exhausted retries should raise the last error."""
    route = respx.get(f"{BASE_URL}/v1/quota")
    route.side_effect = [
        httpx.Response(503, json={"error": {"status": 503, "code": "unavailable", "title": "Service Unavailable", "detail": "Down"}}),
        httpx.Response(503, json={"error": {"status": 503, "code": "unavailable", "title": "Service Unavailable", "detail": "Down"}}),
        httpx.Response(503, json={"error": {"status": 503, "code": "unavailable", "title": "Service Unavailable", "detail": "Still down"}}),
    ]

    with patch("layerv_qurl.client.time.sleep"):
        with pytest.raises(QURLError) as exc_info:
            retry_client.get_quota()

    assert exc_info.value.status == 503
    assert route.call_count == 3


@respx.mock
def test_retry_after_header_respected(retry_client: QURLClient) -> None:
    """Retry-After header value should be used as the delay."""
    route = respx.get(f"{BASE_URL}/v1/quota")
    route.side_effect = [
        httpx.Response(
            429,
            headers={"Retry-After": "5"},
            json={"error": {"status": 429, "code": "rate_limited", "title": "Rate Limited", "detail": "Slow down"}},
        ),
        httpx.Response(
            200,
            json={
                "data": {
                    "plan": "growth",
                    "period_start": "2026-03-01T00:00:00Z",
                    "period_end": "2026-04-01T00:00:00Z",
                },
            },
        ),
    ]

    with patch("layerv_qurl.client.time.sleep") as mock_sleep:
        result = retry_client.get_quota()

    assert result.plan == "growth"
    # The retry delay should use the Retry-After value of 5 seconds
    mock_sleep.assert_called_once_with(5.0)
