"""Tests for per-user dispatch semaphore with LRU eviction."""

from __future__ import annotations

import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

from adapters.discord_bot import _get_user_sem, _user_sems, _USER_SEM_LIMIT, _USER_SEM_MAX_ENTRIES


class TestGetUserSem:
    def setup_method(self):
        _user_sems.clear()

    def test_creates_new_semaphore(self):
        sem = _get_user_sem("user1")
        assert sem is not None
        assert sem._value == _USER_SEM_LIMIT

    def test_returns_same_semaphore_on_second_call(self):
        sem1 = _get_user_sem("user1")
        sem2 = _get_user_sem("user1")
        assert sem1 is sem2

    def test_different_users_get_different_semaphores(self):
        sem1 = _get_user_sem("user1")
        sem2 = _get_user_sem("user2")
        assert sem1 is not sem2

    def test_lru_evicts_oldest_at_capacity(self):
        # Fill to capacity
        for i in range(_USER_SEM_MAX_ENTRIES):
            _get_user_sem(f"user{i}")
        assert len(_user_sems) == _USER_SEM_MAX_ENTRIES

        # Adding one more evicts the oldest (user0)
        _get_user_sem("new_user")
        assert "new_user" in _user_sems
        assert "user0" not in _user_sems
        assert len(_user_sems) == _USER_SEM_MAX_ENTRIES

    def test_lru_refreshes_on_access(self):
        _get_user_sem("old")
        _get_user_sem("middle")
        _get_user_sem("recent")

        # Access "old" to refresh it — it should move to end
        _get_user_sem("old")

        keys = list(_user_sems.keys())
        assert keys[-1] == "old"
        assert keys[0] == "middle"
