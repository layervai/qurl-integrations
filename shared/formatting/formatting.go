// Package formatting provides chat message templates for QURL notifications.
package formatting

import (
	"fmt"

	"github.com/layervai/qurl-integrations/shared/client"
)

// QURLCreated formats a message for a newly created QURL.
func QURLCreated(q *client.CreateOutput) string {
	return "QURL created: " + q.QURLLink
}

// QURLDetails formats a detailed view of a QURL.
func QURLDetails(q *client.QURL) string {
	return fmt.Sprintf("*%s*\nTarget: %s\nStatus: %s",
		q.ResourceID, q.TargetURL, q.Status)
}
