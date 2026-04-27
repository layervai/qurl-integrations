"""
Qurl Email Bot HTTP client module.

Provides an httpx client with connection reuse.
"""

import httpx
from functools import lru_cache


@lru_cache()
def get_http_client() -> httpx.Client:
    """
    Get an HTTP client instance with connection reuse.

    httpx.Client automatically reuses connection pools, suitable for Lambda.
    """
    return httpx.Client(
        timeout=30.0,
        limits=httpx.Limits(
            max_keepalive_connections=10,
            max_connections=20,
        ),
        headers={
            "User-Agent": "qurl-email-bot/1.0",
            "Accept": "application/json",
        },
    )


class QurlApiClient:
    """QURL API client wrapper"""

    def __init__(self, api_key: str, base_url: str):
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self._client = get_http_client()

    def _get_headers(self, extra_headers: dict | None = None) -> dict:
        """Build request headers"""
        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "Content-Type": "application/json",
        }
        if extra_headers:
            headers.update(extra_headers)
        return headers

    def post(self, path: str, data: dict | None = None, files: dict | None = None) -> dict:
        """Send a POST request"""
        url = f"{self.base_url}{path}"
        headers = self._get_headers()

        if files:
            headers.pop("Content-Type", None)
            response = self._client.post(url, data=data, files=files, headers=headers)
        else:
            response = self._client.post(url, json=data, headers=headers)

        response.raise_for_status()
        return response.json()

    def get(self, path: str, params: dict | None = None) -> dict:
        """Send a GET request"""
        url = f"{self.base_url}{path}"
        headers = self._get_headers()
        response = self._client.get(url, params=params, headers=headers)
        response.raise_for_status()
        return response.json()
