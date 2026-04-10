"""Tests for services/upload_client.py — mock httpx, test success/failure/validation."""

import os

# Set env vars before importing config (module-level side effect)
os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from services.upload_client import upload_file


def _mock_client(response):
    """Build a mock client that returns *response* from .post()."""
    client = AsyncMock()
    client.post = AsyncMock(return_value=response)
    return client


def _mock_error_response(status_code: int):
    """Create a mock response that raises httpx.HTTPStatusError on raise_for_status()."""
    resp = MagicMock()
    resp.status_code = status_code
    request = MagicMock(spec=httpx.Request)
    request.url = "https://api.test/upload"
    resp.raise_for_status.side_effect = httpx.HTTPStatusError(
        f"{status_code} error", request=request, response=resp
    )
    return resp


def _ok_response(**overrides):
    """Build a mock 200 response with valid defaults."""
    body = {
        "resource_id": "r_test123456",
        "qurl_link": "https://qurl.link/at_abc123",
        "expires_at": "2026-12-31T00:00:00Z",
    }
    body.update(overrides)
    resp = MagicMock()
    resp.status_code = 200
    resp.json.return_value = body
    return resp


class TestUploadClientSuccess:
    @pytest.mark.asyncio
    async def test_success_returns_expected_keys(self):
        mock = _mock_client(_ok_response())
        with patch("services.upload_client.get_client", return_value=mock):
            result = await upload_file(
                file_bytes=b"test content",
                filename="test.png",
                content_type="image/png",
                owner_id="user123",
            )
        assert result["resource_id"] == "r_test123456"
        assert result["qurl_link"] == "https://qurl.link/at_abc123"
        assert result["expires_at"] == "2026-12-31T00:00:00Z"

    @pytest.mark.asyncio
    async def test_success_with_nested_data_envelope(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "data": {
                "resource_id": "r_nested12345",
                "qurl_link": "https://qurl.link/at_nested",
                "expires_at": "2027-01-01T00:00:00Z",
            }
        }
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            result = await upload_file(
                file_bytes=b"x", filename="f.png",
                content_type="image/png", owner_id="u",
            )
        assert result["resource_id"] == "r_nested12345"

    @pytest.mark.asyncio
    async def test_success_with_camel_case_keys(self):
        resp = MagicMock()
        resp.status_code = 201
        resp.json.return_value = {
            "resourceId": "r_camel_12345",
            "qurlLink": "https://qurl.link/at_camel",
            "expiresAt": "2027-06-01T00:00:00Z",
        }
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            result = await upload_file(
                file_bytes=b"x", filename="f.png",
                content_type="image/png", owner_id="u",
            )
        assert result["resource_id"] == "r_camel_12345"
        assert result["qurl_link"] == "https://qurl.link/at_camel"


class TestUploadClientErrors:
    @pytest.mark.asyncio
    async def test_http_500_raises(self):
        resp = _mock_error_response(500)
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(httpx.HTTPStatusError):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )

    @pytest.mark.asyncio
    async def test_http_400_raises(self):
        resp = _mock_error_response(400)
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(httpx.HTTPStatusError):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )

    @pytest.mark.asyncio
    async def test_missing_resource_id_raises(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {"qurl_link": "https://qurl.link/at_x"}
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(Exception, match="missing resource_id"):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )


class TestUploadClientValidation:
    @pytest.mark.asyncio
    async def test_invalid_resource_id_rejected(self):
        resp = _ok_response(resource_id="INVALID")
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(ValueError, match="invalid resource_id"):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )

    @pytest.mark.asyncio
    async def test_invalid_qurl_link_rejected(self):
        resp = _ok_response(qurl_link="https://evil.com/phishing")
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(ValueError, match="unexpected hostname"):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )

    @pytest.mark.asyncio
    async def test_http_scheme_rejected(self):
        resp = _ok_response(qurl_link="http://qurl.link/at_abc123")
        mock = _mock_client(resp)
        with patch("services.upload_client.get_client", return_value=mock):
            with pytest.raises(ValueError, match="unexpected hostname"):
                await upload_file(
                    file_bytes=b"test", filename="test.png",
                    content_type="image/png", owner_id="user123",
                )

    @pytest.mark.asyncio
    async def test_custom_hostname_accepted(self):
        resp = _ok_response(qurl_link="https://custom.example.com/at_abc123")
        mock = _mock_client(resp)
        with (
            patch("services.upload_client.get_client", return_value=mock),
            patch("services.upload_client.settings") as mock_settings,
        ):
            mock_settings.qurl_api_key = "lv_test_fake"
            mock_settings.upload_api_url = "https://getqurllink.layerv.ai"
            mock_settings.qurl_link_hostname = "custom.example.com"
            result = await upload_file(
                file_bytes=b"test", filename="test.png",
                content_type="image/png", owner_id="user123",
            )
        assert result["qurl_link"] == "https://custom.example.com/at_abc123"
