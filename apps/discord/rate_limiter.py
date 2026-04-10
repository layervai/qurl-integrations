"""Sliding window rate limiter."""

import time
from collections import defaultdict, deque


class RateLimiter:
    """Per-user sliding window rate limiter."""

    def __init__(self, max_per_minute: int = 5):
        self.max_per_minute = max_per_minute
        self._windows: dict[str, deque[float]] = defaultdict(deque)
        self._call_count = 0

    def check(self, user_id: str) -> bool:
        """
        Check if a user is within the rate limit.

        Returns True if the request is allowed, False if rate-limited.
        Records the request timestamp if allowed.
        """
        now = time.monotonic()
        window = self._windows[user_id]

        # Periodic cleanup every 1000 calls
        self._call_count += 1
        if self._call_count % 1000 == 0:
            self._cleanup(now)

        # Remove timestamps older than 60 seconds
        cutoff = now - 60.0
        while window and window[0] < cutoff:
            window.popleft()

        if len(window) >= self.max_per_minute:
            return False

        window.append(now)
        return True

    def _cleanup(self, now: float) -> None:
        """Remove stale entries from the window map."""
        stale = [
            uid
            for uid, window in self._windows.items()
            if not window or window[-1] < now - 60
        ]
        for uid in stale:
            del self._windows[uid]
