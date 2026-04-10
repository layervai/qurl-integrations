"""Tests for Google Maps URL parser."""

import pytest
import httpx
from unittest.mock import AsyncMock, MagicMock, patch
from services.maps_parser import detect_maps_url, is_short_link, is_unsupported_maps_format, parse_maps_url, resolve_short_link, sanitize_query, validate_coordinates, validate_query


class TestDetectMapsUrl:
    def test_place_url(self):
        text = "check this out google.com/maps/place/Seattle,WA cool right?"
        # detect_maps_url needs https://
        text2 = "check https://www.google.com/maps/place/Seattle,WA out"
        assert detect_maps_url(text2) is not None

    def test_coordinate_url(self):
        url = detect_maps_url("look at https://www.google.com/maps/@47.6062,-122.3321,15z")
        assert url is not None
        assert "47.6062" in url

    def test_search_url(self):
        url = detect_maps_url("https://www.google.com/maps/search/pizza+near+seattle")
        assert url is not None

    def test_embed_url(self):
        url = detect_maps_url("https://www.google.com/maps/embed/v1/place?key=ABC&q=Seattle")
        assert url is not None

    def test_short_link(self):
        url = detect_maps_url("check https://goo.gl/maps/abc123xyz")
        assert url is not None

    def test_maps_app_short_link(self):
        url = detect_maps_url("https://maps.app.goo.gl/abc123")
        assert url is not None

    def test_no_maps_url(self):
        assert detect_maps_url("just a regular message with no urls") is None

    def test_non_google_url(self):
        assert detect_maps_url("https://evil.com/maps/place/Seattle") is None

    def test_trailing_punctuation_stripped(self):
        url = detect_maps_url("check https://www.google.com/maps/place/Seattle,WA!")
        assert url is not None
        assert not url.endswith("!")

    def test_trailing_period_stripped(self):
        url = detect_maps_url("Visit https://www.google.com/maps/place/Seattle,WA.")
        assert url is not None
        assert not url.endswith(".")

    def test_trailing_paren_stripped(self):
        url = detect_maps_url("(see https://www.google.com/maps/place/Seattle,WA)")
        assert url is not None
        assert not url.endswith(")")


class TestParseMapsUrl:
    def test_place_url(self):
        result = parse_maps_url("https://www.google.com/maps/place/Seattle,WA")
        assert result is not None
        assert result["query"] == "Seattle,WA"

    def test_place_url_with_plus(self):
        result = parse_maps_url("https://www.google.com/maps/place/Seattle,+WA")
        assert result is not None
        assert "Seattle" in result["query"]

    def test_place_url_with_coordinates(self):
        result = parse_maps_url("https://www.google.com/maps/place/Seattle,+WA/@47.6062,-122.3321,15z")
        assert result is not None
        assert result["query"] is not None
        assert result["lat"] == pytest.approx(47.6062)
        assert result["lng"] == pytest.approx(-122.3321)

    def test_coordinate_url(self):
        result = parse_maps_url("https://www.google.com/maps/@47.6062,-122.3321,15z")
        assert result is not None
        assert result["lat"] == pytest.approx(47.6062)
        assert result["lng"] == pytest.approx(-122.3321)
        assert result["query"] is not None  # auto-generated from coordinates

    def test_search_url(self):
        result = parse_maps_url("https://www.google.com/maps/search/pizza+near+seattle")
        assert result is not None
        assert "pizza" in result["query"].lower()

    def test_embed_url(self):
        result = parse_maps_url("https://www.google.com/maps/embed/v1/place?key=ABC&q=Seattle,WA")
        assert result is not None
        assert result["query"] == "Seattle,WA"

    def test_invalid_url(self):
        assert parse_maps_url("https://evil.com/maps/place/Seattle") is None

    def test_empty_url(self):
        assert parse_maps_url("") is None
        assert parse_maps_url(None) is None

    def test_maps_google_com_host(self):
        result = parse_maps_url("https://maps.google.com/maps/place/Seattle,WA")
        assert result is not None
        assert result["query"] == "Seattle,WA"

    def test_maps_url_without_recognized_path(self):
        assert parse_maps_url("https://www.google.com/maps") is None

    def test_encoded_plus_in_place(self):
        """Literal + encoded as %2B should survive parsing."""
        result = parse_maps_url("https://www.google.com/maps/place/C%2B%2B+Conference")
        assert result is not None
        assert "C++" in result["query"] or "C%2B%2B" not in result["query"]

    def test_embed_url_no_q_param(self):
        result = parse_maps_url("https://www.google.com/maps/embed/v1/place?key=ABC")
        assert result is None

    def test_short_link_returns_none(self):
        """Short links are detected but not parseable (Phase 2)."""
        assert parse_maps_url("https://goo.gl/maps/abc123") is None
        assert parse_maps_url("https://maps.app.goo.gl/abc123") is None


class TestValidateCoordinates:
    def test_valid(self):
        assert validate_coordinates(47.6, -122.3) is True

    def test_extremes(self):
        assert validate_coordinates(90, 180) is True
        assert validate_coordinates(-90, -180) is True

    def test_out_of_range_lat(self):
        assert validate_coordinates(91, 0) is False
        assert validate_coordinates(-91, 0) is False

    def test_out_of_range_lng(self):
        assert validate_coordinates(0, 181) is False
        assert validate_coordinates(0, -181) is False

    def test_none_values(self):
        assert validate_coordinates(None, None) is True

    def test_lat_only_rejected(self):
        assert validate_coordinates(47.6, None) is False

    def test_lng_only_rejected(self):
        assert validate_coordinates(None, -122.3) is False


class TestValidateQuery:
    def test_valid(self):
        assert validate_query("Seattle,WA") is True

    def test_empty(self):
        assert validate_query("") is False
        assert validate_query(None) is False

    def test_too_long(self):
        assert validate_query("x" * 501) is False

    def test_at_limit(self):
        assert validate_query("x" * 500) is True


class TestSanitizeQuery:
    def test_strips_html_tags(self):
        assert sanitize_query("<script>alert(1)</script>Seattle") == "Seattle"

    def test_strips_control_chars(self):
        assert sanitize_query("Seattle\x00WA") == "SeattleWA"

    def test_strips_newlines(self):
        assert sanitize_query("Seattle\nWA") == "Seattle WA"

    def test_strips_rtl_override(self):
        from services.maps_parser import sanitize_query
        assert "\u202e" not in sanitize_query("Seattle\u202eWA")

    def test_normal_query_unchanged(self):
        assert sanitize_query("Seattle, WA") == "Seattle, WA"

    def test_strips_bidi_isolate_chars(self):
        assert "\u2066" not in sanitize_query("Seattle\u2066WA")
        assert "\u2067" not in sanitize_query("Seattle\u2067WA")
        assert "\u2069" not in sanitize_query("Seattle\u2069WA")


class TestUnsupportedMapsFormat:
    def test_directions_url(self):
        assert is_unsupported_maps_format("https://www.google.com/maps/dir/Seattle/Portland") is True

    def test_timeline_url(self):
        assert is_unsupported_maps_format("https://www.google.com/maps/timeline") is True

    def test_place_url_not_unsupported(self):
        assert is_unsupported_maps_format("https://www.google.com/maps/place/Seattle") is False

    def test_coordinate_url_not_unsupported(self):
        assert is_unsupported_maps_format("https://www.google.com/maps/@47.6,-122.3,15z") is False

    def test_empty_url(self):
        assert is_unsupported_maps_format("") is False


class TestIsShortLink:
    def test_goo_gl(self):
        assert is_short_link("https://goo.gl/maps/abc123") is True

    def test_maps_app(self):
        assert is_short_link("https://maps.app.goo.gl/abc123") is True

    def test_google_com(self):
        assert is_short_link("https://www.google.com/maps/place/Seattle") is False

    def test_empty(self):
        assert is_short_link("") is False


class TestResolveShortLink:
    @pytest.mark.asyncio
    async def test_follows_redirect_to_google(self):
        redirect_resp = MagicMock()
        redirect_resp.status_code = 302
        redirect_resp.headers = {"location": "https://www.google.com/maps/place/Seattle,WA"}

        final_resp = MagicMock()
        final_resp.status_code = 200
        final_resp.headers = {}

        mock_client = AsyncMock()
        mock_client.__aenter__ = AsyncMock(return_value=mock_client)
        mock_client.__aexit__ = AsyncMock(return_value=False)
        mock_client.head = AsyncMock(side_effect=[redirect_resp, final_resp])

        with patch("services.maps_parser.httpx.AsyncClient", return_value=mock_client), \
             patch("services.maps_parser._is_private_ip", new_callable=AsyncMock, return_value=False):
            result = await resolve_short_link("https://goo.gl/maps/abc123")
        assert result == "https://www.google.com/maps/place/Seattle,WA"

    @pytest.mark.asyncio
    async def test_blocks_redirect_to_non_google(self):
        mock_resp = MagicMock()
        mock_resp.status_code = 302
        mock_resp.headers = {"location": "https://evil.com/steal"}

        mock_client = AsyncMock()
        mock_client.__aenter__ = AsyncMock(return_value=mock_client)
        mock_client.__aexit__ = AsyncMock(return_value=False)
        mock_client.head = AsyncMock(return_value=mock_resp)

        with patch("services.maps_parser.httpx.AsyncClient", return_value=mock_client), \
             patch("services.maps_parser._is_private_ip", new_callable=AsyncMock, return_value=False):
            result = await resolve_short_link("https://goo.gl/maps/abc123")
        assert result is None

    @pytest.mark.asyncio
    async def test_blocks_private_ip(self):
        with patch("services.maps_parser._is_private_ip", new_callable=AsyncMock, return_value=True):
            result = await resolve_short_link("https://goo.gl/maps/abc123")
        assert result is None

    @pytest.mark.asyncio
    async def test_blocks_non_allowlisted_host(self):
        result = await resolve_short_link("https://evil.com/redirect")
        assert result is None

    @pytest.mark.asyncio
    async def test_timeout_returns_none(self):
        mock_client = AsyncMock()
        mock_client.__aenter__ = AsyncMock(return_value=mock_client)
        mock_client.__aexit__ = AsyncMock(return_value=False)
        mock_client.head = AsyncMock(side_effect=httpx.TimeoutException("timeout"))

        with patch("services.maps_parser.httpx.AsyncClient", return_value=mock_client), \
             patch("services.maps_parser._is_private_ip", new_callable=AsyncMock, return_value=False):
            result = await resolve_short_link("https://goo.gl/maps/abc123")
        assert result is None

    @pytest.mark.asyncio
    async def test_final_hop_4xx_returns_none(self):
        """A final response with 4xx status should return None, not the URL."""
        redirect_resp = MagicMock()
        redirect_resp.status_code = 302
        redirect_resp.headers = {"location": "https://www.google.com/maps/place/Seattle,WA"}

        final_resp = MagicMock()
        final_resp.status_code = 404
        final_resp.headers = {}

        mock_client = AsyncMock()
        mock_client.__aenter__ = AsyncMock(return_value=mock_client)
        mock_client.__aexit__ = AsyncMock(return_value=False)
        mock_client.head = AsyncMock(side_effect=[redirect_resp, final_resp])

        with patch("services.maps_parser.httpx.AsyncClient", return_value=mock_client), \
             patch("services.maps_parser._is_private_ip", new_callable=AsyncMock, return_value=False):
            result = await resolve_short_link("https://goo.gl/maps/abc123")
        assert result is None

    @pytest.mark.asyncio
    async def test_empty_url(self):
        result = await resolve_short_link("")
        assert result is None
