"""Tests for services/mint_link_client.py — mock httpx, test success/failure/validation."""

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from services.mint_link_client import mint_link


def _mock_client(response):
    client = AsyncMock()
    client.__aenter__ = AsyncMock(return_value=client)
    client.__aexit__ = AsyncMock(return_value=False)
    client.post = AsyncMock(return_value=response)
    return client


def _mock_error_response(status_code: int):
    """Create a mock response that raises httpx.HTTPStatusError on raise_for_status()."""
    resp = MagicMock()
    resp.status_code = status_code
    request = MagicMock(spec=httpx.Request)
    request.url = "https://api.test/mint_link"
    resp.raise_for_status.side_effect = httpx.HTTPStatusError(
        f"{status_code} error", request=request, response=resp
    )
    return resp


class TestMintLinkSuccess:
    @pytest.mark.asyncio
    async def test_success_returns_qurl_link_and_expires(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurl_link": "https://qurl.link/at_mint123",
            "expires_at": "2026-12-31T23:59:59Z",
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            result = await mint_link("r_test123456", "recipient1")
        assert result["qurl_link"] == "https://qurl.link/at_mint123"
        assert result["expires_at"] == "2026-12-31T23:59:59Z"

    @pytest.mark.asyncio
    async def test_success_with_data_envelope(self):
        resp = MagicMock()
        resp.status_code = 201
        resp.json.return_value = {
            "data": {
                "qurl_link": "https://qurl.link/at_env123",
                "expires_at": "2027-01-01T00:00:00Z",
            }
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            result = await mint_link("r_test123456", "recipient1")
        assert result["qurl_link"] == "https://qurl.link/at_env123"

    @pytest.mark.asyncio
    async def test_success_with_camel_case_keys(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurlLink": "https://qurl.link/at_camel1",
            "expiresAt": "2027-06-15T00:00:00Z",
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            result = await mint_link("r_test123456", "recipient1")
        assert result["qurl_link"] == "https://qurl.link/at_camel1"
        assert result["expires_at"] == "2027-06-15T00:00:00Z"


class TestMintLinkErrors:
    @pytest.mark.asyncio
    async def test_http_500_raises(self):
        resp = _mock_error_response(500)
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            with pytest.raises(httpx.HTTPStatusError):
                await mint_link("r_test123456", "recipient1")

    @pytest.mark.asyncio
    async def test_missing_qurl_link_raises(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {"expires_at": "2026-12-31T00:00:00Z"}
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            with pytest.raises(Exception, match="missing qurl_link"):
                await mint_link("r_test123456", "recipient1")


class TestMintLinkValidation:
    @pytest.mark.asyncio
    async def test_invalid_qurl_link_prefix_raises(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurl_link": "https://evil.com/steal_data",
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            with pytest.raises(ValueError, match="invalid qurl_link"):
                await mint_link("r_test123456", "recipient1")

    @pytest.mark.asyncio
    async def test_http_scheme_rejected(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurl_link": "http://qurl.link/at_abc123",
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            with pytest.raises(ValueError, match="invalid qurl_link"):
                await mint_link("r_test123456", "recipient1")

    @pytest.mark.asyncio
    async def test_custom_hostname_accepted(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurl_link": "https://custom.example.com/at_abc123",
            "expires_at": "",
        }
        mock = _mock_client(resp)
        with (
            patch("services.mint_link_client.httpx.AsyncClient", return_value=mock),
            patch("services.mint_link_client.settings") as mock_settings,
        ):
            mock_settings.qurl_api_key = "lv_test_fake"
            mock_settings.mint_link_api_url = "https://api.layerv.ai/v1/qurls"
            mock_settings.qurl_link_hostname = "custom.example.com"
            result = await mint_link("r_test123456", "recipient1")
        assert result["qurl_link"] == "https://custom.example.com/at_abc123"


class TestMintLinkRequestBody:
    @pytest.mark.asyncio
    async def test_sends_correct_body(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "qurl_link": "https://qurl.link/at_body123",
            "expires_at": "",
        }
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            await mint_link("r_test123456", "recipient_xyz")

        # Inspect the call arguments
        call_kwargs = mock.post.call_args
        assert "r_test123456" in call_kwargs.args[0]  # URL contains resource_id
        payload = call_kwargs.kwargs["json"]
        assert payload["one_time_use"] is True
        assert payload["expires_in"] == "15m"
        assert payload["label"] == "discord:recipient_xyz"


class TestMintLinkExpiry:
    @pytest.mark.asyncio
    async def test_custom_expires_sent(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {"qurl_link": "https://qurl.link/at_x", "expires_at": ""}
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            await mint_link("r_test123456", "r1", expires_in="1h")
        payload = mock.post.call_args.kwargs["json"]
        assert payload["expires_in"] == "1h"

    @pytest.mark.asyncio
    async def test_default_expires(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {"qurl_link": "https://qurl.link/at_x", "expires_at": ""}
        mock = _mock_client(resp)
        with patch("services.mint_link_client.httpx.AsyncClient", return_value=mock):
            await mint_link("r_test123456", "r1")
        payload = mock.post.call_args.kwargs["json"]
        assert payload["expires_in"] == "15m"
