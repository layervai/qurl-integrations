"""Tests for consolidated dispatch error handling."""

from __future__ import annotations

import os
from unittest.mock import AsyncMock, MagicMock, patch

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import discord
import httpx
import pytest

from adapters.discord_bot import _dispatch_to_recipient, qurl_send
from db import DispatchStatus
from validation import validate_expires


@pytest.fixture(autouse=True)
def mock_infra():
    """Mock infrastructure deps so _dispatch_to_recipient can run."""
    with patch("adapters.discord_bot.log_dispatch", return_value="d-1"), \
         patch("adapters.discord_bot.update_dispatch"), \
         patch("adapters.discord_bot.metrics"):
        yield


class TestDispatchErrors:
    @pytest.mark.asyncio
    async def test_not_found_returns_dm_failed(self):
        with patch("adapters.discord_bot.bot") as mock_bot:
            mock_bot.fetch_user = AsyncMock(
                side_effect=discord.NotFound(MagicMock(), "not found")
            )
            _, status, error, _ = await _dispatch_to_recipient(
                "r_test123456", "111222333", "444555666", None
            )
        assert status == DispatchStatus.DM_FAILED
        assert error == "User not found"

    @pytest.mark.asyncio
    async def test_forbidden_returns_dm_failed(self):
        recipient = AsyncMock()
        recipient.send = AsyncMock(
            side_effect=discord.Forbidden(MagicMock(), "dms disabled")
        )
        with patch("adapters.discord_bot.bot") as mock_bot, \
             patch("adapters.discord_bot.mint_link", new_callable=AsyncMock,
                    return_value={"qurl_link": "https://qurl.link/at_x", "expires_at": ""}):
            mock_bot.fetch_user = AsyncMock(return_value=recipient)
            _, status, error, _ = await _dispatch_to_recipient(
                "r_test123456", "111222333", "444555666", None
            )
        assert status == DispatchStatus.DM_FAILED
        assert error == "DMs disabled"

    @pytest.mark.asyncio
    async def test_timeout_returns_mint_failed(self):
        with patch("adapters.discord_bot.bot") as mock_bot, \
             patch("adapters.discord_bot.mint_link", new_callable=AsyncMock,
                    side_effect=httpx.TimeoutException("timeout")):
            mock_bot.fetch_user = AsyncMock(return_value=MagicMock())
            _, status, error, _ = await _dispatch_to_recipient(
                "r_test123456", "111222333", "444555666", None
            )
        assert status == DispatchStatus.MINT_FAILED
        assert error == "Failed to mint link"

    @pytest.mark.asyncio
    async def test_generic_exception_returns_mint_error(self):
        with patch("adapters.discord_bot.bot") as mock_bot, \
             patch("adapters.discord_bot.mint_link", new_callable=AsyncMock,
                    side_effect=RuntimeError("unexpected")):
            mock_bot.fetch_user = AsyncMock(return_value=MagicMock())
            _, status, error, _ = await _dispatch_to_recipient(
                "r_test123456", "111222333", "444555666", None
            )
        assert status == DispatchStatus.MINT_FAILED
        assert error == "Failed to mint link"


class TestExpiryRejectionPath:
    """Even with Discord choices, verify server-side validation rejects invalid values."""

    def test_invalid_expiry_blocked_by_validate_expires(self):
        """An invalid expiry value must not pass validate_expires (defense-in-depth)."""
        invalid_values = ["99m", "2h", "30d", "", "abc", None]
        for val in invalid_values:
            assert validate_expires(val) is False, f"Expected rejection for {val!r}"

    @pytest.mark.asyncio
    async def test_invalid_expiry_does_not_reach_dispatch(self):
        """Simulate a crafted API call with an invalid expiry bypassing Discord UI.

        The handler should reject before calling _dispatch_to_recipient.
        """
        interaction = AsyncMock()
        interaction.user = MagicMock()
        interaction.user.id = 111222333444555666
        interaction.guild_id = None
        interaction.guild = None
        interaction.response = AsyncMock()
        interaction.followup = AsyncMock()

        # Create a fake Choice object with an invalid value
        fake_choice = MagicMock()
        fake_choice.value = "99m"  # not in EXPIRY_CHOICES_VALUES

        with patch("adapters.discord_bot.rate_limiter") as mock_rl, \
             patch("adapters.discord_bot._dispatch_to_recipient", new_callable=AsyncMock) as mock_dispatch:
            mock_rl.check = MagicMock(return_value=True)
            await qurl_send.callback(interaction, "r_test123456", "<@444555666>", expires=fake_choice)

        # _dispatch_to_recipient must NOT have been called
        mock_dispatch.assert_not_called()
        # Should have sent an ephemeral rejection
        interaction.followup.send.assert_called_once()
        assert "invalid" in interaction.followup.send.call_args.args[0].lower()
