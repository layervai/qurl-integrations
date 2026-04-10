"""Tests for helper functions and constants from adapters/discord_bot.py.

We test the pure logic inline rather than importing from discord_bot.py,
because that module creates a Bot instance on import (which requires a
running event loop and valid Discord token). The logic tested here mirrors
the actual implementation.
"""

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from datetime import datetime, timezone


class TestFormatDiscordTs:
    """Test the _format_discord_ts logic (mirrored from discord_bot.py)."""

    @staticmethod
    def _format_discord_ts(iso_str, fallback="unknown"):
        if not iso_str:
            return fallback
        try:
            dt = datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
            return f"<t:{int(dt.timestamp())}:R>"
        except (ValueError, AttributeError):
            return iso_str

    def test_valid_iso_z(self):
        result = self._format_discord_ts("2026-12-31T23:59:59Z")
        assert result.startswith("<t:")
        assert result.endswith(":R>")
        # The timestamp should parse to a valid integer
        ts_str = result[3:-3]
        assert ts_str.isdigit() or (ts_str.startswith("-") and ts_str[1:].isdigit())

    def test_valid_iso_with_offset(self):
        result = self._format_discord_ts("2026-06-15T12:00:00+00:00")
        assert result.startswith("<t:")
        assert result.endswith(":R>")

    def test_none_returns_fallback(self):
        assert self._format_discord_ts(None) == "unknown"
        assert self._format_discord_ts(None, fallback="N/A") == "N/A"

    def test_empty_string_returns_fallback(self):
        assert self._format_discord_ts("") == "unknown"

    def test_invalid_iso_returns_original(self):
        result = self._format_discord_ts("not-a-date")
        assert result == "not-a-date"

    def test_known_timestamp_value(self):
        # 2026-01-01T00:00:00Z = 1767225600 unix
        dt = datetime(2026, 1, 1, 0, 0, 0, tzinfo=timezone.utc)
        expected_ts = int(dt.timestamp())
        result = self._format_discord_ts("2026-01-01T00:00:00Z")
        assert result == f"<t:{expected_ts}:R>"

    def test_fallback_in_15_minutes(self):
        """M6: When expires_at is empty, fallback should be deterministic."""
        result = self._format_discord_ts("", fallback="in 15 minutes")
        assert result == "in 15 minutes"

        result = self._format_discord_ts(None, fallback="in 15 minutes")
        assert result == "in 15 minutes"


class TestConfigValidation:
    """Test that missing required config fields raise an error."""

    def test_missing_bot_token_raises(self):
        from pydantic import ValidationError
        from config import Settings

        import pytest
        with pytest.raises(ValidationError):
            Settings(
                discord_bot_token=None,  # type: ignore[arg-type]
                discord_client_id="123",
                qurl_api_key="lv_test_x",
            )

    def test_missing_api_key_raises(self):
        from pydantic import ValidationError
        from config import Settings

        import pytest
        with pytest.raises(ValidationError):
            Settings(
                discord_bot_token="tok",
                discord_client_id="123",
                qurl_api_key=None,  # type: ignore[arg-type]
            )

    def test_defaults_applied(self):
        from config import Settings

        s = Settings(
            discord_bot_token="tok",
            discord_client_id="123",
            qurl_api_key="lv_test_x",
        )
        assert s.port == 3000
        assert s.max_file_size_mb == 25
        assert s.sync_commands_globally is False
        assert s.qurl_link_hostname == "qurl.link"
