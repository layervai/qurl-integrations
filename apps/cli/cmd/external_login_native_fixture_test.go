//go:build unix || (windows && connectorpreview)

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	conformance "github.com/layervai/qurl-conformance"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
)

// Real client registration against local protocol fixtures, not a live service.
// Only routing, identity and expiry fields vary from the public golden replies.
func openNativeEnrollmentFixture(ctx context.Context, t *testing.T, cfg *connectorshare.NativeRuntimeConfig) (*connectorshare.NativeRuntime, error) {
	t.Helper()
	contract, err := conformance.AgentAssignmentGolden()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(time.Second)
	cellKey := nativeRecoveryFixtureKey(t, contract.Keys.AssignedCell.StaticPubHex)
	hub := newNativeRecoveryUDPServer(t, nativeRecoveryFixtureKey(t, contract.Keys.Hub.StaticPrivHex), nil, true,
		func(request []byte) ([]byte, error) {
			var incoming struct {
				AgentID  string                 `json:"devId"`
				UserData nativeRecoveryUserData `json:"usrData"`
			}
			if err := json.Unmarshal(request, &incoming); err != nil {
				return nil, err
			}
			if incoming.AgentID == "" || incoming.UserData.Query != "cell_assignment" || incoming.UserData.Mode != "enroll" || incoming.UserData.Credential != conformance.AgentAssignmentBootstrapCredentialFixture {
				return nil, errors.New("unexpected cold enrollment request")
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(contract.InitialAssignment.Result.BodyJSON), &body); err != nil {
				return nil, err
			}
			list := body["list"].(map[string]any)
			list["agent_id"] = incoming.AgentID
			list["assignment_ticket_expires_at"] = now.Add(15 * time.Minute).Format(time.RFC3339)
			if err := setNativeRecoveryAssignment(list["assignment"], 1, base64.StdEncoding.EncodeToString(cellKey), now); err != nil {
				return nil, err
			}
			return json.Marshal(body)
		})
	cell := newNativeRecoveryUDPServer(t, nativeRecoveryFixtureKey(t, contract.Keys.AssignedCell.StaticPrivHex), nil, false,
		func(request []byte) ([]byte, error) {
			var incoming struct {
				OTP      string `json:"otp"`
				UserData struct {
					Query     string `json:"query"`
					Ticket    string `json:"assignment_ticket"`
					DeviceKey string `json:"device_api_key"`
				} `json:"usrData"`
			}
			if err := json.Unmarshal(request, &incoming); err != nil {
				return nil, err
			}
			if incoming.OTP == conformance.AgentAssignmentBootstrapCredentialFixture && incoming.UserData.Ticket == "conformance-assignment-ticket-0001" {
				return []byte(contract.AssignedCellRegistration.Result.BodyJSON), nil
			}
			if incoming.UserData.Query == "agent_registration_completion" && incoming.UserData.DeviceKey != "" {
				return []byte(contract.RegistrationCompletion.Result.BodyJSON), nil
			}
			return nil, errors.New("unexpected cold registration request")
		})
	// These synthetic public addresses satisfy the production address checks;
	// the injected dialer maps them to loopback and rejects every other target.
	hubAddress, cellAddress := netip.MustParseAddr("8.8.4.4"), netip.MustParseAddr("9.9.9.9")
	route := nativeRecoveryRoute{
		hosts:   map[string]netip.Addr{connectorIntegrationHubHost: hubAddress, connectorIntegrationCellHost: cellAddress},
		targets: map[string]string{hubAddress.String(): hub.conn.LocalAddr().String(), cellAddress.String(): cell.conn.LocalAddr().String()},
	}
	cfg.Hub = qurl.HubBootstrap{Host: connectorIntegrationHubHost, Port: 443,
		ServerPublicKeyB64: base64.StdEncoding.EncodeToString(nativeRecoveryFixtureKey(t, contract.Keys.Hub.StaticPubHex))}
	cfg.UDPOptions = []qurl.AgentRuntimeUDPOption{qurl.WithAgentRuntimeUDPResolver(route), qurl.WithAgentRuntimeUDPDialer(route), qurl.WithAgentRuntimeUDPBounds(3*time.Second, 1)}
	runtime, err := connectorshare.OpenNativeRuntime(ctx, *cfg)
	if err != nil {
		return nil, err
	}
	if runtime.OpenKind != connectorshare.NativeOpenRegistration || len(hub.snapshot()) != 1 || len(cell.snapshot()) != 2 {
		_ = runtime.Close()
		t.Fatal("cold login did not complete Hub assignment, cell registration and completion")
	}
	return runtime, nil
}
