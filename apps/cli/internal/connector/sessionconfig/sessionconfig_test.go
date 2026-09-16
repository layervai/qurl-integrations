package sessionconfig

import (
	"errors"
	"testing"
)

func TestResolveNativeSessionConfig(t *testing.T) {
	authority, err := Resolve(" owner-one ")
	if err != nil || authority.OwnerID != "owner-one" {
		t.Fatalf("valid authority = %#v, %v", authority, err)
	}
	if got, err := Resolve(" "); got.OwnerID != "" {
		t.Fatalf("empty owner returned authority %#v", got)
	} else if !errors.Is(err, ErrConfig) {
		t.Fatalf("empty owner error = %v", err)
	}
}
