from validation import validate_resource_id, validate_file_size, validate_file_type, sanitize_filename, validate_cdn_url, validate_snowflake, split_message, validate_expires, DEFAULT_LINK_EXPIRY, EXPIRY_CHOICES_VALUES

class TestValidateResourceId:
    def test_valid(self):
        assert validate_resource_id("r_abc123def456") == "r_abc123def456"

    def test_with_dollar_prefix(self):
        assert validate_resource_id("$r_abc123def456") is None  # validate_resource_id doesn't strip $

    def test_too_short(self):
        assert validate_resource_id("r_abc") is None

    def test_too_long(self):
        assert validate_resource_id("r_" + "a" * 33) is None

    def test_invalid_chars(self):
        assert validate_resource_id("r_abc/../../etc") is None

    def test_no_prefix(self):
        assert validate_resource_id("abc123def456") is None

    def test_hyphens_underscores(self):
        assert validate_resource_id("r_abc-123_def") == "r_abc-123_def"

class TestValidateFileSize:
    def test_under_limit(self):
        assert validate_file_size(1024, max_mb=25) is True

    def test_at_limit(self):
        assert validate_file_size(25 * 1024 * 1024, max_mb=25) is True

    def test_over_limit(self):
        assert validate_file_size(25 * 1024 * 1024 + 1, max_mb=25) is False

class TestValidateFileType:
    def test_png(self):
        assert validate_file_type("image/png", "test.png") is True

    def test_pdf(self):
        assert validate_file_type("application/pdf", "doc.pdf") is True

    def test_exe(self):
        assert validate_file_type("application/x-msdownload", "virus.exe") is False

    def test_html(self):
        assert validate_file_type("text/html", "page.html") is False

    def test_mime_with_charset(self):
        assert validate_file_type("image/jpeg; charset=utf-8", "photo.jpg") is True

class TestSanitizeFilename:
    def test_normal(self):
        assert sanitize_filename("photo.png") == "photo.png"

    def test_path_traversal(self):
        result = sanitize_filename("../../etc/passwd")
        assert "/" not in result
        assert ".." not in result

    def test_null_bytes(self):
        assert "\x00" not in sanitize_filename("file\x00.png")

    def test_long_name(self):
        assert len(sanitize_filename("a" * 300 + ".png")) <= 255

    def test_empty(self):
        result = sanitize_filename("")
        assert result == "unnamed"

class TestValidateCdnUrl:
    def test_valid_cdn(self):
        assert validate_cdn_url("https://cdn.discordapp.com/attachments/123/456/file.png", "cdn.discordapp.com,media.discordapp.net") is True

    def test_valid_media(self):
        assert validate_cdn_url("https://media.discordapp.net/attachments/123/456/file.png", "cdn.discordapp.com,media.discordapp.net") is True

    def test_invalid_host(self):
        assert validate_cdn_url("https://evil.com/file.png", "cdn.discordapp.com,media.discordapp.net") is False

    def test_http_rejected(self):
        assert validate_cdn_url("http://cdn.discordapp.com/file.png", "cdn.discordapp.com,media.discordapp.net") is False

    def test_malformed_url(self):
        assert validate_cdn_url("not-a-url", "cdn.discordapp.com") is False

class TestValidateSnowflake:
    def test_valid(self):
        assert validate_snowflake("123456789012345678") is True

    def test_too_short(self):
        assert validate_snowflake("1234567890") is False

    def test_too_long(self):
        assert validate_snowflake("123456789012345678901") is False

    def test_non_numeric(self):
        assert validate_snowflake("12345678901234567a") is False

class TestSplitMessage:
    def test_short_message(self):
        assert split_message("hello", 2000) == ["hello"]

    def test_exact_limit(self):
        msg = "a" * 2000
        assert split_message(msg, 2000) == [msg]

    def test_over_limit(self):
        msg = "line1\nline2\nline3\n" * 200
        parts = split_message(msg, 2000)
        assert all(len(p) <= 2000 for p in parts)
        assert len(parts) > 1


class TestValidateExpires:
    def test_all_choices_valid(self):
        for v in EXPIRY_CHOICES_VALUES:
            assert validate_expires(v) is True

    def test_default_is_valid(self):
        assert validate_expires(DEFAULT_LINK_EXPIRY) is True

    def test_invalid_values(self):
        assert validate_expires("") is False
        assert validate_expires("abc") is False
        assert validate_expires("30d") is False
        assert validate_expires("2h") is False
        assert validate_expires("10m") is False

    def test_none_is_false(self):
        assert validate_expires(None) is False
