package internal

import "testing"

func TestConnectorActivityURLPreservesTrustedServicePath(t *testing.T) {
	got, err := connectorActivityURL("https://smba.trafficmanager.net/amer/", "conversation:1")
	if err != nil {
		t.Fatalf("connectorActivityURL() error = %v", err)
	}

	want := "https://smba.trafficmanager.net/amer/v3/conversations/conversation%3A1/activities"
	if got != want {
		t.Fatalf("connectorActivityURL() = %q, want %q", got, want)
	}
}

func TestConnectorActivityURLRejectsUntrustedHost(t *testing.T) {
	_, err := connectorActivityURL("https://example.invalid/amer/", "conversation:1")
	if err == nil {
		t.Fatal("connectorActivityURL() error = nil, want host validation failure")
	}
}
