"""
QURL client module tests.
"""

import pytest
from unittest.mock import MagicMock, patch
from services.qurl_client import (
    QurlClient,
    UploadError,
    CreateError,
    MintError,
)


class TestQurlClient:
    """QurlClient tests"""

    def test_upload_file_bytes(self):
        """Test uploading file from bytes"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.return_value = {"resource_id": "r_abc123"}
            mock_client_cls.return_value = mock_client

            client = QurlClient(
                api_key="test",
                upload_api_url="https://upload.example.com",
                mint_link_api_url="https://mint.example.com",
            )
            result = client.upload_file(
                file_bytes=b"PDF content here",
                filename="report.pdf",
                content_type="application/pdf",
                owner_id="owner1",
            )

            assert result.resource_id == "r_abc123"
            assert result.filename == "report.pdf"

    def test_upload_file_fileobj(self):
        """Test uploading file from file-like object"""
        import io

        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.return_value = {"resource_id": "r_fileobj"}
            mock_client_cls.return_value = mock_client

            file_obj = io.BytesIO(b"File content")
            client = QurlClient("test", "https://u", "https://m")
            result = client.upload_file(
                file_bytes=file_obj,
                filename="doc.pdf",
                content_type="application/pdf",
                owner_id="owner1",
            )

            assert result.resource_id == "r_fileobj"
            mock_client.post.assert_called_once()

    def test_upload_file_error(self):
        """Test upload_file raises UploadError on exception"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.side_effect = Exception("Connection refused")
            mock_client_cls.return_value = mock_client

            client = QurlClient("test", "https://u", "https://m")
            with pytest.raises(UploadError) as exc:
                client.upload_file(b"data", "f.pdf", "pdf", "o1")
            assert "Connection refused" in str(exc.value)

    def test_create_qurl(self):
        """Test creating a Qurl"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.return_value = {"id": "q_xyz789"}
            mock_client_cls.return_value = mock_client

            client = QurlClient("test", "https://u", "https://m")
            result = client.create_qurl(
                target_url="https://example.com/doc",
                one_time_use=True,
                label="test-label",
            )

            assert result.resource_id == "q_xyz789"
            assert result.filename == "https://example.com/doc"
            mock_client.post.assert_called_once()
            call_data = mock_client.post.call_args[1]["data"]
            assert call_data["target_url"] == "https://example.com/doc"
            assert call_data["one_time_use"] is True

    def test_create_qurl_error(self):
        """Test create_qurl raises CreateError on exception"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.side_effect = Exception("Timeout")
            mock_client_cls.return_value = mock_client

            client = QurlClient("test", "https://u", "https://m")
            with pytest.raises(CreateError) as exc:
                client.create_qurl("https://example.com")
            assert "Timeout" in str(exc.value)

    def test_mint_link(self):
        """Test minting a link"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.return_value = {
                "id": "l_link123",
                "url": "https://qurl.link/abc",
                "expires_at": "2024-12-01T00:15:00Z",
            }
            mock_client_cls.return_value = mock_client

            client = QurlClient("test", "https://u", "https://m")
            result = client.mint_link(
                resource_id="r_abc",
                recipient_id="bob@example.com",
                one_time_use=True,
                label="bob-email",
                expires_in="15m",
            )

            assert result.link_id == "l_link123"
            assert result.url == "https://qurl.link/abc"
            mock_client.post.assert_called_once()
            call_data = mock_client.post.call_args[1]["data"]
            assert call_data["recipient_id"] == "bob@example.com"
            assert call_data["expires_in"] == "15m"

    def test_mint_link_error(self):
        """Test mint_link raises MintError on exception"""
        with patch("services.qurl_client.QurlApiClient") as mock_client_cls:
            mock_client = MagicMock()
            mock_client.post.side_effect = Exception("Rate limit exceeded")
            mock_client_cls.return_value = mock_client

            client = QurlClient("test", "https://u", "https://m")
            with pytest.raises(MintError) as exc:
                client.mint_link("r_abc", "bob@example.com")
            assert "Rate limit exceeded" in str(exc.value)

    def test_get_qurl_client_defaults(self):
        """Test get_qurl_client uses default URLs"""
        from services.qurl_client import get_qurl_client

        with patch("services.qurl_client.QurlApiClient") as mock_cls:
            mock_cls.return_value = MagicMock()
            client = get_qurl_client(api_key="my-key")
            assert client is not None
            assert client.region == "us-east-1"
