"""
Sender authentication module.

MVP phase: SSM whitelist for sender validation.
Future phase: QURL API customer lookup.
"""

import json
import logging
from dataclasses import dataclass
from typing import Optional

import boto3
from botocore.exceptions import ClientError

from config import Settings, get_settings

logger = logging.getLogger(__name__)


@dataclass
class CustomerInfo:
    """Customer information"""
    owner_id: str
    email: str
    tier: str  # system, growth, enterprise, free


class AuthenticationError(Exception):
    """Authentication error"""
    pass


class NotAuthorizedError(AuthenticationError):
    """Not authorized error"""
    pass


def load_authorized_senders_from_ssm(settings: Settings | None = None) -> set[str]:
    """
    Load authorized sender list from SSM Parameter Store.

    Args:
        settings: Settings object, uses default if None

    Returns:
        set[str]: Set of authorized email addresses
    """
    if settings is None:
        settings = get_settings()

    try:
        ssm = boto3.client("ssm", region_name=settings.aws_region)
        response = ssm.get_parameter(
            Name=settings.authorized_senders_param,
            WithDecryption=False,
        )
        value = response["Parameter"]["Value"]

        if not value:
            return set()

        try:
            senders = json.loads(value)
            if isinstance(senders, list):
                return {s.lower().strip() for s in senders if s}
        except json.JSONDecodeError:
            logger.warning(f"Failed to parse SSM parameter value: {value}")
            return set()

    except ClientError as e:
        logger.error(f"Failed to load authorized senders from SSM: {e}")
        return set()

    return set()


def load_api_key_from_ssm(settings: Settings | None = None) -> str:
    """
    Load QURL API key from SSM Parameter Store.

    Args:
        settings: Settings object, uses default if None

    Returns:
        str: API key
    """
    if settings is None:
        settings = get_settings()

    try:
        ssm = boto3.client("ssm", region_name=settings.aws_region)
        response = ssm.get_parameter(
            Name=settings.qurl_api_key_param,
            WithDecryption=True,
        )
        return response["Parameter"]["Value"]
    except ClientError as e:
        logger.error(f"Failed to load API key from SSM: {e}")
        return ""


def authenticate_sender(sender_email: str, settings: Settings | None = None) -> Optional[CustomerInfo]:
    """
    Authenticate a sender.

    MVP phase: checks if sender email is in SSM whitelist.

    Future phase (Option A): call QURL API customer lookup endpoint
    - GET /v1/customers/by-email?email=...
    - Requires service token
    - Returns tier and owner_id

    Args:
        sender_email: Sender email address
        settings: Settings object

    Returns:
        Optional[CustomerInfo]: Customer info, or None if auth fails
    """
    if settings is None:
        settings = get_settings()

    email = sender_email.lower().strip()

    authorized_senders = load_authorized_senders_from_ssm(settings)

    if not authorized_senders:
        logger.warning("Authorized sender list is empty, check SSM parameter configuration")

    if email not in authorized_senders:
        logger.info(f"Sender {email} not in authorized list")
        return None

    # MVP: return simplified customer info
    # Future: fetch real owner_id and tier from API
    return CustomerInfo(
        owner_id=f"email:{email}",  # MVP uses email as temporary owner_id
        email=email,
        tier="growth",  # MVP assumes all authorized users are on growth plan
    )


def authenticate_sender_with_api(sender_email: str, api_key: str, settings: Settings | None = None) -> Optional[CustomerInfo]:
    """
    Authenticate sender via QURL API (for future phases).

    Requires QURL API to support GET /v1/customers/by-email endpoint.

    Args:
        sender_email: Sender email address
        api_key: QURL API key
        settings: Settings object

    Returns:
        Optional[CustomerInfo]: Customer info, or None if auth fails
    """
    if settings is None:
        settings = get_settings()

    try:
        import httpx

        email = sender_email.lower().strip()
        url = f"{settings.mint_link_api_url.rsplit('/', 1)[0]}/customers/by-email"
        headers = {"Authorization": f"Bearer {api_key}"}

        response = httpx.get(url, params={"email": email}, headers=headers, timeout=10.0)
        response.raise_for_status()

        data = response.json()

        if data.get("frozen", False):
            return None

        tier = data.get("tier", "free")
        if tier == "free":
            return None

        return CustomerInfo(
            owner_id=data["auth0_subject"],
            email=sender_email,
            tier=tier,
        )

    except Exception as e:
        logger.error(f"API authentication failed: {e}")
        return None


def check_spf_dkim(msg) -> tuple[bool, bool]:
    """
    Check SPF and DKIM verification results for an email.

    SES adds X-SES-Spam-Verdict and Authentication-Results headers.

    Args:
        msg: email.message.EmailMessage object

    Returns:
        tuple[bool, bool]: (spf_pass, dkim_pass)
    """
    spf_header = msg.get("Received-SPF", "")
    auth_header = msg.get("Authentication-Results", "")

    spf_pass = "pass" in spf_header.lower() if spf_header else False
    dkim_pass = "pass" in auth_header.lower() if auth_header else False

    return spf_pass, dkim_pass


def verify_email_authentication(msg) -> bool:
    """
    Verify email authentication result.

    Requires SPF or DKIM to pass (at least one).

    Args:
        msg: email.message.EmailMessage object

    Returns:
        bool: Whether verification passed
    """
    spf_pass, dkim_pass = check_spf_dkim(msg)
    return spf_pass or dkim_pass
