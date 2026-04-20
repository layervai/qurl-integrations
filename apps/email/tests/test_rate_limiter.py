"""
Rate limiter module tests.
"""

import pytest
from unittest.mock import MagicMock, patch

from rate_limiter import (
    RateLimiter,
    RateLimitResult,
    check_rate_limit,
)


class TestRateLimitResult:
    """RateLimitResult dataclass tests"""

    def test_result_allowed(self):
        """Test allowed result"""
        result = RateLimitResult(allowed=True, remaining=3, reset_at=1700000000, limit=5)
        assert result.allowed is True
        assert result.remaining == 3
        assert result.reset_at == 1700000000
        assert result.limit == 5

    def test_result_denied(self):
        """Test denied result"""
        result = RateLimitResult(allowed=False, remaining=0, reset_at=1700000000, limit=5)
        assert result.allowed is False
        assert result.remaining == 0


class TestRateLimiter:
    """RateLimiter class tests"""

    def test_window_key_format(self):
        """Test window key format is YYYY-MM-DDTHH"""
        limiter = RateLimiter("test-table", limit_per_hour=5)
        key = limiter._window_key(1704067200)  # 2024-01-01 00:00:00 UTC
        assert key == "2024-01-01T00"

    def test_reset_at_end_of_hour(self):
        """Test reset_at is at the end of the hour"""
        limiter = RateLimiter("test-table", limit_per_hour=5)
        now = 1704067200  # 2024-01-01 00:00:00 UTC
        reset = limiter._reset_at(now)
        assert reset == 1704070800  # 2024-01-01 01:00:00 UTC

    def test_check_no_existing_record(self):
        """Test check when no record exists"""
        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.get_item.return_value = {}

            limiter = RateLimiter("test-table", limit_per_hour=5)
            result = limiter.check("alice@example.com", now=1704067200)

            assert result.allowed is True
            assert result.remaining == 5
            assert result.limit == 5

    def test_check_within_limit(self):
        """Test check when within limit"""
        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.get_item.return_value = {
                "Item": {"count": 3}
            }

            limiter = RateLimiter("test-table", limit_per_hour=5)
            result = limiter.check("alice@example.com", now=1704067200)

            assert result.allowed is True
            assert result.remaining == 2
            assert result.limit == 5

    def test_check_at_limit(self):
        """Test check when at limit"""
        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.get_item.return_value = {
                "Item": {"count": 5}
            }

            limiter = RateLimiter("test-table", limit_per_hour=5)
            result = limiter.check("alice@example.com", now=1704067200)

            assert result.allowed is False
            assert result.remaining == 0

    def test_check_aws_error_fails_open(self):
        """Test that AWS errors fail open (allow request)"""
        from botocore.exceptions import ClientError

        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.get_item.side_effect = ClientError(
                {"Error": {"Code": "ProvisionedThroughputExceededException"}},
                "GetItem"
            )

            limiter = RateLimiter("test-table", limit_per_hour=5)
            result = limiter.check("alice@example.com", now=1704067200)

            # Should fail open
            assert result.allowed is True
            assert result.remaining == 5

    def test_increment_success(self):
        """Test successful increment updates DynamoDB."""
        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.update_item.return_value = {}

            limiter = RateLimiter("test-table", limit_per_hour=5)
            limiter.increment("alice@example.com", now=1704067200)

            mock_table.update_item.assert_called_once()
            call_kwargs = mock_table.update_item.call_args[1]
            assert call_kwargs["Key"] == {
                "sender_email": "alice@example.com",
                "window_key": "2024-01-01T00",
            }
            assert ":inc" in call_kwargs["ExpressionAttributeValues"]
            assert call_kwargs["ConditionExpression"] == "#cnt < :limit"

    def test_increment_at_limit_fails_condition(self):
        """Test increment fails when at limit"""
        from botocore.exceptions import ClientError

        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.update_item.side_effect = ClientError(
                {"Error": {"Code": "ConditionalCheckFailedException"}},
                "UpdateItem"
            )
            mock_table.get_item.return_value = {"Item": {"count": 5}}

            limiter = RateLimiter("test-table", limit_per_hour=5)
            result = limiter.increment("alice@example.com", now=1704067200)

            assert result.allowed is False

    def test_get_limit(self):
        """Test get_limit returns configured limit"""
        limiter = RateLimiter("test-table", limit_per_hour=10)
        assert limiter.get_limit() == 10


class TestRateLimiterIncrement:
    """RateLimiter increment error path tests"""

    def test_increment_non_condition_error(self):
        """Test increment logs error for non-ConditionalCheckFailedException errors"""
        from botocore.exceptions import ClientError

        with patch("rate_limiter.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            # Return a non-CCFE error on update_item, then check returns remaining
            mock_table.update_item.side_effect = ClientError(
                {"Error": {"Code": "ValidationException"}},
                "UpdateItem"
            )
            mock_table.get_item.return_value = {"Item": {"count": 1}}

            limiter = RateLimiter("test-table", limit_per_hour=5)
            # Should not raise, fail open
            result = limiter.increment("alice@example.com", now=1704067200)
            assert result is not None


class TestGetRateLimiter:
    """get_rate_limiter convenience function tests"""

    def test_get_rate_limiter_uses_explicit_settings(self):
        """Test get_rate_limiter uses explicit settings"""
        import rate_limiter as rl

        rl._rate_limiter = None
        mock_settings = MagicMock()
        mock_settings.rate_limit_table = "custom-table"
        mock_settings.aws_region = "eu-west-1"
        mock_settings.rate_limit_per_hour = 20

        with patch("rate_limiter.boto3"):
            limiter = rl.get_rate_limiter(mock_settings)
            assert limiter.table_name == "custom-table"
            assert limiter.limit_per_hour == 20

    def test_get_rate_limiter_uses_default_settings(self):
        """Test get_rate_limiter falls back to get_settings when settings is None"""
        import rate_limiter as rl

        rl._rate_limiter = None
        mock_settings = MagicMock()
        mock_settings.rate_limit_table = "default-table"
        mock_settings.aws_region = "us-east-1"
        mock_settings.rate_limit_per_hour = 5

        with patch("rate_limiter.boto3"):
            with patch("rate_limiter.get_settings", return_value=mock_settings):
                limiter = rl.get_rate_limiter(None)
                assert limiter.table_name == "default-table"

    def test_get_rate_limiter_cached(self):
        """Test get_rate_limiter returns cached instance"""
        import rate_limiter as rl

        rl._rate_limiter = None
        mock_settings = MagicMock()
        mock_settings.rate_limit_table = "test-table"
        mock_settings.aws_region = "us-east-1"
        mock_settings.rate_limit_per_hour = 5

        with patch("rate_limiter.boto3"):
            with patch("rate_limiter.get_settings", return_value=mock_settings):
                lim1 = rl.get_rate_limiter()
                lim2 = rl.get_rate_limiter()
                assert lim1 is lim2
