"""
Email parsing module.

Parses MIME emails: extracts sender, recipients, body, URLs, and attachments.
"""

import base64
import email
import logging
import re
from dataclasses import dataclass
from typing import Optional

logger = logging.getLogger(__name__)

# Email regex (simplified RFC 5322)
EMAIL_RE = re.compile(r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}")

# Recipient block regex: finds "Send to:" / "Share with:" / "Recipients:" blocks.
# Uses re.MULTILINE so ^/$ match line boundaries (not DOTALL which breaks lookahead).
# Pattern: matches header line, then any non-blank lines until a blank line or end.
RECIPIENT_BLOCK_RE = re.compile(
    r"(?:send\s+to|share\s+with|recipients?)\s*:\s*([^\n]*(?:\n(?![ \t]*\n)[^\n]*)*)",
    re.IGNORECASE | re.MULTILINE,
)

# URL regex (excludes qurl.link links, which are already Qurls)
URL_RE = re.compile(
    r"https?://(?!qurl\.link)[^\s<>\"\']+",
    re.IGNORECASE,
)

# Signature delimiters (standard "-- " and markdown "---")
SIGNATURE_DELIMITERS = ["\n-- \n", "\n--\n", "\n---\n"]


@dataclass
class Attachment:
    """Email attachment"""
    filename: str
    content_type: str
    content: bytes
    size: int


@dataclass
class ParsedEmail:
    """Parsed email structure"""
    sender_name: str
    sender_email: str
    recipients: list[str]
    subject: str
    body_text: str
    body_html: Optional[str] = None
    attachments: list[Attachment] = None


def get_body_text(msg) -> str:
    """
    Extract plain text body from email.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        str: Plain text body content
    """
    body = ""

    if msg.is_multipart():
        for part in msg.walk():
            content_type = part.get_content_type()
            content_disposition = part.get_content_disposition()

            if content_disposition in ("attachment", "inline"):
                continue

            if content_type == "text/plain":
                charset = part.get_content_charset() or "utf-8"
                try:
                    body = part.get_payload(decode=True).decode(charset, errors="replace")
                    break
                except Exception as e:
                    logger.warning(f"Failed to decode plain text part: {e}")
                    continue
    else:
        charset = msg.get_content_charset() or "utf-8"
        try:
            body = msg.get_payload(decode=True).decode(charset, errors="replace")
        except Exception as e:
            logger.warning(f"Failed to decode email body: {e}")
            body = ""

    return body


def get_body_html(msg) -> Optional[str]:
    """
    Extract HTML body from email.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        Optional[str]: HTML body content, or None if not found
    """
    if msg.is_multipart():
        for part in msg.walk():
            content_type = part.get_content_type()
            content_disposition = part.get_content_disposition()

            if content_disposition in ("attachment", "inline"):
                continue

            if content_type == "text/html":
                charset = part.get_content_charset() or "utf-8"
                try:
                    return part.get_payload(decode=True).decode(charset, errors="replace")
                except Exception as e:
                    logger.warning(f"Failed to decode HTML part: {e}")
                    continue

    return None


def get_sender(msg) -> tuple[str, str]:
    """
    Extract sender information from email.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        tuple[str, str]: (display name, email address)
    """
    from_header = msg.get("From", "")

    try:
        addr = email.headerregistry.Address(addr_spec=from_header)
        name = addr.username or ""
        email_addr = addr.addr_spec or from_header
    except Exception:
        # Try parsing directly from string
        match = EMAIL_RE.search(from_header)
        if match:
            email_addr = match.group(0)
            name = from_header.replace(f"<{email_addr}>", "").strip()
        else:
            email_addr = from_header
            name = ""

    return name, email_addr.lower().strip()


def parse_recipients(body: str, sender: str, bot_address: str) -> list[str]:
    """
    Parse recipient email addresses from email body.

    Strategy:
    1. First, look for explicit "Send to:" / "Share with:" / "Recipients:" blocks
    2. If not found, fall back to scanning body above signature delimiter

    Args:
        body: Email body text
        sender: Sender email address
        bot_address: Bot email address

    Returns:
        list[str]: List of recipient email addresses (deduplicated and normalized)
    """
    emails = []

    # First: try to find explicit recipient blocks
    match = RECIPIENT_BLOCK_RE.search(body)
    if match:
        block = match.group(1)
        # Collapse continuation lines (lines ending with non-blank content
        # followed by another line) into single space-separated strings so
        # inline comma-separated lists work.
        block_normalized = re.sub(r"\n(?=[^ \t\n])", " ", block)
        emails = EMAIL_RE.findall(block_normalized)
    else:
        # Fallback: scan full body but only above signature delimiter
        body_above_sig = body
        for delimiter in SIGNATURE_DELIMITERS:
            if delimiter in body_above_sig:
                body_above_sig = body_above_sig.split(delimiter)[0]
                break

        emails = EMAIL_RE.findall(body_above_sig)

    # Normalize, deduplicate, exclude sender and bot address
    exclude = {sender.lower(), bot_address.lower()}
    seen = set()
    result = []

    for email_addr in emails:
        e = email_addr.lower().strip()
        if e not in exclude and e not in seen:
            seen.add(e)
            result.append(e)

    return result


def extract_urls(body: str) -> list[str]:
    """
    Extract URLs from email body.

    Excludes qurl.link links as they are already Qurls.

    Args:
        body: Email body text

    Returns:
        list[str]: List of URLs
    """
    urls = URL_RE.findall(body)

    # Clean URLs (remove trailing punctuation)
    cleaned_urls = []
    for url in urls:
        while url and url[-1] in ".,;:!?\"":
            url = url[:-1]
        if url:
            cleaned_urls.append(url)

    return cleaned_urls


def extract_attachments(msg) -> list[Attachment]:
    """
    Extract attachments from email.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        list[Attachment]: List of attachments
    """
    attachments = []

    if msg.is_multipart():
        for part in msg.walk():
            content_disposition = part.get_content_disposition()

            if content_disposition == "attachment":
                attachment = _extract_single_attachment(part)
                if attachment:
                    attachments.append(attachment)
            elif content_disposition == "inline":
                filename = part.get_filename()
                if filename and not part.get_content_type().startswith("text/"):
                    attachment = _extract_single_attachment(part)
                    if attachment:
                        attachments.append(attachment)
    else:
        filename = msg.get_filename()
        if filename:
            attachment = _extract_single_attachment(msg)
            if attachment:
                attachments.append(attachment)

    return attachments


def _extract_single_attachment(part) -> Optional[Attachment]:
    """
    Extract attachment from a single email part.

    Args:
        part: Sub-part of email.message.EmailMessage

    Returns:
        Optional[Attachment]: Attachment object, or None if no valid content
    """
    filename = part.get_filename()

    if not filename:
        # Try getting from Content-Disposition
        filename = part.get_param("filename", header="Content-Disposition")

    if not filename:
        return None

    content_type = part.get_content_type()
    payload = part.get_payload(decode=True)

    if payload is None:
        return None

    if isinstance(payload, str):
        # May be base64 encoded
        try:
            payload = base64.b64decode(payload)
        except Exception:
            return None

    return Attachment(
        filename=filename,
        content_type=content_type,
        content=payload,
        size=len(payload),
    )


def parse_email(msg) -> ParsedEmail:
    """
    Parse a complete email.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        ParsedEmail: Parsed email structure
    """
    sender_name, sender_email = get_sender(msg)
    body_text = get_body_text(msg)
    body_html = get_body_html(msg)
    attachments = extract_attachments(msg)

    subject = msg.get("Subject", "")

    return ParsedEmail(
        sender_name=sender_name,
        sender_email=sender_email,
        recipients=[],  # to be filled by caller using parse_recipients
        subject=subject,
        body_text=body_text,
        body_html=body_html,
        attachments=attachments,
    )


def validate_attachment(filename: str, content_type: str, size: int, max_size_mb: int = 25) -> tuple[bool, str]:
    """
    Validate whether an attachment is allowed to be processed.

    Args:
        filename: File name
        content_type: MIME type
        size: File size in bytes
        max_size_mb: Max allowed size in MB

    Returns:
        tuple[bool, str]: (is_valid, error_message)
    """
    max_size_bytes = max_size_mb * 1024 * 1024
    if size > max_size_bytes:
        return False, f"File size exceeds limit (max {max_size_mb}MB)"

    import os
    ext = os.path.splitext(filename.lower())[1]
    allowed_extensions = [
        ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".docx", ".xlsx"
    ]
    if ext not in allowed_extensions:
        return False, f"Unsupported file type ({ext})"

    return True, ""
