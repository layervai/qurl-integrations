"""
Rate limiter module tests.
"""

import pytest
from unittest.mock import MagicMock

from rate_limiter import RateLimiter, RateLimitResult, check_rate_limit
from config import Settings


def _make_settings(aws_region="us-east-1", rate_limit_per_hour=5):
    return Settings(
        bot_address="qurl@layerv.ai",
        max_recipients=25,
        max_urls_per_email=3,
        max_attachment_size_mb=25,
        authorized_senders_param="/test/authorized-senders",
        qurl_api_key_param="/test/qurl-api-key",
        rate_limit_table="qurl-email-rate-limits",
        rate_limit_per_hour=rate_limit_per_hour,
        aws_region=aws_region,
    )


def _table_name(table_obj):
    return table_obj.name if hasattr(table_obj, "name") else table_obj.table_name


class TestRateLimitResult:
    """RateLimitResult dataclass tests"""

    def test_result_allowed(self):
        result = RateLimitResult(allowed=True, remaining=3, reset_at=1700000000, limit=5)
        assert result.allowed is True
        assert result.remaining == 3
        assert result.reset_at == 1700000000
        assert result.limit == 5

    def test_result_denied(self):
        result = RateLimitResult(allowed=False, remaining=0, reset_at=1700000000, limit=5)
        assert result.allowed is False
        assert result.remaining == 0


class TestRateLimiterWindowHelpers:
    """RateLimiter internal helpers"""

    def test_window_key_format(self):
        limiter = RateLimiter("any-table", limit_per_hour=5)
        key = limiter._window_key(1704067200)  # 2024-01-01 00:00:00 UTC
        assert key == "2024-01-01T00"

    def test_reset_at_end_of_hour(self):
        limiter = RateLimiter("any-table", limit_per_hour=5)
        now = 1704067200  # 2024-01-01 00:00:00 UTC
        reset = limiter._reset_at(now)
        assert reset == 1704070800  # 2024-01-01 01:00:00 UTC


class TestRateLimiterCheck:
    """RateLimiter.check tests with moto DynamoDB"""

    def test_check_no_record_allows(self, aws_services):
        """Test check allows when no record exists"""
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)
        result = limiter.check("alice@example.com", now=1704067200)
        assert result.allowed is True
        assert result.remaining == 5

    def test_check_within_limit(self, aws_services):
        """Test check within limit"""
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)
        limiter.table.put_item(
            Item={
                "sender_email": "alice@example.com",
                "window_key": "2024-01-01T00",
                "count": 3,
                "reset_at": 1704070800,
            }
        )
        result = limiter.check("alice@example.com", now=1704067200)
        assert result.allowed is True
        assert result.remaining == 2

    def test_check_at_limit_denies(self, aws_services):
        """Test check denies when at limit"""
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)
        limiter.table.put_item(
            Item={
                "sender_email": "alice@example.com",
                "window_key": "2024-01-01T00",
                "count": 5,
                "reset_at": 1704070800,
            }
        )
        result = limiter.check("alice@example.com", now=1704067200)
        assert result.allowed is False
        assert result.remaining == 0


class TestRateLimiterIncrement:
    """RateLimiter.increment tests.

    moto's DynamoDB does not support ConditionExpression with
    attribute name aliases (#cnt < :limit). We test the increment
    flow by mocking update_item to simulate ConditionalCheckFailedException
    and verifying check() is called afterward.
    """

    def test_increment_success(self, aws_services):
        """Test successful increment calls update_item and returns updated rate limit"""
        from unittest.mock import MagicMock
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)

        # Mock both update_item and get_item (called by check() at end of increment)
        original_update = limiter.table.update_item
        original_get = limiter.table.get_item
        limiter.table.update_item = MagicMock(return_value={})
        limiter.table.get_item = MagicMock(return_value={"Item": {"count": 1, "reset_at": 1704070800}})
        try:
            result = limiter.increment("alice@example.com", now=1704067200)
            assert result.allowed is True
            assert result.remaining == 4
            limiter.table.update_item.assert_called_once()
        finally:
            limiter.table.update_item = original_update
            limiter.table.get_item = original_get

    def test_increment_increments_count(self, aws_services):
        """Test multiple increments call update_item each time"""
        from unittest.mock import MagicMock
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)

        original_update = limiter.table.update_item
        original_get = limiter.table.get_item
        limiter.table.update_item = MagicMock(return_value={})
        limiter.table.get_item = MagicMock(return_value={"Item": {"count": 0, "reset_at": 1704070800}})
        try:
            for i in range(3):
                limiter.increment("alice@example.com", now=1704067200)
            assert limiter.table.update_item.call_count == 3
        finally:
            limiter.table.update_item = original_update
            limiter.table.get_item = original_get

    def test_increment_at_limit_denies(self, aws_services):
        """Test increment when already at limit triggers ConditionalCheckFailed path"""
        rate_table = aws_services["rate_limit_table"]
        limiter = RateLimiter(_table_name(rate_table), limit_per_hour=5)

        # Pre-populate with count=5 (at limit)
        limiter.table.put_item(
            Item={
                "sender_email": "alice@example.com",
                "window_key": "2024-01-01T00",
                "count": 5,
                "reset_at": 1704070800,
            }
        )

        # Mock update_item to raise ConditionalCheckFailedException (moto limitation)
        from botocore.exceptions import ClientError

        original_update = limiter.table.update_item

        def bad_update(**kw):
            raise ClientError(
                {"Error": {"Code": "ConditionalCheckFailedException"}},
                "UpdateItem"
            )

        limiter.table.update_item = bad_update
        try:
            result = limiter.increment("alice@example.com", now=1704067200)
            assert result.allowed is False
            assert result.remaining == 0
        finally:
            limiter.table.update_item = original_update

    def test_get_limit(self):
        """Test get_limit returns configured limit"""
        limiter = RateLimiter("any-table", limit_per_hour=10)
        assert limiter.get_limit() == 10


class TestCheckRateLimit:
    """check_rate_limit convenience function"""

    def test_check_rate_limit_with_explicit_limiter(self):
        """Test check_rate_limit with explicit limiter"""
        limiter = MagicMock()
        limiter.check.return_value = RateLimitResult(
            allowed=True, remaining=4, reset_at=1700000000, limit=5
        )
        result = check_rate_limit("alice@example.com", limiter=limiter)
        limiter.check.assert_called_once_with("alice@example.com")
        assert result.allowed is True


class TestGetRateLimiter:
    """get_rate_limiter convenience function"""

    def test_get_rate_limiter_uses_explicit_settings(self, aws_services):
        """Test get_rate_limiter uses explicit settings"""
        import rate_limiter as _rl
        _rl._rate_limiter = None
        settings = _make_settings(rate_limit_per_hour=20)
        limiter = _rl.get_rate_limiter(settings)
        assert limiter.limit_per_hour == 20

    def test_get_rate_limiter_uses_default_settings(self, aws_services):
        """Test get_rate_limiter falls back to patched get_settings when settings is None"""
        import rate_limiter as _rl
        _rl._rate_limiter = None
        settings = _make_settings(rate_limit_per_hour=7)
        with pytest.MonkeyPatch.context() as mp:
            # Patch the reference held inside rate_limiter module
            mp.setattr(_rl, "get_settings", lambda: settings)
            limiter = _rl.get_rate_limiter(None)
            assert limiter.limit_per_hour == 7

    def test_get_rate_limiter_cached(self, aws_services):
        """Test get_rate_limiter returns cached instance"""
        import rate_limiter as _rl
        _rl._rate_limiter = None
        settings = _make_settings()
        with pytest.MonkeyPatch.context() as mp:
            mp.setattr(_rl, "get_settings", lambda: settings)
            lim1 = _rl.get_rate_limiter()
            lim2 = _rl.get_rate_limiter()
            assert lim1 is lim2
