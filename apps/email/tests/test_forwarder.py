"""
Email forwarding Lambda tests.
"""

import json
from unittest.mock import MagicMock, patch


class TestLoadForwardMap:
    """load_forward_map tests"""

    def test_load_forward_map_success(self):
        """Test successful load of forward map from SSM"""
        with patch("forwarder.ssm") as mock_ssm:
            mock_ssm.get_parameter.return_value = {
                "Parameter": {
                    "Value": json.dumps({
                        "justin@layerv.ai": "personal@icloud.com",
                        "admin@layerv.ai": "home@gmail.com",
                    })
                }
            }

            mock_settings = MagicMock()
            mock_settings.forward_map_param = "/qurl-email-bot/forward-map"

            with patch("forwarder.get_settings", return_value=mock_settings):
                from forwarder import load_forward_map
                result = load_forward_map()

                assert result["justin@layerv.ai"] == "personal@icloud.com"
                assert result["admin@layerv.ai"] == "home@gmail.com"
                assert mock_ssm.get_parameter.called

    def test_load_forward_map_empty_value(self):
        """Test load when SSM parameter is empty"""
        with patch("forwarder.ssm") as mock_ssm:
            mock_ssm.get_parameter.return_value = {"Parameter": {"Value": ""}}

            mock_settings = MagicMock()
            mock_settings.forward_map_param = "/qurl-email-bot/forward-map"

            with patch("forwarder.get_settings", return_value=mock_settings):
                from forwarder import load_forward_map
                result = load_forward_map()
                assert result == {}

    def test_load_forward_map_invalid_json(self):
        """Test load with invalid JSON"""
        with patch("forwarder.ssm") as mock_ssm:
            mock_ssm.get_parameter.return_value = {"Parameter": {"Value": "not valid json"}}

            mock_settings = MagicMock()
            mock_settings.forward_map_param = "/qurl-email-bot/forward-map"

            with patch("forwarder.get_settings", return_value=mock_settings):
                from forwarder import load_forward_map
                result = load_forward_map()
                assert result == {}

    def test_load_forward_map_ssm_error(self):
        """Test load when SSM raises ClientError"""
        from botocore.exceptions import ClientError

        with patch("forwarder.ssm") as mock_ssm:
            mock_ssm.get_parameter.side_effect = ClientError(
                {"Error": {"Code": "ParameterNotFound"}},
                "GetParameter"
            )

            mock_settings = MagicMock()
            mock_settings.forward_map_param = "/qurl-email-bot/forward-map"

            with patch("forwarder.get_settings", return_value=mock_settings):
                from forwarder import load_forward_map
                result = load_forward_map()
                assert result == {}


class TestForwarderHandler:
    """Forwarder Lambda handler tests"""

    def test_handler_no_records(self):
        """Test handler with no records"""
        with patch("forwarder.load_forward_map", return_value={}):
            from forwarder import handler
            result = handler({"Records": []}, MagicMock())
            assert result["status"] == "ok"

    def test_handler_no_forward_map_entry(self):
        """Test handler skips when no forward map entry exists"""
        with patch("forwarder.load_forward_map", return_value={}):
            from forwarder import handler

            record = {
                "ses": {
                    "mail": {
                        "commonHeaders": {
                            "from": ["alice@external.com"],
                            "to": ["notmapped@layerv.ai"],
                        }
                    },
                    "receipt": {
                        "action": {
                            "bucketName": "test-bucket",
                            "objectKey": "fwd/test.eml",
                        }
                    },
                }
            }

            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"

    def test_handler_no_s3_action(self):
        """Test handler skips when no S3 action in receipt"""
        with patch("forwarder.load_forward_map", return_value={"justin@layerv.ai": "personal@icloud.com"}):
            from forwarder import handler

            record = {
                "ses": {
                    "mail": {
                        "commonHeaders": {
                            "from": ["alice@external.com"],
                            "to": ["justin@layerv.ai"],
                        }
                    },
                    "receipt": {
                        "action": {}  # No bucketName or objectKey
                    },
                }
            }

            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"

    def test_handler_no_from_header(self):
        """Test handler skips when no From header"""
        with patch("forwarder.load_forward_map", return_value={"justin@layerv.ai": "personal@icloud.com"}):
            from forwarder import handler

            record = {
                "ses": {
                    "mail": {
                        "commonHeaders": {
                            "from": [],  # Empty From
                            "to": ["justin@layerv.ai"],
                        }
                    },
                    "receipt": {},
                }
            }

            result = handler({"Records": [record]}, MagicMock())
            assert result["status"] == "ok"

    def test_handler_forwards_email_successfully(self):
        """Test successful email forwarding with header rewriting"""
        with patch("forwarder.load_forward_map", return_value={"justin@layerv.ai": "personal@icloud.com"}):
            with patch("forwarder.s3") as mock_s3:
                with patch("forwarder.ses") as mock_ses:
                    mock_settings = MagicMock()
                    mock_settings.bot_address = "qurl@layerv.ai"

                    with patch("forwarder.get_settings", return_value=mock_settings):
                        from forwarder import handler

                        # Create a minimal raw email
                        raw_email = (
                            b"From: alice@external.com\r\n"
                            b"To: justin@layerv.ai\r\n"
                            b"Subject: Test\r\n\r\n"
                            b"Hello, this is a forwarded email.\r\n"
                        )

                        mock_s3.get_object.return_value = {"Body": MagicMock(read=MagicMock(return_value=raw_email))}
                        mock_ses.send_raw_email.return_value = {"MessageId": "forwarded-123"}

                        record = {
                            "ses": {
                                "mail": {
                                    "commonHeaders": {
                                        "from": ["alice@external.com"],
                                        "to": ["justin@layerv.ai"],
                                    }
                                },
                                "receipt": {
                                    "action": {
                                        "bucketName": "test-bucket",
                                        "objectKey": "fwd/test.eml",
                                    }
                                },
                            }
                        }

                        result = handler({"Records": [record]}, MagicMock())

                        assert result["status"] == "ok"
                        mock_ses.send_raw_email.assert_called_once()
                        call_kwargs = mock_ses.send_raw_email.call_args[1]
                        assert "noreply@layerv.ai" in call_kwargs["Source"]
                        assert call_kwargs["Destinations"] == ["personal@icloud.com"]

    def test_handler_ses_error_does_not_crash(self):
        """Test handler returns error status on SES failure"""
        with patch("forwarder.load_forward_map", return_value={"justin@layerv.ai": "personal@icloud.com"}):
            with patch("forwarder.s3") as mock_s3:
                with patch("forwarder.ses") as mock_ses:
                    mock_settings = MagicMock()
                    mock_settings.bot_address = "qurl@layerv.ai"

                    with patch("forwarder.get_settings", return_value=mock_settings):
                        from forwarder import handler
                        from botocore.exceptions import ClientError

                        raw_email = (
                            b"From: alice@external.com\r\n"
                            b"To: justin@layerv.ai\r\n"
                            b"Subject: Test\r\n\r\n"
                            b"Hello.\r\n"
                        )

                        mock_s3.get_object.return_value = {"Body": MagicMock(read=MagicMock(return_value=raw_email))}
                        mock_ses.send_raw_email.side_effect = ClientError(
                            {"Error": {"Code": "MessageRejected"}},
                            "SendRawEmail"
                        )

                        record = {
                            "ses": {
                                "mail": {
                                    "commonHeaders": {
                                        "from": ["alice@external.com"],
                                        "to": ["justin@layerv.ai"],
                                    }
                                },
                                "receipt": {
                                    "action": {
                                        "bucketName": "test-bucket",
                                        "objectKey": "fwd/test.eml",
                                    }
                                },
                            }
                        }

                        result = handler({"Records": [record]}, MagicMock())
                        assert result["status"] == "ok"  # Still returns ok, logs error internally

    def test_handler_x_headers_added(self):
        """Test X-Original-To and X-Forwarded-For headers are added"""
        with patch("forwarder.load_forward_map", return_value={"justin@layerv.ai": "personal@icloud.com"}):
            with patch("forwarder.s3") as mock_s3:
                with patch("forwarder.ses") as mock_ses:
                    mock_settings = MagicMock()
                    mock_settings.bot_address = "qurl@layerv.ai"

                    with patch("forwarder.get_settings", return_value=mock_settings):
                        from forwarder import handler
                        import email

                        raw_email = (
                            b"From: alice@external.com\r\n"
                            b"To: justin@layerv.ai\r\n"
                            b"Subject: Test\r\n\r\n"
                            b"Hello.\r\n"
                        )

                        mock_s3.get_object.return_value = {"Body": MagicMock(read=MagicMock(return_value=raw_email))}
                        mock_ses.send_raw_email.return_value = {"MessageId": "test-123"}

                        record = {
                            "ses": {
                                "mail": {
                                    "commonHeaders": {
                                        "from": ["alice@external.com"],
                                        "to": ["justin@layerv.ai"],
                                    }
                                },
                                "receipt": {
                                    "action": {
                                        "bucketName": "test-bucket",
                                        "objectKey": "fwd/test.eml",
                                    }
                                },
                            }
                        }

                        handler({"Records": [record]}, MagicMock())

                        # Verify the raw email was modified with X headers
                        call_kwargs = mock_ses.send_raw_email.call_args[1]
                        raw_data = call_kwargs["RawMessage"]["Data"]
                        msg = email.message_from_bytes(raw_data)

                        assert "X-Original-To" in msg
                        assert "X-Forwarded-For" in msg
