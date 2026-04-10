"""End-to-end tests for _handle_reply_to_share flow."""

from __future__ import annotations

import os
from unittest.mock import AsyncMock, MagicMock, patch

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest


def _make_bot_upload_message(resource_id: str = "r_test12345abcde") -> MagicMock:
    """Create a mock of the bot's upload confirmation DM (parent message)."""
    parent = MagicMock()
    parent.author = MagicMock()
    parent.author.id = 999  # bot user id
    parent.content = (
        "**Your resource has been protected!**\n\n"
        "**test.jpg**\n"
        "https://qurl.link/at_xxx\n"
        "Expires <t:1234567890:R>\n\n"
        "Reply to this message and @mention users to share.\n"
        f"\\_resource\\_id:{resource_id}\\_"
    )
    return parent


def _make_reply_message(mentions=None, attachments=None, content=""):
    """Create a mock reply message with mentions."""
    msg = AsyncMock()
    msg.author = MagicMock()
    msg.author.bot = False
    msg.author.id = 111222333444555666
    msg.content = content
    msg.attachments = attachments or []
    msg.reference = MagicMock()
    msg.reference.message_id = 9999
    msg.mentions = mentions or []
    msg.channel = MagicMock()
    msg.channel.fetch_message = AsyncMock()
    msg.reply = AsyncMock()
    return msg


class TestReplyToShare:
    @pytest.mark.asyncio
    async def test_happy_path_dispatches_to_recipients(self):
        """Reply to bot upload DM with @mentions dispatches links."""
        recipient = MagicMock()
        recipient.id = 777888999000111222
        recipient.display_name = "Alice"

        msg = _make_reply_message(mentions=[recipient])
        parent = _make_bot_upload_message()
        msg.channel.fetch_message.return_value = parent

        with patch("adapters.discord_bot.bot") as mock_bot, \
             patch("adapters.discord_bot.rate_limiter") as mock_rl, \
             patch("adapters.discord_bot.get_owner") as mock_get_owner, \
             patch("adapters.discord_bot._dispatch_to_recipient", new_callable=AsyncMock) as mock_dispatch:

            mock_bot.user = MagicMock()
            mock_bot.user.id = 999
            mock_rl.check = MagicMock(return_value=True)
            mock_get_owner.return_value = {
                "discord_user_id": str(msg.author.id),
                "guild_id": None,
                "filename": "test.jpg",
                "expires_at": "2099-12-31T00:00:00Z",
            }
            mock_dispatch.return_value = (str(recipient.id), "sent", None, "Alice")

            from adapters.discord_bot import _handle_reply_to_share
            result = await _handle_reply_to_share(msg)

        assert result is True
        msg.channel.fetch_message.assert_called_once()
        mock_dispatch.assert_called_once()
        # Summary reply sent
        assert msg.reply.call_count >= 2  # "Sending links..." + summary

    @pytest.mark.asyncio
    async def test_reply_with_attachment_rejected(self):
        """Reply with both mentions and attachment should be rejected by on_message."""
        recipient = MagicMock()
        recipient.id = 777888999000111222

        msg = _make_reply_message(
            mentions=[recipient],
            attachments=[MagicMock()],  # has attachment
        )

        with patch("adapters.discord_bot.discord.DMChannel", msg.channel.__class__):
            from adapters.discord_bot import on_message
            await on_message(msg)

        msg.reply.assert_called_once()
        reply_text = msg.reply.call_args.args[0]
        assert "can't share and upload" in reply_text.lower()

    @pytest.mark.asyncio
    async def test_rate_limited_before_fetch(self):
        """Rate limit should fire before fetching the parent message."""
        msg = _make_reply_message(mentions=[MagicMock()])

        with patch("adapters.discord_bot.rate_limiter") as mock_rl:
            mock_rl.check = MagicMock(return_value=False)

            from adapters.discord_bot import _handle_reply_to_share
            result = await _handle_reply_to_share(msg)

        assert result is True
        # fetch_message should NOT have been called
        msg.channel.fetch_message.assert_not_called()
        assert "too many" in msg.reply.call_args.args[0].lower()

    @pytest.mark.asyncio
    async def test_non_bot_parent_falls_through(self):
        """Reply to a non-bot message should return False (fall through)."""
        msg = _make_reply_message(mentions=[MagicMock()])
        parent = MagicMock()
        parent.author.id = 555  # not the bot
        msg.channel.fetch_message.return_value = parent

        with patch("adapters.discord_bot.bot") as mock_bot, \
             patch("adapters.discord_bot.rate_limiter") as mock_rl:
            mock_bot.user = MagicMock()
            mock_bot.user.id = 999
            mock_rl.check = MagicMock(return_value=True)

            from adapters.discord_bot import _handle_reply_to_share
            result = await _handle_reply_to_share(msg)

        assert result is False
