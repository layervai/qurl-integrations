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
