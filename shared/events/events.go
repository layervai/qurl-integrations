// Package events provides webhook event parsing for QURL integrations.
package events

import "time"

// Type identifies a QURL webhook event type.
type Type string

const (
	TypeQURLCreated Type = "qurl.created"
	TypeQURLClicked Type = "qurl.clicked"
	TypeQURLExpired Type = "qurl.expired"
	TypeQURLDeleted Type = "qurl.deleted"
)

// Event is a QURL webhook event.
type Event struct {
	Type      Type      `json:"type"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}
