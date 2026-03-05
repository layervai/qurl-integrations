// Package formatting provides chat message templates for QURL notifications.
package formatting

import (
	"fmt"

	"github.com/layervai/qurl-integrations/shared/client"
)

// QURLCreated formats a message for a newly created QURL.
func QURLCreated(q *client.QURL) string {
	msg := fmt.Sprintf("QURL created: %s", q.LinkURL)
	if q.Title != "" {
		msg = fmt.Sprintf("QURL created: *%s*\n%s → %s", q.Title, q.LinkURL, q.TargetURL)
	}
	return msg
}

// QURLDetails formats a detailed view of a QURL.
func QURLDetails(q *client.QURL) string {
	msg := fmt.Sprintf("*%s*\nLink: %s\nTarget: %s\nClicks: %d",
		q.ShortCode, q.LinkURL, q.TargetURL, q.ClickCount)
	if q.Title != "" {
		msg = fmt.Sprintf("*%s* (`%s`)\nLink: %s\nTarget: %s\nClicks: %d",
			q.Title, q.ShortCode, q.LinkURL, q.TargetURL, q.ClickCount)
	}
	return msg
}
