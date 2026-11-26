//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conformance "github.com/layervai/qurl-conformance"
	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/windows"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Registration and raw requests use real sealed state and fresh Node pipes.
// Registration uses local protocol fixtures; live cold
// enrollment and packaged Desktop qualification remain separate release gates.
func TestWindowsNodeSupervisedColdAndWarmLogin(t *testing.T) {
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
child.stdio[3].end(Buffer.alloc(32,process.argv[3].includes('wrong-key')?0x7b:0x7a));
child.on('error',()=>{clearTimeout(watchdog);process.exit(1)});
child.on('exit',code=>{clearTimeout(watchdog);process.exit(code??1)});`
	for _, phase := range []string{"cold", "warm", "wrong-key", "warm", "request", "request-wrong-key", "request"} {
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
	if phase == "cold" {
		writeWindowsSupervisedEnrollmentToken(t, tokenPath)
	} else if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("warm command must run without its consumed token file: %v", err)
	}
	sealedPath := filepath.Join(stateDir, connectoragentstate.SealedAgentStateFile)
	sealedBefore, err := os.ReadFile(sealedPath) // #nosec G304 -- Test-owned sealed state path.
	if phase == "cold" && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cold login must start without a sealed identity")
	}
	if phase != "cold" && err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	var deviceKey string
	requestPhase := strings.HasPrefix(phase, "request")
	opts := &runOpts{
		args: []string{"--endpoint", srv.URL, "--supervision", "external", "login", "--enrollment-token-file", tokenPath},
		env:  externalLoginEnv(), shareStateDir: stateDir,
		openNativeRuntime: func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			if requestPhase {
				t.Fatal("raw request entered native lifecycle")
			}
			// Warm login must stay offline; no UDP target is allowed here.
			route := nativeRecoveryRoute{}
			cfg.UDPOptions = []qurl.AgentRuntimeUDPOption{qurl.WithAgentRuntimeUDPResolver(route), qurl.WithAgentRuntimeUDPDialer(route)}
			open := connectorshare.OpenNativeRuntime
			if phase == "cold" {
				open = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (*connectorshare.NativeRuntime, error) {
					return openNativeEnrollmentFixture(ctx, t, &cfg)
				}
			}
			runtime, err := open(ctx, cfg)
			if err != nil {
				return nil, err
			}
			if phase != "cold" && runtime.OpenKind != connectorshare.NativeOpenWarm {
				t.Fatal("warm login attempted registration or assignment recovery")
			}
			store, err := runtime.Handoff()
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.LoadAgentState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if state.RegisteredAt == nil || state.PendingActivation != nil || state.EnrollmentCredentialKind != "bootstrap" || state.DeviceAPIKey == "" {
				t.Fatal("external registration did not persist a completed owner-scoped identity")
			}
			deviceKey = state.DeviceAPIKey
			return runtime, nil
		},
	}
	if requestPhase {
		opts.nativeClient = true
		opts.args = []string{"--endpoint", srv.URL, "--supervision", "external", "request", "GET", "/v1/resources", "-o", "json"}
	}
	res := runCLI(t, opts)
	if strings.Contains(res.stdout.String()+res.stderr.String(), conformance.AgentAssignmentBootstrapCredentialFixture) {
		t.Fatal("enrollment credential reached command output")
	}
	if strings.Contains(phase, "wrong-key") {
		if res.code != 3 || len(srv.Requests()) != 0 {
			t.Fatalf("wrong wrapping key was not rejected before network access: exit=%d stderr=%s", res.code, res.stderr.String())
		}
	} else {
		if res.code != 0 {
			t.Fatalf("real Windows %s command exit=%d stderr=%s", phase, res.code, res.stderr.String())
		}
		if requestPhase {
			requests := srv.Requests()
			if len(requests) != 1 || requests[0].Method != "GET" || requests[0].Path != "/v1/resources" {
				t.Fatalf("raw request did not make exactly one target call: calls=%d", len(requests))
			}
			var envelope struct{ Status int }
			if err := json.Unmarshal(res.stdout.Bytes(), &envelope); err != nil || envelope.Status != 200 {
				t.Fatal("raw request did not return the successful response envelope")
			}
		} else {
			assertLoginIdentityAndRuleRead(t, srv.Requests(), deviceKey)
		}
	}
	sealedAfter, err := os.ReadFile(sealedPath) // #nosec G304 -- Test-owned sealed state path.
	if err != nil || len(sealedAfter) == 0 {
		t.Fatalf("sealed identity is missing: %v", err)
	}
	if phase != "cold" && !bytes.Equal(sealedBefore, sealedAfter) {
		t.Fatalf("supervised command rewrote sealed identity: %v", err)
	}
	if bytes.Contains(sealedAfter, []byte(conformance.AgentAssignmentBootstrapCredentialFixture)) ||
		(deviceKey != "" && bytes.Contains(sealedAfter, []byte(deviceKey))) {
		t.Fatal("credential appears in plaintext in the sealed identity")
	}
	if phase == "cold" {
		// Desktop removes the one-shot token after the enrollment process exits.
		if err := os.Remove(tokenPath); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, connectoragentstate.AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext agent state exists: %v", err)
	}
}

func writeWindowsSupervisedEnrollmentToken(t *testing.T, tokenPath string) {
	t.Helper()
	if err := os.WriteFile(tokenPath, []byte(conformance.AgentAssignmentBootstrapCredentialFixture+"\n"), 0o600); err != nil {
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
}
