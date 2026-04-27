"""
Email sending module.

Sends branded emails via SES.
"""

import html
import logging
import re
from pathlib import Path
from typing import Optional

import boto3
from botocore.exceptions import ClientError

from config import get_settings

logger = logging.getLogger(__name__)

# SES client (lazy initialization)
_ses_client = None


def get_ses_client():
    """Get SES client"""
    global _ses_client
    if _ses_client is None:
        settings = get_settings()
        _ses_client = boto3.client("ses", region_name=settings.aws_region)
    return _ses_client


class SESError(Exception):
    """SES send error"""
    pass


def load_template(template_name: str) -> str:
    """
    Load an email template.

    Args:
        template_name: Template name (without extension)

    Returns:
        str: Template content
    """
    template_dir = Path(__file__).parent / "templates"
    html_path = template_dir / f"{template_name}.html"
    txt_path = template_dir / f"{template_name}.txt"

    if html_path.exists():
        return html_path.read_text(encoding="utf-8")
    elif txt_path.exists():
        return txt_path.read_text(encoding="utf-8")
    else:
        logger.warning(f"Template not found: {template_name}")
        return ""


def render_template(template_str: str, **kwargs) -> str:
    """
    Render an email template.

    Args:
        template_str: Template string
        **kwargs: Template variables

    Returns:
        str: Rendered content
    """
    result = template_str
    for key, value in kwargs.items():
        placeholder = f"{{{{{key}}}}}"
        result = result.replace(placeholder, str(value))
    return result


def send_email(
    to_addresses: list[str],
    subject: str,
    html_body: str,
    text_body: Optional[str] = None,
    from_address: Optional[str] = None,
) -> dict:
    """
    Send an email.

    Args:
        to_addresses: List of recipient addresses
        subject: Email subject
        html_body: HTML body
        text_body: Plain text body (optional)
        from_address: From address (optional, defaults to bot address)

    Returns:
        dict: SES response

    Raises:
        SESError: Raised when sending fails
    """
    settings = get_settings()
    ses = get_ses_client()

    if from_address is None:
        from_address = settings.bot_address

    try:
        message = {
            "Subject": {
                "Data": subject,
                "Charset": "UTF-8",
            },
            "Body": {
                "Html": {
                    "Data": html_body,
                    "Charset": "UTF-8",
                },
            },
        }

        if text_body:
            message["Body"]["Text"] = {
                "Data": text_body,
                "Charset": "UTF-8",
            }

        response = ses.send_email(
            Source=from_address,
            Destination={
                "ToAddresses": to_addresses,
            },
            Message=message,
        )

        logger.info(f"Email sent successfully: to={to_addresses}, message_id={response.get('MessageId')}")
        return response

    except ClientError as e:
        logger.error(f"Failed to send email: {e}")
        raise SESError(f"Failed to send email: {e}") from e


def send_link_email(
    to: str,
    sender_name: str,
    sender_email: str,
    resource_name: str,
    link_url: str,
    expires_in: str = "15 minutes",
    from_address: Optional[str] = None,
) -> dict:
    """
    Send a branded email with Qurl link to recipient.

    Args:
        to: Recipient email address
        sender_name: Sender display name
        sender_email: Sender email address
        resource_name: Resource name (file name or URL)
        link_url: Qurl link
        expires_in: Link expiration time description
        from_address: From address (optional)

    Returns:
        dict: SES response
    """
    html_template = load_template("link_email")

    if not html_template:
        html_body = f"""
        <html>
        <body>
            <h2>{html.escape(sender_name)} shared a resource with you</h2>
            <p>Resource: <strong>{html.escape(resource_name)}</strong></p>
            <p>Click the link below to access (one-time use):</p>
            <p><a href="{html.escape(link_url)}">{html.escape(link_url)}</a></p>
            <p><small>Link expires in {expires_in}</small></p>
        </body>
        </html>
        """
        text_body = f"""
{sender_name} shared a resource with you

Resource: {resource_name}

Click the link below to access (one-time use):
{link_url}

Link expires in {expires_in}
        """
    else:
        html_body = render_template(
            html_template,
            sender_name=sender_name,
            sender_email=sender_email,
            resource_name=resource_name,
            link_url=link_url,
            expires_in=expires_in,
        )
        text_body = re.sub(r"<[^>]+>", "", html_body)
        text_body = html.unescape(text_body)

    subject = f"{sender_name} shared \"{resource_name}\" with you"

    return send_email(
        to_addresses=[to],
        subject=subject,
        html_body=html_body,
        text_body=text_body,
        from_address=from_address,
    )


def send_confirmation(
    to: str,
    sender_name: str,
    resource_name: str,
    results: list[dict],
    from_address: Optional[str] = None,
) -> dict:
    """
    Send dispatch summary confirmation email to sender.

    Args:
        to: Sender email address (also recipient of this email)
        sender_name: Sender display name
        resource_name: Resource name
        results: List of dispatch results
        from_address: From address (optional)

    Returns:
        dict: SES response
    """
    html_template = load_template("confirmation")

    sent = [r for r in results if r.get("status") == "sent"]
    skipped = [r for r in results if r.get("status") == "skipped"]
    failed = [r for r in results if r.get("status") not in ("sent", "skipped")]

    html_result_rows = ""
    for r in results:
        if r.get("status") == "sent":
            status_icon = "✓"
            status_text = "Success"
        elif r.get("status") == "skipped":
            status_icon = "○"
            status_text = "Skipped (already sent)"
        else:
            status_icon = "✗"
            status_text = "Failed"
        error_text = f" ({r.get('error', '')})" if r.get("error") else ""
        html_result_rows += f"""
        <tr>
            <td>{status_icon}</td>
            <td>{html.escape(r.get('recipient', ''))}</td>
            <td>{status_text}{html.escape(error_text)}</td>
        </tr>
        """

    if not html_template:
        html_body = f"""
        <html>
        <body>
            <h2>Qurl Dispatch Summary — {html.escape(resource_name)}</h2>

            <h3>Sent Successfully ({len(sent)})</h3>
            <ul>
                {"".join(f'<li>✓ {html.escape(r.get("recipient", ""))}</li>' for r in sent)}
            </ul>

            {"".join(f'''
            <h3>Skipped ({len(skipped)})</h3>
            <ul>
                {"".join(f'<li>○ {html.escape(r.get("recipient", ""))}</li>' for r in skipped)}
            </ul>
            ''') if skipped else ""}

            {"".join(f'''
            <h3>Failed ({len(failed)})</h3>
            <ul>
                {"".join(f'<li>✗ {html.escape(r.get("recipient", ""))} — {html.escape(r.get("error", ""))}</li>' for r in failed)}
            </ul>
            ''') if failed else ""}

            <p><small>Links expire in 15 minutes. All links are one-time use.</small></p>
        </body>
        </html>
        """
    else:
        html_body =         render_template(
            html_template,
            resource_name=resource_name,
            result_rows=html_result_rows,
            total_sent=len(sent),
            total_skipped=len(skipped),
            total_failed=len(failed),
        )

    text_lines = [
        f"Qurl Dispatch Summary — {resource_name}",
        "",
        f"Sent successfully ({len(sent)}):",
    ]
    for r in sent:
        text_lines.append(f"  ✓ {r.get('recipient', '')}")

    if skipped:
        text_lines.append("")
        text_lines.append(f"Skipped ({len(skipped)}) — already sent in a previous request:")
        for r in skipped:
            text_lines.append(f"  ○ {r.get('recipient', '')}")

    if failed:
        text_lines.append("")
        text_lines.append(f"Failed ({len(failed)}):")
        for r in failed:
            text_lines.append(f"  ✗ {r.get('recipient', '')} — {r.get('error', '')}")

    text_lines.extend([
        "",
        "Links expire in 15 minutes. All links are one-time use.",
    ])
    text_body = "\n".join(text_lines)

    subject = f"Qurl Dispatch Summary — {resource_name}"

    return send_email(
        to_addresses=[to],
        subject=subject,
        html_body=html_body,
        text_body=text_body,
        from_address=from_address,
    )


def send_rejection(
    to: str,
    reason: str,
    from_address: Optional[str] = None,
) -> dict:
    """
    Send rejection email to unauthorized sender.

    Args:
        to: Sender email address
        reason: Rejection reason
        from_address: From address (optional)

    Returns:
        dict: SES response
    """
    reason_messages = {
        "not_authorized": "Your email address is not authorized to use the Qurl email bot.",
        "auth_failed": "Email authentication failed, unable to process your request.",
        "frozen": "Your account has been frozen and cannot use this service.",
        "rate_limited": "You have exceeded the share limit. Please wait before trying again.",
    }

    message = reason_messages.get(reason, "Unable to process your request.")

    html_body = f"""
    <html>
    <body>
        <h2>Unable to Process Your Request</h2>
        <p>{message}</p>
        <p>If you believe this is an error, please contact support.</p>
    </body>
    </html>
    """

    text_body = f"""
Unable to Process Your Request

{message}

If you believe this is an error, please contact support.
    """

    subject = "Qurl Email Bot — Request Rejected"

    return send_email(
        to_addresses=[to],
        subject=subject,
        html_body=html_body,
        text_body=text_body,
        from_address=from_address,
    )


def send_usage_help(to: str, from_address: Optional[str] = None) -> dict:
    """
    Send usage instructions to sender.

    Args:
        to: Sender email address
        from_address: From address (optional)

    Returns:
        dict: SES response
    """
    html_body = """
    <html>
    <body>
        <h2>Welcome to the Qurl Email Bot</h2>

        <p>Use this bot to securely share files and links with others.</p>

        <h3>How to Use</h3>
        <ol>
            <li><strong>Send email to:</strong> qurl@layerv.ai</li>
            <li><strong>Subject:</strong> Anything (used in summary email)</li>
            <li><strong>Body:</strong>
                <pre>Send to:
bob@company.com
carol@company.com

Your message here.
                </pre>
            </li>
            <li><strong>Attachments:</strong> Attach files to share (PDF, images, Word, Excel, etc.)</li>
            <li><strong>Or:</strong> Paste URLs in the body (will be converted to one-time Qurls)</li>
        </ol>

        <h3>Examples</h3>
        <p><strong>Share a file:</strong> Attach a PDF and specify recipients in the body.</p>
        <p><strong>Share a link:</strong> Paste a URL in the body, the bot will convert it to a one-time Qurl.</p>

        <h3>Limits</h3>
        <ul>
            <li>Max 25 recipients per email</li>
            <li>Max 25MB per file</li>
            <li>Supported formats: PDF, PNG, JPG, GIF, WebP, DOCX, XLSX</li>
            <li>Links expire in 15 minutes and are one-time use</li>
        </ul>

        <p>If you have questions, please contact support.</p>
    </body>
    </html>
    """

    text_body = """
Welcome to the Qurl Email Bot

Use this bot to securely share files and links with others.

How to Use:

1. Send email to: qurl@layerv.ai
2. Subject: Anything (used in summary email)
3. Body:
   Send to:
   bob@company.com
   carol@company.com

   Your message here.
4. Attachments: Attach files to share (PDF, images, Word, Excel, etc.)
   Or: Paste URLs in the body (will be converted to one-time Qurls)

Examples:

Share a file: Attach a PDF and specify recipients in the body.
Share a link: Paste a URL in the body, the bot will convert it to a one-time Qurl.

Limits:

- Max 25 recipients per email
- Max 25MB per file
- Supported formats: PDF, PNG, JPG, GIF, WebP, DOCX, XLSX
- Links expire in 15 minutes and are one-time use

If you have questions, please contact support.
    """

    subject = "Qurl Email Bot — Usage Guide"

    return send_email(
        to_addresses=[to],
        subject=subject,
        html_body=html_body,
        text_body=text_body,
        from_address=from_address,
    )
