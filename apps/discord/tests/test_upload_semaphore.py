"""Tests for upload concurrency semaphore — exercises real async behavior."""

from __future__ import annotations

import asyncio
import os

os.environ.setdefault("DISCORD_BOT_TOKEN", "test-token")
os.environ.setdefault("DISCORD_CLIENT_ID", "123456")
os.environ.setdefault("QURL_API_KEY", "lv_test_fake")

import pytest


class TestUploadSemaphore:
    def test_initial_limit_is_5(self):
        """Module-level semaphore allows at most 5 concurrent uploads."""
        from adapters.discord_bot import _upload_sem
        assert _upload_sem._value == 5

    def test_semaphore_is_module_level(self):
        """Semaphore is shared across all upload calls (not per-user)."""
        from adapters.discord_bot import _upload_sem as sem1
        from adapters.discord_bot import _upload_sem as sem2
        assert sem1 is sem2

    @pytest.mark.asyncio
    async def test_acquire_and_release(self):
        """Real acquire reduces available slots; release restores them."""
        sem = asyncio.Semaphore(5)
        assert sem._value == 5
        await sem.acquire()
        assert sem._value == 4
        await sem.acquire()
        assert sem._value == 3
        sem.release()
        assert sem._value == 4
        sem.release()
        assert sem._value == 5

    @pytest.mark.asyncio
    async def test_sixth_acquire_blocks(self):
        """With all 5 permits acquired, the 6th acquire blocks until one is released."""
        sem = asyncio.Semaphore(5)

        for _ in range(5):
            await sem.acquire()
        assert sem._value == 0

        sixth_acquired = asyncio.Event()

        async def try_sixth():
            await sem.acquire()
            sixth_acquired.set()

        task = asyncio.create_task(try_sixth())
        await asyncio.sleep(0.05)
        assert not sixth_acquired.is_set(), "6th acquire should block when all permits taken"

        sem.release()
        await asyncio.sleep(0.05)
        assert sixth_acquired.is_set(), "6th acquire should succeed after release"

        # Cleanup
        task.cancel()
        for _ in range(4):
            sem.release()

    @pytest.mark.asyncio
    async def test_concurrent_tasks_respect_limit(self):
        """At most 5 tasks run concurrently inside the semaphore."""
        sem = asyncio.Semaphore(5)
        active = 0
        max_active = 0

        async def worker():
            nonlocal active, max_active
            async with sem:
                active += 1
                max_active = max(max_active, active)
                await asyncio.sleep(0.02)
                active -= 1

        tasks = [asyncio.create_task(worker()) for _ in range(10)]
        await asyncio.gather(*tasks)

        assert max_active == 5, f"Expected max 5 concurrent, got {max_active}"
