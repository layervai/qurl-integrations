"""Google Maps URL parser — extracts location data from various Maps URL formats."""

from __future__ import annotations

import ipaddress
import re
from urllib.parse import urlparse, parse_qs, unquote_plus

import httpx

_CONTROL_CHARS_RE = re.compile(r'[\x00-\x1f\x7f\u200b-\u200f\u2028-\u202f\u2060-\u206f\ufeff]')


def sanitize_query(query: str) -> str:
    """Strip control characters, RTL overrides, zero-width chars, and HTML tags."""
    # Remove HTML tag pairs and their content (e.g. <script>...</script>)
    query = re.sub(r'<[^>]+>.*?</[^>]+>', '', query, flags=re.DOTALL)
    # Remove any remaining standalone HTML tags
    query = re.sub(r'<[^>]+>', '', query)
    # Replace newlines/tabs with spaces before stripping control chars
    query = query.replace('\n', ' ').replace('\r', ' ').replace('\t', ' ')
    # Remove control characters and unicode formatting chars
    query = _CONTROL_CHARS_RE.sub('', query)
    # Collapse whitespace
    query = ' '.join(query.split())
    return query.strip()


# Match any Google Maps URL loosely (for detection)
MAPS_URL_RE = re.compile(
    r'https?://(?:www\.)?(?:maps\.)?google\.com/maps/[^\s]+'
    r'|https?://(?:goo\.gl/maps|maps\.app\.goo\.gl)/[\w-]+'
)


def detect_maps_url(text: str) -> str | None:
    """Return the first Google Maps URL found in text, or None."""
    m = MAPS_URL_RE.search(text)
    if not m:
        return None
    url = m.group(0)
    # Strip trailing punctuation that's likely part of prose, not the URL
    url = url.rstrip("!.,;:?'\")]}»")
    return url


_UNSUPPORTED_PATHS = ("/maps/dir/", "/maps/timeline", "/maps/contrib", "/maps/rpc/")


def is_unsupported_maps_format(url: str) -> bool:
    """Check if the URL is a recognized but unsupported Maps format (directions, timeline, etc.)."""
    if not url:
        return False
    parsed = urlparse(url)
    path = parsed.path
    return any(path.startswith(p) for p in _UNSUPPORTED_PATHS)


def parse_maps_url(url: str) -> dict | None:
    """
    Extract location data from a Google Maps URL.

    Returns dict with:
        query: str | None — place name or search query
        lat: float | None
        lng: float | None
    Or None if the URL is not a recognized Maps format.
    """
    if not url:
        return None

    parsed = urlparse(url)
    host = (parsed.hostname or "").lower()

    if host not in ("google.com", "www.google.com", "maps.google.com", "www.maps.google.com"):
        return None

    path = parsed.path
    result = {"query": None, "lat": None, "lng": None}

    # Embed URL: extract q= param
    if "/maps/embed/" in path:
        params = parse_qs(parsed.query)
        q = params.get("q", [None])[0]
        if q:
            result["query"] = sanitize_query(unquote_plus(q))
            return result
        return None

    # Place URL: /maps/place/QUERY[/@lat,lng,zoom]
    m = re.match(r'/maps/place/([^/@]+)', path)
    if m:
        result["query"] = sanitize_query(unquote_plus(m.group(1)))
        # Try to extract coordinates from the path
        coord_match = re.search(r'@([-\d.]+),([-\d.]+)', path)
        if coord_match:
            try:
                result["lat"] = float(coord_match.group(1))
                result["lng"] = float(coord_match.group(2))
            except ValueError:
                pass
        return result

    # Coordinate URL: /maps/@lat,lng,zoom
    m = re.match(r'/maps/@([-\d.]+),([-\d.]+)', path)
    if m:
        try:
            result["lat"] = float(m.group(1))
            result["lng"] = float(m.group(2))
            result["query"] = f"{result['lat']},{result['lng']}"
            return result
        except ValueError:
            return None

    # Search URL: /maps/search/QUERY
    m = re.match(r'/maps/search/([^/?\s]+)', path)
    if m:
        result["query"] = sanitize_query(unquote_plus(m.group(1)))
        return result

    return None


def validate_coordinates(lat: float | None, lng: float | None) -> bool:
    """Validate latitude and longitude ranges. Both must be present or both None."""
    if (lat is None) != (lng is None):
        return False  # One present, one missing
    if lat is None and lng is None:
        return True  # Both absent is valid (query-only)
    if lat < -90 or lat > 90:
        return False
    if lng < -180 or lng > 180:
        return False
    return True


def validate_query(query: str | None) -> bool:
    """Validate map query string."""
    if not query:
        return False
    # Reject queries that become empty after sanitization
    if not sanitize_query(query):
        return False
    if len(query) > 500:
        return False
    return True


# --- Short-link resolution with SSRF hardening ---

_ALLOWED_REDIRECT_HOSTS = frozenset({
    "google.com", "www.google.com",
    "maps.google.com", "www.maps.google.com",
    "goo.gl", "maps.app.goo.gl",
})

_MAX_REDIRECTS = 3
_RESOLVE_TIMEOUT = 3.0


async def _is_private_ip(hostname: str) -> bool:
    """Check if hostname resolves to a private/reserved/link-local IP (async DNS)."""
    import asyncio
    try:
        loop = asyncio.get_running_loop()
        infos = await loop.getaddrinfo(hostname, None)
        for info in infos:
            addr = info[4][0]
            ip = ipaddress.ip_address(addr)
            if ip.is_private or ip.is_reserved or ip.is_loopback or ip.is_link_local:
                return True
    except (OSError, ValueError):
        pass
    return False


async def resolve_short_link(url: str) -> str | None:
    """
    Follow redirects from a Google Maps short link to get the final URL.

    SSRF hardening:
    - Only follows redirects to allowlisted domains (Google/goo.gl only)
    - Blocks private/reserved IPs via async DNS check
    - Max 3 redirects
    - 3-second timeout
    - Single httpx client reused across hops

    Known limitation (TOCTOU): _is_private_ip() resolves DNS separately from
    the actual HTTP request. An attacker controlling DNS could return a public
    IP for the check and a private IP for the request. This is mitigated by
    the domain allowlist — only Google-owned domains are followed, making DNS
    rebinding attacks impractical. If the allowlist is ever broadened, this
    should be replaced with a custom transport that validates resolved IPs.

    Returns the final URL or None if resolution fails.
    """
    if not url:
        return None

    current_url = url
    try:
        async with httpx.AsyncClient(
            timeout=_RESOLVE_TIMEOUT,
            follow_redirects=False,
            verify=True,
        ) as client:
            for _ in range(_MAX_REDIRECTS):
                parsed = urlparse(current_url)
                host = (parsed.hostname or "").lower()

                # Domain allowlist
                if host not in _ALLOWED_REDIRECT_HOSTS:
                    return None

                # Private IP block
                if await _is_private_ip(host):
                    return None

                resp = await client.head(current_url)

                if resp.status_code in (301, 302, 303, 307, 308):
                    location = resp.headers.get("location")
                    if not location:
                        return None

                    # Validate redirect target
                    next_parsed = urlparse(location)
                    next_host = (next_parsed.hostname or "").lower()
                    if next_host not in _ALLOWED_REDIRECT_HOSTS:
                        return None
                    if await _is_private_ip(next_host):
                        return None

                    current_url = location
                    continue

                # No more redirects — verify the final response is successful
                if resp.status_code < 400:
                    return current_url
                return None  # 4xx/5xx final hop — resolution failed

    except (httpx.HTTPError, httpx.TimeoutException, Exception):
        return None

    # Exhausted redirect limit — resolution failed
    return None


def is_short_link(url: str) -> bool:
    """Check if a URL is a Google Maps short link."""
    return bool(url and ("goo.gl" in url or "maps.app.goo.gl" in url))
