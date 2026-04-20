"""
DynamoDB dispatch log module.

Manages email dispatch records with idempotency checks.
"""

import logging
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Optional

import boto3
from botocore.exceptions import ClientError

from config import Settings, get_settings

logger = logging.getLogger(__name__)


@dataclass
class DispatchRecord:
    """Dispatch record"""
    resource_id: str
    dispatch_id: str
    sender_email: str
    recipient_email: str
    status: str
    link_id_hash: Optional[str] = None
    error: Optional[str] = None
    created_at: Optional[str] = None
    completed_at: Optional[str] = None
    expires_at: Optional[str] = None  # TTL, expires after 30 days


class DispatchStatus:
    SENT = "sent"
    MINT_FAILED = "mint_failed"
    SEND_FAILED = "send_failed"
    SKIPPED = "skipped"  # skipped due to idempotency


class DynamoDBError(Exception):
    """Base DynamoDB error"""
    pass


class DB:
    """DynamoDB operations"""

    def __init__(self, table_name: str, region: str = "us-east-1"):
        self.table_name = table_name
        self.dynamodb = boto3.resource("dynamodb", region_name=region)
        self.table = self.dynamodb.Table(table_name)

    def log_dispatch(
        self,
        resource_id: str,
        sender_email: str,
        recipient_email: str,
        status: str,
        link_id_hash: Optional[str] = None,
        error: Optional[str] = None,
    ) -> str:
        """
        Log a dispatch record.

        Args:
            resource_id: Resource ID
            sender_email: Sender email address
            recipient_email: Recipient email address
            status: Dispatch status
            link_id_hash: Link ID hash (for tracing)
            error: Error message

        Returns:
            str: dispatch_id
        """
        dispatch_id = str(uuid.uuid4())
        now = datetime.now(timezone.utc).isoformat()

        # Calculate expiration (30 days from now)
        from datetime import timedelta
        expires = datetime.now(timezone.utc) + timedelta(days=30)
        expires_at = int(expires.timestamp())

        item = {
            "resource_id": resource_id,
            "dispatch_id": dispatch_id,
            "sender_email": sender_email,
            "recipient_email": recipient_email,
            "status": status,
            "created_at": now,
            "completed_at": now if status != DispatchStatus.SKIPPED else None,
            "expires_at": expires_at,
        }

        if link_id_hash:
            item["link_id_hash"] = link_id_hash

        if error:
            item["error"] = error

        try:
            self.table.put_item(Item=item)
            logger.info(f"Dispatch logged: dispatch_id={dispatch_id}, status={status}")
            return dispatch_id
        except ClientError as e:
            logger.error(f"Failed to log dispatch: {e}")
            raise DynamoDBError(f"Failed to log dispatch: {e}") from e

    def check_idempotent(self, resource_id: str, recipient_email: str) -> bool:
        """
        Check if already sent (idempotency check).

        Args:
            resource_id: Resource ID
            recipient_email: Recipient email address

        Returns:
            bool: True if already sent
        """
        try:
            response = self.table.query(
                KeyConditionExpression="resource_id = :rid",
                FilterExpression="recipient_email = :email AND #status = :status",
                ExpressionAttributeNames={"#status": "status"},
                ExpressionAttributeValues={
                    ":rid": resource_id,
                    ":email": recipient_email,
                    ":status": DispatchStatus.SENT,
                },
                Limit=1,
            )

            items = response.get("Items", [])
            if items:
                logger.info(f"Duplicate dispatch detected: resource_id={resource_id}, recipient={recipient_email}")
                return True

            return False

        except ClientError as e:
            logger.error(f"Idempotency check failed: {e}")
            return False

    def get_dispatch_history(
        self,
        sender_email: str,
        limit: int = 50,
    ) -> list[DispatchRecord]:
        """
        Get dispatch history for a sender.

        Uses sender-index GSI.

        Args:
            sender_email: Sender email address
            limit: Max number of records to return

        Returns:
            list[DispatchRecord]: List of dispatch records
        """
        try:
            response = self.table.query(
                IndexName="sender-index",
                KeyConditionExpression="sender_email = :email",
                ExpressionAttributeValues={":email": sender_email},
                ScanIndexForward=False,
                Limit=limit,
            )

            records = []
            for item in response.get("Items", []):
                records.append(DispatchRecord(
                    resource_id=item["resource_id"],
                    dispatch_id=item["dispatch_id"],
                    sender_email=item["sender_email"],
                    recipient_email=item["recipient_email"],
                    status=item["status"],
                    link_id_hash=item.get("link_id_hash"),
                    error=item.get("error"),
                    created_at=item.get("created_at"),
                    completed_at=item.get("completed_at"),
                    expires_at=item.get("expires_at"),
                ))

            return records

        except ClientError as e:
            logger.error(f"Failed to get dispatch history: {e}")
            return []


# Global instance (lazy initialization)
_db_instance: Optional[DB] = None


def get_db(settings: Settings | None = None) -> DB:
    """
    Get DynamoDB instance.

    Args:
        settings: Settings object

    Returns:
        DB: DynamoDB operations instance
    """
    global _db_instance

    if _db_instance is None:
        if settings is None:
            settings = get_settings()
        _db_instance = DB(
            table_name=settings.dispatch_table,
            region=settings.aws_region,
        )

    return _db_instance


def log_dispatch(
    resource_id: str,
    sender_email: str,
    recipient_email: str,
    status: str,
    link_id_hash: Optional[str] = None,
    error: Optional[str] = None,
    db: DB | None = None,
) -> str:
    """
    Convenience function to log a dispatch record.

    Args:
        resource_id: Resource ID
        sender_email: Sender email address
        recipient_email: Recipient email address
        status: Dispatch status
        link_id_hash: Link ID hash
        error: Error message
        db: DynamoDB instance (optional)

    Returns:
        str: dispatch_id
    """
    if db is None:
        db = get_db()

    return db.log_dispatch(
        resource_id=resource_id,
        sender_email=sender_email,
        recipient_email=recipient_email,
        status=status,
        link_id_hash=link_id_hash,
        error=error,
    )


def check_idempotent(
    resource_id: str,
    recipient_email: str,
    db: DB | None = None,
) -> bool:
    """
    Convenience function to check if already sent.

    Args:
        resource_id: Resource ID
        recipient_email: Recipient email address
        db: DynamoDB instance (optional)

    Returns:
        bool: True if already sent
    """
    if db is None:
        db = get_db()

    return db.check_idempotent(resource_id, recipient_email)
