//go:build clisandbox && cliproduction

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// The exact packaged binary talks to production with its shipped Hub trust.
// The candidate never receives the account key or substitutes a runtime fixture.
func productionSupervisedLogin(t *testing.T, binary, key, owner string) {
	t.Helper()
	namespace, err := sandboxNamespace("failure")
	if err != nil {
		t.Fatal(err)
	}
	lane := runtime.GOOS
	if lane == "darwin" {
		lane = "macos"
	}
	// TODO(upstream-contract): Match the trusted production cleanup's exact
	// enrollment name and existing failure AgentID, even after runner loss.
	name := fmt.Sprintf("qurl CLI journey v2 %s/%s/%s/enrollment", os.Getenv(sandboxRunIDEnv), os.Getenv(sandboxRunAttemptEnv), lane)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	token, err := productionMintEnrollment(ctx, productionProofEndpoint, key, name)
	if err != nil {
		t.Fatal(err)
	}
	state, err := filepath.EvalSymlinks(productionStateDir(t))
	if err != nil {
		t.Fatal("prepare supervised production state")
	}
	tokenPath := filepath.Join(state, "enrollment-token")
	productionWriteEnrollment(t, tokenPath, token)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("supervised production proof requires Node.js")
	}
	payload, err := json.Marshal(map[string]any{
		"binary": binary, "args": []string{"--supervision", "external", "login", "--enrollment-token-file", tokenPath, "-o", "json"},
		"state": state, "tokenPath": tokenPath, "owner": owner,
	})
	if err != nil {
		t.Fatal("encode supervised production command")
	}
	env := map[string]string{
		"QURL_ENDPOINT":                   productionProofEndpoint,
		connectorstate.EnvStateDirPrimary: state,
		connectorstate.EnvAgentID:         namespace.AgentID,
	}
	_, code := productionCommand(t, node, env, string(payload), "-e", productionSupervisedScript)
	if code != 0 {
		t.Fatal("production supervised cold/warm login did not pass")
	}
}

// Use the existing public enrollment API with a run name so the independent
// owner controller can revoke even an unused token after a lost mint response.
func productionMintEnrollment(ctx context.Context, endpoint, key, name string) (string, error) {
	body, err := json.Marshal(map[string]string{"kind": "enrollment_token", "target": "agent", "name": name, "expires_in": "1h"})
	if err != nil {
		return "", errors.New("encode production enrollment request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/api-keys", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("prepare production enrollment request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("qurl-cli-supervised-%x", sha256.Sum256([]byte(name))))
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	reply, err := client.Do(req)
	if err != nil {
		return "", errors.New("production enrollment mint did not complete")
	}
	defer func() { _ = reply.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reply.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || reply.StatusCode != http.StatusCreated {
		return "", errors.New("production enrollment mint was rejected")
	}
	var result struct {
		Data struct {
			Token     string            `json:"api_key"`
			Name      string            `json:"name"`
			Kind      string            `json:"kind"`
			Target    string            `json:"target"`
			Status    string            `json:"status"`
			Scopes    []string          `json:"scopes"`
			Claims    []json.RawMessage `json:"claims"`
			ExpiresAt time.Time         `json:"expires_at"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Data.Name != name || result.Data.Kind != "enrollment_token" || result.Data.Target != "agent" || result.Data.Status != "active" || !slices.Equal(result.Data.Scopes, []string{"qurl:agent"}) || len(result.Data.Claims) != 0 || auth.ValidateKeyShape(result.Data.Token) != nil || !result.Data.ExpiresAt.After(time.Now()) || result.Data.ExpiresAt.After(time.Now().Add(65*time.Minute)) {
		return "", errors.New("production enrollment mint returned an invalid credential")
	}
	return result.Data.Token, nil
}

// Node creates the same inherited libuv pipe used by Desktop on Windows.
// Each login is a fresh process; only the wrapping key survives in this parent.
const productionSupervisedScript = `const fs=require('node:fs'),path=require('node:path'),{spawn}=require('node:child_process'),{randomBytes}=require('node:crypto');
const cfg=JSON.parse(fs.readFileSync(0,'utf8'));
const key=randomBytes(32),token=fs.readFileSync(cfg.tokenPath,'utf8').trim();
const sealed=path.join(cfg.state,'agent_state.sealed.json'),plain=path.join(cfg.state,'agent_state.json');
async function login(){
  const child=spawn(cfg.binary,cfg.args,{env:{...process.env,LAYERV_KEY_PROVIDER:'local-key',LAYERV_LOCAL_KEY_FD:'3'},stdio:['ignore','pipe','pipe','pipe']});
  let out=Buffer.alloc(0),err=Buffer.alloc(0),oversize=false;
  const timer=setTimeout(()=>child.kill(),80000);
  child.stdout.on('data',data=>{out=Buffer.concat([out,data]);if(out.length>65536){oversize=true;child.kill()}});
  child.stderr.on('data',data=>{err=Buffer.concat([err,data]);if(err.length>65536){oversize=true;child.kill()}});
  child.stdio[3].on('error',()=>{});
  child.stdio[3].end(key);
  const code=await new Promise(resolve=>{child.on('error',()=>resolve(-1));child.on('close',resolve)});
  clearTimeout(timer);
  if(code!==0||oversize||out.includes(token)||err.includes(token))throw Error('supervised command failed');
  const identity=JSON.parse(out.toString('utf8'));
  if(identity.owner_id!==cfg.owner||identity.device_enrolled!==true)throw Error('supervised identity mismatch');
}
(async()=>{
  if(fs.existsSync(sealed)||fs.existsSync(plain))throw Error('supervised cold state is not empty');
  await login();
  const before=fs.readFileSync(sealed);
  if(before.length===0||before.includes(token)||fs.existsSync(plain))throw Error('supervised identity was not sealed');
  fs.unlinkSync(cfg.tokenPath);
  await login();
  if(fs.existsSync(cfg.tokenPath)||fs.existsSync(plain)||!before.equals(fs.readFileSync(sealed)))throw Error('supervised warm state changed');
  key.fill(0);
})().catch(()=>{key.fill(0);console.error('supervised production proof failed');process.exitCode=1});`
