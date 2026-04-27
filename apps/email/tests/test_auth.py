"""
Authentication module tests.
"""


from auth import (
    authenticate_sender,
    load_authorized_senders_from_ssm,
    load_api_key_from_ssm,
    check_spf_dkim,
    verify_email_authentication,
    authenticate_sender_with_api,
)
from config import Settings


def _make_settings(aws_region="us-east-1", **kwargs):
    defaults = dict(
        bot_address="qurl@layerv.ai",
        max_recipients=25,
        max_urls_per_email=3,
        max_attachment_size_mb=25,
        authorized_senders_param="/qurl-email-bot/authorized-senders",
        qurl_api_key_param="/qurl-email-bot/qurl-api-key",
        aws_region=aws_region,
    )
    defaults.update(kwargs)
    return Settings(**defaults)


def _ssm_param_name(counter):
    """Unique SSM param name per test to avoid moto overwrite conflicts."""
    return f"/test/sender-list-{counter}"


class TestLoadAuthorizedSenders:
    """Load authorized senders tests"""

    def test_load_from_ssm(self, aws_services):
        """Test loading authorized senders from SSM"""
        settings = _make_settings()
        result = load_authorized_senders_from_ssm(settings)
        assert "sender@example.com" in result
        assert "test@company.com" in result

    def test_empty_list(self, aws_services):
        """Test empty list returns empty set"""
        param = _ssm_param_name(id(self))
        ssm = aws_services["ssm"]
        ssm.put_parameter(Name=param, Type="String", Value="[]")
        settings = _make_settings(authorized_senders_param=param)
        result = load_authorized_senders_from_ssm(settings)
        assert len(result) == 0

    def test_invalid_json(self, aws_services):
        """Test invalid JSON returns empty set"""
        param = _ssm_param_name(id(self))
        ssm = aws_services["ssm"]
        ssm.put_parameter(Name=param, Type="String", Value="not valid json")
        settings = _make_settings(authorized_senders_param=param)
        result = load_authorized_senders_from_ssm(settings)
        assert len(result) == 0

    def test_load_ssm_client_error(self, aws_services):
        """Test SSM ClientError returns empty set for nonexistent param"""
        settings = _make_settings(authorized_senders_param="/nonexistent/param")
        result = load_authorized_senders_from_ssm(settings)
        assert len(result) == 0


class TestLoadApiKey:
    """load_api_key_from_ssm tests"""

    def test_load_api_key_success(self, aws_services):
        """Test loading API key from SSM"""
        settings = _make_settings()
        result = load_api_key_from_ssm(settings)
        assert result == "test-api-key"

    def test_load_api_key_ssm_error(self, aws_services):
        """Test SSM error returns empty string for nonexistent param"""
        settings = _make_settings(qurl_api_key_param="/nonexistent/key")
        result = load_api_key_from_ssm(settings)
        assert result == ""


class TestAuthenticateSender:
    """Sender authentication tests"""

    def test_authorized_sender(self, aws_services):
        """Test authorized sender returns CustomerInfo"""
        settings = _make_settings()
        result = authenticate_sender("sender@example.com", settings)
        assert result is not None
        assert result.email == "sender@example.com"
        assert result.owner_id == "email:sender@example.com"
        assert result.tier == "growth"

    def test_authorized_sender_case_insensitive(self, aws_services):
        """Test email case normalization"""
        settings = _make_settings()
        result = authenticate_sender("Sender@Example.COM", settings)
        assert result is not None
        assert result.email == "sender@example.com"

    def test_unauthorized_sender(self, aws_services):
        """Test unauthorized sender returns None"""
        settings = _make_settings()
        result = authenticate_sender("bob@example.com", settings)
        assert result is None

    def test_empty_authorized_list(self, aws_services):
        """Test empty authorized list returns None"""
        param = _ssm_param_name(id(self))
        ssm = aws_services["ssm"]
        ssm.put_parameter(Name=param, Type="String", Value="[]")
        settings = _make_settings(authorized_senders_param=param)
        result = authenticate_sender("sender@example.com", settings)
        assert result is None


class TestSpfDkim:
    """SPF/DKIM verification tests"""

    def test_spf_pass(self):
        """Test SPF pass"""
        mock_msg = _MockMsg({
            "Received-SPF": "pass from mx.example.com",
            "Authentication-Results": "",
        })
        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is True
        assert dkim_pass is False

    def test_dkim_pass(self):
        """Test DKIM pass"""
        mock_msg = _MockMsg({
            "Received-SPF": "",
            "Authentication-Results": "dkim=pass header.d=example.com",
        })
        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is False
        assert dkim_pass is True

    def test_both_pass(self):
        """Test both SPF and DKIM pass"""
        mock_msg = _MockMsg({
            "Received-SPF": "pass",
            "Authentication-Results": "dkim=pass",
        })
        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is True
        assert dkim_pass is True

    def test_neither_pass(self):
        """Test neither pass"""
        mock_msg = _MockMsg({
            "Received-SPF": "fail",
            "Authentication-Results": "dkim=fail",
        })
        spf_pass, dkim_pass = check_spf_dkim(mock_msg)
        assert spf_pass is False
        assert dkim_pass is False

    def test_verify_email_authentication_spf(self):
        """Test verify returns True when SPF passes"""
        mock_msg = _MockMsg({
            "Received-SPF": "pass",
            "Authentication-Results": "",
        })
        result = verify_email_authentication(mock_msg)
        assert result is True

    def test_verify_email_authentication_dkim(self):
        """Test verify returns True when DKIM passes"""
        mock_msg = _MockMsg({
            "Received-SPF": "fail",
            "Authentication-Results": "dkim=pass",
        })
        result = verify_email_authentication(mock_msg)
        assert result is True

    def test_verify_email_authentication_fail(self):
        """Test verify returns False when both fail"""
        mock_msg = _MockMsg({
            "Received-SPF": "fail",
            "Authentication-Results": "dkim=fail",
        })
        result = verify_email_authentication(mock_msg)
        assert result is False


class _MockMsg:
    """Minimal mock for email.message.EmailMessage."""
    def __init__(self, headers):
        self._headers = headers

    def get(self, key, default=""):
        return self._headers.get(key, default)


class TestAuthenticateSenderWithApi:
    """authenticate_sender_with_api tests"""

    def test_authenticate_with_api_success(self, aws_services):
        """Test successful API authentication"""
        settings = _make_settings(mint_link_api_url="https://api.layerv.ai/v1/qurls")
        mock_resp = _make_httpx_response({"auth0_subject": "auth0|12345", "tier": "growth", "frozen": False})
        with _patch_httpx_get(mock_resp):
            result = authenticate_sender_with_api("Sender@Example.COM", "test-api-key", settings)
            assert result is not None
            assert result.owner_id == "auth0|12345"
            assert result.tier == "growth"

    def test_authenticate_with_api_frozen(self, aws_services):
        """Test frozen account returns None"""
        settings = _make_settings(mint_link_api_url="https://api.layerv.ai/v1/qurls")
        mock_resp = _make_httpx_response({"auth0_subject": "auth0|12345", "tier": "growth", "frozen": True})
        with _patch_httpx_get(mock_resp):
            result = authenticate_sender_with_api("sender@example.com", "key", settings)
            assert result is None

    def test_authenticate_with_api_free_tier(self, aws_services):
        """Test free tier returns None"""
        settings = _make_settings(mint_link_api_url="https://api.layerv.ai/v1/qurls")
        mock_resp = _make_httpx_response({"auth0_subject": "auth0|free", "tier": "free", "frozen": False})
        with _patch_httpx_get(mock_resp):
            result = authenticate_sender_with_api("free@example.com", "key", settings)
            assert result is None

    def test_authenticate_with_api_error(self, aws_services):
        """Test API error returns None gracefully"""
        settings = _make_settings(mint_link_api_url="https://api.layerv.ai/v1/qurls")
        mock_resp = _make_httpx_error()
        with _patch_httpx_get(mock_resp):
            result = authenticate_sender_with_api("sender@example.com", "key", settings)
            assert result is None


# ---------------------------------------------------------------------------
# httpx mock helpers
# ---------------------------------------------------------------------------

def _make_httpx_response(json_data, status=200):
    """Build a minimal mock that satisfies httpx-style Response."""
    response = _MockHttpxResponse(json_data, status)
    return response


def _make_httpx_error():
    """Build a mock that raises on raise_for_status."""
    response = _MockHttpxResponse({}, 500)
    return response


class _MockHttpxResponse:
    """Standalone mock for httpx.Response (avoids importing httpx at test top)."""
    def __init__(self, json_data, status=200):
        self._json = json_data
        self.status_code = status

    def json(self):
        return self._json

    def raise_for_status(self):
        if self.status_code >= 400:
            raise Exception(f"HTTP {self.status_code}")


class _HttpxGetPatcher:
    """Context manager: patches httpx.get globally to return a fixed response."""
    def __init__(self, mock_response):
        self._mock = mock_response

    def __enter__(self):
        import sys
        self._mod = sys.modules["httpx"]
        self._orig = getattr(self._mod, "get", None)
        setattr(self._mod, "get", lambda url, **kw: self._mock)
        return self

    def __exit__(self, *args):
        if self._orig is None:
            delattr(self._mod, "get")
        else:
            setattr(self._mod, "get", self._orig)


def _patch_httpx_get(mock_response):
    return _HttpxGetPatcher(mock_response)
