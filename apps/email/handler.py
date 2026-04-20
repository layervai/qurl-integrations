"""
Lambda entry point.

Processes email events from SQS.
"""

import email
import email.policy
import json
import logging
import os
from email.headerregistry import Address

import boto3

from auth import authenticate_sender, verify_email_authentication
from config import get_settings
from db import log_dispatch, check_idempotent, DispatchStatus
from rate_limiter import get_rate_limiter
from email_parser import (
    parse_recipients,
    extract_attachments,
    extract_urls,
    get_body_text,
    validate_attachment,
)
from email_sender import send_link_email, send_confirmation, send_rejection, send_usage_help
from services.qurl_client import get_qurl_client, UploadError, CreateError, MintError

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
)
logger = logging.getLogger(__name__)

# Global clients (Lambda container reuse)
_s3_client = None
_qurl_client = None
_rate_limiter = None


def get_s3_client():
    """Get S3 client"""
    global _s3_client
    if _s3_client is None:
        settings = get_settings()
        _s3_client = boto3.client("s3", region_name=settings.aws_region)
    return _s3_client


def get_qurl():
    """Get QURL client"""
    global _qurl_client
    if _qurl_client is None:
        settings = get_settings()
        api_key = settings.qurl_api_key or os.environ.get("QURL_API_KEY", "")
        _qurl_client = get_qurl_client(
            api_key=api_key,
            upload_api_url=settings.upload_api_url,
            mint_link_api_url=settings.mint_link_api_url,
            region=settings.aws_region,
        )
    return _qurl_client


def get_limiter():
    """Get rate limiter"""
    global _rate_limiter
    if _rate_limiter is None:
        _rate_limiter = get_rate_limiter()
    return _rate_limiter


def handler(event, context):
    """
    Lambda entry function.

    Args:
        event: SQS event
        context: Lambda context

    Returns:
        dict: Processing result
    """
    logger.info(f"Received event: {len(event.get('Records', []))} records")

    settings = get_settings()
    results = []

    for record in event.get("Records", []):
        try:
            result = process_sqs_record(record, settings)
            results.append({"status": "success", "result": result})
        except Exception as e:
            logger.error(f"Failed to process record: {e}")
            results.append({"status": "error", "error": str(e)})

    return {"processed": len(results), "results": results}


def process_sqs_record(record, settings):
    """
    Process a single SQS record.

    Args:
        record: SQS record
        settings: Settings object

    Returns:
        dict: Processing result
    """
    s3_client = get_s3_client()
    qurl = get_qurl()

    # 1. Parse S3 event
    s3_event = json.loads(record["body"])
    bucket = s3_event["Records"][0]["s3"]["bucket"]["name"]
    key = s3_event["Records"][0]["s3"]["object"]["key"]

    logger.info(f"Processing S3 object: bucket={bucket}, key={key}")

    # 2. Read raw email from S3
    obj = s3_client.get_object(Bucket=bucket, Key=key)
    raw = obj["Body"].read()

    # 3. Parse email
    msg = email.message_from_bytes(raw, policy=email.policy.default)

    # 4. Extract sender
    sender_header = msg["From"]
    if sender_header:
        try:
            addr = Address(addr_spec=sender_header)
            sender_addr = addr.addr_spec.lower()
            sender_name = addr.display_name or addr.username or sender_addr
        except Exception:
            sender_addr = sender_header.lower()
            sender_name = sender_addr
    else:
        sender_addr = ""
        sender_name = ""

    logger.info(f"Sender: {sender_addr}")

    # 5. Authenticate sender
    customer = authenticate_sender(sender_addr, settings)
    if customer is None:
        logger.warning(f"Sender not authorized: {sender_addr}")
        send_rejection(sender_addr, reason="not_authorized")
        cleanup_s3(s3_client, bucket, key)
        return {"status": "rejected", "reason": "not_authorized"}

    # 6. Verify SPF/DKIM
    if not verify_email_authentication(msg):
        logger.warning(f"Email authentication failed: {sender_addr}")
        send_rejection(sender_addr, reason="auth_failed")
        cleanup_s3(s3_client, bucket, key)
        return {"status": "rejected", "reason": "auth_failed"}

    # 7. Check rate limit
    limiter = get_limiter()
    rate_result = limiter.check(sender_addr)
    if not rate_result.allowed:
        logger.warning(f"Rate limit exceeded for: {sender_addr}")
        send_rejection(sender_addr, reason="rate_limited")
        cleanup_s3(s3_client, bucket, key)
        return {"status": "rejected", "reason": "rate_limited"}

    # 8. Parse recipients
    body_text = get_body_text(msg)
    recipients = parse_recipients(body_text, sender_addr, settings.bot_address)

    if not recipients:
        logger.info("No recipients found")
        send_usage_help(sender_addr)
        cleanup_s3(s3_client, bucket, key)
        return {"status": "no_recipients"}

    # Limit recipient count
    if len(recipients) > settings.max_recipients:
        logger.info(f"Recipients exceed limit, truncating to {settings.max_recipients}")
        recipients = recipients[:settings.max_recipients]

    # 9. Extract resources (attachments take priority)
    attachments = extract_attachments(msg)
    resources = []

    if attachments:
        for att in attachments:
            valid, error = validate_attachment(
                att.filename,
                att.content_type,
                att.size,
                settings.max_attachment_size_mb,
            )
            if not valid:
                logger.warning(f"Attachment validation failed: {att.filename} - {error}")
                continue

            try:
                resource = qurl.upload_file(
                    file_bytes=att.content,
                    filename=att.filename,
                    content_type=att.content_type,
                    owner_id=customer.owner_id,
                    label=f"email:{sender_addr}",
                )
                resources.append(resource)
            except UploadError as e:
                logger.error(f"Upload failed: {att.filename} - {e}")

    if not resources:
        # No attachments, try extracting URLs from body
        urls = extract_urls(body_text)

        if not urls:
            logger.info("No attachments and no URLs")
            send_usage_help(sender_addr)
            cleanup_s3(s3_client, bucket, key)
            return {"status": "no_resource"}

        # Limit URL count
        urls = urls[:settings.max_urls_per_email]

        for url in urls:
            try:
                resource = qurl.create_qurl(
                    target_url=url,
                    one_time_use=True,
                    label=f"email:{sender_addr}",
                )
                resources.append(resource)
            except CreateError as e:
                logger.error(f"Failed to create Qurl: {url} - {e}")

    # 10. Mint link and send email for each recipient
    results = []
    dispatched = False
    for resource in resources:
        for recipient in recipients:
            if check_idempotent(resource.resource_id, recipient):
                logger.info(f"Skipping already sent: resource={resource.resource_id}, recipient={recipient}")
                log_dispatch(
                    resource_id=resource.resource_id,
                    sender_email=sender_addr,
                    recipient_email=recipient,
                    status=DispatchStatus.SKIPPED,
                )
                results.append({"recipient": recipient, "status": "skipped"})
                continue

            try:
                link = qurl.mint_link(
                    resource_id=resource.resource_id,
                    recipient_id=recipient,
                    one_time_use=True,
                    label=f"email:{recipient}",
                    expires_in=settings.link_expires_in,
                )

                send_link_email(
                    to=recipient,
                    sender_name=sender_name,
                    sender_email=sender_addr,
                    resource_name=resource.filename,
                    link_url=link.url,
                    expires_in=settings.link_expires_in,
                )

                log_dispatch(
                    resource_id=resource.resource_id,
                    sender_email=sender_addr,
                    recipient_email=recipient,
                    status=DispatchStatus.SENT,
                    link_id_hash=link.hash,
                )

                results.append({"recipient": recipient, "status": "sent"})
                logger.info(f"Sent successfully: {recipient}")
                dispatched = True

            except MintError as e:
                logger.error(f"Failed to mint link: {recipient} - {e}")
                log_dispatch(
                    resource_id=resource.resource_id,
                    sender_email=sender_addr,
                    recipient_email=recipient,
                    status=DispatchStatus.MINT_FAILED,
                    error=str(e),
                )
                results.append({"recipient": recipient, "status": "mint_failed", "error": str(e)})

            except Exception as e:
                logger.error(f"Failed to send email: {recipient} - {e}")
                log_dispatch(
                    resource_id=resource.resource_id,
                    sender_email=sender_addr,
                    recipient_email=recipient,
                    status=DispatchStatus.SEND_FAILED,
                    error=str(e),
                )
                results.append({"recipient": recipient, "status": "send_failed", "error": str(e)})

    # Increment rate limit counter on successful dispatch
    if dispatched:
        limiter.increment(sender_addr)

    # 11. Send confirmation to sender
    resource_name = ", ".join(r.filename for r in resources)
    send_confirmation(
        to=sender_addr,
        sender_name=sender_name,
        resource_name=resource_name,
        results=results,
    )

    # 11. Cleanup S3
    cleanup_s3(s3_client, bucket, key)

    return {
        "status": "processed",
        "recipients": len(recipients),
        "resources": len(resources),
        "results": results,
    }


def cleanup_s3(s3_client, bucket: str, key: str):
    """
    Delete raw email from S3.

    Args:
        s3_client: S3 client
        bucket: Bucket name
        key: Object key
    """
    try:
        s3_client.delete_object(Bucket=bucket, Key=key)
        logger.info(f"Deleted S3 object: {bucket}/{key}")
    except Exception as e:
        logger.error(f"Failed to delete S3 object: {e}")
