"""
Authentication module tests.
"""

import json
from unittest.mock import patch, MagicMock

from auth import (
    authenticate_sender,
    load_authorized_senders_from_ssm,
    check_spf_dkim,
    verify_email_authentication,
)


class TestLoadAuthorizedSenders:
    """Load authorized senders tests"""

    def test_load_from_ssm(self):
        """Test loading authorized senders from SSM"""
        with patch("auth.boto3") as mock_boto3:
            mock_ssm = MagicMock()
            mock_boto3.client.return_value = mock_ssm

            mock_ssm.get_parameter.return_value = {
                "Parameter": {
                    "Value": json.dumps(["alice@example.com", "bob@example.com"])
                }
            }

            settings = MagicMock()
            settings.authorized_senders_param = "/test/param"
            settings.aws_region = "us-east-1"

            result = load_authorized_senders_from_ssm(settings)

            assert "alice@example.com" in result
            assert "bob@example.com" in result
            assert mock_ssm.get_parameter.called

    def test_empty_list(self):
        """Test empty list"""
        with patch("auth.boto3") as mock_boto3:
            mock_ssm = MagicMock()
            mock_boto3.client.return_value = mock_ssm

            mock_ssm.get_parameter.return_value = {
                "Parameter": {"Value": ""}
            }

            settings = MagicMock()
            settings.authorized_senders_param = "/test/param"
            settings.aws_region = "us-east-1"

            result = load_authorized_senders_from_ssm(settings)
            assert len(result) == 0

    def test_invalid_json(self):
        """Test invalid JSON"""
        with patch("auth.boto3") as mock_boto3:
            mock_ssm = MagicMock()
            mock_boto3.client.return_value = mock_ssm

            mock_ssm.get_parameter.return_value = {
                "Parameter": {"Value": "not valid json"}
            }

            settings = MagicMock()
            settings.authorized_senders_param = "/test/param"
            settings.aws_region = "us-east-1"

            result = load_authorized_senders_from_ssm(settings)
            assert len(result) == 0


class TestAuthenticateSender:
    """Sender authentication tests"""

    def test_authorized_sender(self):
        """Test authorized sender"""
        with patch("auth.load_authorized_senders_from_ssm") as mock_load:
            mock_load.return_value = {"alice@example.com", "bob@example.com"}

            settings = MagicMock()
            settings.bot_address = "qurl@layerv.ai"

            result = authenticate_sender("Alice@Example.COM", settings)

            assert result is not None
            assert result.email == "alice@example.com"
            assert result.owner_id == "email:alice@example.com"
            assert result.tier == "growth"

    def test_unauthorized_sender(self):
        """Test unauthorized sender"""
        with patch("auth.load_authorized_senders_from_ssm") as mock_load:
            mock_load.return_value = {"alice@example.com"}

            settings = MagicMock()
            settings.bot_address = "qurl@layerv.ai"

            result = authenticate_sender("bob@example.com", settings)
            assert result is None

    def test_empty_authorized_list(self):
        """Test empty authorized list"""
        with patch("auth.load_authorized_senders_from_ssm") as mock_load:
            mock_load.return_value = set()

            settings = MagicMock()
            settings.bot_address = "qurl@layerv.ai"

            result = authenticate_sender("alice@example.com", settings)
            assert result is None


class TestSpfDkim:
    """SPF/DKIM verification tests"""

    def test_spf_pass(self):
        """Test SPF pass"""
        mock_msg = MagicMock()
        mock_msg.get.side_effect = lambda key, *args: {
            "Received-SPF": "pass from mx.example.com",
            "Authentication-Results": "",
        }.get(key, "")

        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is True
        assert dkim_pass is False

    def test_dkim_pass(self):
        """Test DKIM pass"""
        mock_msg = MagicMock()
        mock_msg.get.side_effect = lambda key, *args: {
            "Received-SPF": "",
            "Authentication-Results": "dkim=pass header.d=example.com",
        }.get(key, "")

        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is False
        assert dkim_pass is True

    def test_both_pass(self):
        """Test both pass"""
        mock_msg = MagicMock()
        mock_msg.get.side_effect = lambda key, *args: {
            "Received-SPF": "pass",
            "Authentication-Results": "dkim=pass",
        }.get(key, "")

        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is True
        assert dkim_pass is True

    def test_verify_email_authentication(self):
        """Test email authentication verification"""
        mock_msg = MagicMock()
        mock_msg.get.side_effect = lambda key, *args: {
            "Received-SPF": "pass",
            "Authentication-Results": "",
        }.get(key, "")

        result = verify_email_authentication(mock_msg)
        assert result is True

    def test_verify_email_authentication_fail(self):
        """Test email authentication verification failure"""
        mock_msg = MagicMock()
        mock_msg.get.side_effect = lambda key, *args: {
            "Received-SPF": "fail",
            "Authentication-Results": "",
        }.get(key, "")

        result = verify_email_authentication(mock_msg)
        assert result is False
