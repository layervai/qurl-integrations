"""Client for file upload API (POST /upload)."""

import logging
from urllib.parse import urlparse

import httpx

from config import settings
from validation import validate_resource_id

logger = logging.getLogger(__name__)


async def upload_file(
    file_bytes: bytes,
    filename: str,
    content_type: str,
    owner_id: str,
) -> dict:
    """
    Upload a file to the QURL upload API.

    POST multipart/form-data to {upload_api_url}/upload
    Auth: Authorization: Bearer <QURL_API_KEY>

    Args:
        file_bytes: Raw file content
        filename: Original filename (sanitized)
        content_type: MIME type
        owner_id: Discord user ID for owner labeling

    Returns:
        dict with keys: resource_id, qurl_link, expires_at

    Raises:
        Exception on upload failure
    """
    base = settings.upload_api_url.rstrip("/")
    url = f"{base}/upload"

    headers = {
        "Authorization": f"Bearer {settings.qurl_api_key}",
    }

    files = {
        "file": (filename, file_bytes, content_type),
    }

    data = {
        "filename": filename,
        "owner_label": f"discord:{owner_id}",
        "content_type": content_type,
        "one_time_use": "true",
    }

    async with httpx.AsyncClient(timeout=30.0) as client:
        resp = await client.post(url, headers=headers, files=files, data=data)

    resp.raise_for_status()

    body = resp.json()

    # Support nested "data" envelope
    payload = body.get("data", body)

    resource_id = payload.get("resource_id") or payload.get("resourceId")
    qurl_link = payload.get("qurl_link") or payload.get("qurlLink")

    if not resource_id or not qurl_link:
        raise Exception("Upload response missing resource_id or qurl_link")

    # Validate resource_id format
    rid = resource_id if isinstance(resource_id, str) else str(resource_id)
    if not validate_resource_id(rid):
        raise ValueError("Upload returned invalid resource_id")

    # Validate qurl_link hostname (configurable)
    parsed = urlparse(qurl_link)
    if parsed.scheme != "https" or parsed.hostname != settings.qurl_link_hostname:
        raise ValueError("API returned qurl_link with unexpected hostname")

    return {
        "resource_id": rid,
        "qurl_link": qurl_link,
        "expires_at": payload.get("expires_at") or payload.get("expiresAt", ""),
    }
