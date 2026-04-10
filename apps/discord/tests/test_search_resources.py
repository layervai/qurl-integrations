"""Tests for search_resources DB function."""

import pytest
from db import init_db, register_owner, search_resources


@pytest.fixture
def db_path(tmp_path):
    path = str(tmp_path / "test.db")
    init_db(path)
    return path


class TestSearchResources:
    def test_empty_query_returns_recent(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "alpha.png")
        register_owner("r_bbbbbb123456", "user1", None, "bravo.pdf")
        results = search_resources("user1", "", 25)
        assert len(results) == 2

    def test_query_matches_filename(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "el-charro-beer.jpg")
        register_owner("r_bbbbbb123456", "user1", None, "client-proposal.pdf")
        results = search_resources("user1", "charro", 25)
        assert len(results) == 1
        assert results[0]["filename"] == "el-charro-beer.jpg"

    def test_query_matches_resource_id(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "file.png")
        results = search_resources("user1", "aaaaaa", 25)
        assert len(results) == 1
        assert results[0]["resource_id"] == "r_aaaaaa123456"

    def test_query_case_insensitive(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "MyDocument.PDF")
        results = search_resources("user1", "mydoc", 25)
        assert len(results) == 1

    def test_no_matches(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "file.png")
        results = search_resources("user1", "nonexistent", 25)
        assert len(results) == 0

    def test_limit_respected(self, db_path):
        for i in range(10):
            register_owner(f"r_test{i:06d}abcd", "user1", None, f"file{i}.png")
        results = search_resources("user1", "", 3)
        assert len(results) == 3

    def test_other_users_resources_not_returned(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "secret.pdf")
        register_owner("r_bbbbbb123456", "user2", None, "other.pdf")
        results = search_resources("user1", "", 25)
        assert len(results) == 1
        assert results[0]["discord_user_id"] == "user1"

    def test_sql_wildcard_in_query(self, db_path):
        """SQL wildcards % and _ in query should be treated as literals."""
        register_owner("r_aaaaaa123456", "user1", None, "100%_complete.pdf")
        register_owner("r_bbbbbb123456", "user1", None, "normal_file.png")
        # Searching for "%" should only match the file with % in the name
        results = search_resources("user1", "%", 25)
        assert len(results) == 1
        assert results[0]["filename"] == "100%_complete.pdf"

    def test_underscore_wildcard_escaped(self, db_path):
        """SQL _ wildcard in filename search should be treated as literal."""
        register_owner("r_aaaaaa123456", "user1", None, "my_file.pdf")
        register_owner("r_bbbbbb123456", "user1", None, "myXfile.pdf")
        # Search for "my_f" should match only the file with underscore, not "myXfile"
        results = search_resources("user1", "my_f", 25)
        assert len(results) == 1
        assert results[0]["filename"] == "my_file.pdf"

    def test_empty_user_returns_nothing(self, db_path):
        register_owner("r_aaaaaa123456", "user1", None, "file.png")
        results = search_resources("nonexistent_user", "", 25)
        assert len(results) == 0
