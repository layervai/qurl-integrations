// Package formatting provides chat message templates for QURL notifications.
package formatting

import (
	"fmt"

	"github.com/layervai/qurl-integrations/shared/client"
)

// QURLCreated formats a message for a newly created QURL.
func QURLCreated(q *client.QURL) string {
	if q.Title != "" {
		return fmt.Sprintf("QURL created: *%s*\n%s → %s", q.Title, q.LinkURL, q.TargetURL)
	}
	return "QURL created: " + q.LinkURL
}

// QURLDetails formats a detailed view of a QURL.
func QURLDetails(q *client.QURL) string {
	if q.Title != "" {
		return fmt.Sprintf("*%s* (`%s`)\nLink: %s\nTarget: %s\nClicks: %d",
			q.Title, q.ShortCode, q.LinkURL, q.TargetURL, q.ClickCount)
	}
	return fmt.Sprintf("*%s*\nLink: %s\nTarget: %s\nClicks: %d",
		q.ShortCode, q.LinkURL, q.TargetURL, q.ClickCount)
}
