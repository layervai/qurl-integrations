package matchedcohort

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	authorityRPCSchema       = 1
	authorityRPCMaxBytes     = 16 << 20
	authorityRPCDialTimeout  = 2 * time.Second
	authorityRPCWriteTimeout = 5 * time.Second
)

// AuthorityRPC is the credential-free qurl child side of the orchestration
// authority boundary. The orchestration process owns AWS clients and exposes
// one owner-only Unix socket. No AWS credential or secret container authority
// enters the qurl child.
//
// One persistent socket is held for the complete credential-writer callback.
// This gives the orchestration server an exact live-holder signal while its
// durable lock row remains the crash/restart authority. Calls outside that
// callback use one short connection each.
type AuthorityRPC struct {
	SocketPath string

	mu   sync.Mutex
	held *authorityRPCConn
}

type authorityRPCRequest struct {
	Schema      int                        `json:"schema"`
	Operation   string                     `json:"operation"`
	Key         string                     `json:"key,omitempty"`
	Candidate   *authorityRPCBlobCandidate `json:"candidate,omitempty"`
	Identity    *IdentityPlan              `json:"identity,omitempty"`
	Challenge   *authorityRPCOTPChallenge  `json:"challenge,omitempty"`
	Writer      *CredentialWriterOperation `json:"writer,omitempty"`
	WriterToken string                     `json:"writer_token,omitempty"`
}

type authorityRPCOTPChallenge struct {
	AgentID                   string `json:"agent_id"`
	CredentialKeyID           string `json:"credential_key_id"`
	CellID                    string `json:"cell_id"`
	AssignmentTicketExpiresAt string `json:"assignment_ticket_expires_at"`
	PendingActivationRecovery bool   `json:"pending_activation_recovery"`
}

type authorityRPCBlobCandidate struct {
	Key             string `json:"key"`
	ExpectedVersion string `json:"expected_version"`
	OperationID     string `json:"operation_id"`
	SHA256          string `json:"sha256"`
	BodyB64         string `json:"body_b64"`
}

type authorityRPCResponse struct {
	Schema      int               `json:"schema"`
	Status      string            `json:"status"`
	Error       string            `json:"error,omitempty"`
	Blob        *authorityRPCBlob `json:"blob,omitempty"`
	Credential  string            `json:"credential,omitempty"`
	OTP         string            `json:"otp,omitempty"`
	WriterToken string            `json:"writer_token,omitempty"`
}

type authorityRPCBlob struct {
	Key             string `json:"key"`
	VersionID       string `json:"version_id"`
	PreviousVersion string `json:"previous_version"`
	OperationID     string `json:"operation_id"`
	SHA256          string `json:"sha256"`
	BodyB64         string `json:"body_b64"`
}

type authorityRPCConn struct {
	net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

// NewAuthorityRPC validates one fixed absolute owner-only socket path.
func NewAuthorityRPC(socketPath string) (*AuthorityRPC, error) {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath || len(socketPath) > 100 {
		return nil, fmt.Errorf("%w: authority socket path", errInvalidAuthority)
	}
	parent, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 || parent.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(parent) {
		return nil, fmt.Errorf("%w: authority socket directory", errInvalidAuthority)
	}
	socket, err := os.Lstat(socketPath)
	if err != nil || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm() != 0o600 || socket.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(socket) {
		return nil, fmt.Errorf("%w: authority socket", errInvalidAuthority)
	}
	return &AuthorityRPC{SocketPath: socketPath}, nil
}

// Load implements BlobAuthority through the closed orchestration socket.
func (a *AuthorityRPC) Load(ctx context.Context, key string) (Blob, error) {
	if !validText(key) {
		return Blob{}, fmt.Errorf("%w: blob key", errInvalidAuthority)
	}
	response, err := a.call(ctx, authorityRPCRequest{Schema: authorityRPCSchema, Operation: "blob_load", Key: key})
	if err != nil {
		return Blob{}, err
	}
	if response.Status == "not_found" {
		return Blob{}, errStateNotFound
	}
	if response.Status != "ok" || response.Blob == nil || response.Error != "" || response.Credential != "" || response.OTP != "" || response.WriterToken != "" {
		return Blob{}, fmt.Errorf("%w: load response", errStateConflict)
	}
	return decodeRPCBlob(*response.Blob)
}

// Commit implements exact CAS BlobAuthority mutation through orchestration.
func (a *AuthorityRPC) Commit(ctx context.Context, candidate BlobCandidate) (Blob, error) { //nolint:gocritic // Implements the immutable interface.
	if !validText(candidate.Key) || !hex64Pattern.MatchString(candidate.OperationID) || !hex64Pattern.MatchString(candidate.SHA256) ||
		Digest(candidate.Body) != candidate.SHA256 {
		return Blob{}, fmt.Errorf("%w: blob candidate", errInvalidAuthority)
	}
	request := authorityRPCRequest{Schema: authorityRPCSchema, Operation: "blob_commit", Candidate: &authorityRPCBlobCandidate{
		Key: candidate.Key, ExpectedVersion: candidate.ExpectedVersion, OperationID: candidate.OperationID,
		SHA256: candidate.SHA256, BodyB64: base64.StdEncoding.EncodeToString(candidate.Body),
	}}
	response, err := a.call(ctx, request)
	if err != nil {
		return Blob{}, err
	}
	switch response.Status {
	case "conflict":
		return Blob{}, errStateConflict
	case "ambiguous":
		return Blob{}, errStateAmbiguous
	case "ok":
	default:
		return Blob{}, fmt.Errorf("%w: commit status", errStateConflict)
	}
	if response.Blob == nil || response.Error != "" || response.Credential != "" || response.OTP != "" || response.WriterToken != "" {
		return Blob{}, fmt.Errorf("%w: commit response", errStateConflict)
	}
	return decodeRPCBlob(*response.Blob)
}

// OTP is available only while the exact writer session is held.
func (a *AuthorityRPC) OTP(ctx context.Context, identity IdentityPlan, challenge qurl.AgentOTPChallenge) (string, error) { //nolint:gocritic // Implements immutable enrollment authority.
	wireChallenge := authorityRPCOTPChallenge{AgentID: challenge.AgentID, CredentialKeyID: challenge.CredentialKeyID,
		CellID: challenge.CellID, AssignmentTicketExpiresAt: challenge.AssignmentTicketExpiresAt.UTC().Format(time.RFC3339Nano),
		PendingActivationRecovery: challenge.PendingActivationRecovery}
	response, err := a.callHeld(ctx, authorityRPCRequest{Schema: authorityRPCSchema, Operation: "enrollment_otp", Identity: &identity, Challenge: &wireChallenge})
	if err != nil {
		return "", err
	}
	if response.Status != "ok" || !validText(response.OTP) || response.Blob != nil || response.Credential != "" || response.WriterToken != "" || response.Error != "" {
		return "", fmt.Errorf("%w: enrollment OTP response", errStateConflict)
	}
	return response.OTP, nil
}

// WithExclusive holds one live RPC connection for the complete operation. A
// callback failure closes the connection without releasing the durable row, so
// a successor must resume the exact persisted operation. Release is attempted
// only after the callback succeeds.
func (a *AuthorityRPC) WithExclusive(ctx context.Context, operation CredentialWriterOperation, fn func(context.Context) error) error { //nolint:gocritic,gocyclo // One live connection deliberately spans validation, acquire, callback, and exact release.
	if fn == nil || operation.Schema != 1 || !validText(operation.OwnerSubject) ||
		(operation.Operation != "provision" && operation.Operation != "rotate" && operation.Operation != "normal-release") ||
		!hex64Pattern.MatchString(operation.GenerationID) || !hex64Pattern.MatchString(operation.PlanSHA256) ||
		!hex64Pattern.MatchString(operation.InvocationToken) {
		return fmt.Errorf("%w: credential writer operation", errInvalidAuthority)
	}
	connection, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	response, err := connection.call(ctx, authorityRPCRequest{Schema: authorityRPCSchema, Operation: "writer_acquire", Writer: &operation})
	if err != nil {
		return err
	}
	if response.Status != "ok" || !hex64Pattern.MatchString(response.WriterToken) || response.Blob != nil || response.Credential != "" || response.OTP != "" || response.Error != "" {
		return fmt.Errorf("%w: writer acquire response", errStateConflict)
	}

	a.mu.Lock()
	if a.held != nil {
		a.mu.Unlock()
		return errStateConflict
	}
	a.held = connection
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.held == connection {
			a.held = nil
		}
		a.mu.Unlock()
	}()
	if err := fn(ctx); err != nil {
		return err
	}
	response, err = connection.call(ctx, authorityRPCRequest{Schema: authorityRPCSchema, Operation: "writer_release", WriterToken: response.WriterToken})
	if err != nil {
		return err
	}
	if response.Status != "ok" || response.WriterToken != "" || response.Blob != nil || response.Credential != "" || response.OTP != "" || response.Error != "" {
		return fmt.Errorf("%w: writer release response", errStateConflict)
	}
	return nil
}

func (a *AuthorityRPC) call(ctx context.Context, request authorityRPCRequest) (authorityRPCResponse, error) { //nolint:gocritic // Request is an immutable wire snapshot.
	a.mu.Lock()
	held := a.held
	a.mu.Unlock()
	if held != nil {
		return held.call(ctx, request)
	}
	connection, err := a.dial(ctx)
	if err != nil {
		return authorityRPCResponse{}, err
	}
	defer func() { _ = connection.Close() }()
	return connection.call(ctx, request)
}

func (a *AuthorityRPC) callHeld(ctx context.Context, request authorityRPCRequest) (authorityRPCResponse, error) { //nolint:gocritic // Request is an immutable wire snapshot.
	a.mu.Lock()
	held := a.held
	a.mu.Unlock()
	if held == nil {
		return authorityRPCResponse{}, fmt.Errorf("%w: credential writer is not held", errStateConflict)
	}
	return held.call(ctx, request)
}

func (a *AuthorityRPC) dial(ctx context.Context) (*authorityRPCConn, error) {
	dialer := net.Dialer{Timeout: authorityRPCDialTimeout}
	connection, err := dialer.DialContext(ctx, "unix", a.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect orchestration authority: %w", err)
	}
	return &authorityRPCConn{Conn: connection, reader: bufio.NewReaderSize(connection, 64<<10)}, nil
}

func (c *authorityRPCConn) call(ctx context.Context, request authorityRPCRequest) (authorityRPCResponse, error) { //nolint:gocritic // Request is an immutable wire snapshot.
	c.mu.Lock()
	defer c.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.SetDeadline(deadline); err != nil {
			return authorityRPCResponse{}, err
		}
	} else if err := c.SetDeadline(time.Now().Add(authorityRPCWriteTimeout)); err != nil {
		return authorityRPCResponse{}, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return authorityRPCResponse{}, err
	}
	if len(raw) > authorityRPCMaxBytes {
		return authorityRPCResponse{}, fmt.Errorf("%w: authority request too large", errInvalidAuthority)
	}
	if err := writeAuthorityRPC(c, append(raw, '\n')); err != nil {
		return authorityRPCResponse{}, fmt.Errorf("write orchestration authority request: %w", err)
	}
	line, err := readAuthorityRPCLine(c.reader)
	if err != nil {
		return authorityRPCResponse{}, fmt.Errorf("read orchestration authority response: %w", err)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var response authorityRPCResponse
	if err := decoder.Decode(&response); err != nil || decoder.Decode(&struct{}{}) != io.EOF || response.Schema != authorityRPCSchema {
		return authorityRPCResponse{}, fmt.Errorf("%w: authority response JSON", errStateConflict)
	}
	canonical, err := json.Marshal(response)
	if err != nil || !bytes.Equal(canonical, line) {
		return authorityRPCResponse{}, fmt.Errorf("%w: authority response is not canonical", errStateConflict)
	}
	return response, nil
}

func readAuthorityRPCLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 64<<10)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > authorityRPCMaxBytes {
			return nil, fmt.Errorf("%w: authority response too large", errStateConflict)
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func writeAuthorityRPC(writer io.Writer, raw []byte) error {
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

func decodeRPCBlob(wire authorityRPCBlob) (Blob, error) { //nolint:gocritic // Wire value is an immutable receipt.
	body, err := base64.StdEncoding.Strict().DecodeString(wire.BodyB64)
	if err != nil {
		return Blob{}, fmt.Errorf("%w: blob body encoding", errStateConflict)
	}
	blob := Blob{Key: wire.Key, VersionID: wire.VersionID, PreviousVersion: wire.PreviousVersion,
		OperationID: wire.OperationID, SHA256: wire.SHA256, Body: body}
	if !validText(blob.Key) || !validText(blob.VersionID) || !hex64Pattern.MatchString(blob.OperationID) ||
		!hex64Pattern.MatchString(blob.SHA256) || Digest(blob.Body) != blob.SHA256 {
		return Blob{}, fmt.Errorf("%w: blob receipt", errStateConflict)
	}
	return blob, nil
}
