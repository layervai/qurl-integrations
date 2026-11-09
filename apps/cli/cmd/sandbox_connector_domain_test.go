//go:build clisandbox && (linux || darwin)

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

const sandboxConnectorProofDomain = "connector-domain-ci-proof.layerv.xyz"

func TestSandboxConnectorCustomDomain(t *testing.T) {
	if os.Getenv("QURL_CLI_SANDBOX_CONNECTOR_DOMAIN") != "enabled" {
		t.Skip("Sandbox Connector domain test is disarmed")
	}
	fixture := startSandboxLocalPublish(t, "smoke")
	defer fixture.interruptAndValidate(t)
	endpoint := fixture.env["QURL_ENDPOINT"] + "/v1/resources/" + url.PathEscape(fixture.local.ResourceID)
	body, err := json.Marshal(map[string]string{"custom_domain": sandboxConnectorProofDomain})
	if err != nil {
		t.Fatal("encode Sandbox Connector domain update")
	}
	update, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal("build Sandbox Connector domain update")
	}
	update.Header.Set("Authorization", "Bearer "+fixture.key)
	update.Header.Set("Content-Type", "application/json")
	updated, err := sandboxCleanupHTTPClient.Do(update)
	if err != nil {
		t.Fatal("send Sandbox Connector domain update")
	}
	_ = updated.Body.Close()
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("Sandbox Connector domain update returned HTTP %d", updated.StatusCode)
	}
	assertSandboxLocalRoute(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID, fixture.marker, 2*time.Minute)
	if fixture.customDomainHits.Load() == 0 {
		t.Fatal("the private origin did not receive the verified custom hostname")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+sandboxConnectorProofDomain+"/", http.NoBody)
	if err != nil {
		t.Fatal("build Sandbox request without admission")
	}
	response, err := sandboxCleanupHTTPClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound {
			t.Fatalf("Sandbox request without admission returned HTTP %d", response.StatusCode)
		}
	}
	removed := runSandboxLocalCLI(t, fixture.binary, fixture.env, fixture.stateDir, "delete", fixture.local.CRID, "--yes")
	if removed.code != 0 {
		t.Fatalf("Sandbox Connector delete returned exit %d", removed.code)
	}
	assertSandboxFailureRemoteDeleted(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID)
}
