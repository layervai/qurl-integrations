"""
conftest.py fixture tests — exercises aws_services, settings, and sample fixtures.
"""

import pytest


class TestConftestFixtures:
    """Exercise conftest fixtures for coverage."""

    def test_settings_fixture(self, settings):
        """Test settings fixture provides expected values"""
        assert settings.bot_address == "qurl@layerv.ai"
        assert settings.max_recipients == 25
        assert settings.max_urls_per_email == 3
        assert settings.max_attachment_size_mb == 25
        assert settings.aws_region == "us-east-1"

    def test_sample_email_simple(self, sample_email_simple):
        """Test sample_email_simple fixture"""
        assert sample_email_simple["from"] == "sender@example.com"
        assert sample_email_simple["to"] == "qurl@layerv.ai"
        assert "bob@company.com" in sample_email_simple["body"]

    def test_sample_email_with_url(self, sample_email_with_url):
        """Test sample_email_with_url fixture"""
        assert "https://example.com/document.pdf" in sample_email_with_url["body"]

    def test_sample_email_with_signature(self, sample_email_with_signature):
        """Test sample_email_with_signature fixture"""
        assert "--" in sample_email_with_signature["body"]

    def test_sample_email_no_recipients(self, sample_email_no_recipients):
        """Test sample_email_no_recipients fixture"""
        assert sample_email_no_recipients["from"] == "sender@example.com"

    def test_sample_email_with_sender_as_recipient(self, sample_email_with_sender_as_recipient):
        """Test sender-as-recipient fixture"""
        assert "sender@example.com" in sample_email_with_sender_as_recipient["body"]

    def test_sample_email_many_recipients(self, sample_email_many_recipients):
        """Test many-recipients fixture"""
        assert "user0@company.com" in sample_email_many_recipients["body"]
        assert "user29@company.com" in sample_email_many_recipients["body"]

    def test_aws_services_s3(self, aws_services):
        """Test aws_services fixture creates S3 bucket"""
        s3 = aws_services["s3"]
        response = s3.list_buckets()
        assert "qurl-email-inbound-test" in [b["Name"] for b in response["Buckets"]]

    def test_aws_services_sqs(self, aws_services):
        """Test aws_services fixture creates SQS queues"""
        sqs = aws_services["sqs"]
        queues = sqs.list_queues()
        queue_urls = queues.get("QueueUrls", [])
        assert aws_services["queue_url"] in queue_urls
        assert aws_services["dlq_url"] in queue_urls

    def test_aws_services_dynamodb(self, aws_services):
        """Test aws_services fixture creates DynamoDB tables"""
        dispatch = aws_services["dispatch_table"]
        assert dispatch.table_name == "qurl-email-dispatch-log"

        rate_limit = aws_services["rate_limit_table"]
        assert rate_limit.table_name == "qurl-email-rate-limits"

    def test_aws_services_ssm(self, aws_services):
        """Test aws_services fixture creates SSM parameters"""
        ssm = aws_services["ssm"]
        senders_param = ssm.get_parameter(Name="/test/authorized-senders")
        value = senders_param["Parameter"]["Value"]
        assert "sender@example.com" in value

        api_key_param = ssm.get_parameter(Name="/test/qurl-api-key", WithDecryption=True)
        assert api_key_param["Parameter"]["Value"] == "test-api-key"

    def test_mock_qurl_client_upload(self, mock_qurl_client):
        """Test mock_qurl_client returns upload values"""
        result = mock_qurl_client.upload_file(
            file_bytes=b"pdf",
            filename="f.pdf",
            content_type="application/pdf",
            owner_id="o1",
        )
        assert result.resource_id == "r_test123"
        assert result.filename == "test.pdf"

    def test_mock_qurl_client_create_qurl(self, mock_qurl_client):
        """Test mock_qurl_client returns create_qurl values"""
        result = mock_qurl_client.create_qurl(target_url="https://example.com")
        assert result.resource_id == "r_url456"

    def test_mock_qurl_client_mint_link(self, mock_qurl_client):
        """Test mock_qurl_client returns mint_link values"""
        result = mock_qurl_client.mint_link(
            resource_id="r_abc",
            recipient_id="bob@example.com",
        )
        assert result.link_id == "l_xyz789"
        assert result.url == "https://qurl.link/abc"
