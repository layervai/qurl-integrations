"""
HTTP client module tests.
"""

import pytest
from unittest.mock import MagicMock, patch
from services.http_client import QurlApiClient


class TestQurlApiClient:
    """QurlApiClient tests"""

    def test_get_headers_basic(self):
        """Test basic headers include auth and content-type"""
        client = QurlApiClient("test-key", "https://api.example.com")
        headers = client._get_headers()
        assert headers["Authorization"] == "Bearer test-key"
        assert headers["Content-Type"] == "application/json"

    def test_get_headers_with_extra(self):
        """Test extra headers are merged"""
        client = QurlApiClient("test-key", "https://api.example.com")
        headers = client._get_headers({"X-Custom": "value"})
        assert headers["X-Custom"] == "value"
        assert headers["Authorization"] == "Bearer test-key"

    def test_post_without_files(self):
        """Test POST without files uses JSON"""
        with patch("services.http_client.get_http_client") as mock_get_client:
            mock_client = MagicMock()
            mock_response = MagicMock()
            mock_response.json.return_value = {"result": "ok"}
            mock_client.post.return_value = mock_response
            mock_get_client.return_value = mock_client

            client = QurlApiClient("test-key", "https://api.example.com")
            result = client.post("/test", data={"foo": "bar"})

            assert result == {"result": "ok"}
            mock_client.post.assert_called_once()
            call_kwargs = mock_client.post.call_args
            # JSON data passed as json= kwarg
            assert call_kwargs[1]["json"] == {"foo": "bar"}

    def test_post_with_files(self):
        """Test POST with files uses multipart/form-data"""
        with patch("services.http_client.get_http_client") as mock_get_client:
            mock_client = MagicMock()
            mock_response = MagicMock()
            mock_response.json.return_value = {}
            mock_client.post.return_value = mock_response
            mock_get_client.return_value = mock_client

            client = QurlApiClient("test-key", "https://api.example.com")
            client.post("/upload", data={"owner": "me"}, files={"file": ("f.txt", b"content", "text/plain")})

            mock_client.post.assert_called_once()
            call_kwargs = mock_client.post.call_args
            # Content-Type should be removed from headers for multipart
            headers = call_kwargs[1]["headers"]
            assert "Content-Type" not in headers
            assert call_kwargs[1]["files"] == {"file": ("f.txt", b"content", "text/plain")}

    def test_get(self):
        """Test GET request"""
        with patch("services.http_client.get_http_client") as mock_get_client:
            mock_client = MagicMock()
            mock_response = MagicMock()
            mock_response.json.return_value = {"id": "123"}
            mock_client.get.return_value = mock_response
            mock_get_client.return_value = mock_client

            client = QurlApiClient("test-key", "https://api.example.com")
            result = client.get("/item/123")

            assert result == {"id": "123"}
            mock_client.get.assert_called_once()
            call_kwargs = mock_client.get.call_args
            assert call_kwargs[1]["params"] is None

    def test_get_with_params(self):
        """Test GET with query params"""
        with patch("services.http_client.get_http_client") as mock_get_client:
            mock_client = MagicMock()
            mock_response = MagicMock()
            mock_response.json.return_value = {"items": []}
            mock_client.get.return_value = mock_response
            mock_get_client.return_value = mock_client

            client = QurlApiClient("test-key", "https://api.example.com")
            client.get("/items", params={"limit": 10})

            mock_client.get.assert_called_once()
            call_kwargs = mock_client.get.call_args
            assert call_kwargs[1]["params"] == {"limit": 10}

    def test_post_raises_on_error(self):
        """Test POST raises on HTTP error"""
        with patch("services.http_client.get_http_client") as mock_get_client:
            mock_client = MagicMock()
            mock_response = MagicMock()
            mock_response.status_code = 500
            mock_response.raise_for_status.side_effect = Exception("500 Server Error")
            mock_client.post.return_value = mock_response
            mock_get_client.return_value = mock_client

            client = QurlApiClient("test-key", "https://api.example.com")
            # raise_for_status is called inside post(), so post must return the response
            with pytest.raises(Exception):
                client.post("/test", data={})
