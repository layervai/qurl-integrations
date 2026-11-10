//go:build clisandbox && (linux || darwin)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
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
	grantFor := func(duration string) (*sandboxGrantedRoute, string) {
		shared := runSandboxLocalCLI(t, fixture.binary, fixture.env, fixture.stateDir, "--quiet", "share", fixture.local.CRID, "--session-duration", duration)
		if shared.code != 0 {
			t.Fatalf("Sandbox Connector share returned exit %d", shared.code)
		}
		link := strings.TrimSpace(shared.stdout.String())
		return prepareSandboxGrantedRoute(t, fixture.env, link, fixture.marker, fixture.backendHits.Load, sandboxConnectorProofDomain), link
	}
	shortGrant, shortLink := grantFor("30s")
	t.Log("The exact custom hostname returned the private origin bytes")
	browser := newSandboxGrantedRouteHTTPClient()
	defer browser.CloseIdleConnections()
	browser.CheckRedirect = sandboxCleanupHTTPClient.CheckRedirect
	cookie := installSandboxConnectorBrowserCookie(t, browser, shortGrant, shortLink)
	beforeDenial := fixture.backendHits.Load()
	// Use the same direct network path without the installed cookie.
	denialCtx, cancelDenial := context.WithTimeout(t.Context(), sandboxDeletedRouteProbeTimeout)
	defer cancelDenial()
	request, err := http.NewRequestWithContext(denialCtx, http.MethodGet, shortGrant.url, http.NoBody)
	if err != nil {
		t.Fatal("build Sandbox request without admission")
	}
	response, err := browser.Do(request)
	if err != nil {
		var dnsError *net.DNSError
		var networkError net.Error
		// The edge can deny access before HTTP. A DNS or certificate error is a failed fixture.
		edgeClosed := errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) ||
			(errors.As(err, &networkError) && networkError.Timeout())
		if errors.As(err, &dnsError) || !edgeClosed {
			t.Fatalf("Sandbox denial probe failed before a valid edge denial (%T)", err)
		}
		t.Log("Sandbox denied the request without an HTTP response")
	} else {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if readErr != nil || bytes.Contains(payload, []byte(fixture.marker)) {
			t.Fatalf("Sandbox denied-browser probe did not withhold the payload (HTTP %d)", response.StatusCode)
		}
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound {
			t.Fatalf("Sandbox request without admission returned HTTP %d", response.StatusCode)
		}
	}
	if fixture.backendHits.Load() != beforeDenial {
		t.Fatal("the browser without a cookie reached the Sandbox origin")
	}
	beforeReload := fixture.backendHits.Load()
	reloadCtx, cancelReload := context.WithTimeout(t.Context(), sandboxDeletedRouteProbeTimeout)
	defer cancelReload()
	reload := request.Clone(reloadCtx)
	reload.AddCookie(cookie)
	response, err = browser.Do(reload)
	if err != nil {
		t.Fatal("Sandbox reload with the installed cookie failed before an HTTP response")
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(fixture.marker)+1)))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(payload) != fixture.marker ||
		fixture.backendHits.Load() <= beforeReload {
		t.Fatalf("Sandbox reload with the installed cookie did not reach the private origin (HTTP %d)", response.StatusCode)
	}
	t.Log("Browser cookie handoff allowed the admitted browser and denied a browser on the same IP")
	// The server reports the network lease, which includes 10 seconds for first arrival.
	untilExpired := time.Until(shortGrant.expiresAt.Add(5 * time.Second))
	if untilExpired > 45*time.Second {
		t.Fatal("Sandbox grant exceeded the 30-second session and 10-second arrival window")
	}
	select {
	case <-time.After(untilExpired):
	case <-t.Context().Done():
		t.Fatal("Sandbox expiry check was canceled")
	}
	beforeExpiryProbe := fixture.backendHits.Load()
	for range 2 {
		probeCtx, cancelProbe := context.WithTimeout(t.Context(), sandboxDeletedRouteProbeTimeout)
		state, probeErr := shortGrant.probe(probeCtx)
		cancelProbe()
		state, failed, unexpectedSuccess := classifySandboxGrantedRouteProbe(state, probeErr, t.Context().Err())
		if failed || unexpectedSuccess || state != sandboxGrantedRouteRefused || fixture.backendHits.Load() != beforeExpiryProbe {
			t.Fatal("Sandbox did not deny expired custom-domain access without reaching the private origin")
		}
	}
	retainedGrant, _ := grantFor("2m")
	removed := runSandboxLocalCLI(t, fixture.binary, fixture.env, fixture.stateDir, "delete", fixture.local.CRID, "--yes")
	if removed.code != 0 {
		t.Fatalf("Sandbox Connector delete returned exit %d", removed.code)
	}
	assertSandboxGrantedRouteFenced(t, retainedGrant)
	assertSandboxFailureRemoteDeleted(t, fixture.binary, fixture.env, fixture.stateDir, fixture.local.CRID)
}

// TODO(upstream-contract): Keep the form and cookie checks aligned with qurl-service's visitor-session smoke.
func installSandboxConnectorBrowserCookie(t *testing.T, browser *http.Client, route *sandboxGrantedRoute, link string) *http.Cookie {
	t.Helper()
	// Match the browser's form POST from qurl.link to the granted origin.
	portal, err := url.Parse(link)
	if err != nil || portal.Scheme != "https" || portal.Host == "" {
		t.Fatal("Sandbox share did not identify its HTTPS portal")
	}
	content, err := http.NewRequestWithContext(t.Context(), http.MethodGet, route.url, http.NoBody)
	if err != nil || route.authorize(content) != nil {
		t.Fatal("Sandbox grant could not authorize its exact HTTPS origin")
	}
	bearer, err := content.Cookie("qurl_vsession")
	if err != nil {
		t.Fatal("Sandbox grant omitted the browser session cookie")
	}
	bootstrapURL := content.URL.Scheme + "://" + content.URL.Host + "/.well-known/qurl/session"
	form := url.Values{"auth_provider_token": {bearer.Value}, "return_to": {content.URL.RequestURI()}}
	bootstrapCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	bootstrap, err := http.NewRequestWithContext(bootstrapCtx, http.MethodPost, bootstrapURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal("build Sandbox browser cookie handoff")
	}
	bootstrap.Header.Set("Origin", portal.Scheme+"://"+portal.Host)
	bootstrap.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := browser.Do(bootstrap)
	if err != nil {
		t.Fatal("Sandbox browser cookie handoff failed before an HTTP response")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != content.URL.RequestURI() {
		t.Fatalf("Sandbox browser cookie handoff returned HTTP %d or the wrong location", response.StatusCode)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == bearer.Name && cookie.Value == bearer.Value && cookie.Domain == "" &&
			cookie.Path == "/" && cookie.HttpOnly && cookie.Secure && cookie.MaxAge > 0 &&
			cookie.SameSite == http.SameSiteLaxMode {
			return cookie
		}
	}
	t.Fatal("Sandbox browser cookie handoff omitted the secure host-only cookie")
	return nil
}
