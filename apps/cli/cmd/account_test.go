package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"

	"github.com/layervai/qurl-go/qurl"
)

func TestAnonymousBootstrapNeedsNoAccountCredential(t *testing.T) {
	opts := &globalOpts{lookupEnv: func(string) (string, bool) { return "", false }}
	bootstrap := newRegisteredAccountBootstrap(opts, nil, "", nil)
	request := qurl.AgentEnrollmentCredentialRequest{AgentID: "anonymous-device", PublicKeyB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))}
	got, err := bootstrap.enrollmentCredential(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want, err := qurl.AnonymousEnrollmentCredential(context.Background(), request)
	if err != nil || got != want || bootstrap.client != nil {
		t.Fatal("anonymous enrollment unexpectedly needed an account client")
	}
}

func TestBrowserRecoveryCannotReturnEmptyCredential(t *testing.T) {
	account, err := qurlapi.New(&qurlapi.Config{BaseURL: "https://api.example.test", APIKey: "browser-token"})
	if err != nil {
		t.Fatal(err)
	}
	b := newRegisteredAccountBootstrap(&globalOpts{}, account, "", &qurlapi.Identity{OwnerID: "device:test"})
	key, err := b.recoveryCredential(context.Background())
	if key != "" || !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("recovery = %q, %v", key, err)
	}
}

func TestSelectAccountOwner(t *testing.T) {
	for _, tc := range []struct {
		owners          []string
		requested, want string
	}{
		{[]string{"auth0|one"}, "", "auth0|one"},
		{[]string{"auth0|one", "auth0|two"}, "", ""},
		{[]string{"auth0|one", "device:one"}, "", "device:one"},
		{[]string{"auth0|one", "device:one", "device:two"}, "", ""},
		{[]string{"auth0|one", "device:one", "device:two"}, "device:two", "device:two"},
		{[]string{"auth0|one"}, "device:foreign", ""},
	} {
		got, err := selectAccountOwner(tc.owners, tc.requested)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("select %v / %q = %q, %v", tc.owners, tc.requested, got, err)
		}
	}
}
