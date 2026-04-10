"""Tests for /qurl clear command and delete_all_resources DB function."""

import os
from unittest.mock import AsyncMock, MagicMock, patch

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest

from db import (
    init_db,
    register_owner,
    delete_all_resources,
    list_resources,
    log_dispatch,
    get_pending_dispatches,
    search_resources,
)
from adapters.discord_bot import qurl_clear


@pytest.fixture
def db_path(tmp_path):
    path = str(tmp_path / "test.db")
    init_db(path)
    return path


class TestDeleteAllResources:
    def test_deletes_all_user_resources(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "a.png")
        register_owner("r_bbbbbb123456", "user1", None, "b.png")
        register_owner("r_cccccc123456", "user1", None, "c.png")

        count = delete_all_resources("user1")
        assert count == 3
        assert list_resources("user1") == []

    def test_does_not_delete_other_users(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "a.png")
        register_owner("r_bbbbbb123456", "user2", None, "b.png")

        count = delete_all_resources("user1")
        assert count == 1
        assert list_resources("user1") == []
        assert len(list_resources("user2")) == 1

    def test_returns_zero_when_no_resources(self, db_path):
        count = delete_all_resources("user_with_nothing")
        assert count == 0

    def test_cascade_deletes_dispatch_log(self, db_path):
        """ON DELETE CASCADE should remove dispatch_log entries when resource is deleted."""
        register_owner("r_aaaaaa123456", "user1", None, "a.png")
        log_dispatch("r_aaaaaa123456", "user1", "recipient1", None)
        log_dispatch("r_aaaaaa123456", "user1", "recipient2", None)

        delete_all_resources("user1")

        # dispatch_log entries should be cascade-deleted
        pending = get_pending_dispatches()
        assert len([d for d in pending if d["resource_id"] == "r_aaaaaa123456"]) == 0

    def test_autocomplete_empty_after_clear(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "photo.png")
        register_owner("r_bbbbbb123456", "user1", None, "doc.pdf")

        delete_all_resources("user1")
        results = search_resources("user1", "", 25)
        assert results == []


class TestQURLClearCommand:
    @pytest.fixture(autouse=True)
    def mock_infra(self):
        with patch("adapters.discord_bot.metrics"), \
             patch("adapters.discord_bot.delete_all_resources") as mock_delete:
            self.mock_delete = mock_delete
            yield

    @pytest.mark.asyncio
    async def test_clear_with_resources(self):
        self.mock_delete.return_value = 5
        interaction = AsyncMock()
        interaction.user = MagicMock()
        interaction.user.id = 111222333444555666
        interaction.response = AsyncMock()
        interaction.followup = AsyncMock()

        await qurl_clear.callback(interaction)

        self.mock_delete.assert_called_once()
        interaction.followup.send.assert_called_once()
        msg = interaction.followup.send.call_args.args[0]
        assert "5" in msg
        assert "Cleared" in msg

    @pytest.mark.asyncio
    async def test_clear_with_no_resources(self):
        self.mock_delete.return_value = 0
        interaction = AsyncMock()
        interaction.user = MagicMock()
        interaction.user.id = 111222333444555666
        interaction.response = AsyncMock()
        interaction.followup = AsyncMock()

        await qurl_clear.callback(interaction)

        interaction.followup.send.assert_called_once()
        msg = interaction.followup.send.call_args.args[0]
        assert "no resources" in msg.lower()

    @pytest.mark.asyncio
    async def test_clear_is_ephemeral(self):
        self.mock_delete.return_value = 3
        interaction = AsyncMock()
        interaction.user = MagicMock()
        interaction.user.id = 111222333444555666
        interaction.response = AsyncMock()
        interaction.followup = AsyncMock()

        await qurl_clear.callback(interaction)

        interaction.response.defer.assert_called_once_with(ephemeral=True)
        assert interaction.followup.send.call_args.kwargs.get("ephemeral") is True
