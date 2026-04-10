"""Integration tests for the on_message Google Maps flow in discord_bot.py."""

import asyncio
from unittest.mock import AsyncMock, MagicMock, patch

import pytest


def _make_message(content: str = "", attachments=None):
    """Create a mock Discord DM message."""
    msg = AsyncMock()
    msg.author = MagicMock()
    msg.author.bot = False
    msg.author.id = 123456789012345678
    msg.content = content
    msg.attachments = attachments or []
    msg.channel = MagicMock()
    msg.channel.__class__.__name__ = "DMChannel"
    # Prevent reply-to-share handler from triggering
    msg.reference = None
    msg.mentions = []
    msg.reply = AsyncMock()
    return msg


@pytest.fixture
def mock_settings():
    with patch("adapters.discord_bot.settings") as mock:
        mock.google_maps_enabled = True
        mock.max_file_size_mb = 25
        mock.cdn_url_allowlist = "cdn.discordapp.com"
        mock.rate_limit_per_minute = 5
        mock.link_expires_in = "15m"
        yield mock


@pytest.fixture
def mock_upload():
    with patch("adapters.discord_bot.upload_file", new_callable=AsyncMock) as mock:
        mock.return_value = {
            "resource_id": "r_map_test12345",
            "qurl_link": "https://qurl.link/at_map_abc",
            "expires_at": "2026-12-31T00:00:00Z",
        }
        yield mock


@pytest.fixture
def mock_register():
    with patch("adapters.discord_bot.register_owner") as mock:
        yield mock


@pytest.fixture
def mock_rate_limiter():
    with patch("adapters.discord_bot.rate_limiter") as mock:
        mock.check = MagicMock(return_value=True)
        yield mock


class TestMapsIntegration:
    @pytest.mark.asyncio
    async def test_maps_url_detected_and_uploaded(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """A DM with a Google Maps URL should upload map metadata and reply with resource info."""
        msg = _make_message("https://www.google.com/maps/place/Seattle,WA check this out")

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_called_once()
        call_kw = mock_upload.call_args.kwargs if mock_upload.call_args.kwargs else {}
        assert b"google-map" in call_kw.get("file_bytes", b"")

        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "single-use link" in reply_text.lower()
        assert "qurl.link" in reply_text

    @pytest.mark.asyncio
    async def test_maps_url_captures_annotation(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """Surrounding text (annotation) should be captured in the upload payload."""
        msg = _make_message("https://www.google.com/maps/place/Seattle,WA this is where the treasure is buried")

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_called_once()
        import json
        call_kw = mock_upload.call_args.kwargs if mock_upload.call_args.kwargs else {}
        payload = json.loads(call_kw.get("file_bytes", b"{}"))
        assert payload.get("caption") == "this is where the treasure is buried"

    @pytest.mark.asyncio
    async def test_maps_url_upload_failure(
        self, mock_settings, mock_register, mock_rate_limiter
    ):
        """Upload failure should show a generic error, not internal details."""
        msg = _make_message("https://www.google.com/maps/place/Seattle,WA")

        with patch("adapters.discord_bot.upload_file", new_callable=AsyncMock, side_effect=Exception("API down")), \
             patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "something went wrong" in reply_text.lower()
        assert "API down" not in reply_text

    @pytest.mark.asyncio
    async def test_maps_disabled_shows_help(
        self, mock_upload, mock_register, mock_rate_limiter
    ):
        """When GOOGLE_MAPS_ENABLED=false, Maps URLs should be ignored and help shown."""
        msg = _make_message("https://www.google.com/maps/place/Seattle,WA")

        with patch("adapters.discord_bot.settings") as mock_settings, \
             patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            mock_settings.google_maps_enabled = False
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "file" in reply_text.lower()

    @pytest.mark.asyncio
    async def test_rate_limited_maps_request(
        self, mock_settings, mock_upload, mock_register
    ):
        """Rate-limited users should get a rate limit message, not an upload."""
        msg = _make_message("https://www.google.com/maps/place/Seattle,WA")

        with patch("adapters.discord_bot.rate_limiter") as mock_rl, \
             patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            mock_rl.check = MagicMock(return_value=False)
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        assert "too many" in msg.reply.call_args.args[0].lower()

    @pytest.mark.asyncio
    async def test_unsupported_format_shows_correct_error(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """A detected but unsupported Maps URL (contrib) should show the unsupported format error."""
        msg = _make_message("https://www.google.com/maps/contrib/12345")

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "isn't supported" in reply_text

    @pytest.mark.asyncio
    async def test_genuine_parse_failure_shows_error(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """A URL that passes detection but fails parsing should show parse error."""
        msg = _make_message("https://www.google.com/maps/data=!1m1!2m2")

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "could not parse" in reply_text.lower()

    @pytest.mark.asyncio
    async def test_no_url_no_attachment_shows_help(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """A plain text DM with no URL and no file should show help."""
        msg = _make_message("hello bot")

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "file" in reply_text.lower()

    @pytest.mark.asyncio
    async def test_short_link_resolved_and_uploaded(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """A goo.gl short link should be resolved then processed like a regular Maps URL."""
        msg = _make_message("https://goo.gl/maps/abc123xyz")

        with patch("adapters.discord_bot.resolve_short_link", new_callable=AsyncMock,
                    return_value="https://www.google.com/maps/place/Portland,OR") as mock_resolve, \
             patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_resolve.assert_called_once()
        mock_upload.assert_called_once()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "single-use link" in reply_text.lower()

    @pytest.mark.asyncio
    async def test_short_link_resolution_failure(
        self, mock_settings, mock_upload, mock_register, mock_rate_limiter
    ):
        """Failed short-link resolution should show a helpful error."""
        msg = _make_message("https://maps.app.goo.gl/broken123")

        with patch("adapters.discord_bot.resolve_short_link", new_callable=AsyncMock,
                    return_value=None), \
             patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_not_called()
        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "could not resolve" in reply_text.lower()
