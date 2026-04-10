"""Tests for ApiKeyRedactor log filter."""

from __future__ import annotations

import logging
import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from run import ApiKeyRedactor


class TestApiKeyRedactor:
    def setup_method(self):
        self.redactor = ApiKeyRedactor()

    def _make_record(self, msg, args=None):
        record = logging.LogRecord(
            name="test", level=logging.INFO, pathname="", lineno=0,
            msg=msg, args=args, exc_info=None,
        )
        return record

    def test_redacts_lasso_live_key(self):
        record = self._make_record("Key is lv_live_abc123def456_xyz789")
        self.redactor.filter(record)
        assert "lv_live_" not in record.msg
        assert "***REDACTED***" in record.msg

    def test_redacts_lasso_test_key(self):
        record = self._make_record("Key is lv_test_abc123def456_xyz789")
        self.redactor.filter(record)
        assert "lv_test_" not in record.msg
        assert "***REDACTED***" in record.msg

    def test_redacts_bearer_token(self):
        record = self._make_record("Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.signature")
        self.redactor.filter(record)
        assert "eyJhbG" not in record.msg
        assert "***REDACTED***" in record.msg

    def test_preserves_normal_messages(self):
        record = self._make_record("Normal log message with no secrets")
        self.redactor.filter(record)
        assert record.msg == "Normal log message with no secrets"

    def test_redacts_in_args_tuple(self):
        record = self._make_record("User %s key %s", ("user1", "lv_live_secret_key_here"))
        self.redactor.filter(record)
        assert "lv_live_" not in str(record.args)

    def test_always_returns_true(self):
        record = self._make_record("any message")
        assert self.redactor.filter(record) is True
