package auth

import (
	"errors"
	"fmt"
	"strings"
)

// SlackBotTokenTypoGuardMin is a loose local lower bound for Slack bot tokens.
const SlackBotTokenTypoGuardMin = 30

// SlackBotTokenTypoGuardMax is a generous local upper bound for Slack bot tokens.
const SlackBotTokenTypoGuardMax = 1024

// ValidateSlackBotTokenShape catches obvious Slack bot-token typos before a
// malformed token is used for Slack Web API calls or persisted after OAuth.
func ValidateSlackBotTokenShape(token string) error {
	if token == "" {
		return nil
	}
	if len(token) < SlackBotTokenTypoGuardMin {
		return fmt.Errorf("slack bot token is shorter than %d characters", SlackBotTokenTypoGuardMin)
	}
	if len(token) > SlackBotTokenTypoGuardMax {
		return fmt.Errorf("slack bot token is longer than %d characters", SlackBotTokenTypoGuardMax)
	}
	if !validSlackBotTokenPrefix(token) {
		return errors.New("slack bot token must start with xoxb- or xoxe.xoxb-")
	}
	for i, b := range []byte(token) {
		if b >= '!' && b <= '~' {
			continue
		}
		return fmt.Errorf("slack bot token contains invalid characters near byte %d", i)
	}
	return nil
}

func validSlackBotTokenPrefix(token string) bool {
	return strings.HasPrefix(token, "xoxb-") ||
		strings.HasPrefix(token, "xoxe.xoxb-")
}
