"""Tests for /qurl status and /qurl revoke slash commands."""

from __future__ import annotations

import os
from unittest.mock import AsyncMock, MagicMock, patch

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest

from adapters.discord_bot import qurl_status, qurl_revoke


def _mock_interaction(user_id: int = 111222333):
    interaction = AsyncMock()
    interaction.user = MagicMock()
    interaction.user.id = user_id
    interaction.response = AsyncMock()
    interaction.followup = AsyncMock()
    interaction.guild_id = None
    return interaction


class TestQURLStatus:
    @pytest.mark.asyncio
    async def test_status_shows_resource_info(self):
        interaction = _mock_interaction()

        with patch("adapters.discord_bot._resolve_owned_resource", new_callable=AsyncMock) as mock_resolve, \
             patch("adapters.discord_bot.get_dispatch_stats", return_value={"sent": 3, "failed": 1}), \
             patch("adapters.discord_bot.get_http_client") as mock_http:
            mock_resolve.return_value = ("r_test123456", {
                "filename": "test.pdf",
                "created_at": "2026-04-01",
                "expires_at": "2026-04-10T00:00:00Z",
            })
            mock_resp = MagicMock()
            mock_resp.status_code = 200
            mock_resp.json.return_value = {"data": {"status": "active"}}
            mock_http.return_value.get = AsyncMock(return_value=mock_resp)

            await qurl_status.callback(interaction, "r_test123456")

        interaction.followup.send.assert_called_once()
        msg = interaction.followup.send.call_args.args[0]
        assert "r_test123456" in msg
        assert "3" in msg  # sent count

    @pytest.mark.asyncio
    async def test_status_not_found(self):
        interaction = _mock_interaction()

        with patch("adapters.discord_bot._resolve_owned_resource", new_callable=AsyncMock, return_value=None):
            await qurl_status.callback(interaction, "r_nonexistent")

        interaction.followup.send.assert_not_called()


class TestQURLRevoke:
    @pytest.mark.asyncio
    async def test_revoke_deletes_resource(self):
        interaction = _mock_interaction()

        with patch("adapters.discord_bot._resolve_owned_resource", new_callable=AsyncMock) as mock_resolve, \
             patch("adapters.discord_bot.delete_resource") as mock_delete, \
             patch("adapters.discord_bot.get_http_client") as mock_http, \
             patch("adapters.discord_bot.metrics"), \
             patch("adapters.discord_bot.logger"):
            mock_resolve.return_value = ("r_test123456", {"filename": "test.pdf"})
            mock_resp = MagicMock()
            mock_resp.status_code = 200
            mock_http.return_value.delete = AsyncMock(return_value=mock_resp)

            await qurl_revoke.callback(interaction, "r_test123456")

        mock_delete.assert_called_once()
        interaction.followup.send.assert_called_once()
        msg = interaction.followup.send.call_args.args[0]
        assert "revoked" in msg.lower()

    @pytest.mark.asyncio
    async def test_revoke_not_found(self):
        interaction = _mock_interaction()

        with patch("adapters.discord_bot._resolve_owned_resource", new_callable=AsyncMock, return_value=None):
            await qurl_revoke.callback(interaction, "r_nonexistent")

        interaction.followup.send.assert_not_called()
