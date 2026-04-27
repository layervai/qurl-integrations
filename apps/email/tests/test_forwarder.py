"""
Email forwarding Lambda tests.
"""

from unittest.mock import patch, MagicMock


class TestLoadForwardMap:
    """load_forward_map tests using moto SSM"""

    def test_load_forward_map_success(self, moto_inject):
        """Test successful load of forward map from SSM"""
        from forwarder import load_forward_map
        # aws_services fixture already created the forward-map SSM param
        result = load_forward_map()
        assert result["justin@layerv.ai"] == "personal@icloud.com"

    def test_load_forward_map_returns_empty_on_empty_value(self, moto_inject):
        """Test load_forward_map returns empty dict when SSM value is empty string.

        moto does not allow creating empty-string SSM params, so we verify
        the empty-check logic directly by patching ssm.get_parameter.
        """
        import forwarder
        original_get = moto_inject["ssm"].get_parameter

        def empty_param(**kw):
            return {"Parameter": {"Value": ""}}

        moto_inject["ssm"].get_parameter = empty_param
        try:
            result = forwarder.load_forward_map()
            assert result == {}
        finally:
            moto_inject["ssm"].get_parameter = original_get

    def test_load_forward_map_invalid_json(self, moto_inject):
        """Test load with invalid JSON"""
        moto_inject["ssm"].put_parameter(
            Name="/test/bad-map",
            Type="String",
            Value="not valid json",
        )
        import forwarder
        original = forwarder.get_settings

        class FakeSettings:
            forward_map_param = "/test/bad-map"

        forwarder.get_settings = lambda: FakeSettings()
        try:
            result = forwarder.load_forward_map()
            assert result == {}
        finally:
            forwarder.get_settings = original

    def test_load_forward_map_ssm_error(self, moto_inject):
        """Test load when SSM raises ClientError"""
        import forwarder
        original = forwarder.get_settings

        class FakeSettings:
            forward_map_param = "/nonexistent/param"

        forwarder.get_settings = lambda: FakeSettings()
        try:
            result = forwarder.load_forward_map()
            assert result == {}
        finally:
            forwarder.get_settings = original


def _fake_settings():
    """Return a settings-like object for forwarder tests."""
    class FakeSettings:
        forward_map_param = "/test/forward-map"
        bot_address = "qurl@layerv.ai"
    return FakeSettings()


def _raw_email_bytes():
    return (
        b"From: alice@external.com\r\n"
        b"To: justin@layerv.ai\r\n"
        b"Subject: Test\r\n\r\n"
        b"Hello, this is a forwarded email.\r\n"
    )


def _ses_record(forward_to="justin@layerv.ai", forward_from="alice@external.com",
                bucket="qurl-email-inbound-test", key="fwd/test.eml",
                s3_bucket=True):
    return {
        "ses": {
            "mail": {
                "commonHeaders": {
                    "from": [forward_from],
                    "to": [forward_to],
                }
            },
            "receipt": {
                "action": {
                    "bucketName": bucket if s3_bucket else None,
                    "objectKey": key if s3_bucket else None,
                }
            },
        }
    }


def _patch_settings_and_ses(moto_inject, raw_bytes=None, ses_side_effect=None):
    """Common patcher: inject settings + optionally mock ses.send_raw_email."""
    import config

    original_settings = config.get_settings
    config.get_settings = lambda: _fake_settings()

    ses_mock = MagicMock(return_value={"MessageId": "fwd-123"})
    if ses_side_effect:
        ses_mock.side_effect = ses_side_effect

    ses_patcher = patch.object(moto_inject["ses"], "send_raw_email", ses_mock)

    def restore():
        config.get_settings = original_settings

    return restore, ses_patcher, ses_mock


def _put_email_in_s3(moto_inject, key, raw_bytes):
    moto_inject["s3"].put_object(
        Bucket="qurl-email-inbound-test",
        Key=key,
        Body=raw_bytes,
    )


class TestForwarderHandler:
    """Forwarder Lambda handler tests using moto S3 + SES"""

    def test_handler_no_records(self, moto_inject):
        """Test handler with no records"""
        restore, ses_patcher, _ = _patch_settings_and_ses(moto_inject)
        with ses_patcher:
            from forwarder import handler
            result = handler({"Records": []}, MagicMock())
            assert result["status"] == "ok"
        restore()

    def test_handler_no_forward_map_entry(self, moto_inject):
        """Test handler skips when no forward map entry"""
        restore, ses_patcher, _ = _patch_settings_and_ses(moto_inject)
        with ses_patcher:
            from forwarder import handler
            record = _ses_record(forward_to="unmapped@layerv.ai")
            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"
        restore()

    def test_handler_no_s3_action(self, moto_inject):
        """Test handler skips when no S3 action in receipt"""
        restore, ses_patcher, _ = _patch_settings_and_ses(moto_inject)
        with ses_patcher:
            from forwarder import handler
            record = _ses_record(s3_bucket=False)
            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"
        restore()

    def test_handler_no_from_header(self, moto_inject):
        """Test handler skips when no From header"""
        restore, ses_patcher, _ = _patch_settings_and_ses(moto_inject)
        with ses_patcher:
            from forwarder import handler
            record = _ses_record(forward_from="")  # empty from
            record["ses"]["mail"]["commonHeaders"]["from"] = []
            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"
        restore()

    def test_handler_forwards_email_successfully(self, moto_inject):
        """Test successful email forwarding with header rewriting"""
        restore, ses_patcher, ses_mock = _patch_settings_and_ses(moto_inject)
        _put_email_in_s3(moto_inject, "fwd/test.eml", _raw_email_bytes())
        with ses_patcher:
            from forwarder import handler
            result = handler({"Records": [_ses_record()]}, MagicMock())
            assert result["status"] == "ok"
            ses_mock.assert_called_once()
            call_kwargs = ses_mock.call_args[1]
            assert "noreply@layerv.ai" in call_kwargs["Source"]
            assert call_kwargs["Destinations"] == ["personal@icloud.com"]
        restore()

    def test_handler_ses_error_does_not_crash(self, moto_inject):
        """Test handler continues after SES send_raw_email error"""
        from botocore.exceptions import ClientError
        restore, ses_patcher, _ = _patch_settings_and_ses(
            moto_inject,
            ses_side_effect=ClientError(
                {"Error": {"Code": "MessageRejected"}},
                "SendRawEmail"
            ),
        )
        _put_email_in_s3(moto_inject, "fwd/test.eml", _raw_email_bytes())
        with ses_patcher:
            from forwarder import handler
            result = handler({"Records": [_ses_record()]}, MagicMock())
            # SES error is caught and logged, handler continues
            assert result["status"] == "ok"
        restore()

    def test_handler_forwarder_ses_error_continues(self, moto_inject):
        """Test handler continues after SESError"""
        from email_sender import SESError
        restore, ses_patcher, _ = _patch_settings_and_ses(
            moto_inject,
            ses_side_effect=SESError("SES rejected"),
        )
        _put_email_in_s3(moto_inject, "fwd/test2.eml", _raw_email_bytes())
        with ses_patcher:
            from forwarder import handler
            result = handler({"Records": [_ses_record(key="fwd/test2.eml")]}, MagicMock())
            assert result["status"] == "ok"
        restore()

    def test_handler_x_headers_added(self, moto_inject):
        """Test X-Original-To and X-Forwarded-For headers are added"""
        restore, ses_patcher, ses_mock = _patch_settings_and_ses(moto_inject)
        _put_email_in_s3(moto_inject, "fwd/hdr.eml", _raw_email_bytes())
        with ses_patcher:
            from forwarder import handler
            import email
            handler({"Records": [_ses_record(key="fwd/hdr.eml")]}, MagicMock())
            call_kwargs = ses_mock.call_args[1]
            raw_data = call_kwargs["RawMessage"]["Data"]
            msg = email.message_from_bytes(raw_data)
            assert "X-Original-To" in msg
            assert "X-Forwarded-For" in msg
        restore()

    def test_handler_record_exception_returns_error(self, moto_inject):
        """Test handler returns error status when S3 raises unexpected exception"""
        restore, ses_patcher, _ = _patch_settings_and_ses(moto_inject)

        # Override S3 get_object to raise
        original_get = moto_inject["s3"].get_object
        moto_inject["s3"].get_object = MagicMock(
            side_effect=RuntimeError("unexpected S3 error")
        )
        try:
            with ses_patcher:
                from forwarder import handler
                result = handler({"Records": [_ses_record()]}, MagicMock())
                assert result["status"] == "error"
                assert "unexpected S3 error" in result["error"]
        finally:
            moto_inject["s3"].get_object = original_get
            restore()

    def test_handler_with_multiple_records(self, moto_inject):
        """Test handler processes multiple records"""
        restore, ses_patcher, ses_mock = _patch_settings_and_ses(moto_inject)
        _put_email_in_s3(moto_inject, "fwd/test.eml", _raw_email_bytes())
        _put_email_in_s3(moto_inject, "fwd/test2.eml", _raw_email_bytes())
        with ses_patcher:
            from forwarder import handler
            record1 = _ses_record(key="fwd/test.eml")
            record2 = _ses_record(forward_to="unmapped@layerv.ai", key="fwd/test2.eml")
            result = handler({"Records": [record1, record2]}, MagicMock())
            assert result["status"] == "ok"
            # Only record1 matches forward map
            ses_mock.assert_called_once()
        restore()
