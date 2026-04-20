"""
DynamoDB module tests.
"""

import pytest
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


class TestDBLogDispatchErrors:
    """DB log_dispatch error path tests"""

    def test_log_dispatch_client_error(self):
        """Test log_dispatch raises DynamoDBError on ClientError"""
        from db import DynamoDBError
        from botocore.exceptions import ClientError

        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.put_item.side_effect = ClientError(
                {"Error": {"Code": "ProvisionedThroughputExceededException"}},
                "PutItem"
            )

            db = DB("test-table")
            with pytest.raises(DynamoDBError):
                db.log_dispatch(
                    resource_id="r_123",
                    sender_email="sender@example.com",
                    recipient_email="recipient@example.com",
                    status=DispatchStatus.SENT,
                )


class TestDBGetDispatchHistory:
    """DB get_dispatch_history tests"""

    def test_get_dispatch_history_success(self):
        """Test successful dispatch history retrieval"""
        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.query.return_value = {
                "Items": [
                    {
                        "resource_id": "r_abc",
                        "dispatch_id": "d_001",
                        "sender_email": "alice@example.com",
                        "recipient_email": "bob@example.com",
                        "status": "sent",
                        "link_id_hash": "hash123",
                        "error": None,
                        "created_at": "2024-01-01T00:00:00Z",
                        "completed_at": "2024-01-01T00:00:01Z",
                        "expires_at": 1704931200,
                    }
                ]
            }

            db = DB("test-table")
            records = db.get_dispatch_history("alice@example.com")

            assert len(records) == 1
            assert records[0].resource_id == "r_abc"
            assert records[0].status == "sent"
            assert records[0].link_id_hash == "hash123"

    def test_get_dispatch_history_empty(self):
        """Test dispatch history with no records"""
        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.query.return_value = {"Items": []}

            db = DB("test-table")
            records = db.get_dispatch_history("nobody@example.com")
            assert len(records) == 0

    def test_get_dispatch_history_client_error(self):
        """Test dispatch history returns empty on ClientError"""
        from botocore.exceptions import ClientError

        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.query.side_effect = ClientError(
                {"Error": {"Code": "ResourceNotFoundException"}},
                "Query"
            )

            db = DB("test-table")
            records = db.get_dispatch_history("alice@example.com")
            assert records == []

    def test_get_dispatch_history_with_limit(self):
        """Test dispatch history respects limit parameter"""
        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.query.return_value = {"Items": []}

            db = DB("test-table")
            db.get_dispatch_history("alice@example.com", limit=10)

            call_kwargs = mock_table.query.call_args[1]
            assert call_kwargs["Limit"] == 10
            assert call_kwargs["ScanIndexForward"] is False


class TestGetDb:
    """get_db convenience function tests"""

    def test_get_db_cached(self):
        """Test get_db returns cached instance"""
        import db as db_module

        with patch("db.boto3"):
            db_module._db_instance = None
            mock_settings = MagicMock()
            mock_settings.dispatch_table = "test-table"
            mock_settings.aws_region = "us-east-1"

            with patch("db.get_settings", return_value=mock_settings):
                instance1 = db_module.get_db()
                instance2 = db_module.get_db()
                assert instance1 is instance2

    def test_get_db_uses_explicit_settings(self):
        """Test get_db uses provided settings instead of get_settings"""
        import db as db_module

        with patch("db.boto3"):
            db_module._db_instance = None
            mock_settings = MagicMock()
            mock_settings.dispatch_table = "custom-table"
            mock_settings.aws_region = "eu-west-1"

            instance = db_module.get_db(mock_settings)
            assert instance.table_name == "custom-table"

    def test_get_db_falls_back_to_get_settings(self):
        """Test get_db calls get_settings when settings is None"""
        import db as db_module

        with patch("db.boto3"):
            db_module._db_instance = None
            mock_settings = MagicMock()
            mock_settings.dispatch_table = "from-defaults"
            mock_settings.aws_region = "us-east-1"

            with patch("db.get_settings", return_value=mock_settings):
                instance = db_module.get_db(None)
                assert instance.table_name == "from-defaults"


class TestDbConvenienceFunctions:
    """Convenience function tests"""

    def test_log_dispatch_convenience_uses_existing_db(self):
        """Test log_dispatch convenience function uses provided DB instance"""
        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table

            mock_db = DB("explicit-table")
            mock_db.table = mock_table

            from db import log_dispatch
            dispatch_id = log_dispatch(
                resource_id="r_conv",
                sender_email="alice@example.com",
                recipient_email="bob@example.com",
                status=DispatchStatus.SENT,
                db=mock_db,
            )

            assert dispatch_id is not None
            # Should use the provided db instance, not call get_db
            mock_table.put_item.assert_called_once()

    def test_check_idempotent_convenience_uses_existing_db(self):
        """Test check_idempotent convenience function uses provided DB instance"""
        with patch("db.boto3") as mock_boto3:
            mock_table = MagicMock()
            mock_boto3.resource.return_value.Table.return_value = mock_table
            mock_table.query.return_value = {"Items": []}

            mock_db = DB("explicit-table")
            mock_db.table = mock_table

            from db import check_idempotent
            result = check_idempotent(
                resource_id="r_conv",
                recipient_email="bob@example.com",
                db=mock_db,
            )

            assert result is False
            mock_table.query.assert_called_once()


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
