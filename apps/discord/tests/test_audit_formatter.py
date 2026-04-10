"""Tests for AuditJsonFormatter and structured AUDIT log events."""

from __future__ import annotations

import json
import logging
import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from run import AuditJsonFormatter


class TestAuditJsonFormatter:
    def setup_method(self):
        self.formatter = AuditJsonFormatter()

    def test_audit_record_outputs_json(self):
        record = logging.LogRecord(
            name="test", level=logging.INFO, pathname="", lineno=0,
            msg="upload_success", args=None, exc_info=None,
        )
        record.audit = {"event": "upload_success", "user": "123", "resource": "r_abc"}
        output = self.formatter.format(record)
        parsed = json.loads(output)
        assert parsed["audit"]["event"] == "upload_success"
        assert parsed["audit"]["user"] == "123"
        assert parsed["level"] == "INFO"

    def test_non_audit_record_outputs_plain_text(self):
        record = logging.LogRecord(
            name="test", level=logging.INFO, pathname="", lineno=0,
            msg="normal log message", args=None, exc_info=None,
        )
        output = self.formatter.format(record)
        assert "normal log message" in output
        # Should NOT be JSON
        assert not output.startswith("{")

    def test_audit_with_non_dict_falls_back_to_plain(self):
        record = logging.LogRecord(
            name="test", level=logging.INFO, pathname="", lineno=0,
            msg="bad audit", args=None, exc_info=None,
        )
        record.audit = "not a dict"
        output = self.formatter.format(record)
        assert "bad audit" in output
        assert not output.startswith("{")

    def test_all_audit_events_have_required_fields(self):
        """Verify each AUDIT event type includes the expected fields."""
        events = [
            {"event": "dispatch_sent", "resource": "r_x", "sender": "s1", "recipient": "r1", "link_hash": "abc123"},
            {"event": "dispatch_failed", "resource": "r_x", "recipient": "r1", "error": "user_not_found"},
            {"event": "upload_success", "user": "u1", "resource": "r_x", "filename": "f.pdf", "expires": "2099-01-01"},
            {"event": "revoke_success", "user": "u1", "resource": "r_x"},
        ]
        for audit_data in events:
            record = logging.LogRecord(
                name="test", level=logging.INFO, pathname="", lineno=0,
                msg=audit_data["event"], args=None, exc_info=None,
            )
            record.audit = audit_data
            output = self.formatter.format(record)
            parsed = json.loads(output)
            assert parsed["audit"]["event"] == audit_data["event"]
            for key in audit_data:
                assert key in parsed["audit"], f"Missing field '{key}' in {audit_data['event']}"
