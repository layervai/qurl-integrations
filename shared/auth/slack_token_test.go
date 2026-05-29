package auth

import (
	"strings"
	"testing"
)

func TestValidateSlackBotTokenShape(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "empty allowed"},
		{name: "bot token", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")+10)},
		{name: "rotating bot token", token: "xoxe.xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxe.xoxb-"))},
		{name: "rotating refresh token", token: "xoxe-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxe-")), wantErr: true},
		{name: "wrong prefix", token: "abc-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("abc-")), wantErr: true},
		{name: "minimum length", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-"))},
		{name: "too short", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")-1), wantErr: true},
		{name: "space rejected", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")) + " bad", wantErr: true},
		{name: "underscore allowed", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")) + "_ok"},
		{name: "dot allowed", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")) + ".ok"},
		{name: "delete control rejected", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")) + string(rune(0x7f)), wantErr: true},
		{name: "non ascii rejected", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMin-len("xoxb-")) + "é", wantErr: true},
		{name: "maximum length", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMax-len("xoxb-"))},
		{name: "too long", token: "xoxb-" + strings.Repeat("a", SlackBotTokenTypoGuardMax-len("xoxb-")+1), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSlackBotTokenShape(tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSlackBotTokenShape(%q) err=%v, wantErr=%v", tc.token, err, tc.wantErr)
			}
		})
	}
}
