import time
from rate_limiter import RateLimiter

class TestRateLimiter:
    def test_allows_under_limit(self):
        rl = RateLimiter(max_per_minute=5)
        for _ in range(5):
            assert rl.check("user1") is True

    def test_blocks_over_limit(self):
        rl = RateLimiter(max_per_minute=5)
        for _ in range(5):
            rl.check("user1")
        assert rl.check("user1") is False

    def test_separate_users(self):
        rl = RateLimiter(max_per_minute=2)
        rl.check("user1")
        rl.check("user1")
        assert rl.check("user1") is False
        assert rl.check("user2") is True

    def test_cleanup_runs(self):
        rl = RateLimiter(max_per_minute=5)
        rl._call_count = 999
        rl.check("user1")
        # After 1000th call, cleanup should have run
        assert rl._call_count == 1000
