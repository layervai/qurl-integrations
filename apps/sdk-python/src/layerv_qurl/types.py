"""Type definitions for the QURL API."""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class AccessPolicy:
    """Access control policy for a QURL."""

    ip_allowlist: list[str] | None = None
    ip_denylist: list[str] | None = None
    geo_allowlist: list[str] | None = None
    geo_denylist: list[str] | None = None
    user_agent_allow_regex: str | None = None
    user_agent_deny_regex: str | None = None


@dataclass
class QURL:
    """A QURL resource as returned by the API."""

    resource_id: str
    target_url: str
    status: str
    created_at: str
    expires_at: str | None = None
    one_time_use: bool = False
    max_sessions: int | None = None
    description: str | None = None
    qurl_site: str | None = None
    qurl_link: str | None = None
    access_policy: AccessPolicy | None = None


@dataclass
class CreateInput:
    """Input for creating a QURL."""

    target_url: str
    expires_in: str | None = None
    one_time_use: bool = False
    max_sessions: int | None = None
    description: str | None = None
    metadata: dict[str, str] | None = None
    access_policy: AccessPolicy | None = None
    custom_domain: str | None = None


@dataclass
class CreateOutput:
    """Response from creating a QURL."""

    resource_id: str
    qurl_link: str
    qurl_site: str
    expires_at: str | None = None


@dataclass
class ExtendInput:
    """Input for extending a QURL's expiration."""

    extend_by: str | None = None
    expires_at: str | None = None


@dataclass
class UpdateInput:
    """Input for updating a QURL."""

    description: str | None = None


@dataclass
class MintInput:
    """Input for minting an access link."""

    expires_at: str | None = None


@dataclass
class MintOutput:
    """Response from minting an access link."""

    qurl_link: str
    expires_at: str | None = None


@dataclass
class ResolveInput:
    """Input for headless QURL resolution."""

    access_token: str


@dataclass
class AccessGrant:
    """Details of the firewall access that was granted."""

    expires_in: int
    granted_at: str
    src_ip: str


@dataclass
class ResolveOutput:
    """Response from headless resolution."""

    target_url: str
    resource_id: str
    access_grant: AccessGrant | None = None


@dataclass
class ListOutput:
    """Response from listing QURLs."""

    qurls: list[QURL] = field(default_factory=list)
    next_cursor: str | None = None
    has_more: bool = False


@dataclass
class Quota:
    """Quota and usage information."""

    plan: str = ""
    period_start: str = ""
    period_end: str = ""
    rate_limits: dict[str, int] | None = None
    usage: dict[str, object] | None = None
