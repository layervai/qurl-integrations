import pytest
from db import init_db, register_owner, get_owner, list_resources, delete_resource, bind_guild, log_dispatch, update_dispatch, get_dispatch_stats

@pytest.fixture
def db_path(tmp_path):
    path = str(tmp_path / "test.db")
    init_db(path)
    return path

class TestOwnerRegistry:
    def test_register_and_get(self, db_path):
        register_owner("r_test123456", "user1", None, "test.png", "2026-12-31T00:00:00Z")
        owner = get_owner("r_test123456")
        assert owner is not None
        assert owner["discord_user_id"] == "user1"

    def test_conflict_does_not_replace(self, db_path):
        register_owner("r_test123456", "user1", None, "test.png")
        register_owner("r_test123456", "user2", None, "other.png")  # should be ignored
        owner = get_owner("r_test123456")
        assert owner["discord_user_id"] == "user1"  # not user2

    def test_list_resources(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "a.png")
        register_owner("r_bbbbbb123456", "user1", None, "b.png")
        resources = list_resources("user1")
        assert len(resources) == 2

    def test_delete(self, db_path):
        register_owner("r_test123456", "user1", None, "test.png")
        delete_resource("r_test123456")
        assert get_owner("r_test123456") is None

    def test_bind_guild(self, db_path):
        register_owner("r_test123456", "user1", None, "test.png")
        result = bind_guild("r_test123456", "guild1")
        assert result is True
        owner = get_owner("r_test123456")
        assert owner["guild_id"] == "guild1"

    def test_bind_guild_no_overwrite(self, db_path):
        register_owner("r_test123456", "user1", "guild1", "test.png")
        result = bind_guild("r_test123456", "guild2")  # should not overwrite
        assert result is False
        owner = get_owner("r_test123456")
        assert owner["guild_id"] == "guild1"

class TestDispatchLog:
    def test_log_and_update(self, db_path):
        register_owner("r_test123456", "user1", None, "test.png")
        dispatch_id = log_dispatch("r_test123456", "sender1", "recipient1", "guild1")
        assert isinstance(dispatch_id, str)
        assert len(dispatch_id) == 36  # UUID format
        update_dispatch(dispatch_id, "sent")
        stats = get_dispatch_stats("r_test123456")
        assert stats["sent"] >= 1
