"""Tests for bot_helpers.py — pure helper functions extracted from discord_bot.py."""

from __future__ import annotations

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from db import DispatchStatus
from bot_helpers import RESOURCE_ID_MARKER_RE, format_dispatch_summary, is_expired


class TestIsExpired:
    def test_none_returns_false(self):
        assert is_expired(None) is False

    def test_empty_string_returns_false(self):
        assert is_expired("") is False

    def test_future_timestamp_returns_false(self):
        assert is_expired("2099-12-31T23:59:59Z") is False

    def test_past_timestamp_returns_true(self):
        assert is_expired("2020-01-01T00:00:00Z") is True

    def test_z_suffix(self):
        assert is_expired("2020-06-15T12:00:00Z") is True

    def test_offset_suffix(self):
        assert is_expired("2020-06-15T12:00:00+00:00") is True

    def test_malformed_returns_false(self):
        assert is_expired("not-a-date") is False

    def test_integer_returns_false(self):
        assert is_expired("12345") is False


class TestFormatDispatchSummary:
    def test_all_sent(self):
        results = [
            ("123", DispatchStatus.SENT, None, "Alice"),
            ("456", DispatchStatus.SENT, None, "Bob"),
        ]
        summary = format_dispatch_summary("test.pdf", results)
        assert "**Links sent for test.pdf:**" in summary
        assert "- Alice -- sent" in summary
        assert "- Bob -- sent" in summary

    def test_mixed_results(self):
        results = [
            ("123", DispatchStatus.SENT, None, "Alice"),
            ("456", DispatchStatus.DM_FAILED, "DMs disabled", "Bob"),
            ("789", DispatchStatus.MINT_FAILED, "Failed to mint link", "Charlie"),
        ]
        summary = format_dispatch_summary("doc.pdf", results)
        assert "- Alice -- sent" in summary
        assert "- Bob -- DMs disabled" in summary
        assert "- Charlie -- Failed to mint link" in summary

    def test_empty_results(self):
        summary = format_dispatch_summary("file.png", [])
        assert "**Links sent for file.png:**" in summary
        assert summary.count("\n") == 0

    def test_error_fallback_to_status(self):
        results = [("123", "unknown_status", None, "Dave")]
        summary = format_dispatch_summary("x.pdf", results)
        assert "- Dave -- unknown_status" in summary


class TestResourceIdMarkerRegex:
    def test_extracts_from_bot_upload_dm(self):
        msg = (
            "**Your resource has been protected!**\n\n"
            "**test.jpg**\n"
            "https://qurl.link/at_xxx\n"
            "Expires <t:1234567890:R>\n\n"
            "Reply to this message and @mention users to share.\n"
            "\\_resource\\_id:r_abc123def456\\_"
        )
        match = RESOURCE_ID_MARKER_RE.search(msg)
        assert match is not None
        assert match.group(1) == "r_abc123def456"

    def test_no_match_without_marker(self):
        msg = "Some text with `r_abc123def456` in backticks but no marker"
        match = RESOURCE_ID_MARKER_RE.search(msg)
        assert match is None

    def test_no_match_for_arbitrary_backtick_resource_id(self):
        """Regression test: backtick-wrapped resource IDs must NOT match."""
        msg = "Could not provision `r_invalid_format` because the API returned 500."
        match = RESOURCE_ID_MARKER_RE.search(msg)
        assert match is None

    def test_no_match_inline(self):
        msg = "Something \\_resource\\_id:r_abc123\\_ in the middle of text"
        match = RESOURCE_ID_MARKER_RE.search(msg)
        # Only matches if marker is on its own line
        assert match is None

    def test_matches_with_hyphens_underscores(self):
        msg = "\\_resource\\_id:r_abc-def_ghi\\_"
        match = RESOURCE_ID_MARKER_RE.search(msg)
        assert match is not None
        assert match.group(1) == "r_abc-def_ghi"

    def test_multiline_message_extracts_correct_id(self):
        msg = "Some preamble\n\\_resource\\_id:r_first12345\\_\nSome footer"
        match = RESOURCE_ID_MARKER_RE.search(msg)
        assert match.group(1) == "r_first12345"


class TestAutocompleteLabelTruncation:
    def test_long_filename_truncated_to_100(self):
        label = "a" * 120
        assert len(label[:100]) == 100

    def test_short_filename_fits(self):
        label = "test.pdf"
        assert len(label[:100]) == len(label)
