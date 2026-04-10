"""Entry point for QURL Discord Bot."""

from __future__ import annotations

import asyncio
import json as json_mod
import logging
import re
import signal
import time

from aiohttp import web

from config import settings
from db import prune_old_dispatches, init_db, recover_stale_dispatches
from metrics import periodic_flush as metrics_flush


class AuditJsonFormatter(logging.Formatter):
    """Emit AUDIT log records as structured JSON; all others as plain text.

    Thread-safe: uses a local Formatter for plain text to avoid shared state.
    """

    _plain_formatter = logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s")

    def format(self, record: logging.LogRecord) -> str:
        audit = getattr(record, "audit", None)
        if audit and isinstance(audit, dict):
            entry = {
                "timestamp": self.formatTime(record),
                "level": record.levelname,
                "logger": record.name,
                "audit": audit,
            }
            return json_mod.dumps(entry, default=str)
        return self._plain_formatter.format(record)


handler = logging.StreamHandler()
handler.setFormatter(AuditJsonFormatter())
logging.basicConfig(level=logging.INFO, handlers=[handler])
logger = logging.getLogger(__name__)


class ApiKeyRedactor(logging.Filter):
    """Redact API keys, Discord bot tokens, and Bearer tokens from log messages."""

    # M8: Expanded patterns to catch Bearer tokens
    _patterns = [
        re.compile(r"lv_(live|test)_[A-Za-z0-9_\-]+"),
        re.compile(r"Bearer\s+[A-Za-z0-9._\-]{20,}"),
    ]

    def filter(self, record: logging.LogRecord) -> bool:
        record.msg = self._redact(record.msg) if isinstance(record.msg, str) else record.msg
        if record.args:
            if isinstance(record.args, dict):
                record.args = {k: self._redact(v) if isinstance(v, str) else v for k, v in record.args.items()}
            elif isinstance(record.args, tuple):
                record.args = tuple(self._redact(a) if isinstance(a, str) else a for a in record.args)
        return True

    def _redact(self, text: str) -> str:
        for pattern in self._patterns:
            text = pattern.sub("***REDACTED***", text)
        return text


logging.getLogger().addFilter(ApiKeyRedactor())

start_time: float | None = None
_bot_ref = None  # set during main()


# M10: Separate /health (liveness, always 200) from /ready (readiness)
async def health_handler(request: web.Request) -> web.Response:
    """Liveness probe — always returns 200 if the process is running."""
    uptime = int(time.time() - start_time) if start_time else 0
    return web.json_response({"status": "ok", "uptime": uptime})


async def ready_handler(request: web.Request) -> web.Response:
    """Readiness probe — returns 200 only when bot is connected to Discord."""
    ready = _bot_ref is not None and _bot_ref.is_ready()
    return web.json_response(
        {"status": "ready" if ready else "not_ready"},
        status=200 if ready else 503,
    )


async def start_health_server() -> web.AppRunner:
    """Start the health check HTTP server."""
    app = web.Application()
    app.router.add_get("/health", health_handler)
    app.router.add_get("/ready", ready_handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, settings.host, settings.port)
    await site.start()
    logger.info("Health server on %s:%d (/health, /ready)", settings.host, settings.port)
    return runner


async def _periodic_retention() -> None:
    """Prune dispatch log entries older than 90 days, every 24 hours."""
    while True:
        await asyncio.sleep(86400)  # 24 hours
        try:
            deleted = await asyncio.to_thread(prune_old_dispatches)
            if deleted:
                logger.info("Retention: pruned %d old dispatch log entries", deleted)
        except Exception as e:
            logger.error("Retention task failed: %s", e)


async def main() -> None:
    global start_time, _bot_ref
    start_time = time.time()

    # Init database
    init_db(settings.db_path)
    logger.info("Database initialized at %s", settings.db_path)

    # M9 / B2: Mark stale pending dispatches as failed_recovered
    recovered = recover_stale_dispatches()
    if recovered:
        logger.warning(
            "Recovered %d stale pending dispatches from previous run (marked failed_recovered)",
            recovered,
        )

    # Start health check
    health_runner = await start_health_server()

    # Import bot here to ensure config is loaded first
    # M1: import bot directly (no get_bot() facade)
    from adapters.discord_bot import bot

    _bot_ref = bot

    # B2: Start periodic retention task
    retention_task = asyncio.create_task(_periodic_retention())
    metrics_task = asyncio.create_task(metrics_flush())

    async def shutdown() -> None:
        logger.info("Shutting down...")
        metrics_task.cancel()
        retention_task.cancel()
        await bot.close()
        # Close shared HTTP client pool
        from services.http_client import close_client
        await close_client()
        await health_runner.cleanup()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, lambda: asyncio.create_task(shutdown()))

    try:
        logger.info("Starting Qurl Discord Bot...")
        await bot.start(settings.discord_bot_token)
    finally:
        metrics_task.cancel()
        retention_task.cancel()
        await health_runner.cleanup()


if __name__ == "__main__":
    asyncio.run(main())
