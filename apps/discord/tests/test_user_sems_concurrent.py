"""Tests for _user_sems LRU eviction under concurrent access."""

from __future__ import annotations

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest

from adapters.discord_bot import _get_user_sem, _user_sems, _USER_SEM_MAX_ENTRIES


class TestUserSemsLRUConcurrent:
    def setup_method(self):
        _user_sems.clear()

    def test_eviction_preserves_recent_users(self):
        """Fill to capacity, access an old user, then add one more — old user should survive."""
        for i in range(_USER_SEM_MAX_ENTRIES):
            _get_user_sem(f"user{i}")

        # Access user0 to refresh it (move to end)
        _get_user_sem("user0")

        # Add a new user — should evict user1 (oldest), not user0 (refreshed)
        _get_user_sem("new_user")

        assert "user0" in _user_sems, "Refreshed user should survive eviction"
        assert "user1" not in _user_sems, "Oldest non-refreshed user should be evicted"
        assert "new_user" in _user_sems

    @pytest.mark.asyncio
    async def test_semaphore_works_after_eviction_and_recreation(self):
        """After a user is evicted and recreated, the new semaphore should work."""
        _get_user_sem("evictme")
        # Fill to capacity
        for i in range(_USER_SEM_MAX_ENTRIES):
            _get_user_sem(f"filler{i}")

        # evictme should be gone
        assert "evictme" not in _user_sems

        # Recreate — should get a fresh semaphore
        sem = _get_user_sem("evictme")
        assert sem._value == 10

        # Verify it actually works
        await sem.acquire()
        assert sem._value == 9
        sem.release()
        assert sem._value == 10

    def test_concurrent_users_get_independent_semaphores(self):
        """Two users' semaphores should be independent."""
        sem_a = _get_user_sem("alice")
        sem_b = _get_user_sem("bob")

        assert sem_a is not sem_b
        assert sem_a._value == 10
        assert sem_b._value == 10
