"""
QURL API client module.

Provides file upload, Qurl creation, and link minting functionality.
"""

import hashlib
import io
import logging
from dataclasses import dataclass
from typing import BinaryIO

from .http_client import QurlApiClient

logger = logging.getLogger(__name__)


@dataclass
class UploadedResource:
    """Uploaded resource info"""
    resource_id: str
    filename: str
    content_type: str
    size: int
    hash: str  # for idempotency check


@dataclass
class MintedLink:
    """Minted link info"""
    link_id: str
    url: str
    hash: str  # for logging
    expires_at: str


class QurlApiError(Exception):
    """QURL API base error"""
    pass


class UploadError(QurlApiError):
    """File upload error"""
    pass


class CreateError(QurlApiError):
    """Qurl creation error"""
    pass


class MintError(QurlApiError):
    """Link minting error"""
    pass


class QurlClient:
    """QURL API client"""

    def __init__(
        self,
        api_key: str,
        upload_api_url: str,
        mint_link_api_url: str,
        region: str = "us-east-1",
    ):
        self.upload_client = QurlApiClient(api_key, upload_api_url)
        self.mint_client = QurlApiClient(api_key, mint_link_api_url)
        self.region = region

    def upload_file(
        self,
        file_bytes: bytes | BinaryIO,
        filename: str,
        content_type: str,
        owner_id: str,
        label: str = "",
    ) -> UploadedResource:
        """
        Upload file to QURL API.

        Args:
            file_bytes: File content or file object
            filename: File name
            content_type: MIME type
            owner_id: Owner ID
            label: Label (for tracing)

        Returns:
            UploadedResource: Uploaded resource info

        Raises:
            UploadError: Raised when upload fails
        """
        try:
            if hasattr(file_bytes, "read"):
                content = file_bytes.read()
                file_bytes.seek(0)
            else:
                content = file_bytes

            file_hash = hashlib.sha256(content).hexdigest()[:16]

            files = {
                "file": (filename, io.BytesIO(content) if isinstance(content, bytes) else content, content_type),
            }
            data = {
                "owner_id": owner_id,
                "label": label or f"email:{owner_id}",
            }

            response = self.upload_client.post("/upload", data=data, files=files)

            return UploadedResource(
                resource_id=response["resource_id"],
                filename=filename,
                content_type=content_type,
                size=len(content) if isinstance(content, bytes) else 0,
                hash=file_hash,
            )

        except Exception as e:
            logger.error(f"File upload failed: {e}")
            raise UploadError(f"File upload failed: {e}") from e

    def create_qurl(
        self,
        target_url: str,
        one_time_use: bool = True,
        label: str = "",
    ) -> UploadedResource:
        """
        Create a Qurl (wrapping an external URL).

        Args:
            target_url: Target URL
            one_time_use: Whether it's one-time use
            label: Label

        Returns:
            UploadedResource: Created resource info

        Raises:
            CreateError: Raised when creation fails
        """
        try:
            data = {
                "target_url": target_url,
                "one_time_use": one_time_use,
                "label": label,
            }

            response = self.mint_client.post("/v1/qurls", data=data)

            return UploadedResource(
                resource_id=response["id"],
                filename=target_url,
                content_type="text/url",
                size=len(target_url),
                hash=hashlib.sha256(target_url.encode()).hexdigest()[:16],
            )

        except Exception as e:
            logger.error(f"Qurl creation failed: {e}")
            raise CreateError(f"Qurl creation failed: {e}") from e

    def mint_link(
        self,
        resource_id: str,
        recipient_id: str,
        one_time_use: bool = True,
        label: str = "",
        expires_in: str = "15m",
    ) -> MintedLink:
        """
        Mint a one-time use link for a recipient.

        Args:
            resource_id: Resource ID
            recipient_id: Recipient ID (email)
            one_time_use: Whether it's one-time use
            label: Label
            expires_in: Expiration time

        Returns:
            MintedLink: Minted link info

        Raises:
            MintError: Raised when minting fails
        """
        try:
            data = {
                "recipient_id": recipient_id,
                "one_time_use": one_time_use,
                "label": label,
                "expires_in": expires_in,
            }

            response = self.mint_client.post(f"/v1/qurls/{resource_id}/mint_link", data=data)

            return MintedLink(
                link_id=response["id"],
                url=response["url"],
                hash=hashlib.sha256(response["url"].encode()).hexdigest()[:16],
                expires_at=response.get("expires_at", ""),
            )

        except Exception as e:
            logger.error(f"Link minting failed: {e}")
            raise MintError(f"Link minting failed: {e}") from e


def get_qurl_client(
    api_key: str = "",
    upload_api_url: str = "https://getqurllink.layerv.ai",
    mint_link_api_url: str = "https://api.layerv.ai/v1/qurls",
    region: str = "us-east-1",
) -> QurlClient:
    """
    Get QURL client instance.

    Can directly fetch API key from SSM:
    ssm = boto3.client("ssm")
    api_key = ssm.get_parameter(Name="/qurl-email-bot/qurl-api-key", WithDecryption=True)["Parameter"]["Value"]
    """
    return QurlClient(
        api_key=api_key,
        upload_api_url=upload_api_url,
        mint_link_api_url=mint_link_api_url,
        region=region,
    )
