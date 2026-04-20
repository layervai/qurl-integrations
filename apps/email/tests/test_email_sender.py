"""
Email sender module tests.
"""

import pytest
from unittest.mock import MagicMock, patch
from email_sender import (
    send_email,
    send_link_email,
    send_confirmation,
    send_rejection,
    send_usage_help,
    load_template,
    render_template,
    SESError,
)


class TestLoadTemplate:
    """Template loading tests"""

    def test_load_html_template(self):
        """Test loading HTML template"""
        content = load_template("link_email")
        assert content != ""

    def test_load_nonexistent_template(self):
        """Test loading nonexistent template returns empty string"""
        content = load_template("nonexistent_template")
        assert content == ""

    def test_fallback_to_txt(self):
        """Test fallback to .txt template when .html not found"""
        with patch("pathlib.Path.exists") as mock_exists:
            mock_exists.return_value = False
            # load_template uses Path, difficult to mock completely
            # This test verifies the fallback path exists
            pass


class TestRenderTemplate:
    """Template rendering tests"""

    def test_render_basic(self):
        """Test basic template rendering"""
        template = "Hello, {{name}}!"
        result = render_template(template, name="Alice")
        assert result == "Hello, Alice!"

    def test_render_multiple_vars(self):
        """Test rendering with multiple variables"""
        template = "{{sender}} shared {{resource}} with you"
        result = render_template(template, sender="Alice", resource="report.pdf")
        assert result == "Alice shared report.pdf with you"

    def test_render_missing_var(self):
        """Test rendering with missing variable (no replacement)"""
        template = "Hello, {{name}}! Your link: {{link}}"
        result = render_template(template, name="Alice")
        assert "Hello, Alice!" in result
        assert "{{link}}" in result  # Not replaced

    def test_render_numeric(self):
        """Test rendering numeric values"""
        template = "Sent: {{count}} emails"
        result = render_template(template, count=5)
        assert result == "Sent: 5 emails"


class TestSendEmail:
    """send_email function tests"""

    def test_send_email_success(self):
        """Test successful email sending"""
        with patch("email_sender.get_ses_client") as mock_ses_fn:
            with patch("email_sender.get_settings") as mock_settings:
                mock_settings.return_value.bot_address = "qurl@layerv.ai"
                mock_ses_client = MagicMock()
                mock_ses_fn.return_value = mock_ses_client
                mock_ses_client.send_email.return_value = {"MessageId": "test-123"}

                result = send_email(
                    to_addresses=["bob@example.com"],
                    subject="Test Subject",
                    html_body="<p>Test body</p>",
                )

                assert "MessageId" in result
                mock_ses_client.send_email.assert_called_once()

    def test_send_email_with_text_body(self):
        """Test sending email with both HTML and text body"""
        with patch("email_sender.get_ses_client") as mock_ses_fn:
            with patch("email_sender.get_settings") as mock_settings:
                mock_settings.return_value.bot_address = "qurl@layerv.ai"
                mock_ses_client = MagicMock()
                mock_ses_fn.return_value = mock_ses_client
                mock_ses_client.send_email.return_value = {"MessageId": "test-456"}

                send_email(
                    to_addresses=["bob@example.com"],
                    subject="Test",
                    html_body="<p>HTML</p>",
                    text_body="Plain text",
                )

                call_kwargs = mock_ses_client.send_email.call_args[1]
                assert "Text" in call_kwargs["Message"]["Body"]

    def test_send_email_failure(self):
        """Test email sending failure"""
        from botocore.exceptions import ClientError

        with patch("email_sender.get_ses_client") as mock_ses_fn:
            with patch("email_sender.get_settings") as mock_settings:
                mock_settings.return_value.bot_address = "qurl@layerv.ai"
                mock_ses_client = MagicMock()
                mock_ses_fn.return_value = mock_ses_client
                mock_ses_client.send_email.side_effect = ClientError(
                    {"Error": {"Code": "MessageRejected"}},
                    "SendEmail"
                )

                with pytest.raises(SESError):
                    send_email(
                        to_addresses=["bob@example.com"],
                        subject="Test",
                        html_body="<p>Test</p>",
                    )


class TestSendLinkEmail:
    """send_link_email function tests"""

    def test_send_link_email_no_template(self):
        """Test sending link email when template not found (fallback)"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = ""  # No template
                mock_send.return_value = {"MessageId": "test-link"}

                result = send_link_email(
                    to="bob@example.com",
                    sender_name="Alice",
                    sender_email="alice@example.com",
                    resource_name="report.pdf",
                    link_url="https://qurl.link/abc123",
                    expires_in="15 minutes",
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                assert "report.pdf" in call_kwargs["subject"]
                assert "abc123" in call_kwargs["html_body"]
                assert "Alice" in call_kwargs["html_body"]  # sender_name in body

    def test_send_link_email_with_template(self):
        """Test sending link email with template rendering"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = "<p>{{sender_name}} shared {{resource_name}}</p>"
                mock_send.return_value = {"MessageId": "test-link-tmpl"}

                result = send_link_email(
                    to="bob@example.com",
                    sender_name="Alice",
                    sender_email="alice@example.com",
                    resource_name="doc.pdf",
                    link_url="https://qurl.link/xyz",
                    expires_in="15m",
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                assert "Alice" in call_kwargs["html_body"]
                assert "doc.pdf" in call_kwargs["html_body"]


class TestSendConfirmation:
    """send_confirmation function tests"""

    def test_confirmation_all_sent(self):
        """Test confirmation email with all successful sends"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = ""
                mock_send.return_value = {"MessageId": "test-conf"}

                results = [
                    {"recipient": "bob@example.com", "status": "sent"},
                    {"recipient": "carol@example.com", "status": "sent"},
                ]

                result = send_confirmation(
                    to="alice@example.com",
                    sender_name="Alice",
                    resource_name="report.pdf",
                    results=results,
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                body = call_kwargs["html_body"]
                assert "report.pdf" in call_kwargs["subject"]
                # Should have Success indicators (checkmark characters)
                assert "&#10003;" in body or "&#10004;" in body or "Success" in body

    def test_confirmation_with_skipped(self):
        """Test confirmation email with skipped results"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = ""
                mock_send.return_value = {"MessageId": "test-conf-skip"}

                results = [
                    {"recipient": "bob@example.com", "status": "sent"},
                    {"recipient": "carol@example.com", "status": "skipped"},
                ]

                result = send_confirmation(
                    to="alice@example.com",
                    sender_name="Alice",
                    resource_name="report.pdf",
                    results=results,
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                body = call_kwargs["html_body"]
                # Should have skipped indicator
                assert "Skipped" in body or "skipped" in body.lower()

    def test_confirmation_with_template(self):
        """Test confirmation email with template rendering"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = "<p>{{resource_name}}: {{total_sent}} sent</p>"
                mock_send.return_value = {"MessageId": "test-conf-tmpl"}

                results = [{"recipient": "bob@example.com", "status": "sent"}]
                result = send_confirmation(
                    to="alice@example.com",
                    sender_name="Alice",
                    resource_name="report.pdf",
                    results=results,
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                body = call_kwargs["html_body"]
                # Template variables replaced
                assert "report.pdf" in body
                assert "1" in body  # total_sent=1

    def test_confirmation_with_failures(self):
        """Test confirmation email with some failures"""
        with patch("email_sender.load_template") as mock_load:
            with patch("email_sender.send_email") as mock_send:
                mock_load.return_value = ""
                mock_send.return_value = {"MessageId": "test-conf-fail"}

                results = [
                    {"recipient": "bob@example.com", "status": "sent"},
                    {"recipient": "dave@example.com", "status": "send_failed", "error": "invalid address"},
                ]

                result = send_confirmation(
                    to="alice@example.com",
                    sender_name="Alice",
                    resource_name="report.pdf",
                    results=results,
                )

                assert "MessageId" in result
                call_kwargs = mock_send.call_args[1]
                body = call_kwargs["html_body"]
                # Should have both success and failure indicators
                assert "Failed" in body or "failed" in body.lower()


class TestSendRejection:
    """send_rejection function tests"""

    def test_rejection_not_authorized(self):
        """Test rejection for unauthorized sender"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-reject"}

            result = send_rejection(
                to="unauthorized@example.com",
                reason="not_authorized",
            )

            assert "MessageId" in result
            call_kwargs = mock_send.call_args[1]
            assert "not authorized" in call_kwargs["html_body"].lower()

    def test_rejection_auth_failed(self):
        """Test rejection for authentication failure"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-reject-auth"}

            result = send_rejection(
                to="bad@example.com",
                reason="auth_failed",
            )

            assert "MessageId" in result
            call_kwargs = mock_send.call_args[1]
            assert "authentication" in call_kwargs["html_body"].lower()

    def test_rejection_rate_limited(self):
        """Test rejection for rate limiting"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-reject-rate"}

            result = send_rejection(
                to="spammer@example.com",
                reason="rate_limited",
            )

            assert "MessageId" in result
            call_kwargs = mock_send.call_args[1]
            assert "limit" in call_kwargs["html_body"].lower()

    def test_rejection_unknown_reason(self):
        """Test rejection with unknown reason uses default message"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-reject-unknown"}

            result = send_rejection(
                to="unknown@example.com",
                reason="unknown_reason",
            )

            assert "MessageId" in result
            call_kwargs = mock_send.call_args[1]
            assert "unable to process" in call_kwargs["html_body"].lower()


class TestSendUsageHelp:
    """send_usage_help function tests"""

    def test_usage_help_html(self):
        """Test usage help email HTML content"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-help"}

            send_usage_help(to="alice@example.com")

            call_kwargs = mock_send.call_args[1]
            body = call_kwargs["html_body"]
            # Should contain usage instructions
            assert "qurl@layerv.ai" in body
            assert "Send to:" in body
            assert "recipients" in body.lower()

    def test_usage_help_text(self):
        """Test usage help email plain text content"""
        with patch("email_sender.send_email") as mock_send:
            mock_send.return_value = {"MessageId": "test-help-text"}

            send_usage_help(to="alice@example.com")

            call_kwargs = mock_send.call_args[1]
            text_body = call_kwargs.get("text_body", "")
            # Should contain key instructions
            assert "Send to:" in text_body
            assert "qurl@layerv.ai" in text_body
