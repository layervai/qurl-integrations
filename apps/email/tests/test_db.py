"""
DynamoDB module tests.
"""

import pytest
from botocore.exceptions import ClientError

from db import DB, DispatchStatus, DynamoDBError, get_db, log_dispatch, check_idempotent
from config import Settings


def _make_settings(aws_region="us-east-1"):
    return Settings(
        bot_address="qurl@layerv.ai",
        max_recipients=25,
        max_urls_per_email=3,
        max_attachment_size_mb=25,
        authorized_senders_param="/test/authorized-senders",
        qurl_api_key_param="/test/qurl-api-key",
        dispatch_table="qurl-email-dispatch-log",
        aws_region=aws_region,
    )


def _table_name(table_obj):
    return table_obj.name if hasattr(table_obj, "name") else table_obj.table_name


class TestDispatchStatus:
    """Dispatch status constants"""

    def test_status_values(self):
        assert DispatchStatus.SENT == "sent"
        assert DispatchStatus.MINT_FAILED == "mint_failed"
        assert DispatchStatus.SEND_FAILED == "send_failed"
        assert DispatchStatus.SKIPPED == "skipped"


class TestDBLogDispatch:
    """DB log_dispatch tests"""

    def test_log_dispatch_success(self, aws_services):
        """Test successful dispatch logging"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        dispatch_id = db.log_dispatch(
            resource_id="r_123",
            sender_email="sender@example.com",
            recipient_email="recipient@example.com",
            status=DispatchStatus.SENT,
            link_id_hash="hash123",
        )
        assert dispatch_id is not None
        # Verify the item was actually stored in moto DynamoDB
        item = db.table.get_item(
            Key={"resource_id": "r_123", "dispatch_id": dispatch_id}
        )["Item"]
        assert item["status"] == DispatchStatus.SENT
        assert item["link_id_hash"] == "hash123"

    def test_log_dispatch_with_error(self, aws_services):
        """Test logging with error field"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        dispatch_id = db.log_dispatch(
            resource_id="r_err",
            sender_email="sender@example.com",
            recipient_email="recipient@example.com",
            status=DispatchStatus.SEND_FAILED,
            error="Connection timeout",
        )
        assert dispatch_id is not None

    def test_log_dispatch_skipped_status(self, aws_services):
        """Test logging skipped dispatch has completed_at=None"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        dispatch_id = db.log_dispatch(
            resource_id="r_skip",
            sender_email="sender@example.com",
            recipient_email="recipient@example.com",
            status=DispatchStatus.SKIPPED,
        )
        assert dispatch_id is not None
        item = db.table.get_item(
            Key={"resource_id": "r_skip", "dispatch_id": dispatch_id}
        )["Item"]
        assert item["status"] == DispatchStatus.SKIPPED
        assert item.get("completed_at") is None

    def test_log_dispatch_client_error(self, aws_services):
        """Test log_dispatch raises DynamoDBError on ClientError"""
        dispatch_table = aws_services["dispatch_table"]
        # Force a conditional check failure by patching
        db = DB(_table_name(dispatch_table))
        original_put = db.table.put_item

        def bad_put(**kw):
            raise ClientError(
                {"Error": {"Code": "ProvisionedThroughputExceededException"}},
                "PutItem"
            )

        db.table.put_item = bad_put
        with pytest.raises(DynamoDBError):
            db.log_dispatch(
                resource_id="r_123",
                sender_email="sender@example.com",
                recipient_email="recipient@example.com",
                status=DispatchStatus.SENT,
            )


class TestDBCheckIdempotent:
    """DB check_idempotent tests"""

    def test_idempotent_no_existing_record(self, aws_services):
        """Test returns False when no existing sent record"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        result = db.check_idempotent("brand_new_r", "recipient@example.com")
        assert result is False

    def test_idempotent_existing_sent(self, aws_services):
        """Test returns True when a sent record already exists"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        # First log a sent dispatch
        db.log_dispatch(
            resource_id="r_abc",
            sender_email="sender@example.com",
            recipient_email="recipient@example.com",
            status=DispatchStatus.SENT,
        )
        # Then check idempotency
        result = db.check_idempotent("r_abc", "recipient@example.com")
        assert result is True

    def test_idempotent_different_recipient(self, aws_services):
        """Test returns False for different recipient on same resource"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        db.log_dispatch(
            resource_id="r_abc",
            sender_email="sender@example.com",
            recipient_email="recipient1@example.com",
            status=DispatchStatus.SENT,
        )
        result = db.check_idempotent("r_abc", "recipient2@example.com")
        assert result is False

    def test_idempotent_failed_status_not_idempotent(self, aws_services):
        """Test failed status does not block retry"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        db.log_dispatch(
            resource_id="r_fail",
            sender_email="sender@example.com",
            recipient_email="recipient@example.com",
            status=DispatchStatus.SEND_FAILED,
        )
        result = db.check_idempotent("r_fail", "recipient@example.com")
        assert result is False

    def test_idempotent_client_error_fails_open(self, aws_services):
        """Test DynamoDB error returns False (fail open)"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))

        def bad_query(**kw):
            raise ClientError(
                {"Error": {"Code": "ProvisionedThroughputExceededException"}},
                "Query"
            )

        original_query = db.table.query
        db.table.query = bad_query
        try:
            result = db.check_idempotent("r_123", "recipient@example.com")
            assert result is False
        finally:
            db.table.query = original_query


class TestDBGetDispatchHistory:
    """DB get_dispatch_history tests"""

    def test_get_dispatch_history_success(self, aws_services):
        """Test successful dispatch history retrieval"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        db.log_dispatch(
            resource_id="r_hist",
            sender_email="alice@example.com",
            recipient_email="bob@example.com",
            status=DispatchStatus.SENT,
            link_id_hash="hash_xyz",
        )
        records = db.get_dispatch_history("alice@example.com")
        assert len(records) >= 1
        sent = [r for r in records if r.resource_id == "r_hist"][0]
        assert sent.recipient_email == "bob@example.com"
        assert sent.link_id_hash == "hash_xyz"

    def test_get_dispatch_history_empty(self, aws_services):
        """Test dispatch history with no records"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        records = db.get_dispatch_history("nobody@example.com")
        assert len(records) == 0

    def test_get_dispatch_history_respects_limit(self, aws_services):
        """Test dispatch history respects limit parameter"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        for i in range(3):
            db.log_dispatch(
                resource_id=f"r_{i}",
                sender_email="alice@example.com",
                recipient_email=f"r{i}@example.com",
                status=DispatchStatus.SENT,
            )
        records = db.get_dispatch_history("alice@example.com", limit=2)
        assert len(records) == 2

    def test_get_dispatch_history_client_error(self, aws_services):
        """Test dispatch history returns empty list on ClientError"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))

        def bad_query(**kw):
            raise ClientError(
                {"Error": {"Code": "ResourceNotFoundException"}},
                "Query"
            )

        original_query = db.table.query
        db.table.query = bad_query
        try:
            records = db.get_dispatch_history("alice@example.com")
            assert records == []
        finally:
            db.table.query = original_query


class TestGetDb:
    """get_db convenience function tests"""

    def test_get_db_cached(self, aws_services):
        """Test get_db returns same instance on repeated calls"""
        import db as _db
        _db._db_instance = None
        settings = _make_settings()
        instance1 = get_db(settings)
        instance2 = get_db(settings)
        assert instance1 is instance2

    def test_get_db_uses_explicit_settings(self, aws_services):
        """Test get_db uses provided settings"""
        import db as _db
        _db._db_instance = None
        settings = _make_settings()
        instance = get_db(settings)
        assert instance.table_name == "qurl-email-dispatch-log"

    def test_get_db_falls_back_to_get_settings(self, aws_services):
        """Test get_db calls get_settings when settings is None"""
        import db as _db
        _db._db_instance = None
        settings = _make_settings()
        with pytest.MonkeyPatch.context() as mp:
            import config as _cfg
            original = _cfg.get_settings
            _cfg.get_settings = lambda: settings
            try:
                instance = get_db(None)
                assert instance.table_name == "qurl-email-dispatch-log"
            finally:
                _cfg.get_settings = original


class TestDbConvenienceFunctions:
    """Convenience function tests"""

    def test_log_dispatch_convenience(self, aws_services):
        """Test log_dispatch convenience function"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        dispatch_id = log_dispatch(
            resource_id="r_conv",
            sender_email="alice@example.com",
            recipient_email="bob@example.com",
            status=DispatchStatus.SENT,
            db=db,
        )
        assert dispatch_id is not None

    def test_check_idempotent_convenience_no_record(self, aws_services):
        """Test check_idempotent convenience function"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        result = check_idempotent("r_conv2", "bob@example.com", db=db)
        assert result is False

    def test_check_idempotent_convenience_has_record(self, aws_services):
        """Test check_idempotent convenience function finds existing record"""
        dispatch_table = aws_services["dispatch_table"]
        db = DB(_table_name(dispatch_table))
        db.log_dispatch(
            resource_id="r_conv3",
            sender_email="alice@example.com",
            recipient_email="bob@example.com",
            status=DispatchStatus.SENT,
        )
        result = check_idempotent("r_conv3", "bob@example.com", db=db)
        assert result is True
