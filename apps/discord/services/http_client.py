"""Shared httpx.AsyncClient with connection pooling.

Reuses TCP+TLS connections across API calls. Created lazily on first use.
Must be closed on shutdown via close_client().
"""

from __future__ import annotations

import httpx

_client: httpx.AsyncClient | None = None


def get_client(timeout: float = 30.0) -> httpx.AsyncClient:
    """Get or create the shared AsyncClient."""
    global _client
    if _client is None or _client.is_closed:
        _client = httpx.AsyncClient(
            timeout=timeout,
            limits=httpx.Limits(
                max_connections=20,
                max_keepalive_connections=10,
            ),
        )
    return _client


async def close_client() -> None:
    """Close the shared client. Call on bot shutdown."""
    global _client
    if _client is not None and not _client.is_closed:
        await _client.aclose()
        _client = None
