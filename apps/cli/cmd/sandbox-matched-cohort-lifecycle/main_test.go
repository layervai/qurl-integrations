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

const (
	testNHPSourceSHA    = "a70e5d66dda604459b0a37ed7c634da8c8e46c3d"
	testQURLGoSourceSHA = "c92478b3f70ff027fe7bd9c306b7a9fd96553b64"
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
		{"invalid qurl-go source shape", func(value *lifecycleInput) { value.QURLGoSourceSHA = strings.Repeat("2", 39) }},
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
		{"old mode", append(raw[:len(raw)-1], []byte(`,"mode":"shared-sandbox"}`)...)},
		{"old color", append(raw[:len(raw)-1], []byte(`,"color":"blue"}`)...)},
		{"old closure", append(raw[:len(raw)-1], []byte(`,"prepared_closure":{}}`)...)},
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

func TestValidateSelectedRouteRejectsDirectAndRelayHostDrift(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	cohort := matchedcohort.CohortPlan{HubHost: "hub.sandbox.layerv.xyz", HubPort: 443, HubServerPublicKeyB64: key,
		CellEndpoint: qurl.NHPUDPEndpoint{Host: "cell.sandbox.layerv.xyz", Port: 443, ServerPublicKeyB64: key}}
	for _, transport := range []string{transportDirect, transportRelay} {
		input := lifecycleInput{Transport: transport,
			AdmissionHub:  qurl.HubBootstrap{Host: cohort.HubHost, Port: cohort.HubPort, ServerPublicKeyB64: cohort.HubServerPublicKeyB64},
			AdmissionCell: cohort.CellEndpoint, RecoveryEndpoint: cohort.CellEndpoint}
		if err := validateSelectedRoute(input, cohort); err != nil {
			t.Fatalf("%s exact route: %v", transport, err)
		}
		input.AdmissionHub.Host = "other-hub.sandbox.layerv.xyz"
		if err := validateSelectedRoute(input, cohort); err == nil {
			t.Fatalf("%s accepted Hub host drift", transport)
		}
		input.AdmissionHub.Host = cohort.HubHost
		input.AdmissionCell.Host = "other-cell.sandbox.layerv.xyz"
		if err := validateSelectedRoute(input, cohort); err == nil {
			t.Fatalf("%s accepted cell host drift", transport)
		}
		input.AdmissionCell = cohort.CellEndpoint
		input.RecoveryEndpoint.Host = "other-recovery.sandbox.layerv.xyz"
		if err := validateSelectedRoute(input, cohort); err == nil {
			t.Fatalf("%s accepted recovery host drift", transport)
		}
		input.RecoveryEndpoint = cohort.CellEndpoint
		input.RecoveryEndpoint.ServerPublicKeyB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x53}, 32))
		if err := validateSelectedRoute(input, cohort); err == nil {
			t.Fatalf("%s accepted recovery key drift", transport)
		}
	}
}

func TestValidateAuthorityBlobRejectsAliasedOuterKey(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	input := validInput(t, directory, binary)
	authority, blob := validAuthorityBlob(t, &input)
	input.Authority = matchedcohort.StateReference{Key: blob.Key, VersionID: blob.VersionID, SHA256: blob.SHA256}
	if got, err := validateAuthorityBlob(input, blob); err != nil || got.GenerationID != authority.GenerationID {
		t.Fatalf("exact authority blob = %#v, %v", got, err)
	}
	sourceDrift := input
	sourceDrift.QURLGoSourceSHA = strings.Repeat("2", 40)
	if _, err := validateAuthorityBlob(sourceDrift, blob); err == nil {
		t.Fatal("cross-artifact qurl-go source drift accepted")
	}
	alias := "restored/authority"
	input.Authority.Key = alias
	blob.Key = alias
	if _, err := validateAuthorityBlob(input, blob); err == nil {
		t.Fatal("aliased outer authority key accepted")
	}
}

func validAuthorityBlob(t *testing.T, input *lifecycleInput) (matchedcohort.Authority, matchedcohort.Blob) {
	t.Helper()
	generationID := strings.Repeat("c", 64)
	owner := "sandbox-sharing-owner@clients"
	cohort := matchedcohort.CohortPlan{ServerASG: "sandbox-server", ACASG: "sandbox-ac", RelayASG: "sandbox-relay",
		SessionControlTable: "sandbox-session-control", QURLAgentKeysTable: "sandbox-control-agent-keys", CellID: "sandbox-cell",
		AssignmentGeneration: 1, HubHost: input.AdmissionHub.Host, HubPort: input.AdmissionHub.Port,
		HubServerPublicKeyB64: input.AdmissionHub.ServerPublicKeyB64, CellEndpoint: input.AdmissionCell}
	authority := matchedcohort.Authority{Schema: matchedcohort.AuthoritySchema, Environment: matchedcohort.EnvironmentSandbox,
		GenerationID: generationID, OwnerSubject: owner, AWSAccountID: "111122223333", AWSRegion: "us-east-2",
		NHPSourceSHA: testNHPSourceSHA, QURLGoSourceSHA: testQURLGoSourceSHA,
		EnrollmentCredentialReceipt: matchedcohort.StateReference{Key: "generations/" + generationID + "/enrollment-credential-receipt",
			VersionID: strings.Repeat("1", 64), SHA256: strings.Repeat("2", 64)}, Cohorts: []matchedcohort.CohortPlan{cohort}}
	labels := []string{labelDirectA, labelDirectB, labelRelayC, labelRelayD}
	resources := []string{"qurl-tunnel-server-a", "qurl-tunnel-server-b", "qurl-tunnel-server-c", "qurl-tunnel-server-a"}
	ports := []int{7000, 7001, 7002, 7000}
	for index, label := range labels {
		prefix := "generations/" + generationID + "/shared/" + label + "/"
		authority.Identities = append(authority.Identities, matchedcohort.FixedIdentity{Label: label, OwnerID: owner,
			AgentID: "canary-shared-" + label, AgentPublicKeyB64: input.AdmissionCell.ServerPublicKeyB64,
			AgentKeySchemaVersion:    matchedcohort.AgentKeySchemaVersion,
			EnrollmentCredentialKind: matchedcohort.EnrollmentCredentialKindAccount,
			DeviceAPIKeyID:           "key-" + label, ConnectorID: "connector-shared-" + label,
			ResourceID: "resource-" + label, CRID: "crid-" + label, ConnectorRoutingID: "route-" + label,
			KnockResourceID: "knock-" + label,
			Selector:        matchedcohort.FRPSSelector{ResourceID: resources[index], Host: "connect.sandbox.layerv.xyz", Port: ports[index]},
			AgentState:      matchedcohort.StateReference{Key: prefix + "agent-state", VersionID: strings.Repeat("3", 64), SHA256: strings.Repeat("4", 64)},
			ConnectorState:  matchedcohort.StateReference{Key: prefix + "connector-state", VersionID: strings.Repeat("5", 64), SHA256: strings.Repeat("6", 64)}})
	}
	raw, err := matchedcohort.CanonicalJSON(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := matchedcohort.ValidateAuthority(authority); err != nil {
		t.Fatalf("authority fixture: %v", err)
	}
	key := "generations/" + generationID + "/authority"
	return authority, matchedcohort.Blob{Key: key, VersionID: strings.Repeat("d", 64), SHA256: matchedcohort.Digest(raw), Body: raw}
}

func TestLoadInputAcceptsOnlyExactRecoveryFirstShape(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	input := validInput(t, directory, binary)
	input.Operation = operationRecoverPrepared
	input.Phase = "fixed_shared_recovery_first"
	input.RunIDs = input.RunIDs[:1]
	if _, _, err := loadInput(writeInput(t, directory, &input)); err != nil {
		t.Fatalf("exact recovery-first input = %v", err)
	}
	for name, mutate := range map[string]func(*lifecycleInput){
		"relay":           func(value *lifecycleInput) { value.Transport = transportRelay },
		"old color phase": func(value *lifecycleInput) { value.Phase = "fixed_blue_recovery_first" },
		"arbitrary phase": func(value *lifecycleInput) { value.Phase = "recovery" },
		"extra RunID":     func(value *lifecycleInput) { value.RunIDs = append(value.RunIDs, "1123456789abcdef") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			changed.RunIDs = append([]string(nil), input.RunIDs...)
			mutate(&changed)
			if _, _, err := loadInput(writeInput(t, directory, &changed)); err == nil {
				t.Fatal("mutated recovery-first input accepted")
			}
		})
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
	relay.Phase = "fixed_shared_relay"
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
		Phase: "fixed_shared_direct", Transport: "direct",
		Authority: matchedcohort.StateReference{Key: "generations/" + strings.Repeat("c", 64) + "/authority",
			VersionID: strings.Repeat("d", 64), SHA256: strings.Repeat("e", 64)},
		AdmissionHub: qurl.HubBootstrap{Host: "shared-hub.sandbox.example", Port: 443, ServerPublicKeyB64: key},
		AdmissionCell: qurl.NHPUDPEndpoint{Host: "shared-cell.sandbox.example", Port: 443,
			ServerPublicKeyB64: key},
		RecoveryEndpoint: qurl.NHPUDPEndpoint{Host: "shared-cell.sandbox.example", Port: 443,
			ServerPublicKeyB64: key},
		RunIDs:       []string{"0123456789abcdef", "1123456789abcdef", "2123456789abcdef"},
		PreparedAtMS: time.Unix(1_800_000_000, 0).UnixMilli(), ExpiresAtMS: time.Unix(1_800_000_000, 0).Add(20 * time.Minute).UnixMilli(),
		APIEndpoint: sandboxAPIEndpoint, APIKeyFile: apiKey,
		DeploymentFile: deployment, DeploymentSHA256: stableFileDigest(deployment, false), QURLBinary: binary,
		CommandSHA256:    strings.Repeat("f", 64),
		QURLBinarySHA256: stableFileDigest(binary, true), QURLSourceSHA: strings.Repeat("1", 40),
		QURLGoSourceSHA: testQURLGoSourceSHA, ClientVersion: "sandbox-test"}
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
