package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestWhoamiLocalReadsOnlyPublicIdentityWithoutNetworkOrWrites(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	stateDir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := connectoragentstate.NewSDKStore(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	state := bootstrapRegisteredState(t)
	sdk, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.SaveAgentState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(stateDir, connectorstate.AgentStateFile)
	before, err := os.ReadFile(file) // #nosec G304 -- fixed agent-state filename inside test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "whoami", "--local", "-o", "json"}, shareStateDir: stateDir})
	if res.code != 0 {
		t.Fatalf("local identity failed: %s", res.stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(res.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["recovery_pending"] != false || got["recovery_issue_pending"] != false || got["agent_id"] != state.AgentID || got["device_key_id"] != state.DeviceAPIKeyID {
		t.Fatalf("unexpected identity: %v", got)
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("local identity contacted the API")
	}
	after, err := os.ReadFile(file) // #nosec G304 -- fixed agent-state filename inside test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	afterStat, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !stat.ModTime().Equal(afterStat.ModTime()) {
		t.Fatal("local identity rewrote state")
	}
	for _, secret := range []string{state.DeviceAPIKey, state.PrivateKeyB64} {
		if strings.Contains(res.stdout.String()+res.stderr.String(), secret) {
			t.Fatal("identity output disclosed a secret")
		}
	}
}

func TestWhoamiLocalDoesNotCreateAnAbsentNamespace(t *testing.T) {
	dir := filepath.Join(connectorStateTestDir(t), "absent")
	res := runCLI(t, &runOpts{args: []string{"whoami", "--local", "-o", "json"}, shareStateDir: dir})
	if res.code == 0 {
		t.Fatal("absent identity accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("absent namespace was modified: %v", err)
	}
}

func TestWhoamiLocalReportsPendingRecoveryWithoutRetainingRevokedKey(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	dir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := connectoragentstate.NewSDKStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	state := bootstrapRegisteredState(t)
	now := state.RegisteredAt.UTC()
	state.PendingCredentialRecovery = &qurl.PendingAgentCredentialRecovery{
		RecoveryGrant: "qrg1.test-grant", RecoveryGrantIssuedAt: now,
		RecoveryGrantExpiresAt: now.Add(15 * time.Minute), RecoveryAnchorGrantExpiresAt: now.Add(15 * time.Minute),
		RecoveryExpiresAt: now.Add(15*time.Minute + 90*24*time.Hour), DeviceAPIKey: state.DeviceAPIKey, Assignment: *state.Assignment,
	}
	state.DeviceAPIKey, state.DeviceAPIKeyID = "", ""
	sdk, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.SaveAgentState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{args: []string{"whoami", "--local", "-o", "json"}, shareStateDir: dir})
	if res.code != 0 {
		t.Fatalf("pending identity failed: %s", res.stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(res.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["agent_id"] != state.AgentID || got["device_key_id"] != nil || got["recovery_pending"] != true || got["recovery_issue_pending"] != false {
		t.Fatalf("unexpected pending identity: %v", got)
	}
}

func TestWhoamiLocalRejectsCorruptStateWithoutEgress(t *testing.T) {
	dir := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, connectorstate.AgentStateFile), []byte(`{"device_api_key":"do-not-print-corrupt-secret"`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "whoami", "--local", "-o", "json"}, shareStateDir: dir})
	if res.code == 0 || len(srv.Requests()) != 0 || strings.Contains(res.stdout.String()+res.stderr.String(), "do-not-print-corrupt-secret") {
		t.Fatal("corrupt state was accepted, disclosed or caused network access")
	}
}
