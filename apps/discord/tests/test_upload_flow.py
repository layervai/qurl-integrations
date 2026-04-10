"""End-to-end test for DM upload flow in on_message."""

from __future__ import annotations

import os
from unittest.mock import AsyncMock, MagicMock, patch

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest


def _make_upload_message():
    msg = AsyncMock()
    msg.author = MagicMock()
    msg.author.bot = False
    msg.author.id = 111222333444555666
    msg.content = ""
    msg.reference = None
    msg.mentions = []
    msg.channel = MagicMock()
    msg.reply = AsyncMock()

    attachment = MagicMock()
    attachment.filename = "test.pdf"
    attachment.size = 1024
    attachment.content_type = "application/pdf"
    attachment.url = "https://cdn.discordapp.com/attachments/123/456/test.pdf"
    attachment.read = AsyncMock(return_value=b"fake pdf content")
    msg.attachments = [attachment]

    return msg


class TestUploadFlow:
    @pytest.mark.asyncio
    async def test_upload_success(self):
        msg = _make_upload_message()

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__), \
             patch("adapters.discord_bot.rate_limiter") as mock_rl, \
             patch("adapters.discord_bot.upload_file", new_callable=AsyncMock) as mock_upload, \
             patch("adapters.discord_bot.register_owner"), \
             patch("adapters.discord_bot.metrics"), \
             patch("adapters.discord_bot.logger"):
            mock_rl.check = MagicMock(return_value=True)
            mock_upload.return_value = {
                "resource_id": "r_upload12345",
                "qurl_link": "https://qurl.link.layerv.xyz/#at_test",
                "expires_at": "2026-12-31T00:00:00Z",
            }

            from adapters.discord_bot import on_message
            await on_message(msg)

        mock_upload.assert_called_once()
        msg.reply.assert_called_once()
        reply = msg.reply.call_args.args[0]
        assert "protected" in reply.lower()
        assert "qurl.link" in reply
