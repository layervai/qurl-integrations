"""Pure helper functions for the QURL Discord Bot.

Extracted from adapters/discord_bot.py so tests can import directly
without triggering Bot instantiation.
"""

from __future__ import annotations

import asyncio
import re
from datetime import datetime, timezone

import discord

from db import DispatchStatus

# Deterministic marker for resource_id in bot DM messages.
# Reply-to-share parses this marker — NOT arbitrary backtick-wrapped strings.
# Format: \_resource\_id:r_xxx\_ on its own line (backslash-escaped to prevent Discord italics).
RESOURCE_ID_MARKER_RE = re.compile(r"^\\_resource\\_id:(r_[a-zA-Z0-9_-]+)\\_$", re.MULTILINE)


def is_expired(expires_at: str | None) -> bool:
    """Check if a resource has expired based on local expires_at timestamp.

    Returns False when parsing fails — lets the mint API decide.
    """
    if not expires_at:
        return False
    try:
        exp = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
        return exp < datetime.now(timezone.utc)
    except (ValueError, AttributeError):
        return False


def format_dispatch_summary(filename: str, results: list[tuple]) -> str:
    """Format dispatch results into a summary message."""
    lines = [f"**Links sent for {filename}:**"]
    for _uid, status, error, name in results:
        if status == DispatchStatus.SENT:
            lines.append(f"- {name} -- sent")
        else:
            lines.append(f"- {name} -- {error or status}")
    return "\n".join(lines)


# Global semaphore for guild member lookups — limits Discord API rate impact.
# Shared across all users intentionally: Discord's rate limit is per-bot, not per-user.
# 5 concurrent slots is conservative; increase if 3-second interaction timeouts occur.
_guild_check_sem = asyncio.Semaphore(5)


async def check_guild_members(
    guild: discord.Guild, user_ids: list[str]
) -> list[str]:
    """Verify which user_ids are members of a guild. Parallel fetch with rate-limit semaphore."""
    async def _check_one(uid: str) -> str | None:
        async with _guild_check_sem:
            try:
                await guild.fetch_member(int(uid))
                return uid
            except (discord.NotFound, discord.HTTPException):
                return None

    checks = await asyncio.gather(*[_check_one(uid) for uid in user_ids])
    return [uid for uid in checks if uid]
