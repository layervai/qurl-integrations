"""Tests for _check_guild_members helper."""

from __future__ import annotations

import os
from unittest.mock import AsyncMock, MagicMock

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import discord
import pytest

from bot_helpers import check_guild_members


@pytest.fixture
def mock_guild():
    guild = MagicMock(spec=discord.Guild)
    return guild


class TestCheckGuildMembers:
    @pytest.mark.asyncio
    async def test_all_members_found(self, mock_guild):
        mock_guild.fetch_member = AsyncMock(return_value=MagicMock())
        result = await check_guild_members(mock_guild, ["111", "222", "333"])
        assert result == ["111", "222", "333"]
        assert mock_guild.fetch_member.call_count == 3

    @pytest.mark.asyncio
    async def test_some_members_not_found(self, mock_guild):
        async def fake_fetch(uid):
            if uid == 222:
                raise discord.NotFound(MagicMock(), "not found")
            return MagicMock()

        mock_guild.fetch_member = fake_fetch
        result = await check_guild_members(mock_guild, ["111", "222", "333"])
        assert result == ["111", "333"]

    @pytest.mark.asyncio
    async def test_all_members_not_found(self, mock_guild):
        mock_guild.fetch_member = AsyncMock(
            side_effect=discord.NotFound(MagicMock(), "not found")
        )
        result = await check_guild_members(mock_guild, ["111", "222"])
        assert result == []

    @pytest.mark.asyncio
    async def test_empty_user_list(self, mock_guild):
        mock_guild.fetch_member = AsyncMock()
        result = await check_guild_members(mock_guild, [])
        assert result == []
        mock_guild.fetch_member.assert_not_called()

    @pytest.mark.asyncio
    async def test_http_exception_treated_as_not_found(self, mock_guild):
        mock_guild.fetch_member = AsyncMock(
            side_effect=discord.HTTPException(MagicMock(), "server error")
        )
        result = await check_guild_members(mock_guild, ["111"])
        assert result == []
