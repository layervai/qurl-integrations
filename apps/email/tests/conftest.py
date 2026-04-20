"""
Test configuration and shared fixtures.
"""

import json
import pytest
from unittest.mock import MagicMock


@pytest.fixture
def sample_email_simple():
    """Simple email sample"""
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "Test Email",
        "body": """Send to:
bob@company.com
carol@company.com

Hello, this is a test email.
""",
    }


@pytest.fixture
def sample_email_with_url():
    """Email sample with URL"""
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "Share a link",
        "body": """Send to:
bob@company.com

Check out this link: https://example.com/document.pdf

Best regards
""",
    }


@pytest.fixture
def sample_email_with_signature():
    """Email sample with signature"""
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "Report",
        "body": """Send to:
bob@company.com

Here is the report you requested.

Best,
John

--
John Doe
CEO, Example Corp
john@example.com
""",
    }


@pytest.fixture
def sample_email_no_recipients():
    """Email sample with no recipients"""
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "No recipients",
        "body": """Hello,

I want to share something but forgot to add recipients.

Best
""",
    }


@pytest.fixture
def sample_email_with_sender_as_recipient():
    """Email sample where sender is also a recipient"""
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "Test",
        "body": """Send to:
sender@example.com
bob@company.com

Test email.
""",
    }


@pytest.fixture
def sample_email_many_recipients():
    """Email sample with over 25 recipients"""
    recipients = [f"user{i}@company.com" for i in range(30)]
    return {
        "from": "sender@example.com",
        "to": "qurl@layerv.ai",
        "subject": "Many recipients",
        "body": f"""Send to:
{chr(10).join(recipients)}

Test email.
""",
    }


@pytest.fixture
def settings():
    """Test settings"""
    from config import Settings
    return Settings(
        bot_address="qurl@layerv.ai",
        max_recipients=25,
        max_urls_per_email=3,
        max_attachment_size_mb=25,
        authorized_senders_param="/test/authorized-senders",
        qurl_api_key_param="/test/qurl-api-key",
        aws_region="us-east-1",
    )


@pytest.fixture
def aws_services():
    """
    Mock AWS services using moto.

    Sets up the following AWS resources for testing:
    - S3 bucket (qurl-email-inbound-test)
    - SQS queue (qurl-email-processing) + DLQ (qurl-email-processing-dlq)
    - DynamoDB: qurl-email-dispatch-log (with sender-index GSI)
    - DynamoDB: qurl-email-rate-limits
    - SSM: authorized-senders, qurl-api-key, forward-map
    """
    from moto import mock_aws
    import boto3

    with mock_aws():
        s3 = boto3.client("s3", region_name="us-east-1")
        s3.create_bucket(Bucket="qurl-email-inbound-test")

        # SQS queues
        sqs = boto3.client("sqs", region_name="us-east-1")
        dlq_url = sqs.create_queue(
            QueueName="qurl-email-processing-dlq",
            Attributes={
                "VisibilityTimeout": "180",
                "MessageRetentionPeriod": "604800",
            },
        )["QueueUrl"]
        dlq_arn = sqs.get_queue_attributes(
            QueueUrl=dlq_url, AttributeNames=["QueueArn"]
        )["Attributes"]["QueueArn"]
        queue_url = sqs.create_queue(
            QueueName="qurl-email-processing",
            Attributes={
                "VisibilityTimeout": "180",
                "MessageRetentionPeriod": "86400",
                "RedrivePolicy": json.dumps({
                    "deadLetterTargetArn": dlq_arn,
                    "maxReceiveCount": 3,
                }),
            },
        )["QueueUrl"]

        # DynamoDB: dispatch log with sender-index GSI
        dynamodb = boto3.resource("dynamodb", region_name="us-east-1")
        dispatch_table = dynamodb.create_table(
            TableName="qurl-email-dispatch-log",
            KeySchema=[
                {"AttributeName": "resource_id", "KeyType": "HASH"},
                {"AttributeName": "dispatch_id", "KeyType": "RANGE"},
            ],
            AttributeDefinitions=[
                {"AttributeName": "resource_id", "AttributeType": "S"},
                {"AttributeName": "dispatch_id", "AttributeType": "S"},
                {"AttributeName": "sender_email", "AttributeType": "S"},
                {"AttributeName": "created_at", "AttributeType": "S"},
            ],
            GlobalSecondaryIndexes=[
                {
                    "IndexName": "sender-index",
                    "KeySchema": [
                        {"AttributeName": "sender_email", "KeyType": "HASH"},
                        {"AttributeName": "created_at", "KeyType": "RANGE"},
                    ],
                    "Projection": {"ProjectionType": "ALL"},
                },
            ],
            BillingMode="PAY_PER_REQUEST",
        )

        # DynamoDB: rate limits (sliding window)
        rate_limit_table = dynamodb.create_table(
            TableName="qurl-email-rate-limits",
            KeySchema=[
                {"AttributeName": "sender_email", "KeyType": "HASH"},
                {"AttributeName": "window_key", "KeyType": "RANGE"},
            ],
            AttributeDefinitions=[
                {"AttributeName": "sender_email", "AttributeType": "S"},
                {"AttributeName": "window_key", "AttributeType": "S"},
            ],
            BillingMode="PAY_PER_REQUEST",
        )

        # SSM parameters
        ssm = boto3.client("ssm", region_name="us-east-1")
        ssm.put_parameter(
            Name="/test/authorized-senders",
            Type="String",
            Value=json.dumps(["sender@example.com", "test@company.com"]),
        )
        ssm.put_parameter(
            Name="/test/qurl-api-key",
            Type="SecureString",
            Value="test-api-key",
        )
        ssm.put_parameter(
            Name="/test/forward-map",
            Type="String",
            Value=json.dumps({
                "justin@layerv.ai": "personal@icloud.com",
            }),
        )

        # SES (moto support is limited, just verify it can be created)
        ses = boto3.client("ses", region_name="us-east-1")

        yield {
            "s3": s3,
            "sqs": sqs,
            "dynamodb": dynamodb,
            "dispatch_table": dispatch_table,
            "rate_limit_table": rate_limit_table,
            "ssm": ssm,
            "ses": ses,
            "queue_url": queue_url,
            "dlq_url": dlq_url,
        }


@pytest.fixture
def mock_qurl_client():
    """Mock QURL client"""
    from services.qurl_client import UploadedResource, MintedLink

    mock = MagicMock()

    mock.upload_file.return_value = UploadedResource(
        resource_id="r_test123",
        filename="test.pdf",
        content_type="application/pdf",
        size=1024,
        hash="abc123",
    )

    mock.create_qurl.return_value = UploadedResource(
        resource_id="r_url456",
        filename="https://example.com",
        content_type="text/url",
        size=100,
        hash="def456",
    )

    mock.mint_link.return_value = MintedLink(
        link_id="l_xyz789",
        url="https://qurl.link/abc",
        hash="hash789",
        expires_at="2024-01-01T00:15:00Z",
    )

    return mock
