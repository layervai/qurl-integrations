//go:build windows && connectorpreview

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/windows"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// This source-only gate uses the reviewed Connector candidate. After its real
// release is pinned, remove the preview tag and run this in the normal suite.
// Device registration is seeded; live cold enrollment remains a release gate.
func TestWindowsNodeSupervisedCredentialAndWarmLogin(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("supervised CLI integration requires Node.js")
	}
	stateDir := connectorStateTestDir(t)
	const script = `const {spawn}=require('node:child_process');
const child=spawn(process.argv[1],['-test.run=^TestWindowsNodeSupervisedLoginChild$','-test.v'],{
 env:{...process.env,QURL_TEST_SUPERVISED_PHASE:process.argv[3],QURL_TEST_SUPERVISED_STATE:process.argv[2],LAYERV_KEY_PROVIDER:'local-key',LAYERV_LOCAL_KEY_FD:'3'},
 stdio:['ignore','inherit','inherit','pipe']
});
const watchdog=setTimeout(()=>child.kill(),15000);
child.stdio[3].on('error',()=>{});
child.stdio[3].end(Buffer.alloc(32,process.argv[3]==='wrong-key'?0x7b:0x7a));
child.on('error',()=>{clearTimeout(watchdog);process.exit(1)});
child.on('exit',code=>{clearTimeout(watchdog);process.exit(code??1)});`
	for _, phase := range []string{"seed", "warm", "wrong-key", "warm"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		command := exec.CommandContext(ctx, node, "-e", script, "--", os.Args[0], stateDir, phase) // #nosec G204 G702 -- Resolved Node runs a fixed fixture and this test executable, without a shell.
		output, err := command.CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestWindowsNodeSupervisedLoginChild") {
			t.Fatalf("supervised %s process: %v\n%s", phase, err, output)
		}
	}
}

func TestWindowsNodeSupervisedLoginChild(t *testing.T) {
	phase := os.Getenv("QURL_TEST_SUPERVISED_PHASE")
	if phase == "" {
		return
	}
	// TestMain pins the file provider to keep ordinary tests off the host TPM.
	// Restore this fixture's provider; Node's inherited descriptor stays intact.
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	stateDir := os.Getenv("QURL_TEST_SUPERVISED_STATE")
	tokenPath := filepath.Join(stateDir, "enrollment-token")
	state := bootstrapRegisteredState(t)
	if phase == "seed" {
		seedWindowsSupervisedIdentity(t, stateDir, tokenPath, state)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("warm command must run without its consumed token file: %v", err)
	}
	sealedPath := filepath.Join(stateDir, connectoragentstate.SealedAgentStateFile)
	sealedBefore, err := os.ReadFile(sealedPath) // #nosec G304 -- Test-owned sealed state path.
	if err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
		env:  externalLoginEnv(), shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return connectorshare.OpenNativeRuntime(ctx, cfg)
		},
	})
	if phase == "wrong-key" {
		if res.code != 3 || len(srv.Requests()) != 0 {
			t.Fatalf("wrong wrapping key was not rejected before network access: exit=%d stderr=%s", res.code, res.stderr.String())
		}
	} else {
		if res.code != 0 {
			t.Fatalf("real Windows warm login exit=%d stderr=%s", res.code, res.stderr.String())
		}
		assertLoginIdentityAndRuleRead(t, srv.Requests(), state.DeviceAPIKey)
	}
	sealedAfter, err := os.ReadFile(sealedPath) // #nosec G304 -- Test-owned sealed state path.
	if err != nil || !bytes.Equal(sealedBefore, sealedAfter) {
		t.Fatalf("warm login rewrote sealed identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, connectoragentstate.AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext agent state exists: %v", err)
	}
}

func seedWindowsSupervisedIdentity(t *testing.T, stateDir, tokenPath string, state *qurl.AgentState) {
	t.Helper()
	const token = "supervised-windows-test-enrollment"
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(tokenPath, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	provider := oneShotEnrollmentToken(tokenPath)
	credential, err := provider(context.Background(), qurl.AgentEnrollmentCredentialRequest{})
	if err != nil || credential != token {
		t.Fatalf("supervised credential was not accepted: %v", err)
	}
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	store, err := connectoragentstate.NewSDKStore(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	sdk, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdk.SaveAgentState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
}
