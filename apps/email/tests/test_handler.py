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
