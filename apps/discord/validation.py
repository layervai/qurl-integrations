"""Input validation utilities."""

from __future__ import annotations

import os
import re
from urllib.parse import urlparse

# --- Link expiry constants ---
DEFAULT_LINK_EXPIRY = "15m"

EXPIRY_CHOICES_VALUES = {"5m", "15m", "1h", "24h", "7d"}


def validate_expires(value: str) -> bool:
    """Validate that the expiry value is one of the allowed choices."""
    return isinstance(value, str) and value in EXPIRY_CHOICES_VALUES

# Allowed MIME types for file uploads
ALLOWED_CONTENT_TYPES = {
    "image/png",
    "image/jpeg",
    "image/gif",
    "image/webp",
    "application/pdf",
}

# Maps uploads use application/json but bypass validate_file_type
# (they go through _handle_maps_url, not the DM upload path).
# Do NOT add application/json to ALLOWED_CONTENT_TYPES — it would
# let users DM arbitrary .json files through the upload flow.

# Allowed file extensions (fallback when content_type is missing)
ALLOWED_EXTENSIONS = {
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".webp",
    ".pdf",
}

# Resource ID regex: r_ followed by 6-32 alphanumeric/underscore/hyphen chars
_RESOURCE_ID_RE = re.compile(r"^r_[a-zA-Z0-9_-]{6,32}$")

# Discord snowflake: 17-20 digit integer
SNOWFLAKE_RE = re.compile(r"^\d{17,20}$")


def validate_resource_id(text: str) -> str | None:
    """
    Validate a resource ID string.

    Returns the cleaned resource_id if valid, or None if invalid.
    """
    text = text.strip()
    if _RESOURCE_ID_RE.match(text):
        return text
    return None


def validate_snowflake(uid: str) -> bool:
    """Validate that a string is a valid Discord snowflake ID (17-20 digits)."""
    return bool(SNOWFLAKE_RE.match(uid))


def validate_file_size(size_bytes: int, max_mb: int = 25) -> bool:
    """Check if file size is within the allowed limit."""
    return 0 < size_bytes <= max_mb * 1024 * 1024


def validate_file_type(content_type: str | None, filename: str | None) -> bool:
    """
    Validate file type by content_type and/or filename extension.

    Returns True if the file type is allowed.

    NOTE: MIME type is from Discord metadata (user-controlled). No magic-byte validation
    is performed. The upstream file viewer should perform its own content-type verification.
    """
    # Check content_type first
    if content_type:
        # content_type may include params like "image/png; charset=utf-8"
        mime = content_type.split(";")[0].strip().lower()
        if mime in ALLOWED_CONTENT_TYPES:
            return True

    # Fallback to extension check
    if filename:
        _, ext = os.path.splitext(filename)
        if ext.lower() in ALLOWED_EXTENSIONS:
            return True

    return False


def sanitize_filename(filename: str) -> str:
    """
    Sanitize a filename for safe storage.

    - Strip path separators and null bytes
    - Limit to 255 characters
    - Replace problematic characters
    """
    if not filename:
        return "unnamed"

    # Remove null bytes
    filename = filename.replace("\x00", "")

    # Extract just the filename (strip any path components)
    filename = filename.replace("\\", "/")
    filename = filename.split("/")[-1]

    # Remove control characters
    filename = re.sub(r"[\x00-\x1f\x7f]", "", filename)

    # Replace sequences of whitespace with single space
    filename = re.sub(r"\s+", " ", filename).strip()

    # Limit length
    if len(filename) > 255:
        name, ext = os.path.splitext(filename)
        filename = name[: 255 - len(ext)] + ext

    if not filename:
        return "unnamed"

    return filename


def validate_cdn_url(url: str, allowlist: str) -> bool:
    """
    Validate that a URL comes from an allowed CDN domain (SSRF protection).

    Args:
        url: The URL to validate
        allowlist: Comma-separated list of allowed domains
    """
    if not url:
        return False

    try:
        parsed = urlparse(url)
    except Exception:
        return False

    # Must be HTTPS
    if parsed.scheme != "https":
        return False

    hostname = parsed.hostname
    if not hostname:
        return False

    allowed_domains = [d.strip().lower() for d in allowlist.split(",") if d.strip()]
    hostname_lower = hostname.lower()

    for domain in allowed_domains:
        if hostname_lower == domain or hostname_lower.endswith("." + domain):
            return True

    return False


def split_message(text: str, max_len: int = 2000) -> list[str]:
    """Split a message into chunks that fit within Discord's character limit."""
    if len(text) <= max_len:
        return [text]
    parts = []
    while text:
        if len(text) <= max_len:
            parts.append(text)
            break
        split = text.rfind("\n", 0, max_len)
        if split <= 0:
            split = max_len
        parts.append(text[:split])
        text = text[split:].lstrip("\n")
    return parts
