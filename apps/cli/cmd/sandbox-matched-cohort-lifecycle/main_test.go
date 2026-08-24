//go:build linux && !android

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/matchedcohort"
)

func TestLoadInputPinsCanonicalPrivateBinaryAuthority(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	input := validInput(t, directory, binary)
	path := writeInput(t, directory, &input)
	loaded, raw, err := loadInput(path)
	if err != nil || loaded.ReleaseID != input.ReleaseID || matchedcohort.Digest(raw) == "" {
		t.Fatalf("load input = %#v %v", loaded, err)
	}
	mutations := []struct {
		name   string
		mutate func(*lifecycleInput)
	}{
		{"production", func(value *lifecycleInput) { value.Environment = "prod" }},
		{"wrong binary", func(value *lifecycleInput) { value.QURLBinarySHA256 = strings.Repeat("0", 64) }},
		{"wrong authority", func(value *lifecycleInput) { value.Authority.VersionID = "version" }},
		{"wrong admission Hub", func(value *lifecycleInput) { value.AdmissionHub.Port = 62206 }},
		{"wrong admission cell", func(value *lifecycleInput) { value.AdmissionCell.Port = 62206 }},
		{"wrong recovery", func(value *lifecycleInput) { value.RecoveryEndpoint.Port = 62206 }},
		{"wrong deployment digest", func(value *lifecycleInput) { value.DeploymentSHA256 = strings.Repeat("0", 64) }},
		{"wrong command digest shape", func(value *lifecycleInput) { value.CommandSHA256 = "moving-tag" }},
		{"wrong qurl-go source", func(value *lifecycleInput) { value.QURLGoSourceSHA = strings.Repeat("2", 40) }},
		{"wrong API endpoint", func(value *lifecycleInput) { value.APIEndpoint = "https://api.layerv.ai" }},
		{"duplicate RunID", func(value *lifecycleInput) { value.RunIDs[2] = value.RunIDs[0] }},
		{"unknown transport", func(value *lifecycleInput) { value.Transport = "unknown" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := input
			changed.RunIDs = append([]string(nil), input.RunIDs...)
			mutation.mutate(&changed)
			path := writeInput(t, directory, &changed)
			if _, _, err := loadInput(path); err == nil {
				t.Fatal("mutated input accepted")
			}
		})
	}
}

func TestLoadInputRejectsDuplicateUnknownAndPrivateFileMutations(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	input := validInput(t, directory, binary)
	raw, _ := json.Marshal(input)
	cases := []struct {
		name string
		body []byte
	}{
		{"duplicate", bytes.Replace(raw, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1)},
		{"unknown", append(raw[:len(raw)-1], []byte(`,"extra":true}`)...)},
		{"no LF", raw},
		{"double LF", append(append(bytes.Clone(raw), '\n'), '\n')},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(test.name, " ", "-"))
			payload := test.body
			if test.name != "no LF" && test.name != "double LF" {
				payload = append(bytes.Clone(test.body), '\n')
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadInput(path); err == nil {
				t.Fatal("input mutation accepted")
			}
		})
	}
	base := writeInput(t, directory, &input)
	link := filepath.Join(directory, "link")
	if err := os.Link(base, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadInput(link); err == nil {
		t.Fatal("hard-linked input accepted")
	}
}

func TestLoadInputAcceptsOnlyExactFourOperationClosurePhase(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	input := validInput(t, directory, binary)
	input.Operation = operationPrepareClosed
	input.Phase = "active_blue_closure_recovery"
	input.Color = "blue"
	input.RunIDs = append(input.RunIDs, "3123456789abcdef")
	if _, _, err := loadInput(writeInput(t, directory, &input)); err != nil {
		t.Fatalf("exact closure preparation input = %v", err)
	}
	input.Attempt = 2
	input.PreparedClosure = &closurePreparation{Intent: matchedcohort.StateReference{Key: "closure-intent", VersionID: strings.Repeat("f", 64), SHA256: strings.Repeat("1", 64)},
		OperationKeys: []string{"op-a", "op-b", "op-c", "op-d"}}
	if _, _, err := loadInput(writeInput(t, directory, &input)); err != nil {
		t.Fatalf("exact closure successor input = %v", err)
	}
	input.Operation = operationVerifyClosed
	if _, _, err := loadInput(writeInput(t, directory, &input)); err != nil {
		t.Fatalf("exact closure verification input = %v", err)
	}
	mutations := []func(*lifecycleInput){
		func(value *lifecycleInput) { value.Operation = "other" },
		func(value *lifecycleInput) { value.Phase = "active_green_closure_recovery" },
		func(value *lifecycleInput) { value.Transport = "relay" },
		func(value *lifecycleInput) { value.RunIDs = value.RunIDs[:3] },
		func(value *lifecycleInput) { value.RunIDs[3] = value.RunIDs[0] },
		func(value *lifecycleInput) { value.Attempt = matchedcohort.MaxClosureAttempts + 1 },
		func(value *lifecycleInput) {
			value.PreparedClosure.OperationKeys = value.PreparedClosure.OperationKeys[:3]
		},
		func(value *lifecycleInput) {
			value.PreparedClosure.OperationKeys[3] = value.PreparedClosure.OperationKeys[0]
		},
	}
	for index, mutate := range mutations {
		changed := input
		changed.RunIDs = append([]string(nil), input.RunIDs...)
		prepared := *input.PreparedClosure
		prepared.OperationKeys = append([]string(nil), input.PreparedClosure.OperationKeys...)
		changed.PreparedClosure = &prepared
		mutate(&changed)
		if _, _, err := loadInput(writeInput(t, directory, &changed)); err == nil {
			t.Fatalf("closure mutation %d accepted", index)
		}
	}
	firstWithPrior := input
	firstWithPrior.Operation = operationPrepareClosed
	firstWithPrior.Attempt = 1
	if _, _, err := loadInput(writeInput(t, directory, &firstWithPrior)); err == nil {
		t.Fatal("first closure attempt accepted a predecessor")
	}
	successorWithoutPrior := input
	successorWithoutPrior.Operation = operationPrepareClosed
	successorWithoutPrior.PreparedClosure = nil
	if _, _, err := loadInput(writeInput(t, directory, &successorWithoutPrior)); err == nil {
		t.Fatal("closure successor accepted without predecessor")
	}
}

func TestLoadInputBindsExactDirectAndRelayDeploymentSemantics(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	direct := validInput(t, directory, binary)
	if _, _, err := loadInput(writeInput(t, directory, &direct)); err != nil {
		t.Fatalf("exact direct deployment: %v", err)
	}
	swappedRelay := direct
	swappedRelay.Transport = transportRelay
	swappedRelay.RelayHostname = "relay.sandbox.layerv.xyz"
	if _, _, err := loadInput(writeInput(t, directory, &swappedRelay)); err == nil {
		t.Fatal("direct deployment accepted under relay label")
	}

	relay := direct
	relay.Transport = transportRelay
	relay.Phase = "candidate-relay"
	relay.RelayHostname = "relay.sandbox.layerv.xyz"
	writeDeployment(t, relay.DeploymentFile, &qurl.Deployment{Issuers: []qurl.ManifestIssuer{{Kid: "sandbox-issuer", SPKIDERB64: "fixture"}},
		Cells: []qurl.DeploymentCell{}, RelayAllowlist: []string{relay.RelayHostname}, Hub: &relay.AdmissionHub})
	relay.DeploymentSHA256 = stableFileDigest(relay.DeploymentFile, false)
	if _, _, err := loadInput(writeInput(t, directory, &relay)); err != nil {
		t.Fatalf("exact relay deployment: %v", err)
	}
	swappedDirect := relay
	swappedDirect.Transport = transportDirect
	swappedDirect.RelayHostname = ""
	if _, _, err := loadInput(writeInput(t, directory, &swappedDirect)); err == nil {
		t.Fatal("relay deployment accepted under direct label")
	}

	for _, test := range []struct {
		name   string
		mutate func(*qurl.Deployment, *lifecycleInput)
	}{
		{name: "wrong relay hostname", mutate: func(_ *qurl.Deployment, input *lifecycleInput) { input.RelayHostname = "other.sandbox.layerv.xyz" }},
		{name: "extra relay", mutate: func(deployment *qurl.Deployment, _ *lifecycleInput) {
			deployment.RelayAllowlist = append(deployment.RelayAllowlist, "other.sandbox.layerv.xyz")
		}},
		{name: "relay with cell", mutate: func(deployment *qurl.Deployment, _ *lifecycleInput) {
			deployment.Cells = []qurl.DeploymentCell{{CellID: "sandbox-cell", Host: relay.AdmissionCell.Host, Port: 443,
				ServerPublicKeyB64: relay.AdmissionCell.ServerPublicKeyB64}}
		}},
		{name: "wrong Hub", mutate: func(deployment *qurl.Deployment, _ *lifecycleInput) {
			deployment.Hub.Host = "other-hub.sandbox.layerv.xyz"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := relay
			deployment := qurl.Deployment{Issuers: []qurl.ManifestIssuer{{Kid: "sandbox-issuer", SPKIDERB64: "fixture"}},
				Cells: []qurl.DeploymentCell{}, RelayAllowlist: []string{relay.RelayHostname}, Hub: &qurl.HubBootstrap{
					Host: relay.AdmissionHub.Host, Port: relay.AdmissionHub.Port, ServerPublicKeyB64: relay.AdmissionHub.ServerPublicKeyB64}}
			test.mutate(&deployment, &input)
			writeDeployment(t, input.DeploymentFile, &deployment)
			input.DeploymentSHA256 = stableFileDigest(input.DeploymentFile, false)
			if _, _, err := loadInput(writeInput(t, directory, &input)); err == nil {
				t.Fatal("mutated transport deployment accepted")
			}
		})
	}
}

func TestReportIsDurableNoOverwriteAndExactReplay(t *testing.T) {
	directory := privateDirectory(t)
	path := filepath.Join(directory, "report.json")
	report := lifecycleReport{Schema: 1, Environment: "sandbox", Operation: "lifecycle", ReleaseID: strings.Repeat("a", 64), Attempt: 1}
	if err := writeReport(path, &report); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, &report); err != nil {
		t.Fatalf("exact report replay: %v", err)
	}
	changed := report
	changed.Phase = "changed"
	if err := writeReport(path, &changed); err == nil {
		t.Fatal("different report overwrote existing receipt")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("report metadata = %v %v", info, err)
	}
}

func TestReportCrashWindowsConvergeOnlyExactReceipt(t *testing.T) {
	report := lifecycleReport{Schema: 1, Environment: "sandbox", Operation: operationLifecycle,
		ReleaseID: strings.Repeat("a", 64), Attempt: 1}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	for _, test := range []struct {
		name      string
		temporary []byte
		installed bool
		wantError bool
	}{
		{name: "complete before link", temporary: raw},
		{name: "partial write", temporary: raw[:len(raw)/2]},
		{name: "installed two link", temporary: raw, installed: true},
		{name: "conflicting bytes", temporary: []byte("conflict"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := privateDirectory(t)
			path := filepath.Join(directory, "report.json")
			temporary := filepath.Join(directory, ".lifecycle-report.tmp")
			if err := os.WriteFile(temporary, test.temporary, 0o600); err != nil {
				t.Fatal(err)
			}
			if test.installed {
				if err := os.Link(temporary, path); err != nil {
					t.Fatal(err)
				}
			}
			err := writeReport(path, &report)
			if test.wantError {
				if err == nil {
					t.Fatal("conflicting temporary receipt was normalized")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path) //nolint:gosec // Fixed child of the private test directory.
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("installed report = %q, %v", got, err)
			}
			if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
				t.Fatalf("temporary receipt remains: %v", err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Nlink != 1 {
				t.Fatalf("installed receipt links = %#v, %v", stat, err)
			}
		})
	}
}

func TestOutcomeServersReturnExactReleaseAndIdentityBytes(t *testing.T) {
	release := strings.Repeat("b", 64)
	servers, targets, expected, err := startOutcomeServers(release, [2]string{"direct-a", "direct-b"})
	if err != nil {
		t.Fatal(err)
	}
	defer stopOutcomeServers(servers)
	for label, target := range targets {
		response, err := http.Get("http://" + target + "/") //nolint:noctx // Loopback fixture.
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || !bytes.Equal(body, expected[label]) {
			t.Fatalf("%s body = %q %v", label, body, readErr)
		}
	}
}

func TestWriteAllHandlesShortAndZeroWrites(t *testing.T) {
	short := &shortWriter{maximum: 2}
	if err := writeAll(short, []byte("abcdef")); err != nil || string(short.value) != "abcdef" {
		t.Fatalf("short write = %q %v", short.value, err)
	}
	zero := &shortWriter{zero: true}
	if err := writeAll(zero, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write = %v", err)
	}
}

type shortWriter struct {
	maximum int
	zero    bool
	value   []byte
}

func (w *shortWriter) Write(raw []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	count := min(len(raw), w.maximum)
	w.value = append(w.value, raw[:count]...)
	return count, nil
}

func validInput(t *testing.T, directory, binary string) lifecycleInput {
	t.Helper()
	apiKey := filepath.Join(directory, "api-key")
	deployment := filepath.Join(directory, "deployment.json")
	if err := os.WriteFile(apiKey, []byte("lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	input := lifecycleInput{Schema: 1, Environment: "sandbox", Operation: "lifecycle", ReleaseID: strings.Repeat("a", 64), Attempt: 1,
		Phase: "candidate-direct", Color: "green", Transport: "direct",
		Authority: matchedcohort.StateReference{Key: "generations/" + strings.Repeat("c", 64) + "/authority",
			VersionID: strings.Repeat("d", 64), SHA256: strings.Repeat("e", 64)},
		AdmissionHub: qurl.HubBootstrap{Host: "green-candidate.sandbox.example", Port: 443, ServerPublicKeyB64: key},
		AdmissionCell: qurl.NHPUDPEndpoint{Host: "green-ac-candidate.sandbox.example", Port: 443,
			ServerPublicKeyB64: key},
		RecoveryEndpoint: qurl.NHPUDPEndpoint{Host: "green-recovery.sandbox.example", Port: 443,
			ServerPublicKeyB64: key},
		RunIDs:       []string{"0123456789abcdef", "1123456789abcdef", "2123456789abcdef"},
		PreparedAtMS: time.Unix(1_800_000_000, 0).UnixMilli(), ExpiresAtMS: time.Unix(1_800_000_000, 0).Add(20 * time.Minute).UnixMilli(),
		APIEndpoint: sandboxAPIEndpoint, APIKeyFile: apiKey,
		DeploymentFile: deployment, DeploymentSHA256: stableFileDigest(deployment, false), QURLBinary: binary,
		CommandSHA256:    strings.Repeat("f", 64),
		QURLBinarySHA256: stableFileDigest(binary, true), QURLSourceSHA: strings.Repeat("1", 40),
		QURLGoSourceSHA: matchedcohort.RequiredQURLGoSourceSHA, ClientVersion: "sandbox-test"}
	writeDeployment(t, deployment, &qurl.Deployment{Issuers: []qurl.ManifestIssuer{{Kid: "sandbox-issuer", SPKIDERB64: "fixture"}},
		Cells: []qurl.DeploymentCell{{CellID: "sandbox-cell", Host: input.AdmissionCell.Host, Port: input.AdmissionCell.Port,
			ServerPublicKeyB64: input.AdmissionCell.ServerPublicKeyB64}}, RelayAllowlist: []string{}, Hub: &input.AdmissionHub})
	input.DeploymentSHA256 = stableFileDigest(deployment, false)
	return input
}

func writeDeployment(t *testing.T, path string, deployment *qurl.Deployment) {
	t.Helper()
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeInput(t *testing.T, directory string, input *lifecycleInput) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "input-"+matchedcohort.Digest(raw)[:12]+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Test authority directory must be owner-only and searchable.
		t.Fatal(err)
	}
	return directory
}
