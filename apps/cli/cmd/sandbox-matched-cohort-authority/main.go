//go:build linux && !android

// Command sandbox-matched-cohort-authority is the sandbox-only fixed canary
// provisioning and rotation worker. The caller owns AWS and exposes the closed
// owner-only local authority socket. This process never loads AWS credentials.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"

	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/matchedcohort"
)

const (
	privateJSONMaxBytes = 2 << 20
	operationProvision  = "provision"
	operationRotate     = "rotate"
	sandboxAPIOrigin    = "https://api.layerv.xyz"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type provisionReport struct {
	Schema          int                          `json:"schema"`
	Environment     string                       `json:"environment"`
	Operation       string                       `json:"operation"`
	GenerationID    string                       `json:"generation_id"`
	NHPSourceSHA    string                       `json:"nhp_source_sha"`
	QURLGoSourceSHA string                       `json:"qurl_go_source_sha"`
	Authority       matchedcohort.StateReference `json:"authority"`
}

type rotationReport struct {
	Schema          int                          `json:"schema"`
	Environment     string                       `json:"environment"`
	Operation       string                       `json:"operation"`
	GenerationID    string                       `json:"generation_id"`
	NHPSourceSHA    string                       `json:"nhp_source_sha"`
	QURLGoSourceSHA string                       `json:"qurl_go_source_sha"`
	Registry        matchedcohort.StateReference `json:"registry"`
}

type commandInput struct {
	operation       string
	socketPath      string
	invocationToken string
	inputPath       string
	apiKeyPath      string
	reportPath      string
}

type fileEnrollmentAuthority struct {
	apiKey      string
	keyPrefix   string
	identityAPI interface {
		Me(context.Context) (*qurlapi.Identity, error)
	}
	otp *matchedcohort.AuthorityRPC
}

func (a fileEnrollmentAuthority) VerifyEnrollmentCredential(ctx context.Context, expectedOwner string) (matchedcohort.EnrollmentCredentialReceipt, error) {
	identity, err := a.identityAPI.Me(ctx)
	if err != nil {
		return matchedcohort.EnrollmentCredentialReceipt{}, fmt.Errorf("authenticate ordinary enrollment key: %w", err)
	}
	expectedScopes := []string{"qurl:agent", "qurl:read", "qurl:write"}
	if identity == nil || identity.OwnerID != expectedOwner || identity.AuthType != "api_key" || identity.Key == nil ||
		identity.Key.Kind != "api_key" || identity.Key.KeyPrefix != a.keyPrefix || identity.Key.ExpiresAt != nil ||
		!slices.Equal(identity.Key.Scopes, expectedScopes) {
		return matchedcohort.EnrollmentCredentialReceipt{}, errors.New("ordinary enrollment key identity contradicts the fixed owner contract")
	}
	return matchedcohort.EnrollmentCredentialReceipt{Schema: 1, OwnerID: identity.OwnerID, AuthType: identity.AuthType,
		KeyID: identity.Key.KeyID, Kind: identity.Key.Kind, Scopes: slices.Clone(identity.Key.Scopes), KeyPrefix: identity.Key.KeyPrefix}, nil
}

func (a fileEnrollmentAuthority) EnrollmentCredential(context.Context, matchedcohort.IdentityPlan) (string, error) {
	return a.apiKey, nil
}

func (a fileEnrollmentAuthority) OTP(ctx context.Context, identity matchedcohort.IdentityPlan, challenge qurl.AgentOTPChallenge) (string, error) { //nolint:gocritic // Implements the exact immutable enrollment callback.
	return a.otp.OTP(ctx, identity, challenge)
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox matched-cohort authority failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	input, err := parseCommand(args)
	if err != nil {
		return err
	}
	if err := validatePrivateOutputPath(input.reportPath); err != nil {
		return err
	}
	authority, err := matchedcohort.NewAuthorityRPC(input.socketPath)
	if err != nil {
		return err
	}
	if input.operation == operationProvision {
		return runProvision(ctx, authority, &input)
	}
	return runRotation(ctx, authority, &input)
}

func parseCommand(args []string) (commandInput, error) {
	if len(args) == 0 {
		return commandInput{}, errors.New("operation must be provision or rotate")
	}
	if args[0] != operationProvision && args[0] != operationRotate {
		return commandInput{}, errors.New("operation must be provision or rotate")
	}
	provision := args[0] == operationProvision
	if (provision && (len(args) != 11 || args[7] != "--api-key-file" || args[9] != "--report-file")) ||
		(!provision && (len(args) != 9 || args[7] != "--report-file")) ||
		args[1] != "--authority-socket" || args[3] != "--invocation-token" || args[5] != "--input-file" {
		return commandInput{}, errors.New("usage: provision|rotate --authority-socket ABS --invocation-token 64HEX --input-file ABS --api-key-file ABS --report-file ABS")
	}
	operation, socketPath, invocationToken, inputPath := args[0], args[2], args[4], args[6]
	apiKeyPath, reportPath := "", args[8]
	if provision {
		apiKeyPath, reportPath = args[8], args[10]
	}
	if !hex64.MatchString(invocationToken) {
		return commandInput{}, errors.New("invocation token must be exact lowercase 64-hex")
	}
	return commandInput{operation: operation, socketPath: socketPath, invocationToken: invocationToken,
		inputPath: inputPath, apiKeyPath: apiKeyPath, reportPath: reportPath}, nil
}

func runProvision(ctx context.Context, authority *matchedcohort.AuthorityRPC, input *commandInput) error {
	var plan matchedcohort.Plan
	if err := readCanonicalPrivateJSON(input.inputPath, &plan); err != nil {
		return fmt.Errorf("read provisioning plan: %w", err)
	}
	apiKey, err := readExactAPIKey(input.apiKeyPath)
	if err != nil {
		return fmt.Errorf("read ordinary API key: %w", err)
	}
	defer clear(apiKey)
	apiClient, err := qurlapi.New(&qurlapi.Config{BaseURL: sandboxAPIOrigin, APIKey: string(apiKey), Version: "sandbox-matched-cohort-authority"})
	if err != nil {
		return fmt.Errorf("construct sandbox identity client: %w", err)
	}
	enrollment := fileEnrollmentAuthority{apiKey: string(apiKey), keyPrefix: string(apiKey[:12]), identityAPI: apiClient, otp: authority}
	result, err := (&matchedcohort.Provisioner{Blobs: authority, Credentials: enrollment, WriterLock: authority,
		InvocationToken: input.invocationToken}).Provision(ctx, plan)
	if err != nil {
		return err
	}
	report := provisionReport{Schema: 1, Environment: matchedcohort.EnvironmentSandbox, Operation: operationProvision,
		GenerationID: result.Authority.GenerationID, NHPSourceSHA: result.Authority.NHPSourceSHA,
		QURLGoSourceSHA: result.Authority.QURLGoSourceSHA, Authority: result.Reference}
	return writeCanonicalPrivateJSON(input.reportPath, report)
}

func readExactAPIKey(path string) ([]byte, error) {
	info, err := privateRegularFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := readPrivateFile(path, info)
	if err != nil {
		return nil, err
	}
	if len(raw) < 2 || raw[len(raw)-1] != '\n' || bytes.ContainsAny(raw[:len(raw)-1], " \t\r\n") {
		clear(raw)
		return nil, errors.New("API key file must contain one exact key and one terminal LF")
	}
	key := raw[:len(raw)-1]
	if err := auth.ValidateKeyShape(string(key)); err != nil {
		clear(raw)
		return nil, err
	}
	result := bytes.Clone(key)
	clear(raw)
	return result, nil
}

func runRotation(ctx context.Context, authority *matchedcohort.AuthorityRPC, command *commandInput) error {
	var input provisionReport
	if err := readCanonicalPrivateJSON(command.inputPath, &input); err != nil {
		return fmt.Errorf("read provisioning receipt: %w", err)
	}
	if input.Schema != 1 || input.Environment != matchedcohort.EnvironmentSandbox || input.Operation != operationProvision ||
		!hex64.MatchString(input.GenerationID) || input.NHPSourceSHA != matchedcohort.RequiredNHPSourceSHA ||
		input.QURLGoSourceSHA != matchedcohort.RequiredQURLGoSourceSHA ||
		input.Authority.Key != "generations/"+input.GenerationID+"/authority" {
		return errors.New("provisioning receipt is not exact")
	}
	blob, err := authority.Load(ctx, input.Authority.Key)
	if err != nil || blob.VersionID != input.Authority.VersionID || blob.SHA256 != input.Authority.SHA256 {
		return errors.New("provisioned authority readback is not exact")
	}
	var completed matchedcohort.Authority
	if err := decodeCanonical(blob.Body, &completed); err != nil || matchedcohort.ValidateAuthority(completed) != nil ||
		completed.GenerationID != input.GenerationID || completed.NHPSourceSHA != input.NHPSourceSHA ||
		completed.QURLGoSourceSHA != input.QURLGoSourceSHA {
		return errors.New("provisioned authority body is not exact")
	}
	_, reference, err := (&matchedcohort.Rotator{Blobs: authority, WriterLock: authority,
		RegistryKey: "fixed-canary/sandbox/registry", InvocationToken: command.invocationToken}).Activate(ctx,
		matchedcohort.ProvisionedGeneration{Authority: completed, Reference: input.Authority})
	if err != nil {
		return err
	}
	report := rotationReport{Schema: 1, Environment: matchedcohort.EnvironmentSandbox, Operation: operationRotate,
		GenerationID: completed.GenerationID, NHPSourceSHA: completed.NHPSourceSHA,
		QURLGoSourceSHA: completed.QURLGoSourceSHA, Registry: reference}
	return writeCanonicalPrivateJSON(command.reportPath, report)
}

func readCanonicalPrivateJSON(path string, value any) error {
	info, err := privateRegularFile(path)
	if err != nil {
		return err
	}
	if info.Size() < 2 || info.Size() > privateJSONMaxBytes {
		return errors.New("private JSON size is invalid")
	}
	raw, err := readPrivateFile(path, info)
	if err != nil {
		return err
	}
	if raw[len(raw)-1] != '\n' || bytes.ContainsAny(raw[:len(raw)-1], "\r\n") {
		return errors.New("private JSON must have exactly one terminal LF")
	}
	return decodeCanonical(raw[:len(raw)-1], value)
}

func readPrivateFile(path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // Absolute clean path and exact lstat/fstat identity are required below.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	observed, err := file.Stat()
	if err != nil || !os.SameFile(expected, observed) {
		return nil, errors.New("private file identity changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, privateJSONMaxBytes+1))
	if err != nil || len(raw) > privateJSONMaxBytes || int64(len(raw)) != observed.Size() {
		return nil, errors.New("private file read is incomplete")
	}
	return raw, nil
}

func decodeCanonical(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("private JSON schema is not exact")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, raw) {
		return errors.New("private JSON is not canonical")
	}
	return nil
}

func privateRegularFile(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("private path must be absolute and clean")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		(info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) {
		return nil, errors.New("private file must be owner-only regular file with one link")
	}
	return info, nil
}

func validatePrivateOutputPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("report path must be absolute and clean")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	stat, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !parent.IsDir() || parent.Mode().Perm() != 0o700 || parent.Mode()&os.ModeSymlink != 0 {
		return errors.New("report directory must be owner-only")
	}
	if _, err := os.Lstat(path); err == nil {
		if _, privateErr := privateRegularFile(path); privateErr != nil {
			return errors.New("existing report is not owner-only")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeCanonicalPrivateJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if info, statErr := os.Lstat(path); statErr == nil {
		existing, readErr := readPrivateFile(path, info)
		if readErr != nil {
			return readErr
		}
		if bytes.Equal(existing, raw) {
			return nil
		}
		return errors.New("existing report bytes conflict")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // Parent is exact owner-only and the leaf is fixed plus O_EXCL/O_NOFOLLOW.
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if err := writeAll(file, raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link installs the completed inode only when the final path is absent. An
	// ordinary rename would replace a competing report after preflight.
	if err := os.Link(temporary, path); err != nil {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return err
		}
		existing, readErr := readPrivateFile(path, info)
		if readErr != nil || !bytes.Equal(existing, raw) {
			return err
		}
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeAll(writer io.Writer, raw []byte) error {
	for len(raw) > 0 {
		written, err := writer.Write(raw)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(raw) {
			return io.ErrShortWrite
		}
		raw = raw[written:]
	}
	return nil
}
