package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

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
