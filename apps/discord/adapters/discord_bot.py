"""Discord adapter for QURL bot — slash commands and DM upload flow."""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import re
import time
from datetime import datetime
from typing import Optional

import discord
import httpx
from discord import app_commands
from services.http_client import get_client as get_http_client
from discord.ext import commands

import metrics
from bot_helpers import RESOURCE_ID_MARKER_RE, check_guild_members, format_dispatch_summary, is_expired
from config import settings
from db import (
    DispatchStatus,
    bind_guild,
    delete_resource,
    get_dispatch_stats,
    get_owner,
    delete_all_resources,
    list_resources,
    log_dispatch,
    register_owner,
    search_resources,
    update_dispatch,
)
from rate_limiter import RateLimiter
from services.maps_parser import (
    MAPS_URL_RE,
    detect_maps_url,
    is_short_link,
    is_unsupported_maps_format,
    parse_maps_url,
    resolve_short_link,
    sanitize_query,
    validate_coordinates,
    validate_query,
)
from services.mint_link_client import mint_link
from services.upload_client import upload_file
from validation import (
    DEFAULT_LINK_EXPIRY,
    sanitize_filename,
    split_message,
    validate_cdn_url,
    validate_expires,
    validate_file_type,
    validate_resource_id,
    validate_snowflake,
)

logger = logging.getLogger(__name__)

# --- Expiry choices for /qurl send ---
EXPIRY_CHOICES = [
    app_commands.Choice(name="5 minutes", value="5m"),
    app_commands.Choice(name="15 minutes (default)", value="15m"),
    app_commands.Choice(name="1 hour", value="1h"),
    app_commands.Choice(name="24 hours", value="24h"),
    app_commands.Choice(name="7 days", value="7d"),
]

_EXPIRY_DISPLAY = {
    "5m": "5 minutes",
    "15m": "15 minutes",
    "1h": "1 hour",
    "24h": "24 hours",
    "7d": "7 days",
}
rate_limiter = RateLimiter(settings.rate_limit_per_minute)

# Cap concurrent uploads to bound peak memory (~5 x 25MB = 125MB worst case).
# Validation (size, type, CDN) runs outside the semaphore for instant rejection.
_upload_sem = asyncio.Semaphore(5)


class QurlBot(commands.Bot):
    def __init__(self) -> None:
        intents = discord.Intents.default()
        # MessageContent intent NOT required for slash commands or DM attachments.
        # However, reply-to-share reads message.mentions in DM replies — Discord
        # exempts DMs from the MessageContent privileged intent requirement, so
        # mentions are populated without it. If this ever breaks, enable:
        # intents.message_content = True
        intents.dm_messages = True
        intents.guilds = True
        # members intent required for guild.fetch_member in check_guild_members (bot_helpers.py)
        intents.members = True
        super().__init__(command_prefix="!", intents=intents)

    async def setup_hook(self) -> None:
        if settings.sync_commands_globally:
            await self.tree.sync()
            logger.info("Slash commands synced globally (may take up to 1 hour to propagate)")
        else:
            logger.info("Skipping global command sync (set SYNC_COMMANDS_GLOBALLY=true to enable)")


# M1: Simple module-level singleton — no get_bot() facade
bot = QurlBot()


def _format_discord_ts(iso_str: str | None, fallback: str = "unknown") -> str:
    """Convert ISO 8601 timestamp to Discord relative timestamp format."""
    if not iso_str:
        return fallback
    try:
        dt = datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
        return f"<t:{int(dt.timestamp())}:R>"
    except (ValueError, AttributeError):
        return iso_str


async def _resolve_owned_resource(
    interaction: discord.Interaction, resource_id: str
) -> tuple[str, dict] | None:
    """Validate resource_id, check ownership. Returns (rid, owner_info) or None on failure."""
    rid = resource_id.lstrip("$").strip()
    if not validate_resource_id(rid):
        await interaction.followup.send("Invalid resource ID format.", ephemeral=True)
        return None
    user_id = str(interaction.user.id)
    owner_info = await asyncio.to_thread(get_owner, rid)
    if not owner_info or owner_info["discord_user_id"] != user_id:
        await interaction.followup.send(
            "Resource not found or you are not the owner.", ephemeral=True
        )
        return None
    return rid, owner_info


async def _resource_autocomplete(
    interaction: discord.Interaction, current: str
) -> list[app_commands.Choice[str]]:
    """Autocomplete handler — shows user's resources by name. Uses DB-side filtering for performance."""
    t0 = time.monotonic()
    try:
        user_id = str(interaction.user.id)
        # Strip $ prefix for backwards compat with manual resource_id input (e.g. "$r_abc")
        query = current.lower().lstrip("$")
        resources = await asyncio.to_thread(search_resources, user_id, query, 25)
        matches = []
        for r in resources:
            label = r.get("filename", r["resource_id"])
            matches.append(app_commands.Choice(name=label[:100], value=r["resource_id"]))
        return matches
    except Exception:
        logger.exception("Autocomplete failed for user %s", interaction.user.id)
        return []
    finally:
        # Measure full callback wall time including failures (Discord enforces 3s timeout)
        metrics.timing("AutocompleteLatency", (time.monotonic() - t0) * 1000)


# --- Shared Dispatch Helper (used by both reply-to-share and /qurl send) ---

# Known dispatch error mappings — module level so it's not recreated per exception.
# HTTPStatusError handled separately (needs dynamic status code).
_DISPATCH_ERRORS = {
    discord.NotFound:       (DispatchStatus.DM_FAILED,   "user_not_found", "User not found"),
    discord.Forbidden:      (DispatchStatus.DM_FAILED,   "dms_disabled",   "DMs disabled"),
    httpx.TimeoutException: (DispatchStatus.MINT_FAILED, "mint_timeout",   "Failed to mint link"),
}

# Per-user semaphore: prevents one user's bulk dispatch from starving others.
# 10 concurrent dispatches per sender; LRU eviction at 1000 entries.
_user_sems: dict[str, asyncio.Semaphore] = {}
_USER_SEM_LIMIT = 10
_USER_SEM_MAX_ENTRIES = 1000


def _get_user_sem(sender_id: str) -> asyncio.Semaphore:
    """Get or create a per-user dispatch semaphore with LRU eviction.

    No asyncio.Lock needed: this function is fully synchronous (no await points),
    so the GIL prevents interleaving between coroutines. The entire pop-check-insert
    sequence runs atomically.
    """
    sem = _user_sems.pop(sender_id, None)  # Remove to re-insert at end (LRU refresh)
    if sem is None:
        # Evict oldest entry if at capacity
        if len(_user_sems) >= _USER_SEM_MAX_ENTRIES:
            oldest = next(iter(_user_sems))
            del _user_sems[oldest]
        sem = asyncio.Semaphore(_USER_SEM_LIMIT)
    _user_sems[sender_id] = sem  # Insert at end (most recent)
    return sem


async def _dispatch_to_recipient(
    rid: str, sender_id: str, recipient_id: str, guild_id: str | None,
    expires_in: str = DEFAULT_LINK_EXPIRY,
) -> tuple[str, str, str | None, str]:
    """Mint a link and DM one recipient. Returns (recipient_id, status, error, display_name)."""
    async with _get_user_sem(sender_id):
        t0 = time.monotonic()
        dispatch_id = await asyncio.to_thread(
            log_dispatch, rid, sender_id, recipient_id, guild_id
        )
        try:
            # Fetch recipient BEFORE minting to avoid orphaned links if user doesn't exist
            recipient = await bot.fetch_user(int(recipient_id))
            mint_t0 = time.monotonic()
            link_data = await mint_link(rid, recipient_id, expires_in=expires_in)
            metrics.timing("MintApiLatency", (time.monotonic() - mint_t0) * 1000)
            qurl_link = link_data["qurl_link"]
            expires_at = link_data.get("expires_at", "")
            expires_display = _format_discord_ts(expires_at, fallback=f"in {_EXPIRY_DISPLAY.get(expires_in, expires_in)}")
            await recipient.send(
                f"**You've been granted access to a resource.**\n\n"
                f"{qurl_link}\n\n"
                f"_Single use - expires {expires_display}_"
            )
            link_hash = hashlib.sha256(qurl_link.encode()).hexdigest()[:16]
            await asyncio.to_thread(
                update_dispatch, dispatch_id, DispatchStatus.SENT, None, link_hash
            )
            metrics.timing("DispatchLatency", (time.monotonic() - t0) * 1000)
            metrics.incr("DispatchSent")
            logger.info(
                "dispatch_sent",
                extra={"audit": {"event": "dispatch_sent", "resource": rid, "sender": sender_id, "recipient": recipient_id, "link_hash": link_hash}},
            )
            return (recipient_id, DispatchStatus.SENT, None, recipient.display_name)
        except (discord.NotFound, discord.Forbidden, httpx.TimeoutException, httpx.HTTPStatusError) as exc:
            # Known dispatch errors — uses module-level _DISPATCH_ERRORS mapping
            if type(exc) in _DISPATCH_ERRORS:
                status, error_code, display = _DISPATCH_ERRORS[type(exc)]
            else:
                status, error_code, display = DispatchStatus.MINT_FAILED, f"mint_{exc.response.status_code}", "Failed to mint link"
            metrics.incr("DispatchFailed")
            logger.info(
                "dispatch_failed",
                extra={"audit": {"event": "dispatch_failed", "resource": rid, "recipient": recipient_id, "error": error_code}},
            )
            await asyncio.to_thread(update_dispatch, dispatch_id, status, error_code)
            return (recipient_id, status, display, recipient_id)
        except Exception:
            # Unexpected error — log full traceback so bugs surface instead of hiding
            logger.exception("Unexpected dispatch error for %s -> %s", rid, recipient_id)
            metrics.incr("DispatchFailed")
            await asyncio.to_thread(
                update_dispatch, dispatch_id, DispatchStatus.MINT_FAILED, "unexpected_error"
            )
            return (recipient_id, DispatchStatus.MINT_FAILED, "Failed to mint link", recipient_id)


async def _handle_reply_to_share(message: discord.Message) -> bool:
    """Handle reply-to-share flow. Returns True if handled, False to fall through."""
    ref = message.reference
    if not ref or not ref.message_id:
        return False

    # Rate limit before any network/DB work
    user_id = str(message.author.id)
    if not rate_limiter.check(user_id):
        metrics.incr("RateLimitHit")
        await message.reply("Too many requests. Please wait a moment.")
        return True

    try:
        parent = await message.channel.fetch_message(ref.message_id)
    except (discord.NotFound, discord.HTTPException):
        await message.reply("Could not read the replied message. Please try again.")
        return True

    # Only handle replies to bot's own messages
    if not bot.user or parent.author.id != bot.user.id:
        return False  # Not a bot message or bot not ready — fall through

    # Extract resource_id from deterministic marker line — not arbitrary backtick strings
    rid_match = RESOURCE_ID_MARKER_RE.search(parent.content)
    if not rid_match:
        await message.reply("Could not find a resource ID in that message.")
        return True

    rid = rid_match.group(1)
    if not validate_resource_id(rid):
        await message.reply("Invalid resource ID.")
        return True

    owner_info = await asyncio.to_thread(get_owner, rid)
    if not owner_info or owner_info["discord_user_id"] != user_id:
        await message.reply("You are not the owner of this resource.")
        return True

    if is_expired(owner_info.get("expires_at")):
        await message.reply("This resource has expired.")
        return True

    # Parse mentioned users (exclude bot and self), validate snowflakes, deduplicate
    bot_uid = bot.user.id if bot.user else None
    recipient_ids = [
        str(u.id) for u in message.mentions
        if u.id != bot_uid and str(u.id) != user_id
    ]
    recipient_ids = list(dict.fromkeys(recipient_ids))
    recipient_ids = [uid for uid in recipient_ids if validate_snowflake(uid)]

    if not recipient_ids:
        await message.reply("Please @mention at least one recipient.")
        return True

    if len(recipient_ids) > 25:
        await message.reply("Maximum 25 recipients per share.")
        return True

    # Guild membership check: if resource is bound to a guild, verify recipients (parallel)
    bound_guild = owner_info.get("guild_id")
    if bound_guild:
        try:
            guild = bot.get_guild(int(bound_guild))
            if guild:
                recipient_ids = await check_guild_members(guild, recipient_ids)
                if not recipient_ids:
                    await message.reply("None of the mentioned users are in the resource's server.")
                    return True
            else:
                logger.debug("Guild %s not in bot cache — skipping membership check for %s", bound_guild, rid)
        except Exception as e:
            logger.debug("Guild membership check skipped for %s: %s", rid, e)

    await message.reply(f"Sending links to {len(recipient_ids)} user(s)...")

    filename = owner_info.get("filename", rid)
    tasks = [
        _dispatch_to_recipient(rid, user_id, uid, bound_guild)
        for uid in recipient_ids
    ]
    results = await asyncio.gather(*tasks)
    summary = format_dispatch_summary(filename, results)
    for part in split_message(summary):
        await message.reply(part)
    return True


# --- Google Maps URL Handler ---


async def _handle_maps_url(message: discord.Message, user_id: str) -> bool:
    """Handle a DM containing a Google Maps URL. Returns True if handled."""
    maps_url = detect_maps_url(message.content or "")
    if not maps_url:
        return False

    if not rate_limiter.check(user_id):
        metrics.incr("RateLimitHit")
        await message.reply("Too many requests. Please wait a moment.")
        return True

    # Resolve short links (goo.gl, maps.app.goo.gl)
    if is_short_link(maps_url):
        resolved = await resolve_short_link(maps_url)
        if resolved:
            maps_url = resolved
        else:
            await message.reply(
                "Could not resolve the short Maps link. Please send the full google.com/maps URL."
            )
            return True

    # Directions, timeline, contrib, rpc — recognized but not supported
    if is_unsupported_maps_format(maps_url):
        await message.reply(
            "This Google Maps URL format isn't supported. Please send a place or coordinate link."
        )
        return True

    maps_data = parse_maps_url(maps_url)
    if not maps_data:
        await message.reply("Could not parse the Google Maps URL. Please send a valid Maps link.")
        return True

    has_query = validate_query(maps_data.get("query"))
    has_coords = maps_data.get("lat") is not None and validate_coordinates(
        maps_data.get("lat"), maps_data.get("lng")
    )
    if not has_query and not has_coords:
        await message.reply("Could not parse the Google Maps URL. Please send a valid Maps link.")
        return True

    # Capture annotation context — text surrounding the URL that the user typed
    caption = re.sub(MAPS_URL_RE, "", message.content or "").strip()[:500]
    caption = sanitize_query(caption) if caption else None

    location_label = maps_data.get("query") or f"{maps_data.get('lat')},{maps_data.get('lng')}"

    try:
        payload = {
            "type": "google-map",
            "query": maps_data.get("query"),
            "lat": maps_data.get("lat"),
            "lng": maps_data.get("lng"),
        }
        if caption:
            payload["caption"] = caption

        result = await upload_file(
            file_bytes=json.dumps(payload).encode(),
            filename="map.json",
            content_type="application/json",
            owner_id=user_id,
        )

        owner_filename = f"map:{location_label}"
        if caption:
            owner_filename += f"|{caption[:100]}"

        await asyncio.to_thread(
            register_owner,
            result["resource_id"],
            user_id,
            None,
            owner_filename,
            result.get("expires_at"),
        )

        metrics.incr("UploadSuccess")
        logger.info(
            "upload_success",
            extra={"audit": {"event": "upload_success", "type": "map", "user": user_id, "resource": result["resource_id"], "location": location_label}},
        )

        expires_display = _format_discord_ts(result.get("expires_at"))

        await message.reply(
            f"**Map location wrapped in a single-use link.**\n\n"
            f"\u26a0\ufe0f Recipients can screenshot or read coordinates from the browser. "
            f"The watermark traces any leak back to the specific recipient.\n\n"
            f"**Location:** {location_label}\n"
            f"**Link:** {result['qurl_link']}\n"
            f"**Expires:** {expires_display}\n\n"
            f"Reply to this message and @mention users to share, "
            f"or use `/qurl send` in any server channel.\n"
            f"\\_resource\\_id:{result['resource_id']}\\_"
        )
        return True
    except Exception:
        metrics.incr("UploadFailed")
        logger.exception("Map upload failed for user %s", user_id)
        await message.reply("Something went wrong protecting the map location. Please try again.")
        return True


# --- DM Upload Flow ---


@bot.event
async def on_message(message: discord.Message) -> None:
    if message.author.bot:
        return

    # Only handle DMs
    if not isinstance(message.channel, discord.DMChannel):
        return

    # --- Reply-to-share: user replies to a bot upload confirmation and @mentions recipients ---
    if message.reference and message.mentions:
        if message.attachments:
            await message.reply(
                "Can't share and upload at the same time. "
                "Reply without an attachment to share, or send the file separately."
            )
            return
        handled = await _handle_reply_to_share(message)
        if handled:
            return

    # DM with no attachment — check for Maps URL, then help hint
    if not message.attachments:
        user_id = str(message.author.id)
        if settings.google_maps_enabled:
            handled = await _handle_maps_url(message, user_id)
            if handled:
                return

        help_msg = "Send me a file to protect it, or reply to a protected resource and @mention users to share."
        if settings.google_maps_enabled:
            help_msg = "Send me a file or Google Maps link to protect it, or reply to a protected resource and @mention users to share."
        await message.reply(help_msg)
        return

    user_id = str(message.author.id)

    if not rate_limiter.check(user_id):
        metrics.incr("RateLimitHit")
        await message.reply("Too many requests. Please wait a moment.")
        return

    # Warn about multiple attachments
    if len(message.attachments) > 1:
        await message.reply(
            "Please send one file at a time. Only the first attachment will be processed."
        )

    attachment = message.attachments[0]

    # Validate file size
    max_bytes = settings.max_file_size_mb * 1024 * 1024
    if attachment.size > max_bytes:
        await message.reply(
            f"File too large. Maximum size is {settings.max_file_size_mb}MB."
        )
        return

    # Validate file type
    content_type = attachment.content_type or ""
    if not validate_file_type(content_type, attachment.filename):
        await message.reply(
            "Unsupported file type. Allowed: PNG, JPG, GIF, WebP, PDF."
        )
        return

    # Validate CDN URL (SSRF protection)
    if not validate_cdn_url(attachment.url, settings.cdn_url_allowlist):
        await message.reply("Invalid attachment source.")
        return

    # Sanitize filename
    safe_filename = sanitize_filename(attachment.filename)

    async with _upload_sem:
        try:
            # Download file from Discord CDN with timeout
            try:
                file_bytes = await asyncio.wait_for(attachment.read(), timeout=15.0)
            except asyncio.TimeoutError:
                await message.reply("Download timed out. Please try again.")
                return

            # Re-check file size after download (metadata may differ from actual)
            if len(file_bytes) > max_bytes:
                await message.reply(f"File too large. Maximum size is {settings.max_file_size_mb}MB.")
                return

            # Upload to QURL API
            result = await upload_file(
                file_bytes=file_bytes,
                filename=safe_filename,
                content_type=content_type or "application/octet-stream",
                owner_id=user_id,
            )

            # Register ownership in local DB (with expires_at)
            await asyncio.to_thread(
                register_owner,
                result["resource_id"],
                user_id,
                None,
                safe_filename,
                result.get("expires_at"),
            )

            metrics.incr("UploadSuccess")
            logger.info(
                "upload_success",
                extra={"audit": {"event": "upload_success", "user": user_id, "resource": result["resource_id"], "filename": safe_filename, "expires": result.get("expires_at", "unknown")}},
            )

            # Build reply
            expires_display = _format_discord_ts(result.get("expires_at"))

            await message.reply(
                f"**Your resource has been protected!**\n\n"
                f"**{safe_filename}**\n"
                f"{result['qurl_link']}\n"
                f"Expires {expires_display}\n\n"
                f"This link is **single-use**. Once opened, it's consumed.\n\n"
                f"**To share with others** — reply to this message and @mention them:\n"
                f"> @alice @bob\n\n"
                f"Or use `/qurl send` in any server channel (autocomplete will show your resources).\n"
                f"\\_resource\\_id:{result['resource_id']}\\_"
            )
        except (httpx.HTTPStatusError, httpx.TimeoutException) as e:
            metrics.incr("UploadFailed")
            logger.error("Upload API error for user %s: %s", user_id, e)
            await message.reply("Something went wrong. Please try again.")
        except ValueError as e:
            logger.error("Upload validation error for user %s: %s", user_id, e)
            await message.reply("Something went wrong. Please try again.")
        except Exception:
            logger.exception("Unexpected upload error for user %s", user_id)
            await message.reply("Something went wrong. Please try again.")


# --- Slash Command Group ---

qurl_group = app_commands.Group(name="qurl", description="Manage QURL protected resources")


@qurl_group.command(name="send", description="Send protected resource links to users")
@app_commands.describe(
    resource="Select a resource to share (type to search)",
    users="Users to send links to (@mention or comma-separated IDs)",
    expires="How long each link lasts (default: 15 minutes)",
)
@app_commands.autocomplete(resource=_resource_autocomplete)
@app_commands.choices(expires=EXPIRY_CHOICES)
async def qurl_send(
    interaction: discord.Interaction,
    resource: str,
    users: str,
    expires: Optional[app_commands.Choice[str]] = None,
) -> None:
    """Dispatch per-recipient QURL links."""
    user_id = str(interaction.user.id)

    if not rate_limiter.check(user_id):
        metrics.incr("RateLimitHit")
        await interaction.response.send_message(
            "Too many requests. Please wait a moment.", ephemeral=True
        )
        return

    # Defer IMMEDIATELY before any async/DB work (3-second timeout fix)
    await interaction.response.defer(ephemeral=True)

    # Resolve expiry: Discord enforces choices at UI layer, but validate
    # server-side as defense-in-depth (e.g. API calls bypassing Discord).
    expires_value = expires.value if expires else DEFAULT_LINK_EXPIRY
    if not validate_expires(expires_value):
        await interaction.followup.send(
            "Invalid expiry value.", ephemeral=True
        )
        return

    metrics.incr(f"ExpiryChoice_{expires_value}")

    # resource param comes from autocomplete (value = resource_id) or manual input
    rid = resource.lstrip("$").strip()

    # Validate resource_id format
    if not validate_resource_id(rid):
        await interaction.followup.send("Invalid resource ID format.", ephemeral=True)
        return

    # Check ownership (run in thread since SQLite is synchronous)
    owner_info = await asyncio.to_thread(get_owner, rid)
    if not owner_info:
        await interaction.followup.send(
            f"Resource `{rid}` not found.", ephemeral=True
        )
        return
    if owner_info["discord_user_id"] != user_id:
        await interaction.followup.send(
            "You are not the owner of this resource.", ephemeral=True
        )
        return

    # Check if resource has expired locally (before wasting API calls)
    if is_expired(owner_info.get("expires_at")):
        await interaction.followup.send("This resource has expired.", ephemeral=True)
        return

    # Guild scoping: bind to first dispatch guild
    guild_id = str(interaction.guild_id) if interaction.guild_id else None

    # B4: bind_guild returns rowcount — single DB round-trip, no race window
    if not owner_info.get("guild_id"):
        if guild_id:
            bound = await asyncio.to_thread(bind_guild, rid, guild_id)
            if not bound:
                await interaction.followup.send(
                    "This resource was just bound to another server. Please try again.",
                    ephemeral=True,
                )
                return
    elif owner_info["guild_id"] != guild_id:
        await interaction.followup.send(
            "This resource is bound to a different server.", ephemeral=True
        )
        return

    # Parse mentioned users — handle commas, mentions, and raw IDs
    tokens = [t.strip() for t in users.replace(" ", ",").split(",") if t.strip()]
    parsed_ids = []
    for token in tokens:
        # Strip mention wrapper <@!123> or <@123>
        m = re.match(r"^<@!?(\d+)>$", token)
        if m:
            parsed_ids.append(m.group(1))
        elif token.isdigit():
            parsed_ids.append(token)

    # Deduplicate while preserving order
    mentioned_ids = list(dict.fromkeys(parsed_ids))

    # Validate snowflake format
    mentioned_ids = [uid for uid in mentioned_ids if validate_snowflake(uid)]

    if not mentioned_ids:
        await interaction.followup.send(
            "Please mention at least one user.", ephemeral=True
        )
        return

    # Max 25 recipients
    if len(mentioned_ids) > 25:
        await interaction.followup.send(
            "Maximum 25 recipients per dispatch.", ephemeral=True
        )
        return

    # Filter out self and bot
    bot_user_id = str(bot.user.id) if bot.user else ""
    mentioned_ids = [
        uid for uid in mentioned_ids if uid != user_id and uid != bot_user_id
    ]

    if not mentioned_ids:
        await interaction.followup.send("No valid recipients.", ephemeral=True)
        return

    # Verify guild membership (parallel, shared helper)
    if interaction.guild:
        mentioned_ids = await check_guild_members(interaction.guild, mentioned_ids)

    if not mentioned_ids:
        await interaction.followup.send(
            "None of the mentioned users are in this server.", ephemeral=True
        )
        return

    await interaction.followup.send(
        f"Dispatching links to {len(mentioned_ids)} user(s)...", ephemeral=True
    )

    # Use shared dispatch helper
    tasks = [
        _dispatch_to_recipient(rid, user_id, uid, guild_id, expires_in=expires_value)
        for uid in mentioned_ids
    ]
    results = await asyncio.gather(*tasks)

    filename = owner_info.get("filename", rid)
    summary = format_dispatch_summary(filename, results)
    for part in split_message(summary):
        await interaction.followup.send(part, ephemeral=True)


@qurl_group.command(name="list", description="List your protected resources")
async def qurl_list(interaction: discord.Interaction) -> None:
    user_id = str(interaction.user.id)
    resources = await asyncio.to_thread(list_resources, user_id)

    if not resources:
        await interaction.response.send_message(
            "No protected resources found.", ephemeral=True
        )
        return

    lines = [f"**Your protected resources ({len(resources)}):**\n"]
    for r in resources[:20]:  # Limit display
        filename = r.get("filename", "untitled")
        expires_ts = _format_discord_ts(r.get("expires_at"), fallback="")
        expires_display = f" -- expires {expires_ts}" if expires_ts else ""
        lines.append(f"- **{filename}**{expires_display} (`{r['resource_id']}`)")

    # M7: Add "and N more" indicator when truncated
    if len(resources) > 20:
        lines.append(f"\n_...and {len(resources) - 20} more_")

    text = "\n".join(lines)
    parts = split_message(text)
    await interaction.response.send_message(parts[0], ephemeral=True)
    for part in parts[1:]:
        await interaction.followup.send(part, ephemeral=True)


@qurl_group.command(name="revoke", description="Revoke a protected resource")
@app_commands.describe(resource="Select a resource to revoke (type to search)")
@app_commands.autocomplete(resource=_resource_autocomplete)
async def qurl_revoke(interaction: discord.Interaction, resource: str) -> None:
    resource_id = resource
    await interaction.response.defer(ephemeral=True)
    result = await _resolve_owned_resource(interaction, resource_id)
    if not result:
        return
    rid, _ = result

    # M3: Local delete first (idempotent), then upstream
    await asyncio.to_thread(delete_resource, rid)

    # M4: Inline the httpx call (was _qurl_api_request)
    upstream_ok = False
    try:
        url = f"{settings.mint_link_api_url.rstrip('/')}/{rid}"
        headers = {"Authorization": f"Bearer {settings.qurl_api_key}"}
        client = get_http_client()
        resp = await client.delete(url, headers=headers, timeout=10.0)
        if resp.status_code < 300:
            upstream_ok = True
        else:
            logger.warning("Upstream revoke returned %d for %s", resp.status_code, rid)
    except Exception as e:
        logger.error("Upstream revoke failed for %s: %s", rid, e)

    if upstream_ok:
        metrics.incr("RevokeSuccess")
        logger.info("revoke_success", extra={"audit": {"event": "revoke_success", "user": str(interaction.user.id), "resource": rid}})
        await interaction.followup.send(f"Resource `{rid}` has been revoked.", ephemeral=True)
    else:
        await interaction.followup.send(
            f"Resource `{rid}` removed locally, but upstream revoke failed. "
            "The resource may still be active on the server.",
            ephemeral=True,
        )


@qurl_group.command(name="clear", description="Delete all your protected resources")
async def qurl_clear(interaction: discord.Interaction) -> None:
    """Clear all local resources for the invoking user.

    Design decision: only deletes local registry entries, does NOT revoke
    upstream QURL links. Upstream revocation would require fetching all
    resources, calling the revoke API for each, and handling partial failures.
    For MVP, local-only clear with an explicit warning is acceptable.
    See #31 for future upstream revocation consideration.
    """
    await interaction.response.defer(ephemeral=True)
    user_id = str(interaction.user.id)

    count = await asyncio.to_thread(delete_all_resources, user_id)

    if count == 0:
        await interaction.followup.send("You have no resources to clear.", ephemeral=True)
        return

    metrics.incr("ClearSuccess")
    logger.info(
        "clear_success",
        extra={"audit": {"event": "clear_all", "user": user_id, "count": count}},
    )
    await interaction.followup.send(
        f"Cleared **{count}** resource(s) from your local registry.\n\n"
        "_Note: upstream QURL links may still be active until they expire._",
        ephemeral=True,
    )


@qurl_group.command(name="status", description="Check status of a protected resource")
@app_commands.describe(resource="Select a resource to check (type to search)")
@app_commands.autocomplete(resource=_resource_autocomplete)
async def qurl_status(interaction: discord.Interaction, resource: str) -> None:
    resource_id = resource
    await interaction.response.defer(ephemeral=True)
    result = await _resolve_owned_resource(interaction, resource_id)
    if not result:
        return
    rid, owner_info = result

    stats = await asyncio.to_thread(get_dispatch_stats, rid)

    # M4: Inline the httpx call (was _qurl_api_request)
    upstream_lines = []
    try:
        url = f"{settings.mint_link_api_url.rstrip('/')}/{rid}"
        headers = {"Authorization": f"Bearer {settings.qurl_api_key}"}
        client = get_http_client()
        resp = await client.get(url, headers=headers, timeout=10.0)
        if resp.status_code == 200:
            try:
                data = resp.json()
            except (ValueError, TypeError):
                data = {}
            payload = data.get("data", data)
            upstream_status = payload.get("status", "unknown")
            upstream_created = payload.get("created_at", "unknown")
            upstream_expires = payload.get("expires_at", "unknown")
            upstream_lines.append(f"- Upstream status: {upstream_status}")
            upstream_lines.append(f"- Upstream created: {upstream_created}")
            upstream_lines.append(f"- Upstream expires: {upstream_expires}")
        else:
            upstream_lines.append(
                f"- Upstream status: unavailable (HTTP {resp.status_code})"
            )
    except Exception as e:
        logger.warning("Upstream GET for %s failed: %s", rid, e)
        upstream_lines.append("- Upstream status: unavailable (request failed)")

    upstream_section = "\n".join(upstream_lines)

    await interaction.followup.send(
        f"**Status for `{rid}`**\n"
        f"- Resource: {owner_info.get('filename', 'unknown')}\n"
        f"- Created: {owner_info['created_at']}\n"
        f"- Links sent: {stats.get('sent', 0)}\n"
        f"- Failed: {stats.get('failed', 0)}\n"
        f"{upstream_section}",
        ephemeral=True,
    )


@qurl_group.command(name="help", description="Show QURL bot help")
async def qurl_help(interaction: discord.Interaction) -> None:
    await interaction.response.send_message(
        "**Qurl Bot -- Help**\n\n"
        "DM a file or Maps link to @QurlBot to protect it.\n\n"
        "**Sharing options:**\n"
        "  Reply to the bot's DM and @mention users (easiest)\n"
        "  `/qurl send` -- pick a resource from autocomplete, tag recipients\n\n"
        "**Other commands:**\n"
        "  `/qurl list` -- your protected resources\n"
        "  `/qurl status` -- check link usage\n"
        "  `/qurl revoke` -- revoke a resource and all links\n"
        "  `/qurl clear` -- delete all your resources\n"
        "  `/qurl help` -- show this message\n\n"
        "Every recipient gets a unique, single-use link by DM.\n"
        "Links self-destruct on access.",
        ephemeral=True,
    )


bot.tree.add_command(qurl_group)
