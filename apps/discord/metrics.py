"""Lightweight CloudWatch custom metrics for QURL Discord Bot.

Buffers metrics in memory and flushes to CloudWatch periodically.
Falls back to logging if boto3 is unavailable or credentials are missing.

Thread safety: incr() and timing() use a threading.Lock so they are safe
to call from both the async event loop and asyncio.to_thread() workers.
"""

from __future__ import annotations

import asyncio
import logging
import threading
from typing import Any

logger = logging.getLogger(__name__)

_NAMESPACE = "QurlBot"
_FLUSH_INTERVAL = 60  # seconds
_MAX_COUNTER_KEYS = 10_000
_MAX_TIMING_SAMPLES = 10_000

# In-memory buffers, protected by _sync_lock for thread-safe writes
_counters: dict[str, float] = {}
_timings: dict[str, list[float]] = {}
_sync_lock = threading.Lock()


def incr(metric: str, value: float = 1.0) -> None:
    """Increment a counter metric. Thread-safe."""
    with _sync_lock:
        if metric in _counters:
            _counters[metric] += value
        elif len(_counters) < _MAX_COUNTER_KEYS:
            _counters[metric] = value
        else:
            logger.warning("Metrics counter buffer full, dropping metric %s", metric)


def timing(metric: str, duration_ms: float) -> None:
    """Record a timing metric in milliseconds. Thread-safe."""
    with _sync_lock:
        if metric in _timings:
            samples = _timings[metric]
            if len(samples) < _MAX_TIMING_SAMPLES:
                samples.append(duration_ms)
        elif len(_timings) < _MAX_COUNTER_KEYS:
            _timings[metric] = [duration_ms]
        else:
            logger.warning("Metrics timing buffer full, dropping metric %s", metric)


async def _flush() -> None:
    """Flush buffered metrics to CloudWatch."""
    try:
        import boto3
    except ImportError:
        logger.debug("boto3 not available — skipping CloudWatch metrics flush")
        return

    # Snapshot and reset buffers under the sync lock
    with _sync_lock:
        counters = dict(_counters)
        _counters.clear()
        timings_snapshot = {k: list(v) for k, v in _timings.items()}
        _timings.clear()

    if not counters and not timings_snapshot:
        return

    metric_data: list[dict[str, Any]] = []

    for name, value in counters.items():
        if value > 0:
            metric_data.append({
                "MetricName": name,
                "Value": value,
                "Unit": "Count",
            })

    for name, values in timings_snapshot.items():
        if values:
            metric_data.append({
                "MetricName": name,
                "StatisticValues": {
                    "SampleCount": len(values),
                    "Sum": sum(values),
                    "Minimum": min(values),
                    "Maximum": max(values),
                },
                "Unit": "Milliseconds",
            })

    if not metric_data:
        return

    try:
        client = boto3.client("cloudwatch")
        for i in range(0, len(metric_data), 25):
            await asyncio.to_thread(
                client.put_metric_data,
                Namespace=_NAMESPACE,
                MetricData=metric_data[i : i + 25],
            )
        logger.debug("Flushed %d metrics to CloudWatch", len(metric_data))
    except Exception:
        logger.error(
            "metric_flush_failed",
            extra={"audit": {"event": "metric_flush_failed", "count": len(metric_data)}},
            exc_info=True,
        )


_flush_lock: asyncio.Lock | None = None


def _get_flush_lock() -> asyncio.Lock:
    """Lazily create flush lock inside the running event loop (Python 3.12 compat)."""
    global _flush_lock
    if _flush_lock is None:
        _flush_lock = asyncio.Lock()
    return _flush_lock


async def periodic_flush() -> None:
    """Background task that flushes metrics every FLUSH_INTERVAL seconds."""
    while True:
        await asyncio.sleep(_FLUSH_INTERVAL)
        lock = _get_flush_lock()
        if lock.locked():
            logger.warning("Previous metrics flush still running, skipping this cycle")
            continue
        async with lock:
            await _flush()
