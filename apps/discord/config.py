"""Configuration management using pydantic-settings."""

from __future__ import annotations

import re

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

# Hostname: lowercase letters, digits, hyphens, dots. No port, no scheme.
_HOSTNAME_RE = re.compile(r"^[a-z0-9]([a-z0-9\-\.]*[a-z0-9])?$")


class Settings(BaseSettings):
    discord_bot_token: str
    discord_client_id: str
    qurl_api_key: str
    upload_api_url: str = "https://getqurllink.layerv.ai"
    mint_link_api_url: str = "https://api.layerv.ai/v1/qurls"
    host: str = "0.0.0.0"
    port: int = 3000
    cdn_url_allowlist: str = "cdn.discordapp.com,media.discordapp.net"
    rate_limit_per_minute: int = 5
    max_file_size_mb: int = 25
    db_path: str = "data/qurl_bot.db"
    sync_commands_globally: bool = False
    qurl_link_hostname: str = "qurl.link"
    link_expires_in: str = "15m"
    # Feature gate: Maps support is disabled by default until Phase 2
    # (upload service type:google-map support) is deployed. Set
    # GOOGLE_MAPS_ENABLED=true in ECS task definition env vars to enable.
    # TODO: read from SSM Parameter Store at runtime for no-redeploy toggle.
    google_maps_enabled: bool = False

    @field_validator("qurl_link_hostname")
    @classmethod
    def _validate_hostname(cls, v: str) -> str:
        v = v.strip().lower()
        if not v or not _HOSTNAME_RE.match(v):
            raise ValueError(f"QURL_LINK_HOSTNAME must be a valid hostname, got: {v!r}")
        return v

    model_config = SettingsConfigDict(
        env_file=".env", env_file_encoding="utf-8", extra="ignore"
    )


settings = Settings()
