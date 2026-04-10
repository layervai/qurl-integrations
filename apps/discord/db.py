"""SQLite persistent storage for owner registry and dispatch log."""

from __future__ import annotations

import contextlib
import logging
import os
import sqlite3
import stat
import uuid
from datetime import datetime, timezone
from typing import Any

logger = logging.getLogger(__name__)


class DispatchStatus:
    PENDING = "pending"
    SENT = "sent"
    DM_FAILED = "dm_failed"
    MINT_FAILED = "mint_failed"


_db_path: str = "data/qurl_bot.db"


@contextlib.contextmanager
def _db_conn():
    """Open-and-close a SQLite connection per call."""
    dir_name = os.path.dirname(_db_path)
    if dir_name:
        os.makedirs(dir_name, exist_ok=True)
    conn = sqlite3.connect(_db_path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    try:
        yield conn
    finally:
        conn.close()


def init_db(db_path: str = "data/qurl_bot.db") -> None:
    """Initialize database tables."""
    global _db_path
    _db_path = db_path
    with _db_conn() as conn:
        conn.executescript(
            """
            -- NOTE: rebuilt from scratch on container restart / EFS loss.
            -- Autocomplete UX silently degrades until users re-upload.
            -- See #31 (item 20) for the planned hydration from qurl-service.
            CREATE TABLE IF NOT EXISTS owner_registry (
                resource_id   TEXT PRIMARY KEY,
                discord_user_id TEXT NOT NULL,
                guild_id      TEXT,
                filename      TEXT,
                expires_at    TEXT,
                created_at    TEXT NOT NULL DEFAULT (datetime('now'))
            );

            CREATE TABLE IF NOT EXISTS dispatch_log (
                id            INTEGER PRIMARY KEY AUTOINCREMENT,
                resource_id   TEXT NOT NULL,
                sender_id     TEXT NOT NULL,
                recipient_id  TEXT NOT NULL,
                guild_id      TEXT,
                dispatch_id   TEXT,
                link_id_hash  TEXT,
                status        TEXT NOT NULL DEFAULT 'pending',
                error         TEXT,
                created_at    TEXT NOT NULL DEFAULT (datetime('now')),
                completed_at  TEXT,
                FOREIGN KEY (resource_id) REFERENCES owner_registry(resource_id)
                    ON DELETE CASCADE
            );

            CREATE INDEX IF NOT EXISTS idx_dispatch_resource
                ON dispatch_log(resource_id, created_at);
            CREATE INDEX IF NOT EXISTS idx_dispatch_pending
                ON dispatch_log(status) WHERE status = 'pending';
            CREATE INDEX IF NOT EXISTS idx_dispatch_id
                ON dispatch_log(dispatch_id);
            CREATE INDEX IF NOT EXISTS idx_owners_user
                ON owner_registry(discord_user_id);
            """
        )
        conn.commit()

    # Set DB file permissions to 0600 (owner read/write only)
    os.chmod(db_path, stat.S_IRUSR | stat.S_IWUSR)


def register_owner(
    resource_id: str,
    discord_user_id: str,
    guild_id: str | None = None,
    filename: str | None = None,
    expires_at: str | None = None,
) -> None:
    """Register a resource owner. Does nothing if resource_id already exists."""
    with _db_conn() as conn:
        cursor = conn.execute(
            """
            INSERT INTO owner_registry
                (resource_id, discord_user_id, guild_id, filename, expires_at)
            VALUES (?, ?, ?, ?, ?)
            ON CONFLICT(resource_id) DO NOTHING
            """,
            (resource_id, discord_user_id, guild_id, filename, expires_at),
        )
        conn.commit()
        if cursor.rowcount == 0:
            logger.warning("Resource %s already registered — skipping", resource_id)


def get_owner(resource_id: str) -> dict[str, Any] | None:
    """Get owner info for a resource."""
    with _db_conn() as conn:
        row = conn.execute(
            "SELECT * FROM owner_registry WHERE resource_id = ?", (resource_id,)
        ).fetchone()
        if row is None:
            return None
        return dict(row)


def list_resources(discord_user_id: str) -> list[dict[str, Any]]:
    """List all resources owned by a user."""
    with _db_conn() as conn:
        rows = conn.execute(
            "SELECT * FROM owner_registry WHERE discord_user_id = ? ORDER BY created_at DESC",
            (discord_user_id,),
        ).fetchall()
        return [dict(r) for r in rows]


def search_resources(
    discord_user_id: str, query: str = "", limit: int = 25
) -> list[dict[str, Any]]:
    """Search resources by filename or resource_id. Optimized for autocomplete."""
    with _db_conn() as conn:
        if query:
            # Escape SQL LIKE wildcards so % and _ are treated as literals
            escaped = query.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
            pattern = f"%{escaped}%"
            rows = conn.execute(
                """SELECT * FROM owner_registry
                   WHERE discord_user_id = ?
                     AND (filename LIKE ? ESCAPE '\\' OR resource_id LIKE ? ESCAPE '\\')
                   ORDER BY created_at DESC LIMIT ?""",
                (discord_user_id, pattern, pattern, limit),
            ).fetchall()
        else:
            rows = conn.execute(
                """SELECT * FROM owner_registry
                   WHERE discord_user_id = ?
                   ORDER BY created_at DESC LIMIT ?""",
                (discord_user_id, limit),
            ).fetchall()
        return [dict(r) for r in rows]


def delete_resource(resource_id: str) -> None:
    """Delete a resource and its dispatch log entries."""
    with _db_conn() as conn:
        conn.execute("DELETE FROM owner_registry WHERE resource_id = ?", (resource_id,))
        conn.commit()


def delete_all_resources(discord_user_id: str) -> int:
    """Delete all resources owned by a user. Returns the count of deleted resources."""
    with _db_conn() as conn:
        cursor = conn.execute(
            "DELETE FROM owner_registry WHERE discord_user_id = ?",
            (discord_user_id,),
        )
        conn.commit()
        return cursor.rowcount


def bind_guild(resource_id: str, guild_id: str) -> bool:
    """Bind resource to guild. Returns True if bound, False if already bound to another guild."""
    with _db_conn() as conn:
        cursor = conn.execute(
            "UPDATE owner_registry SET guild_id = ? WHERE resource_id = ? AND guild_id IS NULL",
            (guild_id, resource_id),
        )
        conn.commit()
        return cursor.rowcount > 0


def log_dispatch(
    resource_id: str,
    sender_id: str,
    recipient_id: str,
    guild_id: str | None = None,
) -> str:
    """Log a dispatch attempt. Returns the dispatch_id (UUID)."""
    dispatch_id = str(uuid.uuid4())
    with _db_conn() as conn:
        conn.execute(
            """
            INSERT INTO dispatch_log (resource_id, sender_id, recipient_id, guild_id, dispatch_id, status)
            VALUES (?, ?, ?, ?, ?, 'pending')
            """,
            (resource_id, sender_id, recipient_id, guild_id, dispatch_id),
        )
        conn.commit()
    return dispatch_id


def update_dispatch(
    dispatch_id: str,
    status: str,
    error: str | None = None,
    link_id_hash: str | None = None,
) -> None:
    """Update a dispatch log entry by dispatch_id."""
    with _db_conn() as conn:
        now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
        conn.execute(
            """
            UPDATE dispatch_log
            SET status = ?, error = ?, link_id_hash = COALESCE(?, link_id_hash), completed_at = ?
            WHERE dispatch_id = ?
            """,
            (status, error, link_id_hash, now, dispatch_id),
        )
        conn.commit()


def get_pending_dispatches() -> list[dict[str, Any]]:
    """Get all pending dispatch entries."""
    with _db_conn() as conn:
        rows = conn.execute(
            "SELECT * FROM dispatch_log WHERE status = 'pending' ORDER BY created_at ASC"
        ).fetchall()
        return [dict(r) for r in rows]


def get_dispatch_stats(resource_id: str) -> dict[str, int]:
    """Get dispatch statistics for a resource."""
    with _db_conn() as conn:
        rows = conn.execute(
            "SELECT status, COUNT(*) as cnt FROM dispatch_log WHERE resource_id = ? GROUP BY status",
            (resource_id,),
        ).fetchall()
        stats: dict[str, int] = {}
        failed_count = 0
        for row in rows:
            status = row["status"]
            count = row["cnt"]
            if status == "sent":
                stats["sent"] = count
            else:
                failed_count += count
        stats["sent"] = stats.get("sent", 0)
        stats["failed"] = failed_count
        return stats


def _prune_old_dispatches() -> int:
    """Delete dispatch log entries older than 90 days. Returns count deleted."""
    with _db_conn() as conn:
        cursor = conn.execute(
            "DELETE FROM dispatch_log WHERE created_at < datetime('now', '-90 days')"
        )
        conn.commit()
        return cursor.rowcount


def recover_stale_dispatches() -> int:
    """Mark any pending dispatches as failed_recovered (crash recovery)."""
    with _db_conn() as conn:
        cursor = conn.execute(
            "UPDATE dispatch_log SET status = 'failed_recovered' WHERE status = 'pending'"
        )
        conn.commit()
        return cursor.rowcount
