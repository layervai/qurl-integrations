"""
Lambda Handler tests.
"""

import json
from unittest.mock import MagicMock, patch
from handler import handler, process_sqs_record


class TestHandler:
    """Handler tests"""

    def test_handler_empty_event(self):
        """Test empty event"""
        result = handler({"Records": []}, MagicMock())
        assert result["processed"] == 0

    def test_handler_single_record(self):
        """Test single record"""
        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {
                        "bucket": {"name": "test-bucket"},
                        "object": {"key": "bot/test-email.eml"}
                    }
                }]
            })
        }

        with patch("handler.get_s3_client") as mock_s3:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.verify_email_authentication"):
                    with patch("handler.send_rejection") as mock_reject:
                        mock_s3_client = MagicMock()
                        mock_s3.return_value = mock_s3_client

                        mock_body = MagicMock()
                        mock_body.read.return_value = b"From: unauthorized@example.com\r\n\r\nTest"
                        mock_s3_client.get_object.return_value = {"Body": mock_body}

                        mock_auth.return_value = None

                        result = handler({"Records": [mock_record]}, MagicMock())

                        assert result["processed"] == 1
                        assert mock_reject.called


class TestHandlerHelpers:
    """Handler helper function tests"""

    def test_get_s3_client(self):
        """Test S3 client is cached"""
        from handler import get_s3_client
        from handler import _s3_client
        import handler as h

        h._s3_client = None
        client1 = get_s3_client()
        client2 = get_s3_client()
        assert client1 is client2

    def test_get_qurl_client(self):
        """Test QURL client is cached"""
        from handler import get_qurl
        import handler as h

        h._qurl_client = None
        with patch("handler.get_settings") as mock_settings:
            mock_settings.return_value = MagicMock(
                qurl_api_key="test-key",
                upload_api_url="https://u.example.com",
                mint_link_api_url="https://m.example.com",
                aws_region="us-east-1",
            )
            client1 = get_qurl()
            client2 = get_qurl()
            assert client1 is client2

    def test_get_limiter(self):
        """Test rate limiter is cached"""
        from handler import get_limiter
        import handler as h

        h._rate_limiter = None
        with patch("handler.get_rate_limiter") as mock_factory:
            mock_factory.return_value = MagicMock()
            limiter1 = get_limiter()
            limiter2 = get_limiter()
            assert limiter1 is limiter2


class TestHandlerExceptions:
    """Handler exception handling tests"""

    def test_handler_record_exception(self):
        """Test handler continues processing when one record fails"""
        import handler as h
        from unittest.mock import patch, MagicMock

        h._s3_client = None
        h._qurl_client = None
        h._rate_limiter = None

        bad_record = {"body": "not valid json"}
        good_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }

        with patch("handler.get_s3_client") as mock_s3:
            mock_client = MagicMock()
            mock_s3.return_value = mock_client
            mock_body = MagicMock()
            mock_body.read.return_value = b"From: test@example.com\r\n\r\nbody"
            mock_client.get_object.return_value = {"Body": mock_body}
            with patch("handler.authenticate_sender", return_value=None):
                with patch("handler.send_rejection"):
                    with patch("handler.cleanup_s3"):
                        result = h.handler({"Records": [bad_record, good_record]}, MagicMock())

        assert result["processed"] == 2
        assert result["results"][0]["status"] == "error"
        assert result["results"][1]["status"] == "success"


class TestProcessSqsRecord:
    """SQS record processing tests"""

    def test_process_unauthorized_sender(self):
        """Test processing unauthorized sender"""
        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {
                        "bucket": {"name": "test-bucket"},
                        "object": {"key": "bot/test.eml"}
                    }
                }]
            })
        }

        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.send_rejection") as mock_reject:
                    with patch("handler.cleanup_s3"):
                        mock_s3 = MagicMock()
                        mock_s3_fn.return_value = mock_s3

                        import email.message
                        msg = email.message.EmailMessage()
                        msg["From"] = "unauthorized@example.com"
                        msg["Subject"] = "Test"
                        msg["To"] = "qurl@layerv.ai"
                        msg.set_payload("Test body")

                        mock_body = MagicMock()
                        mock_body.read.return_value = msg.as_bytes()
                        mock_s3.get_object.return_value = {"Body": mock_body}

                        mock_auth.return_value = None

                        result = process_sqs_record(mock_record, mock_settings)

                        assert result["status"] == "rejected"
                        assert result["reason"] == "not_authorized"
                        mock_reject.assert_called_once()

    def test_process_auth_failed(self):
        """Test processing with failed email authentication"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.verify_email_authentication", return_value=False):
                    with patch("handler.send_rejection") as mock_reject:
                        with patch("handler.cleanup_s3"):
                            mock_s3 = MagicMock()
                            mock_s3_fn.return_value = mock_s3
                            mock_body = MagicMock()
                            import email.message
                            msg = email.message.EmailMessage()
                            msg["From"] = "sender@example.com"
                            msg.set_payload("body")
                            mock_body.read.return_value = msg.as_bytes()
                            mock_s3.get_object.return_value = {"Body": mock_body}
                            mock_auth.return_value = MagicMock(owner_id="owner1")
                            result = process_sqs_record(mock_record, mock_settings)
                            assert result["status"] == "rejected"
                            assert result["reason"] == "auth_failed"

    def test_process_rate_limited(self):
        """Test processing with rate limit exceeded"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=False)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.verify_email_authentication", return_value=True):
                    with patch("handler.get_limiter", return_value=mock_limiter):
                        with patch("handler.send_rejection") as mock_reject:
                            with patch("handler.cleanup_s3"):
                                mock_s3 = MagicMock()
                                mock_s3_fn.return_value = mock_s3
                                mock_body = MagicMock()
                                import email.message
                                msg = email.message.EmailMessage()
                                msg["From"] = "sender@example.com"
                                msg.set_payload("Send to:\nbob@company.com\n\nTest")
                                mock_body.read.return_value = msg.as_bytes()
                                mock_s3.get_object.return_value = {"Body": mock_body}
                                mock_auth.return_value = MagicMock(owner_id="owner1")
                                result = process_sqs_record(mock_record, mock_settings)
                                assert result["status"] == "rejected"
                                assert result["reason"] == "rate_limited"

    def test_process_no_recipients(self):
        """Test processing when no recipients found"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.verify_email_authentication", return_value=True):
                    with patch("handler.get_limiter", return_value=mock_limiter):
                        with patch("handler.send_usage_help"):
                            with patch("handler.cleanup_s3"):
                                mock_s3 = MagicMock()
                                mock_s3_fn.return_value = mock_s3
                                mock_body = MagicMock()
                                import email.message
                                msg = email.message.EmailMessage()
                                msg["From"] = "sender@example.com"
                                msg.set_payload("Hello, no recipients here.")
                                mock_body.read.return_value = msg.as_bytes()
                                mock_s3.get_object.return_value = {"Body": mock_body}
                                mock_auth.return_value = MagicMock(owner_id="owner1")
                                result = process_sqs_record(mock_record, mock_settings)
                                assert result["status"] == "no_recipients"

    def test_process_no_resource(self):
        """Test processing when no attachments or URLs found"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.authenticate_sender") as mock_auth:
                with patch("handler.verify_email_authentication", return_value=True):
                    with patch("handler.get_limiter", return_value=mock_limiter):
                        with patch("handler.send_usage_help"):
                            with patch("handler.cleanup_s3"):
                                mock_s3 = MagicMock()
                                mock_s3_fn.return_value = mock_s3
                                mock_body = MagicMock()
                                import email.message
                                msg = email.message.EmailMessage()
                                msg["From"] = "sender@example.com"
                                msg.set_payload("Send to:\nbob@company.com\n\nNo links here.")
                                mock_body.read.return_value = msg.as_bytes()
                                mock_s3.get_object.return_value = {"Body": mock_body}
                                mock_auth.return_value = MagicMock(owner_id="owner1")
                                result = process_sqs_record(mock_record, mock_settings)
                                assert result["status"] == "no_resource"

    def test_process_skipped_idempotent(self):
        """Test processing skips already-sent resource"""
        import handler as h
        from handler import DispatchStatus

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.get_qurl") as mock_qurl_fn:
                with patch("handler.authenticate_sender") as mock_auth:
                    with patch("handler.verify_email_authentication", return_value=True):
                        with patch("handler.get_limiter", return_value=mock_limiter):
                            with patch("handler.check_idempotent", return_value=True):
                                with patch("handler.log_dispatch") as mock_log:
                                    with patch("email_sender.get_ses_client") as mock_ses:
                                        mock_ses.return_value = MagicMock()
                                        with patch("handler.cleanup_s3"):
                                            mock_s3 = MagicMock()
                                            mock_s3_fn.return_value = mock_s3
                                            mock_qurl = MagicMock()
                                            mock_qurl_fn.return_value = mock_qurl
                                            mock_body = MagicMock()
                                            import email.message
                                            msg = email.message.EmailMessage()
                                            msg["From"] = "sender@example.com"
                                            msg.set_payload("Send to:\nbob@company.com\n\nhttps://example.com")
                                            mock_body.read.return_value = msg.as_bytes()
                                            mock_s3.get_object.return_value = {"Body": mock_body}
                                            mock_auth.return_value = MagicMock(owner_id="owner1")
                                            mock_qurl.create_qurl.return_value = MagicMock(
                                                resource_id="r1", filename="https://example.com"
                                            )
                                            result = process_sqs_record(mock_record, mock_settings)
                                            assert result["status"] == "processed"
                                            # Verify SKIPPED was logged
                                            skip_calls = [c for c in mock_log.call_args_list
                                                          if c[1].get("status") == DispatchStatus.SKIPPED]
                                            assert len(skip_calls) > 0

    def test_process_mint_error(self):
        """Test processing with mint link error"""
        import handler as h
        from services.qurl_client import MintError

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.get_qurl") as mock_qurl_fn:
                with patch("handler.authenticate_sender") as mock_auth:
                    with patch("handler.verify_email_authentication", return_value=True):
                        with patch("handler.get_limiter", return_value=mock_limiter):
                            with patch("handler.log_dispatch") as mock_log:
                                with patch("email_sender.get_ses_client") as mock_ses:
                                    mock_ses.return_value = MagicMock()
                                    with patch("handler.cleanup_s3"):
                                        with patch("handler.check_idempotent", return_value=False):
                                            mock_s3 = MagicMock()
                                            mock_s3_fn.return_value = mock_s3
                                            mock_qurl = MagicMock()
                                            mock_qurl_fn.return_value = mock_qurl
                                            mock_body = MagicMock()
                                            import email.message
                                            msg = email.message.EmailMessage()
                                            msg["From"] = "sender@example.com"
                                            msg.set_payload("Send to:\nbob@company.com\n\nhttps://example.com")
                                            mock_body.read.return_value = msg.as_bytes()
                                            mock_s3.get_object.return_value = {"Body": mock_body}
                                            mock_auth.return_value = MagicMock(owner_id="owner1")
                                            mock_qurl.create_qurl.return_value = MagicMock(
                                                resource_id="r1", filename="https://example.com"
                                            )
                                            mock_qurl.mint_link.side_effect = MintError("Link minting failed")
                                            result = process_sqs_record(mock_record, mock_settings)
                                            assert result["status"] == "processed"
                                        # Verify mint_failed was logged
                                        from handler import DispatchStatus
                                        dispatch_calls = [c for c in mock_log.call_args_list if c[1].get("status") == DispatchStatus.MINT_FAILED]
                                        assert len(dispatch_calls) > 0

    def test_process_successful_dispatch(self):
        """Test successful processing and rate limit increment"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.get_qurl") as mock_qurl_fn:
                with patch("handler.authenticate_sender") as mock_auth:
                    with patch("handler.verify_email_authentication", return_value=True):
                            with patch("handler.get_limiter", return_value=mock_limiter):
                                with patch("handler.send_link_email"):
                                    with patch("handler.log_dispatch"):
                                        with patch("email_sender.get_ses_client") as mock_ses:
                                            mock_ses.return_value = MagicMock()
                                            with patch("handler.cleanup_s3"):
                                                with patch("handler.check_idempotent", return_value=False):
                                                    mock_s3 = MagicMock()
                                                    mock_s3_fn.return_value = mock_s3
                                                    mock_qurl = MagicMock()
                                                    mock_qurl_fn.return_value = mock_qurl
                                                    mock_body = MagicMock()
                                                    import email.message
                                                    msg = email.message.EmailMessage()
                                                    msg["From"] = "sender@example.com"
                                                    msg.set_payload("Send to:\nbob@company.com\n\nhttps://example.com")
                                                    mock_body.read.return_value = msg.as_bytes()
                                                    mock_s3.get_object.return_value = {"Body": mock_body}
                                                    mock_auth.return_value = MagicMock(owner_id="owner1")
                                                    mock_qurl.create_qurl.return_value = MagicMock(
                                                        resource_id="r1", filename="https://example.com"
                                                    )
                                                    mock_qurl.mint_link.return_value = MagicMock(
                                                        url="https://qurl.link/abc", hash="abc123"
                                                    )
                                                    result = process_sqs_record(mock_record, mock_settings)
                                                    assert result["status"] == "processed"
                                                    mock_limiter.increment.assert_called_once()

    def test_process_sender_with_display_name(self):
        """Test sender with display name is parsed correctly"""
        import handler as h

        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 25
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.get_qurl") as mock_qurl_fn:
                with patch("handler.authenticate_sender") as mock_auth:
                    with patch("handler.verify_email_authentication", return_value=True):
                        with patch("handler.get_limiter", return_value=mock_limiter):
                            with patch("handler.send_link_email") as mock_send:
                                with patch("handler.log_dispatch"):
                                    with patch("email_sender.get_ses_client") as mock_ses:
                                        mock_ses.return_value = MagicMock()
                                        with patch("handler.cleanup_s3"):
                                            with patch("handler.check_idempotent", return_value=False):
                                                mock_s3 = MagicMock()
                                                mock_s3_fn.return_value = mock_s3
                                                mock_qurl = MagicMock()
                                                mock_qurl_fn.return_value = mock_qurl
                                                mock_body = MagicMock()
                                                import email.message
                                                msg = email.message.EmailMessage()
                                                msg["From"] = "John Doe <john@example.com>"
                                                msg.set_payload("Send to:\nbob@company.com\n\nhttps://example.com")
                                                mock_body.read.return_value = msg.as_bytes()
                                                mock_s3.get_object.return_value = {"Body": mock_body}
                                                mock_auth.return_value = MagicMock(owner_id="owner1")
                                                mock_qurl.create_qurl.return_value = MagicMock(
                                                    resource_id="r1", filename="https://example.com"
                                                )
                                                mock_qurl.mint_link.return_value = MagicMock(
                                                    url="https://qurl.link/abc", hash="abc123"
                                                )
                                                result = process_sqs_record(mock_record, mock_settings)
                                                assert result["status"] == "processed"
                                                call_args = mock_send.call_args
                                                sender_name = call_args[1]["sender_name"].lower()
                                                assert "john" in sender_name

    def test_process_recipient_truncation(self):
        """Test recipient count is truncated to max"""
        import handler as h

        many_recipients = "\n".join(f"user{i}@example.com" for i in range(30))
        mock_record = {
            "body": json.dumps({
                "Records": [{
                    "s3": {"bucket": {"name": "t"}, "object": {"key": "k"}}
                }]
            })
        }
        mock_settings = MagicMock()
        mock_settings.bot_address = "qurl@layerv.ai"
        mock_settings.max_recipients = 5
        mock_settings.max_urls_per_email = 3
        mock_settings.max_attachment_size_mb = 25
        mock_settings.link_expires_in = "15m"
        mock_settings.aws_region = "us-east-1"

        mock_limiter = MagicMock()
        mock_limiter.check.return_value = MagicMock(allowed=True)

        with patch("handler.get_s3_client") as mock_s3_fn:
            with patch("handler.get_qurl") as mock_qurl_fn:
                with patch("handler.authenticate_sender") as mock_auth:
                    with patch("handler.verify_email_authentication", return_value=True):
                        with patch("handler.get_limiter", return_value=mock_limiter):
                            with patch("handler.send_link_email"):
                                with patch("handler.log_dispatch"):
                                    with patch("email_sender.get_ses_client") as mock_ses:
                                        mock_ses.return_value = MagicMock()
                                        with patch("handler.cleanup_s3"):
                                            with patch("handler.check_idempotent", return_value=False):
                                                mock_s3 = MagicMock()
                                                mock_s3_fn.return_value = mock_s3
                                                mock_qurl = MagicMock()
                                                mock_qurl_fn.return_value = mock_qurl
                                                mock_body = MagicMock()
                                                import email.message
                                                msg = email.message.EmailMessage()
                                                msg["From"] = "sender@example.com"
                                                msg.set_payload(f"Send to:\n{many_recipients}\n\nhttps://example.com")
                                                mock_body.read.return_value = msg.as_bytes()
                                                mock_s3.get_object.return_value = {"Body": mock_body}
                                                mock_auth.return_value = MagicMock(owner_id="owner1")
                                                mock_qurl.create_qurl.return_value = MagicMock(
                                                    resource_id="r1", filename="https://example.com"
                                                )
                                                mock_qurl.mint_link.return_value = MagicMock(
                                                    url="https://qurl.link/abc", hash="abc123"
                                                )
                                                result = process_sqs_record(mock_record, mock_settings)
                                                assert result["recipients"] == 5


class TestCleanupS3:
    """S3 cleanup tests"""

    def test_cleanup_s3_success(self):
        """Test successful S3 cleanup"""
        from handler import cleanup_s3
        mock_s3 = MagicMock()
        cleanup_s3(mock_s3, "bucket", "key")
        mock_s3.delete_object.assert_called_once_with(Bucket="bucket", Key="key")

    def test_cleanup_s3_ignore_error(self):
        """Test cleanup ignores errors"""
        from handler import cleanup_s3
        mock_s3 = MagicMock()
        mock_s3.delete_object.side_effect = Exception("S3 error")
        # Should not raise
        cleanup_s3(mock_s3, "bucket", "key")
