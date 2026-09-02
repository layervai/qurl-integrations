package internal

import "strings"

// sanitizeLogValue neutralises CR/LF in user- or API-supplied text before it
// reaches a log line (go/log-injection).
func sanitizeLogValue(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
