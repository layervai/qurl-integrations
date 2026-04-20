"""
DynamoDB module tests.
"""

from unittest.mock import MagicMock, patch

from db import (
    DispatchStatus,
    DB,
)


class TestDispatchStatus:
    """Dispatch status tests"""

    def test_status_values(self):
        """Test status values"""
        assert DispatchStatus.SENT == "sent"
        assert DispatchStatus.MINT_FAILED == "mint_failed"
        assert DispatchStatus.SEND_FAILED == "send_failed"
        assert DispatchStatus.SKIPPED == "skipped"


class TestDBLogDispatch:
    """DB log dispatch tests"""

    def test_log_dispatch_success(self):
        """Test successful dispatch logging"""
        with patch("db.boto3") as mock_boto3:
            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            db = DB("test-table")
            dispatch_id = db.log_dispatch(
                resource_id="r_123",
                sender_email="sender@example.com",
                recipient_email="recipient@example.com",
                status=DispatchStatus.SENT,
                link_id_hash="hash123",
            )

            assert dispatch_id is not None
            assert mock_table.put_item.called

    def test_log_dispatch_with_error(self):
        """Test logging with error"""
        with patch("db.boto3") as mock_boto3:
            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            db = DB("test-table")
            dispatch_id = db.log_dispatch(
                resource_id="r_123",
                sender_email="sender@example.com",
                recipient_email="recipient@example.com",
                status=DispatchStatus.SEND_FAILED,
                error="Connection timeout",
            )

            assert dispatch_id is not None

    def test_log_dispatch_skipped_status(self):
        """Test logging skipped dispatch (idempotency)"""
        with patch("db.boto3") as mock_boto3:
            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            db = DB("test-table")
            dispatch_id = db.log_dispatch(
                resource_id="r_456",
                sender_email="sender@example.com",
                recipient_email="recipient@example.com",
                status=DispatchStatus.SKIPPED,
            )

            assert dispatch_id is not None
            assert mock_table.put_item.called
            # Verify completed_at is None for SKIPPED
            call_args = mock_table.put_item.call_args
            item = call_args.kwargs.get("Item") or call_args[1].get("Item")
            assert item["status"] == DispatchStatus.SKIPPED
            assert item["completed_at"] is None


class TestCheckIdempotent:
    """Idempotency check tests"""

    def test_idempotent_check_no_existing(self):
        """Test no existing record"""
        with patch("db.boto3") as mock_boto3:
            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            mock_table.query.return_value = {"Items": []}

            db = DB("test-table")
            result = db.check_idempotent("r_123", "recipient@example.com")

            assert result is False

    def test_idempotent_check_existing(self):
        """Test existing sent record"""
        with patch("db.boto3") as mock_boto3:
            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            mock_table.query.return_value = {
                "Items": [{
                    "resource_id": "r_123",
                    "recipient_email": "recipient@example.com",
                    "status": "sent",
                }]
            }

            db = DB("test-table")
            result = db.check_idempotent("r_123", "recipient@example.com")

            assert result is True

    def test_idempotent_check_failure(self):
        """Test idempotency check failure (default to not skip)"""
        with patch("db.boto3") as mock_boto3:
            from botocore.exceptions import ClientError

            mock_dynamodb = MagicMock()
            mock_table = MagicMock()
            mock_boto3.resource.return_value = mock_dynamodb
            mock_dynamodb.Table.return_value = mock_table

            mock_table.query.side_effect = ClientError(
                {"Error": {"Code": "ProvisionedThroughputExceededException"}},
                "Query"
            )

            db = DB("test-table")
            result = db.check_idempotent("r_123", "recipient@example.com")

            assert result is False
