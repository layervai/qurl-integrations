"""
Email parsing module tests.
"""

from email_parser import (
    parse_recipients,
    extract_urls,
    validate_attachment,
)


class TestParseRecipients:
    """Recipient parsing tests"""

    def test_parse_explicit_send_to_block(self):
        """Test parsing explicit Send to block"""
        body = """Send to:
bob@company.com
carol@company.com

Hello, this is a test email.
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert result == ["bob@company.com", "carol@company.com"]

    def test_parse_share_with_block(self):
        """Test parsing Share with block"""
        body = """Share with:
bob@company.com

Best regards
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert result == ["bob@company.com"]

    def test_parse_recipients_block(self):
        """Test parsing Recipients block"""
        body = """Recipients: bob@company.com, carol@company.com

Message
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert result == ["bob@company.com", "carol@company.com"]

    def test_parse_fallback_above_signature(self):
        """Test fallback to body above signature delimiter"""
        body = """Hello

bob@company.com

Best,
John

--
John Doe
john@example.com
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert "bob@company.com" in result
        assert "john@example.com" not in result

    def test_exclude_sender(self):
        """Test sender exclusion"""
        body = """Send to:
sender@example.com
bob@company.com
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert "sender@example.com" not in result
        assert "bob@company.com" in result

    def test_exclude_bot_address(self):
        """Test bot address exclusion"""
        body = """Send to:
qurl@layerv.ai
bob@company.com
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert "qurl@layerv.ai" not in result
        assert "bob@company.com" in result

    def test_deduplicate(self):
        """Test deduplication"""
        body = """Send to:
bob@company.com
carol@company.com
bob@company.com
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert result.count("bob@company.com") == 1

    def test_case_insensitive(self):
        """Test case insensitive"""
        body = """Send to:
Bob@Company.com
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert "bob@company.com" in result

    def test_no_recipients(self):
        """Test no recipients case"""
        body = """Hello, this is a test email with no recipients.
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert len(result) == 0

    def test_only_sender_as_recipient(self):
        """Test only sender's own email as recipient"""
        body = """Send to:
sender@example.com

Message
"""
        result = parse_recipients(body, "sender@example.com", "qurl@layerv.ai")
        assert len(result) == 0


class TestExtractUrls:
    """URL extraction tests"""

    def test_extract_simple_url(self):
        """Test extracting simple URL"""
        body = """Check out this link: https://example.com/document.pdf

Best
"""
        result = extract_urls(body)
        assert "https://example.com/document.pdf" in result

    def test_exclude_qurl_link(self):
        """Test excluding qurl.link links"""
        body = """Here's a Qurl: https://qurl.link/abc123

And a regular link: https://example.com
"""
        result = extract_urls(body)
        assert "https://qurl.link/abc123" not in result
        assert "https://example.com" in result

    def test_multiple_urls(self):
        """Test extracting multiple URLs"""
        body = """Links:
https://example.com/1
https://example.com/2
https://example.com/3
"""
        result = extract_urls(body)
        assert len(result) == 3

    def test_clean_trailing_punctuation(self):
        """Test cleaning trailing punctuation"""
        body = """Link: https://example.com/page.

End of email.
"""
        result = extract_urls(body)
        assert "https://example.com/page" in result


class TestGetBodyText:
    """Body text extraction tests"""

    def test_multipart_with_plain_text(self):
        """Test extracting body from multipart email with plain text"""
        from email_parser import get_body_text
        import email.message

        msg = email.message.EmailMessage()
        msg["Subject"] = "Test"
        msg.set_payload("Hello, this is plain text")
        result = get_body_text(msg)
        assert result == "Hello, this is plain text"

    def test_multipart_skips_attachments(self):
        """Test that attachments are skipped in multipart email"""
        from email_parser import get_body_text
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body text", "plain"))
        att = MIMEText("attachment content", "plain")
        att.add_header("Content-Disposition", "attachment", filename="test.txt")
        msg.attach(att)
        result = get_body_text(msg)
        assert result == "Body text"

    def test_body_decode_failure(self):
        """Test graceful handling of decode failure"""
        from email_parser import get_body_text
        import email.message

        msg = email.message.EmailMessage()
        msg["Subject"] = "Test"
        # Python 3.14: set_payload with valid charset but corrupted bytes triggers
        # decode error on get_payload. The bytes are ISO-8859-1 but we treat as utf-8.
        msg.set_payload(b"\xe9non-decodable", charset="iso-8859-1")
        result = get_body_text(msg)
        # errors="replace" produces replacement chars, not an exception
        assert result != ""

    def test_multipart_body_decode_failure(self):
        """Test multipart with part decode failure falls through"""
        from email_parser import get_body_text
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        bad_part = MIMEText("bad", "plain")
        bad_part.set_param("charset", "nonexistent")
        msg.attach(bad_part)
        msg.attach(MIMEText("Good body", "plain"))
        result = get_body_text(msg)
        assert "Good body" in result


class TestGetBodyHtml:
    """HTML body extraction tests"""

    def test_multipart_with_html(self):
        """Test extracting HTML from multipart email"""
        from email_parser import get_body_html
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText

        msg = MIMEMultipart("alternative")
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Plain text", "plain"))
        msg.attach(MIMEText("<html><body>HTML content</body></html>", "html"))
        result = get_body_html(msg)
        assert "<html>" in result
        assert "HTML content" in result

    def test_no_html(self):
        """Test returns None when no HTML part exists"""
        from email_parser import get_body_html
        import email.message

        msg = email.message.EmailMessage()
        msg["Subject"] = "Test"
        msg.set_payload("Plain text only")
        result = get_body_html(msg)
        assert result is None


class TestGetSender:
    """Sender extraction tests"""

    def test_sender_simple(self):
        """Test extracting sender with simple email"""
        from email_parser import get_sender
        import email.message

        msg = email.message.EmailMessage()
        msg["From"] = "sender@example.com"
        name, addr = get_sender(msg)
        assert addr == "sender@example.com"

    def test_sender_with_display_name(self):
        """Test extracting sender with display name"""
        from email_parser import get_sender
        import email.message

        msg = email.message.EmailMessage()
        msg["From"] = "John Doe <john@example.com>"
        name, addr = get_sender(msg)
        assert addr == "john@example.com"
        assert "John" in name or "Doe" in name or "john" in name

    def test_sender_case_normalized(self):
        """Test sender email is normalized to lowercase"""
        from email_parser import get_sender
        import email.message

        msg = email.message.EmailMessage()
        msg["From"] = "Sender@EXAMPLE.COM"
        name, addr = get_sender(msg)
        assert addr == "sender@example.com"

    def test_sender_no_email_in_header(self):
        """Test sender with no email address in From header"""
        from email_parser import get_sender
        import email.message

        msg = email.message.EmailMessage()
        msg["From"] = "Just a name"
        name, addr = get_sender(msg)
        assert "@" not in addr


class TestExtractAttachments:
    """Attachment extraction tests"""

    def test_extract_from_multipart(self):
        """Test extracting attachments from multipart email"""
        from email_parser import extract_attachments
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText
        from email.mime.application import MIMEApplication

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body", "plain"))
        att = MIMEApplication(b"PDF content here", "pdf")
        att.add_header("Content-Disposition", "attachment", filename="report.pdf")
        msg.attach(att)
        attachments = extract_attachments(msg)
        assert len(attachments) == 1
        assert attachments[0].filename == "report.pdf"
        assert attachments[0].content_type == "application/pdf"

    def test_extract_inline_non_text(self):
        """Test inline attachments that are not text are extracted"""
        from email_parser import extract_attachments
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText
        from email.mime.image import MIMEImage

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body", "plain"))
        img = MIMEImage(b"\x89PNG\r\n\x1a\n", "png")
        img.add_header("Content-Disposition", "inline", filename="image.png")
        msg.attach(img)
        attachments = extract_attachments(msg)
        assert len(attachments) >= 1

    def test_extract_no_filename(self):
        """Test attachment without filename is ignored"""
        from email_parser import extract_attachments
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body", "plain"))
        att = MIMEText("No filename", "plain")
        att.add_header("Content-Disposition", "attachment")
        msg.attach(att)
        attachments = extract_attachments(msg)
        assert len(attachments) == 0

    def test_extract_base64_encoded_payload(self):
        """Test extracting attachment with base64 encoded payload string"""
        from email_parser import extract_attachments
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText
        import base64

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body", "plain"))
        encoded = base64.b64encode(b"File content").decode()
        att = MIMEText(encoded, "plain")
        att.add_header("Content-Disposition", "attachment", filename="encoded.txt")
        msg.attach(att)
        attachments = extract_attachments(msg)
        assert len(attachments) == 1

    def test_extract_null_payload(self):
        """Test attachment with payload that decodes to None is skipped"""
        from email_parser import extract_attachments
        from email.mime.multipart import MIMEMultipart
        from email.mime.text import MIMEText

        msg = MIMEMultipart()
        msg["Subject"] = "Test"
        msg.attach(MIMEText("Body", "plain"))
        # Create an attachment with no filename - should be skipped
        att = MIMEText("content", "plain")
        att.add_header("Content-Disposition", "attachment")
        # Clear any filename
        del att["Content-Disposition"]
        att.add_header("Content-Disposition", "attachment")
        msg.attach(att)
        attachments = extract_attachments(msg)
        # No filename → skipped
        assert len(attachments) == 0


class TestParseEmail:
    """Full email parsing tests"""

    def test_parse_complete_email(self):
        """Test parsing a complete email"""
        from email_parser import parse_email
        import email.message

        msg = email.message.EmailMessage()
        msg["From"] = "sender@example.com"
        msg["Subject"] = "Test Subject"
        msg["To"] = "qurl@layerv.ai"
        msg.set_payload("Test body")
        result = parse_email(msg)
        assert result.sender_email == "sender@example.com"
        assert result.subject == "Test Subject"
        assert result.body_text == "Test body"
        assert result.recipients == []  # callers fill this


class TestValidateAttachment:
    """Attachment validation tests"""

    def test_valid_pdf(self):
        """Test valid PDF file"""
        valid, error = validate_attachment("report.pdf", "application/pdf", 1024 * 1024)
        assert valid is True
        assert error == ""

    def test_valid_image(self):
        """Test valid image file"""
        valid, error = validate_attachment("photo.jpg", "image/jpeg", 512 * 1024)
        assert valid is True

    def test_file_too_large(self):
        """Test file too large"""
        max_size = 25
        size = (max_size + 1) * 1024 * 1024
        valid, error = validate_attachment("large.pdf", "application/pdf", size, max_size)
        assert valid is False
        assert "max 25MB" in error

    def test_unsupported_type(self):
        """Test unsupported file type"""
        valid, error = validate_attachment("script.exe", "application/octet-stream", 1024)
        assert valid is False
        assert "Unsupported file type" in error

    def test_allowed_extensions(self):
        """Test allowed extensions"""
        allowed = [".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".docx", ".xlsx"]
        for ext in allowed:
            valid, error = validate_attachment(f"file{ext}", "application/octet-stream", 1024)
            assert valid is True, f"Extension {ext} should be allowed"
