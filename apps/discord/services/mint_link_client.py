"""Client for mint_link API (generate per-recipient QURL link)."""

from __future__ import annotations

import logging
from urllib.parse import urlparse

from config import settings
from services.http_client import get_client
from validation import DEFAULT_LINK_EXPIRY

logger = logging.getLogger(__name__)


async def mint_link(
    resource_id: str, recipient_id: str, expires_in: str | None = None
) -> dict:
    """
    Mint a single-use QURL link for a recipient.

    POST to {mint_link_api_url}/{resource_id}/mint_link
    Auth: Authorization: Bearer <QURL_API_KEY>

    Args:
        resource_id: QURL resource ID (e.g. r_abc123def)
        recipient_id: Discord user ID of the recipient
        expires_in: Optional duration (e.g. "15m", "1h", "24h"). Defaults to settings.link_expires_in.

    Returns:
        dict with keys: qurl_link, expires_at

    Raises:
        Exception on mint failure
    """
    base = settings.mint_link_api_url.rstrip("/")
    url = f"{base}/{resource_id}/mint_link"

    headers = {
        "Authorization": f"Bearer {settings.qurl_api_key}",
        "Content-Type": "application/json",
    }

    payload = {
        "expires_in": expires_in or DEFAULT_LINK_EXPIRY,
        "one_time_use": True,
        "label": f"discord:{recipient_id}",
    }

    client = get_client(timeout=10.0)
    resp = await client.post(url, headers=headers, json=payload)

    resp.raise_for_status()

    body = resp.json()

    # Support nested "data" envelope
    data = body.get("data", body)

    qurl_link = data.get("qurl_link") or data.get("qurlLink")
    if not qurl_link:
        raise ValueError("Mint link response missing qurl_link")

    # Validate qurl_link hostname (configurable)
    parsed = urlparse(qurl_link)
    if parsed.scheme != "https" or parsed.hostname != settings.qurl_link_hostname:
        raise ValueError("Mint API returned invalid qurl_link")

    return {
        "qurl_link": qurl_link,
        "expires_at": data.get("expires_at") or data.get("expiresAt", ""),
    }
