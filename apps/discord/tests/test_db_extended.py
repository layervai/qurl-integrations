"""Extended tests for db.py — covers bind_guild, get_pending_dispatches,
get_dispatch_stats, DispatchStatus constants, init_db edge cases,
expires_at round-trip, full round-trip, and recover_stale_dispatches."""

import os
import stat

import pytest

from db import (
    DispatchStatus,
    bind_guild,
    get_dispatch_stats,
    get_owner,
    get_pending_dispatches,
    init_db,
    log_dispatch,
    recover_stale_dispatches,
    register_owner,
    update_dispatch,
)


@pytest.fixture
def db_path(tmp_path):
    path = str(tmp_path / "test_ext.db")
    init_db(path)
    return path


# --- DispatchStatus constants ---


class TestDispatchStatusConstants:
    def test_pending(self):
        assert DispatchStatus.PENDING == "pending"

    def test_sent(self):
        assert DispatchStatus.SENT == "sent"

    def test_dm_failed(self):
        assert DispatchStatus.DM_FAILED == "dm_failed"

    def test_mint_failed(self):
        assert DispatchStatus.MINT_FAILED == "mint_failed"


# --- bind_guild ---


class TestBindGuild:
    def test_bind_guild_when_null(self, db_path):
        register_owner("r_bind_null_01", "user1", None, "file.png")
        result = bind_guild("r_bind_null_01", "guild_abc")
        assert result is True
        owner = get_owner("r_bind_null_01")
        assert owner["guild_id"] == "guild_abc"

    def test_bind_guild_does_not_overwrite(self, db_path):
        register_owner("r_bind_exist1", "user1", "guild_original", "file.png")
        result = bind_guild("r_bind_exist1", "guild_attacker")
        assert result is False
        owner = get_owner("r_bind_exist1")
        assert owner["guild_id"] == "guild_original"

    def test_bind_guild_nonexistent_resource(self, db_path):
        # Should not raise — just a no-op UPDATE, returns False
        result = bind_guild("r_does_not_exist", "guild_x")
        assert result is False


# --- get_pending_dispatches ---


class TestGetPendingDispatches:
    def test_returns_only_pending(self, db_path):
        register_owner("r_pend_test01", "user1", None, "file.png")
        d1 = log_dispatch("r_pend_test01", "sender", "recip1")
        d2 = log_dispatch("r_pend_test01", "sender", "recip2")
        d3 = log_dispatch("r_pend_test01", "sender", "recip3")

        # Mark d1 as sent, d3 as failed — only d2 stays pending
        update_dispatch(d1, DispatchStatus.SENT)
        update_dispatch(d3, DispatchStatus.MINT_FAILED, "error")

        pending = get_pending_dispatches()
        assert len(pending) == 1
        assert pending[0]["dispatch_id"] == d2

    def test_empty_when_none_pending(self, db_path):
        assert get_pending_dispatches() == []


# --- get_dispatch_stats ---


class TestGetDispatchStats:
    def test_counts_correctly(self, db_path):
        register_owner("r_stats_test1", "user1", None, "file.png")
        d1 = log_dispatch("r_stats_test1", "s", "r1")
        d2 = log_dispatch("r_stats_test1", "s", "r2")
        d3 = log_dispatch("r_stats_test1", "s", "r3")
        log_dispatch("r_stats_test1", "s", "r4")

        update_dispatch(d1, DispatchStatus.SENT)
        update_dispatch(d2, DispatchStatus.SENT)
        update_dispatch(d3, DispatchStatus.DM_FAILED, "blocked")
        # d4 stays pending

        stats = get_dispatch_stats("r_stats_test1")
        assert stats["sent"] == 2
        assert stats["pending"] == 1  # d4 still pending
        assert stats["failed"] == 1   # d3 dm_failed

    def test_no_dispatches_returns_zeros(self, db_path):
        register_owner("r_stats_empty1", "user1", None, "file.png")
        stats = get_dispatch_stats("r_stats_empty1")
        assert stats["sent"] == 0
        assert stats["failed"] == 0


# --- init_db edge cases ---


class TestInitDbEdgeCases:
    def test_reinit_with_different_path(self, tmp_path):
        """Calling init_db twice with different paths should use the new database."""
        path1 = str(tmp_path / "db1.db")
        path2 = str(tmp_path / "db2.db")
        init_db(path1)
        register_owner("r_init_close1", "user1", None, "file.png")
        assert get_owner("r_init_close1") is not None

        # Re-init with a different path
        init_db(path2)
        # Old data is gone (new database)
        assert get_owner("r_init_close1") is None

    def test_sets_file_permissions(self, tmp_path):
        path = str(tmp_path / "perms.db")
        init_db(path)
        mode = os.stat(path).st_mode
        # Should be 0600 (owner read/write only)
        assert mode & (stat.S_IRUSR | stat.S_IWUSR) == (stat.S_IRUSR | stat.S_IWUSR)
        assert not (mode & stat.S_IRGRP)
        assert not (mode & stat.S_IROTH)


# --- expires_at round-trip ---


class TestRegisterOwnerExpiresAt:
    def test_stores_and_retrieves_expires_at(self, db_path):
        register_owner(
            "r_expire_test1", "user1", None, "file.png", "2026-12-31T00:00:00Z"
        )
        owner = get_owner("r_expire_test1")
        assert owner["expires_at"] == "2026-12-31T00:00:00Z"

    def test_none_expires_at(self, db_path):
        register_owner("r_expire_none1", "user1", None, "file.png", None)
        owner = get_owner("r_expire_none1")
        assert owner["expires_at"] is None


# --- B5: Full register → dispatch → stats round-trip ---


class TestFullRegisterDispatchStatsRoundtrip:
    def test_full_register_dispatch_stats_roundtrip(self, db_path):
        """register_owner -> log_dispatch -> update_dispatch -> get_dispatch_stats"""
        register_owner("r_roundtrip01", "user1", "guild1", "test.pdf", "2026-06-01T00:00:00Z")

        owner = get_owner("r_roundtrip01")
        assert owner is not None
        assert owner["discord_user_id"] == "user1"
        assert owner["guild_id"] == "guild1"

        d1 = log_dispatch("r_roundtrip01", "user1", "recip1", "guild1")
        d2 = log_dispatch("r_roundtrip01", "user1", "recip2", "guild1")
        d3 = log_dispatch("r_roundtrip01", "user1", "recip3", "guild1")

        # UUID format
        assert isinstance(d1, str) and len(d1) == 36
        assert isinstance(d2, str) and len(d2) == 36
        assert d1 != d2 != d3

        update_dispatch(d1, DispatchStatus.SENT)
        update_dispatch(d2, DispatchStatus.DM_FAILED, "DMs disabled")
        # d3 stays pending

        stats = get_dispatch_stats("r_roundtrip01")
        assert stats["sent"] == 1
        assert stats["pending"] == 1  # d3 still pending
        assert stats["failed"] == 1   # d2 dm_failed


# --- B5 / M9: recover_stale_dispatches ---


class TestRecoverStaleDispatches:
    def test_marks_pending_as_failed_recovered(self, db_path):
        register_owner("r_recover_001", "user1", None, "file.png")
        d1 = log_dispatch("r_recover_001", "user1", "recip1")
        log_dispatch("r_recover_001", "user1", "recip2")
        log_dispatch("r_recover_001", "user1", "recip3")

        # Mark d1 as sent — should not be affected
        update_dispatch(d1, DispatchStatus.SENT)

        # d2 and d3 are still pending
        recovered = recover_stale_dispatches()
        assert recovered == 2

        # No more pending
        pending = get_pending_dispatches()
        assert len(pending) == 0

    def test_returns_zero_when_nothing_pending(self, db_path):
        assert recover_stale_dispatches() == 0
