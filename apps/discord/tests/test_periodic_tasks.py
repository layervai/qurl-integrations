"""Tests for periodic retention and metrics flush tasks."""

from __future__ import annotations

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest

from db import init_db, register_owner, log_dispatch, prune_old_dispatches


class TestPruneOldDispatches:
    @pytest.fixture
    def db_path(self, tmp_path):
        path = str(tmp_path / "test.db")
        init_db(path)
        return path

    def test_prune_returns_zero_when_empty(self, db_path):
        count = prune_old_dispatches()
        assert count == 0

    def test_prune_does_not_delete_recent(self, db_path):
        register_owner("r_recent12345", "user1", None, "recent.pdf")
        log_dispatch("r_recent12345", "user1", "recipient1", None)

        count = prune_old_dispatches()
        assert count == 0  # recent entries should not be pruned


class TestMetricsFlush:
    @pytest.mark.asyncio
    async def test_flush_lock_prevents_overlap(self):
        import metrics
        lock = metrics._get_flush_lock()
        # Lock should not be locked initially
        assert not lock.locked()

        # Acquire lock to simulate in-progress flush
        await lock.acquire()
        assert lock.locked()

        # Release
        lock.release()
        assert not lock.locked()
