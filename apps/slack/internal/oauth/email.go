package oauth

import (
	"errors"
	"net/mail"
	"strings"
)

const maxSetupEmailBytes = 254

var errEmailInvalid = errors.New("email: invalid")

// NormalizeEmail returns the canonical email form stored in setup state.
// The setup command accepts only a bare address, not display-name syntax, so
// Slack text like `Alice <alice@example.com>` cannot silently bind a different
// address than the user saw in the command.
func NormalizeEmail(raw string) (string, error) {
	email := strings.TrimSpace(raw)
	if email == "" || len(email) > maxSetupEmailBytes {
		return "", errEmailInvalid
	}
	if strings.ContainsAny(email, " \t\r\n<>") || strings.ContainsRune(email, stateSeparatorRune) {
		return "", errEmailInvalid
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", errEmailInvalid
	}
	if addr.Name != "" || addr.Address != email {
		return "", errEmailInvalid
	}
	parts := strings.Split(addr.Address, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errEmailInvalid
	}
	return strings.ToLower(addr.Address), nil
}
