"""Error types for the QURL API client."""

from __future__ import annotations


class QURLError(Exception):
    """Error raised by the QURL API client."""

    def __init__(
        self,
        *,
        status: int,
        code: str,
        title: str,
        detail: str,
        invalid_fields: dict[str, str] | None = None,
        request_id: str | None = None,
        retry_after: int | None = None,
    ) -> None:
        super().__init__(f"{title} ({status}): {detail}")
        self.status = status
        self.code = code
        self.title = title
        self.detail = detail
        self.invalid_fields = invalid_fields
        self.request_id = request_id
        self.retry_after = retry_after
