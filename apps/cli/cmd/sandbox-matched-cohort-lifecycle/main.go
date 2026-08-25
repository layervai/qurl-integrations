//go:build linux && !android

// Command sandbox-matched-cohort-lifecycle runs the protected sandbox-only
// fixed-canary lifecycle and recovery-first outcomes.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/unix"

	"github.com/layervai/qurl-integrations/apps/cli/internal/matchedcohort"
)

const (
	inputSchema              = 1
	reportSchema             = 1
	maxInputSize             = 1 << 20
	operationLifecycle       = "lifecycle"
	operationRecoverPrepared = "recover-prepared"
	transportDirect          = "direct"
	transportRelay           = "relay"
	labelDirectA             = "direct-a"
	labelDirectB             = "direct-b"
	labelRelayC              = "relay-c"
	labelRelayD              = "relay-d"
	sandboxAPIEndpoint       = "https://api.layerv.xyz"
)

type lifecycleInput struct {
	Schema           int                          `json:"schema"`
	Environment      string                       `json:"environment"`
	Operation        string                       `json:"operation"`
	ReleaseID        string                       `json:"release_id"`
	Phase            string                       `json:"phase"`
	Attempt          uint64                       `json:"attempt"`
	Transport        string                       `json:"transport"`
	Authority        matchedcohort.StateReference `json:"authority"`
	AdmissionHub     qurl.HubBootstrap            `json:"admission_hub"`
	AdmissionCell    qurl.NHPUDPEndpoint          `json:"admission_cell_endpoint"`
	RecoveryEndpoint qurl.NHPUDPEndpoint          `json:"recovery_endpoint"`
	RunIDs           []string                     `json:"run_ids"`
	PreparedAtMS     int64                        `json:"prepared_at_ms"`
	ExpiresAtMS      int64                        `json:"expires_at_ms"`
	APIEndpoint      string                       `json:"api_endpoint"`
	APIKeyFile       string                       `json:"api_key_file"`
	DeploymentFile   string                       `json:"deployment_file"`
	DeploymentSHA256 string                       `json:"deployment_sha256"`
	RelayHostname    string                       `json:"relay_hostname"`
	CommandSHA256    string                       `json:"lifecycle_command_sha256"`
	QURLBinary       string                       `json:"qurl_binary"`
	QURLBinarySHA256 string                       `json:"qurl_binary_sha256"`
	QURLSourceSHA    string                       `json:"qurl_source_sha"`
	QURLGoSourceSHA  string                       `json:"qurl_go_source_sha"`
	ClientVersion    string                       `json:"client_version"`
}

type lifecycleReport struct {
	Schema           int                          `json:"schema"`
	Environment      string                       `json:"environment"`
	Operation        string                       `json:"operation"`
	ReleaseID        string                       `json:"release_id"`
	Phase            string                       `json:"phase"`
	Attempt          uint64                       `json:"attempt"`
	Transport        string                       `json:"transport"`
	InputSHA256      string                       `json:"input_sha256"`
	Authority        matchedcohort.StateReference `json:"authority"`
	StateUpdates     []matchedcohort.StateUpdate  `json:"state_updates"`
	CommandSHA256    string                       `json:"lifecycle_command_sha256"`
	QURLBinarySHA256 string                       `json:"qurl_binary_sha256"`
	QURLSourceSHA    string                       `json:"qurl_source_sha"`
	QURLGoSourceSHA  string                       `json:"qurl_go_source_sha"`
	Lifecycle        *lifecycleReportOutcome      `json:"lifecycle,omitempty"`
	Recovery         *recoveryReportOutcome       `json:"recovery,omitempty"`
}

type recoveryReportOutcome struct {
	Status  string                             `json:"status"`
	Label   string                             `json:"label"`
	Receipt matchedcohort.RecoveryFirstReceipt `json:"receipt"`
}

type lifecycleReportOutcome struct {
	Status          string                             `json:"status"`
	Intent          matchedcohort.StateReference       `json:"intent"`
	PrimaryFirstKey string                             `json:"primary_first_key"`
	SiblingKey      string                             `json:"sibling_key"`
	ReplacementKey  string                             `json:"replacement_key"`
	Outcome         *matchedcohort.LifecycleOutcome    `json:"outcome,omitempty"`
	Settlement      *matchedcohort.LifecycleSettlement `json:"settlement,omitempty"`
}

type commandArgs struct {
	socketPath string
	inputPath  string
	reportPath string
}

func parseArgs(args []string) (commandArgs, error) {
	flags := flag.NewFlagSet("sandbox-matched-cohort-lifecycle", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var values commandArgs
	flags.StringVar(&values.socketPath, "authority-socket", "", "")
	flags.StringVar(&values.inputPath, "input-file", "", "")
	flags.StringVar(&values.reportPath, "report-file", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return commandArgs{}, errors.New("invalid command arguments")
	}
	if !cleanAbsolute(values.socketPath) || !cleanAbsolute(values.inputPath) || !cleanAbsolute(values.reportPath) ||
		values.inputPath == values.reportPath || values.socketPath == values.inputPath || values.socketPath == values.reportPath {
		return commandArgs{}, errors.New("command paths are not exact distinct absolute paths")
	}
	return values, nil
}

func loadInput(path string) (lifecycleInput, []byte, error) { //nolint:gocyclo // This is the closed input trust boundary.
	raw, err := readPrivateFile(path, maxInputSize)
	if err != nil {
		return lifecycleInput{}, nil, err
	}
	var input lifecycleInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return lifecycleInput{}, nil, errors.New("lifecycle input JSON is invalid")
	}
	canonical, err := json.Marshal(input)
	if err != nil || !bytes.Equal(canonical, raw[:len(raw)-1]) {
		return lifecycleInput{}, nil, errors.New("lifecycle input is not canonical")
	}
	if input.Schema != inputSchema || input.Environment != matchedcohort.EnvironmentSandbox ||
		(input.Operation != operationLifecycle && input.Operation != operationRecoverPrepared) || !hex64(input.ReleaseID) ||
		(input.Transport != transportDirect && input.Transport != transportRelay) ||
		input.Phase == "" || input.Phase != strings.TrimSpace(input.Phase) || input.Attempt == 0 ||
		input.PreparedAtMS <= 0 || input.ExpiresAtMS <= input.PreparedAtMS || input.ExpiresAtMS-input.PreparedAtMS > int64(30*time.Minute/time.Millisecond) ||
		input.Authority.Key == "" || input.Authority.Key != strings.TrimSpace(input.Authority.Key) ||
		!hex64(input.Authority.VersionID) || !hex64(input.Authority.SHA256) ||
		input.AdmissionHub.Port != 443 || input.AdmissionHub.Host == "" || !validRaw32Key(input.AdmissionHub.ServerPublicKeyB64) ||
		input.AdmissionCell.Port != 443 || input.AdmissionCell.Host == "" || !validRaw32Key(input.AdmissionCell.ServerPublicKeyB64) ||
		input.RecoveryEndpoint.Port != 443 || input.RecoveryEndpoint.Host == "" || !validRaw32Key(input.RecoveryEndpoint.ServerPublicKeyB64) ||
		!cleanAbsolute(input.APIKeyFile) || !cleanAbsolute(input.DeploymentFile) || !cleanAbsolute(input.QURLBinary) ||
		input.APIEndpoint != sandboxAPIEndpoint || !hex64(input.DeploymentSHA256) || !hex64(input.CommandSHA256) ||
		!hex64(input.QURLBinarySHA256) ||
		!hex40(input.QURLSourceSHA) || !hex40(input.QURLGoSourceSHA) ||
		input.ClientVersion == "" || input.ClientVersion != strings.TrimSpace(input.ClientVersion) {
		return lifecycleInput{}, nil, errors.New("lifecycle input authority is invalid")
	}
	wantRunIDs := 3
	if input.Operation == operationRecoverPrepared {
		wantRunIDs = 1
		if input.Transport != transportDirect || input.Phase != "fixed_shared_recovery_first" {
			return lifecycleInput{}, nil, errors.New("recovery-first input is invalid")
		}
	} else if input.Phase != "fixed_shared_"+input.Transport {
		return lifecycleInput{}, nil, errors.New("shared lifecycle input phase is invalid")
	}
	if len(input.RunIDs) != wantRunIDs {
		return lifecycleInput{}, nil, errors.New("lifecycle RunID count is invalid")
	}
	seenRunIDs := make(map[string]struct{}, len(input.RunIDs))
	for _, runID := range input.RunIDs {
		if err := qurl.ValidateCycleRunID(runID); err != nil {
			return lifecycleInput{}, nil, errors.New("lifecycle RunID is invalid")
		}
		if _, exists := seenRunIDs[runID]; exists {
			return lifecycleInput{}, nil, errors.New("lifecycle RunIDs are not distinct")
		}
		seenRunIDs[runID] = struct{}{}
	}
	if stableFileDigest(input.QURLBinary, true) != input.QURLBinarySHA256 ||
		stableFileDigest(input.DeploymentFile, false) != input.DeploymentSHA256 {
		return lifecycleInput{}, nil, errors.New("released qurl binary or deployment digest does not match input")
	}
	if _, err := validateTransportDeployment(input); err != nil {
		return lifecycleInput{}, nil, err
	}
	return input, raw, nil
}

func run(ctx context.Context, args commandArgs) (lifecycleReport, error) { //nolint:gocognit,gocyclo // Both protected operations share one closed preflight.
	input, inputRaw, err := loadInput(args.inputPath)
	if err != nil {
		return lifecycleReport{}, err
	}
	rpc, err := matchedcohort.NewAuthorityRPC(args.socketPath)
	if err != nil {
		return lifecycleReport{}, err
	}
	authorityBlob, err := rpc.Load(ctx, input.Authority.Key)
	if err != nil {
		return lifecycleReport{}, errors.New("fixed canary authority reference does not match durable storage")
	}
	authority, err := validateAuthorityBlob(input, authorityBlob)
	if err != nil {
		return lifecycleReport{}, err
	}
	deployment, err := validateTransportDeployment(input)
	if err != nil {
		return lifecycleReport{}, err
	}
	cohort, err := selectedCohort(authority)
	if err != nil || validateSelectedRoute(input, cohort) != nil ||
		(input.Transport == transportDirect && deployment.Cells[0].CellID != cohort.CellID) {
		return lifecycleReport{}, errors.New("customer deployment does not match selected physical cohort")
	}
	sessionRuntime, err := matchedcohort.NewQURLSessionRuntime(input.AdmissionHub)
	if err != nil {
		return lifecycleReport{}, err
	}
	consumer := &matchedcohort.Consumer{Blobs: rpc, Runtime: sessionRuntime}
	if input.Operation == operationRecoverPrepared {
		var identity *matchedcohort.FixedIdentity
		for index := range authority.Identities {
			candidate := &authority.Identities[index]
			if candidate.Label == labelDirectA {
				identity = candidate
				break
			}
		}
		if identity == nil {
			return lifecycleReport{}, errors.New("recovery-first fixed identity is absent")
		}
		receipt, recoverErr := consumer.RecoverPrepared(ctx, authority, matchedcohort.PrepareOperationRequest{
			ReleaseID: input.ReleaseID, Phase: input.Phase, AWSAccountID: authority.AWSAccountID, AWSRegion: authority.AWSRegion,
			Identity: *identity, Cohort: cohort, ExpectedAgentState: identity.AgentState, RecoveryEndpoint: input.RecoveryEndpoint,
			RunID: input.RunIDs[0], RunAttempt: input.Attempt, PreparedAt: time.UnixMilli(input.PreparedAtMS).UTC(),
			ExpiresAt: time.UnixMilli(input.ExpiresAtMS).UTC(),
		})
		if recoverErr != nil {
			return lifecycleReport{}, recoverErr
		}
		return lifecycleReport{Schema: reportSchema, Environment: matchedcohort.EnvironmentSandbox,
			Operation: input.Operation, ReleaseID: input.ReleaseID, Phase: input.Phase, Attempt: input.Attempt,
			Transport: input.Transport, InputSHA256: matchedcohort.Digest(inputRaw), Authority: input.Authority,
			StateUpdates: []matchedcohort.StateUpdate{}, CommandSHA256: input.CommandSHA256,
			QURLBinarySHA256: input.QURLBinarySHA256, QURLSourceSHA: input.QURLSourceSHA,
			QURLGoSourceSHA: input.QURLGoSourceSHA,
			Recovery:        &recoveryReportOutcome{Status: "completed", Label: labelDirectA, Receipt: receipt}}, nil
	}
	labels := []string{labelDirectA, labelDirectB}
	if input.Transport == transportRelay {
		labels = []string{labelRelayC, labelRelayD}
	}
	stateUpdates, err := consumer.RefreshSelectedIdentityStates(ctx, authority, labels,
		input.AdmissionHub, input.AdmissionCell, nil)
	if err != nil {
		return lifecycleReport{}, err
	}
	stateReferences := make(map[string]matchedcohort.StateReference, len(stateUpdates))
	for _, update := range stateUpdates {
		stateReferences[update.Label] = update.After
	}
	prepared, err := consumer.PrepareLifecycle(ctx, authority, matchedcohort.LifecycleIntentInput{
		ReleaseID: input.ReleaseID, Phase: input.Phase, Attempt: input.Attempt, Transport: input.Transport,
		RecoveryEndpoint: input.RecoveryEndpoint, RunIDs: [3]string{input.RunIDs[0], input.RunIDs[1], input.RunIDs[2]},
		PreparedAt: time.UnixMilli(input.PreparedAtMS).UTC(), ExpiresAt: time.UnixMilli(input.ExpiresAtMS).UTC(),
		AgentStates: stateReferences,
	})
	if err != nil {
		return lifecycleReport{}, err
	}
	needsSettlement, err := consumer.LifecycleAttemptNeedsSettlement(ctx, prepared)
	if err != nil {
		return lifecycleReport{}, err
	}
	if needsSettlement {
		settlement, settleErr := consumer.SettlePreparedLifecycle(ctx, authority, prepared, 15*time.Second)
		if settleErr != nil {
			return lifecycleReport{}, settleErr
		}
		return lifecycleReport{Schema: reportSchema, Environment: matchedcohort.EnvironmentSandbox,
			Operation: input.Operation, ReleaseID: input.ReleaseID, Phase: input.Phase, Attempt: input.Attempt,
			Transport: input.Transport, InputSHA256: matchedcohort.Digest(inputRaw), Authority: input.Authority, StateUpdates: stateUpdates,
			CommandSHA256: input.CommandSHA256, QURLBinarySHA256: input.QURLBinarySHA256,
			QURLSourceSHA: input.QURLSourceSHA, QURLGoSourceSHA: input.QURLGoSourceSHA,
			Lifecycle: &lifecycleReportOutcome{Status: "settled-retry-required", Intent: prepared.IntentReference,
				PrimaryFirstKey: prepared.PrimaryFirstKey, SiblingKey: prepared.SiblingKey,
				ReplacementKey: prepared.ReplacementKey, Settlement: &settlement}}, nil
	}
	labelPair := [2]string{labelDirectA, labelDirectB}
	if input.Transport == transportRelay {
		labelPair = [2]string{labelRelayC, labelRelayD}
	}
	servers, targets, expected, err := startOutcomeServers(input.ReleaseID, labelPair)
	if err != nil {
		return lifecycleReport{}, err
	}
	defer stopOutcomeServers(servers)
	stateRoot, err := os.MkdirTemp(filepath.Dir(args.reportPath), ".matched-cohort-state-")
	if err != nil {
		return lifecycleReport{}, errors.New("create private lifecycle state root")
	}
	defer func() { _ = os.RemoveAll(stateRoot) }()
	if err := os.Chmod(stateRoot, 0o700); err != nil { //nolint:gosec // Private runtime state must be owner-only and searchable.
		return lifecycleReport{}, errors.New("secure lifecycle state root")
	}
	launcher := &matchedcohort.ManagedSessionLauncher{Consumer: consumer, Targets: targets,
		ClientVersion: input.ClientVersion, ReadyTimeout: 15 * time.Second}
	probe := &matchedcohort.BinaryGetProbe{Binary: input.QURLBinary, APIEndpoint: input.APIEndpoint,
		BinarySHA256: input.QURLBinarySHA256, APIKeyFile: input.APIKeyFile, DeploymentFile: input.DeploymentFile,
		DeploymentSHA256: input.DeploymentSHA256, StateRoot: stateRoot,
		Expected: expected, Timeout: 15 * time.Second}
	var selected []matchedcohort.FixedIdentity
	for index := range authority.Identities {
		identity := authority.Identities[index]
		if identity.Label == labelPair[0] || identity.Label == labelPair[1] {
			selected = append(selected, identity)
		}
	}
	if len(selected) != 2 {
		return lifecycleReport{}, errors.New("selected fixed-canary identity pair is invalid")
	}
	if err := probe.Preflight(selected[0], selected[1]); err != nil {
		return lifecycleReport{}, err
	}
	outcome, err := consumer.RunPreparedLifecycle(ctx, authority, prepared, launcher, probe)
	if err != nil {
		return lifecycleReport{}, err
	}
	return lifecycleReport{Schema: reportSchema, Environment: matchedcohort.EnvironmentSandbox, Operation: input.Operation,
		ReleaseID: input.ReleaseID, Phase: input.Phase, Attempt: input.Attempt, Transport: input.Transport,
		InputSHA256: matchedcohort.Digest(inputRaw), Authority: input.Authority, StateUpdates: stateUpdates,
		CommandSHA256: input.CommandSHA256, QURLBinarySHA256: input.QURLBinarySHA256, QURLSourceSHA: input.QURLSourceSHA,
		QURLGoSourceSHA: input.QURLGoSourceSHA, Lifecycle: &lifecycleReportOutcome{Status: "completed", Intent: prepared.IntentReference,
			PrimaryFirstKey: prepared.PrimaryFirstKey, SiblingKey: prepared.SiblingKey,
			ReplacementKey: prepared.ReplacementKey, Outcome: &outcome}}, nil
}

// validateAuthorityBlob binds the outer durable key and every canonical body
// field before any qurl runtime is constructed.
func validateAuthorityBlob(input lifecycleInput, authorityBlob matchedcohort.Blob) (matchedcohort.Authority, error) { //nolint:gocritic // Input and blob are immutable invocation snapshots.
	if authorityBlob.Key != input.Authority.Key || authorityBlob.VersionID != input.Authority.VersionID ||
		authorityBlob.SHA256 != input.Authority.SHA256 || matchedcohort.Digest(authorityBlob.Body) != authorityBlob.SHA256 {
		return matchedcohort.Authority{}, errors.New("fixed canary authority reference does not match durable storage")
	}
	var authority matchedcohort.Authority
	decoder := json.NewDecoder(bytes.NewReader(authorityBlob.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return matchedcohort.Authority{}, errors.New("fixed canary authority JSON is invalid")
	}
	canonicalAuthority, err := matchedcohort.CanonicalJSON(authority)
	if err != nil || !bytes.Equal(canonicalAuthority, authorityBlob.Body) || matchedcohort.ValidateAuthority(authority) != nil ||
		input.Authority.Key != "generations/"+authority.GenerationID+"/authority" ||
		authority.QURLGoSourceSHA != input.QURLGoSourceSHA {
		return matchedcohort.Authority{}, errors.New("fixed canary authority is not exact")
	}
	return authority, nil
}

func validateSelectedRoute(input lifecycleInput, cohort matchedcohort.CohortPlan) error { //nolint:gocritic // Both values are immutable authority snapshots.
	if input.AdmissionHub.Host != cohort.HubHost || input.AdmissionHub.Port != cohort.HubPort ||
		input.AdmissionHub.ServerPublicKeyB64 != cohort.HubServerPublicKeyB64 || input.AdmissionCell != cohort.CellEndpoint ||
		input.RecoveryEndpoint != cohort.CellEndpoint {
		return errors.New("customer route does not match shared cohort")
	}
	return nil
}

func startOutcomeServers(releaseID string, labels [2]string) (servers []*http.Server, targets map[string]string,
	expected map[string][]byte, err error,
) {
	servers = make([]*http.Server, 0, 2)
	targets = make(map[string]string, 2)
	expected = make(map[string][]byte, 2)
	var listenConfig net.ListenConfig
	for _, label := range labels {
		body := []byte("qurl sandbox matched cohort " + releaseID + " " + label + "\n")
		listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			stopOutcomeServers(servers)
			return nil, nil, nil, errors.New("start local lifecycle outcome server")
		}
		handlerBody := bytes.Clone(body)
		server := &http.Server{ReadHeaderTimeout: 2 * time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet || request.URL.Path != "/" {
				http.Error(writer, "not found", http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write(handlerBody)
		})}
		servers = append(servers, server)
		targets[label], expected[label] = listener.Addr().String(), body
		go func() { _ = server.Serve(listener) }()
	}
	return servers, targets, expected, nil
}

func stopOutcomeServers(servers []*http.Server) {
	for _, server := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
	}
}

func writeReport(path string, report *lifecycleReport) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return errors.New("encode lifecycle report")
	}
	raw = append(raw, '\n')
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("report directory is not owner-only")
	}
	tempPath := filepath.Join(parent, ".lifecycle-report.tmp")
	if err := reconcileReportTemporary(path, tempPath, raw); err != nil {
		return err
	}
	if current, readErr := readPrivateFile(path, maxInputSize); readErr == nil {
		if bytes.Equal(current, raw) {
			return nil
		}
		return errors.New("existing lifecycle report differs")
	} else if !errors.Is(readErr, os.ErrNotExist) && !os.IsNotExist(readErr) {
		if _, statErr := os.Lstat(path); statErr == nil {
			return errors.New("existing lifecycle report is unsafe")
		}
	}
	temp, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Parent is an exact owner-only directory.
	if err != nil {
		return errors.New("create lifecycle report temporary file")
	}
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := writeAll(temp, raw); err != nil || temp.Sync() != nil || temp.Close() != nil {
		return errors.New("persist lifecycle report bytes")
	}
	if err := os.Link(tempPath, path); err != nil {
		return errors.New("install lifecycle report without overwrite")
	}
	if err := os.Remove(tempPath); err != nil {
		return errors.New("remove lifecycle report temporary name")
	}
	return syncDirectory(parent)
}

// reconcileReportTemporary converges the two crash windows of the no-overwrite
// link install. A complete one-link temporary is installed; a strict prefix is
// a pre-fsync interrupted write and is removed; an installed two-link receipt
// is reduced to its final one-link form. No conflicting byte is normalized.
func reconcileReportTemporary(path, tempPath string, expected []byte) error {
	tempInfo, err := os.Lstat(tempPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("inspect lifecycle report temporary file")
	}
	targetInfo, targetErr := os.Lstat(path)
	switch {
	case targetErr == nil:
		if !os.SameFile(tempInfo, targetInfo) {
			return errors.New("lifecycle report temporary file conflicts with installed receipt")
		}
		raw, readErr := readReportTemporary(tempPath, 2, len(expected))
		if readErr != nil || !bytes.Equal(raw, expected) {
			return errors.New("installed lifecycle report temporary receipt is invalid")
		}
		if err := os.Remove(tempPath); err != nil {
			return errors.New("finish lifecycle report temporary unlink")
		}
		return syncDirectory(filepath.Dir(path))
	case !os.IsNotExist(targetErr):
		return errors.New("inspect installed lifecycle report")
	}
	raw, readErr := readReportTemporary(tempPath, 1, len(expected))
	if readErr != nil {
		return readErr
	}
	if bytes.Equal(raw, expected) {
		if err := os.Link(tempPath, path); err != nil {
			return errors.New("resume lifecycle report install")
		}
		if err := os.Remove(tempPath); err != nil {
			return errors.New("finish resumed lifecycle report install")
		}
		return syncDirectory(filepath.Dir(path))
	}
	if len(raw) >= len(expected) || !bytes.Equal(raw, expected[:len(raw)]) {
		return errors.New("lifecycle report temporary bytes conflict with expected receipt")
	}
	if err := os.Remove(tempPath); err != nil {
		return errors.New("remove interrupted lifecycle report temporary file")
	}
	return syncDirectory(filepath.Dir(path))
}

func linuxUnsigned64[T ~uint32 | ~uint64](value T) uint64 {
	return uint64(value)
}

func readReportTemporary(path string, expectedLinks uint64, limit int) ([]byte, error) { //nolint:gocyclo // One exact temporary-file metadata/readback fence stays together.
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() < 0 || before.Size() > int64(limit) {
		return nil, errors.New("lifecycle report temporary metadata is invalid")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || linuxUnsigned64(stat.Nlink) != expectedLinks || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return nil, errors.New("lifecycle report temporary ownership is invalid")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("open lifecycle report temporary file")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open lifecycle report temporary handle")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("lifecycle report temporary changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(raw) > limit || int64(len(raw)) != opened.Size() {
		return nil, errors.New("read lifecycle report temporary file")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("lifecycle report temporary changed while reading")
	}
	return raw, nil
}

func syncDirectory(parent string) error {
	directory, err := os.Open(parent) //nolint:gosec // Parent passed exact owner-only metadata checks.
	if err != nil {
		return errors.New("open lifecycle report directory")
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return errors.New("sync lifecycle report directory")
	}
	if err := directory.Close(); err != nil {
		return errors.New("close lifecycle report directory")
	}
	return nil
}

func readPrivateFile(path string, limit int64) ([]byte, error) { //nolint:gocyclo // One exact metadata/readback fence is kept together.
	if !cleanAbsolute(path) {
		return nil, errors.New("private input path is invalid")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || (before.Mode().Perm() != 0o400 && before.Mode().Perm() != 0o600) ||
		before.Size() <= 1 || before.Size() > limit {
		return nil, errors.New("private input metadata is invalid")
	}
	value, ok := before.Sys().(*syscall.Stat_t)
	if !ok || value.Nlink != 1 || (value.Uid != 0 && value.Uid != uint32(os.Geteuid())) {
		return nil, errors.New("private input ownership is invalid")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("open private input")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open private input handle")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("private input changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) != opened.Size() || raw[len(raw)-1] != '\n' ||
		bytes.Contains(raw[:len(raw)-1], []byte{'\n'}) || bytes.Contains(raw, []byte{'\r'}) {
		return nil, errors.New("private input bytes are invalid")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("private input changed while reading")
	}
	return raw, nil
}

func stableFileDigest(path string, executable bool) string { //nolint:gocyclo // One exact metadata/readback fence is kept together.
	if !cleanAbsolute(path) {
		return ""
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o022 != 0 ||
		(executable && before.Mode().Perm()&0o111 == 0) {
		return ""
	}
	value, ok := before.Sys().(*syscall.Stat_t)
	if !ok || value.Nlink != 1 || (value.Uid != 0 && value.Uid != uint32(os.Geteuid())) {
		return ""
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return ""
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return ""
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return ""
	}
	openedAfter, openErr := file.Stat()
	after, statErr := os.Lstat(path)
	if openErr != nil || statErr != nil || !os.SameFile(opened, openedAfter) || !os.SameFile(before, after) ||
		openedAfter.Size() != opened.Size() || !openedAfter.ModTime().Equal(opened.ModTime()) ||
		after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeAll(writer io.Writer, raw []byte) error {
	for len(raw) > 0 {
		count, err := writer.Write(raw)
		if err != nil {
			return err
		}
		if count <= 0 || count > len(raw) {
			return io.ErrShortWrite
		}
		raw = raw[count:]
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func hex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func hex40(value string) bool {
	return len(value) == 40 && hex64(value+strings.Repeat("0", 24))
}

func validRaw32Key(value string) bool {
	raw, err := base64.StdEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.StdEncoding.EncodeToString(raw) == value
}

func validateTransportDeployment(input lifecycleInput) (*qurl.Deployment, error) { //nolint:gocritic // Input is one immutable authority projection.
	deployment, err := qurl.LoadDeployment(input.DeploymentFile)
	if err != nil || deployment == nil || deployment.Hub == nil || *deployment.Hub != input.AdmissionHub {
		return nil, errors.New("customer deployment Hub does not match signed admission authority")
	}
	switch input.Transport {
	case transportDirect:
		if input.RelayHostname != "" || len(deployment.Cells) != 1 || len(deployment.RelayAllowlist) != 0 {
			return nil, errors.New("direct deployment transport is not exact")
		}
		cell := deployment.Cells[0]
		if cell.CellID == "" || cell.Host != input.AdmissionCell.Host || cell.Port != input.AdmissionCell.Port ||
			cell.ServerPublicKeyB64 != input.AdmissionCell.ServerPublicKeyB64 {
			return nil, errors.New("direct deployment cell does not match signed admission authority")
		}
	case transportRelay:
		if !validDeploymentHostname(input.RelayHostname) || len(deployment.Cells) != 0 ||
			len(deployment.RelayAllowlist) != 1 || deployment.RelayAllowlist[0] != input.RelayHostname {
			return nil, errors.New("relay deployment transport is not exact")
		}
	default:
		return nil, errors.New("customer deployment transport is invalid")
	}
	return deployment, nil
}

func selectedCohort(authority matchedcohort.Authority) (matchedcohort.CohortPlan, error) { //nolint:gocritic // Authority is one immutable projection.
	if len(authority.Cohorts) != 1 {
		return matchedcohort.CohortPlan{}, errors.New("shared cohort is absent")
	}
	return authority.Cohorts[0], nil
}

func validDeploymentHostname(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && strings.Contains(value, ".") &&
		!strings.ContainsAny(value, "/:@\x00\r\n")
}

func main() {
	args, err := parseArgs(os.Args[1:])
	if err == nil {
		var report lifecycleReport
		report, err = run(context.Background(), args)
		if err == nil {
			err = writeReport(args.reportPath, &report)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox matched-cohort lifecycle failed")
		os.Exit(1)
	}
}
