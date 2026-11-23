// Package slacklog formats Slack identifiers for structured logs.
package slacklog

import "fmt"

const maxIDBytes = 64

// Preserve the existing Web API log marker for every Slack ID kind.
const invalidIDFormat = "invalid-slack-owner-id-len-%d"

// ID keeps bounded uppercase alphanumeric identifiers readable for triage.
// It is neither an authorization check nor anonymization. Invalid inputs reveal only length.
func ID(id string) string {
	if id == "" {
		return ""
	}
	// TODO(upstream-contract): if Slack broadens the ID alphabet, update this
	// allowlist and the sanitizer tests in lockstep.
	if len(id) > maxIDBytes {
		return fmt.Sprintf(invalidIDFormat, len(id))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Sprintf(invalidIDFormat, len(id))
		}
	}
	return id
}

// IDs formats a group without changing the caller's identities.
func IDs(ids []string) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = ID(id)
	}
	return result
}
