"""
Qurl Email Bot configuration module.

Manages environment variables using pydantic-settings.
"""

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Lambda function configuration"""

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
        extra="ignore",
    )

    # QURL API configuration
    qurl_api_key: str = ""
    upload_api_url: str = "https://getqurllink.layerv.ai"
    mint_link_api_url: str = "https://api.layerv.ai/v1/qurls"

    # Lambda configuration
    bot_address: str = "qurl@layerv.ai"
    link_expires_in: str = "15m"  # 15 minutes
    max_recipients: int = 25
    max_urls_per_email: int = 3
    max_attachment_size_mb: int = 25

    # AWS resource configuration
    inbound_bucket: str = ""
    dispatch_table: str = "qurl-email-dispatch-log"

    # SSM parameter paths
    authorized_senders_param: str = "/qurl-email-bot/authorized-senders"
    qurl_api_key_param: str = "/qurl-email-bot/qurl-api-key"
    forward_map_param: str = "/qurl-email-bot/forward-map"

    # SES configuration
    ses_source_arn: str = ""

    # AWS region
    aws_region: str = "us-east-1"

    # Rate limiting (per hour)
    rate_limit_per_hour: int = 5

    # Rate limit table name
    rate_limit_table: str = "qurl-email-rate-limits"

    # Allowed attachment types
    allowed_attachment_types: list[str] = [
        "application/pdf",
        "image/png",
        "image/jpeg",
        "image/gif",
        "image/webp",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    ]

    # Allowed attachment extensions
    allowed_attachment_extensions: list[str] = [
        ".pdf",
        ".png",
        ".jpg",
        ".jpeg",
        ".gif",
        ".webp",
        ".docx",
        ".xlsx",
    ]


@lru_cache()
def get_settings() -> Settings:
    """Get cached settings instance"""
    return Settings()
