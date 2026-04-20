"""
Rate limiting module.

DynamoDB-backed rate limiter for per-sender share limits.
"""

import logging
import time
from dataclasses import dataclass
from typing import Optional

import boto3
from botocore.exceptions import ClientError

from config import Settings, get_settings

logger = logging.getLogger(__name__)


@dataclass
class RateLimitResult:
    allowed: bool
    remaining: int
    reset_at: int  # Unix timestamp
    limit: int


class RateLimitError(Exception):
    """Rate limit exceeded"""
    def __init__(self, result: RateLimitResult):
        self.result = result
        super().__init__(f"Rate limit exceeded. {result.remaining}/{result.limit} shares remaining. Resets at {result.reset_at}.")


class RateLimiter:
    """
    DynamoDB-backed sliding window rate limiter.

    Table: rate-limits
    PK: sender_email (String)
    SK: window_key (String, e.g. "2024-01-15T14")
    Attributes: count (Number), reset_at (Number, Unix timestamp)
    TTL: reset_at (auto-cleanup after window expires)
    """

    def __init__(self, table_name: str, region: str = "us-east-1", limit_per_hour: int = 5):
        self.table_name = table_name
        self.region = region
        self.limit_per_hour = limit_per_hour
        self.dynamodb = boto3.resource("dynamodb", region_name=region)
        self.table = self.dynamodb.Table(table_name)

    def _window_key(self, timestamp: float) -> str:
        """Get the current hour window key."""
        return time.strftime("%Y-%m-%dT%H", time.gmtime(timestamp))

    def _reset_at(self, timestamp: float) -> int:
        """Unix timestamp at the end of the current hour window."""
        return int(timestamp) + 3600 - (int(timestamp) % 3600)

    def check(self, sender_email: str, now: Optional[float] = None) -> RateLimitResult:
        """
        Check if sender is within rate limit.

        Args:
            sender_email: Sender's email address
            now: Current timestamp (for testing)

        Returns:
            RateLimitResult: Whether the request is allowed
        """
        if now is None:
            now = time.time()

        window = self._window_key(now)
        reset_at = self._reset_at(now)

        try:
            response = self.table.get_item(
                Key={"sender_email": sender_email, "window_key": window},
                ProjectionExpression="#cnt, reset_at",
                ExpressionAttributeNames={"#cnt": "count"},
            )
            item = response.get("Item")

            if item:
                count = int(item.get("count", 0))
            else:
                count = 0

            remaining = max(0, self.limit_per_hour - count)
            allowed = count < self.limit_per_hour

            return RateLimitResult(
                allowed=allowed,
                remaining=remaining,
                reset_at=reset_at,
                limit=self.limit_per_hour,
            )

        except ClientError as e:
            logger.error(f"Rate limit check failed: {e}")
            # Fail open: allow if we can't check
            return RateLimitResult(
                allowed=True,
                remaining=self.limit_per_hour,
                reset_at=reset_at,
                limit=self.limit_per_hour,
            )

    def increment(self, sender_email: str, now: Optional[float] = None) -> RateLimitResult:
        """
        Increment the rate limit counter for a sender.

        Args:
            sender_email: Sender's email address
            now: Current timestamp (for testing)

        Returns:
            RateLimitResult: Updated rate limit status
        """
        if now is None:
            now = time.time()

        window = self._window_key(now)
        reset_at = self._reset_at(now)

        try:
            self.table.update_item(
                Key={"sender_email": sender_email, "window_key": window},
                UpdateExpression="SET #cnt = if_not_exists(#cnt, :zero) + :inc, reset_at = :reset",
                ExpressionAttributeNames={"#cnt": "count"},
                ExpressionAttributeValues={
                    ":inc": 1,
                    ":zero": 0,
                    ":reset": reset_at,
                },
                ConditionExpression="#cnt < :limit",
            )

        except ClientError as e:
            error_code = e.response.get("Error", {}).get("Code", "")
            if error_code != "ConditionalCheckFailedException":
                logger.error(f"Rate limit increment failed: {e}")
                # Fail open: allow if we can't update

        current = self.check(sender_email, now)
        return current

    def get_limit(self) -> int:
        """Get the configured limit per hour."""
        return self.limit_per_hour


# Global instance (lazy initialization)
_rate_limiter: Optional[RateLimiter] = None


def get_rate_limiter(settings: Settings | None = None) -> RateLimiter:
    """Get RateLimiter instance."""
    global _rate_limiter
    if _rate_limiter is None:
        if settings is None:
            settings = get_settings()
        _rate_limiter = RateLimiter(
            table_name=settings.rate_limit_table,
            region=settings.aws_region,
            limit_per_hour=settings.rate_limit_per_hour,
        )
    return _rate_limiter


def check_rate_limit(
    sender_email: str,
    limiter: RateLimiter | None = None,
) -> RateLimitResult:
    """
    Convenience function to check rate limit.

    Args:
        sender_email: Sender's email address
        limiter: RateLimiter instance (optional)

    Returns:
        RateLimitResult: Whether the request is allowed
    """
    if limiter is None:
        limiter = get_rate_limiter()
    return limiter.check(sender_email)
