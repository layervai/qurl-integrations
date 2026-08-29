package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestVerifyNativeSessionOwner(t *testing.T) {
	tests := []struct {
		name, expected, authenticated, wantErr              string
		identityEmpty, readFailure, readerMissing, wantRead bool
	}{
		{name: "authenticated match", expected: "owner-one", authenticated: "owner-one", wantRead: true},
		{name: "authenticated mismatch", expected: "owner-one", authenticated: "owner-two", wantErr: "does not match authenticated owner", wantRead: true},
		{name: "empty response", expected: "owner-one", identityEmpty: true, wantErr: "identity response is empty", wantRead: true},
		{name: "read failure", expected: "owner-one", readFailure: true, wantErr: "read identity", wantRead: true},
		{name: "reader unavailable", expected: "owner-one", readerMissing: true, wantErr: "reader is unavailable"},
		{name: "no session authority", expected: " ", authenticated: "owner-two", wantErr: "owner authority is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := false
			var readIdentity nativeRegisteredIdentityReader
			if !tt.readerMissing {
				readIdentity = func(context.Context, *connectorshare.NativeRuntime, string, string) (*qurlapi.Identity, error) {
					read = true
					if tt.readFailure {
						return nil, errors.New("read identity")
					}
					if tt.identityEmpty {
						return &qurlapi.Identity{}, nil
					}
					return &qurlapi.Identity{OwnerID: tt.authenticated}, nil
				}
			}
			err := verifyNativeSessionOwner(context.Background(), nil, "https://api.example.test", "test", tt.expected, readIdentity)
			if read != tt.wantRead {
				t.Fatalf("identity read=%t, want %t", read, tt.wantRead)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyNativeSessionOwner() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyNativeSessionOwner() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestHeadlessOwnerVerificationFailuresArePermanent(t *testing.T) {
	if !isPermanentHeadlessNativeOpenError(fmt.Errorf("outer: %w", errNativeSessionOwnerVerification)) {
		t.Fatal("owner verification rejection was not classified as permanent")
	}
	if isPermanentHeadlessNativeOpenError(errors.New("temporary network failure")) {
		t.Fatal("temporary network failure was classified as permanent")
	}
}
