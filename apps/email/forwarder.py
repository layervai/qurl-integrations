"""
Email forwarding Lambda.

Handles personal email forwarding: rewrites headers to pass SPF and sends via SES.
Triggered by SES receipt rule (Lambda action), not S3.
"""

import email
import email.headerregistry
import email.policy
import json
import logging

import boto3
from botocore.exceptions import ClientError

from config import get_settings
from email_sender import SESError

logger = logging.getLogger(__name__)

ses = boto3.client("ses", region_name="us-east-1")
ssm = boto3.client("ssm", region_name="us-east-1")
s3 = boto3.client("s3")


def load_forward_map() -> dict[str, str]:
    """Load forward map from SSM Parameter Store."""
    settings = get_settings()
    try:
        response = ssm.get_parameter(
            Name=settings.forward_map_param,
            WithDecryption=True,
        )
        value = response["Parameter"]["Value"]
        if not value:
            return {}
        return json.loads(value)
    except ClientError as e:
        logger.error(f"Failed to load forward map from SSM: {e}")
        return {}
    except json.JSONDecodeError:
        logger.error("Forward map is not valid JSON")
        return {}


def handler(event, context):
    """
    Lambda entry function for SES email forwarding.

    SES passes the message directly via Lambda action (event contains SES record).

    Args:
        event: SES event with mail data
        context: Lambda context

    Returns:
        dict: Processing result
    """
    settings = get_settings()
    forward_map = load_forward_map()

    for record in event.get("Records", []):
        try:
            ses_record = record.get("ses", {})
            mail = ses_record.get("mail", {})
            receipt = ses_record.get("receipt", {})

            common_headers = mail.get("commonHeaders", {})
            original_from_list = common_headers.get("from", [])
            original_to_list = common_headers.get("to", [])

            if not original_from_list:
                logger.warning("No From header found, skipping")
                continue

            original_from = original_from_list[0]
            original_to = original_to_list[0]

            logger.info(f"Processing forward: from={original_from}, to={original_to}")

            dest = forward_map.get(original_to.lower())
            if not dest:
                logger.info(f"No forward destination for {original_to}, skipping")
                continue

            action = receipt.get("action", {})
            bucket = action.get("bucketName")
            key = action.get("objectKey")

            if bucket and key:
                raw = s3.get_object(Bucket=bucket, Key=key)["Body"].read()
            else:
                logger.warning("No S3 bucket/key in SES action, skipping")
                continue

            msg = email.message_from_bytes(raw, policy=email.policy.default)

            msg.replace_header(
                "From",
                f"Forwarded via LayerV <noreply@{settings.bot_address.split('@')[1]}>"
            )
            msg["Reply-To"] = original_from
            msg.replace_header("To", dest)
            msg["X-Original-To"] = ", ".join(original_to_list)
            msg["X-Forwarded-For"] = original_to

            try:
                ses.send_raw_email(
                    Source=f"noreply@{settings.bot_address.split('@')[1]}",
                    Destinations=[dest],
                    RawMessage={"Data": msg.as_bytes()},
                )
                logger.info(f"Forwarded email from {original_from} to {dest}")
            except (ClientError, SESError) as e:
                logger.error(f"Failed to send forwarded email: {e}")

        except Exception as e:
            logger.error(f"Failed to process SES record: {e}")
            return {"status": "error", "error": str(e)}

    return {"status": "ok"}
