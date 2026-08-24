//go:build linux && !android

package matchedcohort

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestAuthorityRPCBlobOTPAndWriterBoundary(t *testing.T) {
	server := newFakeAuthorityRPCServer(t)
	client, err := NewAuthorityRPC(server.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Load(ctx, "fixed/missing"); !errors.Is(err, errStateNotFound) {
		t.Fatalf("missing Load = %v", err)
	}
	body := []byte(`{"schema":1}`)
	candidate := BlobCandidate{Key: "fixed/blob", OperationID: strings.Repeat("1", 64), SHA256: Digest(body), Body: body}
	committed, err := client.Commit(ctx, candidate)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	loaded, err := client.Load(ctx, candidate.Key)
	if err != nil || !sameCommittedBlob(loaded, candidate) || committed.VersionID != loaded.VersionID {
		t.Fatalf("Load after commit = %#v %#v %v", committed, loaded, err)
	}

	plan := validPlan("6")
	operation := CredentialWriterOperation{Schema: 1, OwnerSubject: plan.OwnerSubject, Operation: "provision",
		GenerationID: plan.GenerationID, PlanSHA256: strings.Repeat("2", 64), InvocationToken: strings.Repeat("3", 64)}
	err = client.WithExclusive(ctx, operation, func(locked context.Context) error {
		otp, otpErr := client.OTP(locked, plan.Identities[0], qurl.AgentOTPChallenge{AgentID: plan.Identities[0].AgentID,
			CredentialKeyID: "credential-key", CellID: plan.Cohorts[0].CellID,
			AssignmentTicketExpiresAt: time.Unix(1_800_000_000, 123).UTC(), PendingActivationRecovery: true})
		if otpErr != nil || otp != "123456" {
			t.Fatalf("OTP = %q %v", otp, otpErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithExclusive: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.acquireCount != 1 || server.releaseCount != 1 || server.otpCount != 1 || server.credentialOutsideLock {
		t.Fatalf("server counters = %#v", server)
	}
}

func TestAuthorityRPCCallbackFailureRetainsDurableWriter(t *testing.T) {
	server := newFakeAuthorityRPCServer(t)
	client, err := NewAuthorityRPC(server.path)
	if err != nil {
		t.Fatal(err)
	}
	plan := validPlan("7")
	want := errors.New("network boundary failed")
	err = client.WithExclusive(context.Background(), CredentialWriterOperation{Schema: 1, OwnerSubject: plan.OwnerSubject,
		Operation: "provision", GenerationID: plan.GenerationID, PlanSHA256: strings.Repeat("4", 64), InvocationToken: strings.Repeat("5", 64)},
		func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("WithExclusive error = %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.acquireCount != 1 || server.releaseCount != 0 {
		t.Fatalf("failed callback released durable writer: acquire=%d release=%d", server.acquireCount, server.releaseCount)
	}
}

func TestAuthorityRPCRejectsUnsafePathAndNoncanonicalResponse(t *testing.T) {
	if _, err := NewAuthorityRPC("relative.sock"); err == nil {
		t.Fatal("relative socket accepted")
	}
	directory := shortPrivateTempDir(t)
	if err := os.Chmod(directory, 0o755); err != nil { //nolint:gosec // Fixture proves a non-private directory is rejected.
		t.Fatal(err)
	}
	if _, err := NewAuthorityRPC(filepath.Join(directory, "authority.sock")); err == nil {
		t.Fatal("non-private socket directory accepted")
	}
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directory requires owner traversal for the Unix socket.
		t.Fatal(err)
	}
	path := filepath.Join(directory, "authority.sock")
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = bufio.NewReader(connection).ReadBytes('\n')
		_, _ = connection.Write([]byte(`{ "schema":1,"status":"not_found"}` + "\n"))
	}()
	client, err := NewAuthorityRPC(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Load(context.Background(), "fixed/missing"); !errors.Is(err, errStateConflict) {
		t.Fatalf("noncanonical response = %v", err)
	}
}

type fakeAuthorityRPCServer struct {
	t      *testing.T
	path   string
	listen net.Listener

	mu                    sync.Mutex
	serial                int
	blobs                 map[string]authorityRPCBlob
	acquireCount          int
	releaseCount          int
	otpCount              int
	credentialOutsideLock bool
}

func newFakeAuthorityRPCServer(t *testing.T) *fakeAuthorityRPCServer {
	t.Helper()
	directory := shortPrivateTempDir(t)
	server := &fakeAuthorityRPCServer{t: t, path: filepath.Join(directory, "authority.sock"), blobs: map[string]authorityRPCBlob{}}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(server.path, 0o600); err != nil {
		t.Fatal(err)
	}
	server.listen = listener
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func shortPrivateTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "matched-cohort-authority-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Directory requires owner traversal for the Unix socket.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func (s *fakeAuthorityRPCServer) serve() {
	for {
		connection, err := s.listen.Accept()
		if err != nil {
			return
		}
		go s.serveConnection(connection)
	}
}

func (s *fakeAuthorityRPCServer) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	writerHeld := false
	for {
		raw, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request authorityRPCRequest
		if err := json.Unmarshal(raw[:len(raw)-1], &request); err != nil {
			return
		}
		response := s.response(request, writerHeld)
		if request.Operation == "writer_acquire" && response.Status == "ok" {
			writerHeld = true
		}
		if request.Operation == "writer_release" && response.Status == "ok" {
			writerHeld = false
		}
		encoded, _ := json.Marshal(response)
		if err := writeAuthorityRPC(connection, append(encoded, '\n')); err != nil {
			return
		}
	}
}

func (s *fakeAuthorityRPCServer) response(request authorityRPCRequest, writerHeld bool) authorityRPCResponse { //nolint:gocritic // Closed fixture protocol is explicit.
	s.mu.Lock()
	defer s.mu.Unlock()
	response := authorityRPCResponse{Schema: authorityRPCSchema}
	switch request.Operation {
	case "blob_load":
		value, ok := s.blobs[request.Key]
		if !ok {
			response.Status = "not_found"
			return response
		}
		response.Status, response.Blob = "ok", &value
	case "blob_commit":
		candidate := request.Candidate
		if candidate == nil {
			response.Status, response.Error = "rejected", "missing candidate"
			return response
		}
		current, exists := s.blobs[candidate.Key]
		if (exists && current.VersionID != candidate.ExpectedVersion) || (!exists && candidate.ExpectedVersion != "") {
			response.Status = "conflict"
			return response
		}
		s.serial++
		value := authorityRPCBlob{Key: candidate.Key, VersionID: "version-" + string(rune('0'+s.serial)), PreviousVersion: candidate.ExpectedVersion,
			OperationID: candidate.OperationID, SHA256: candidate.SHA256, BodyB64: candidate.BodyB64}
		s.blobs[candidate.Key] = value
		response.Status, response.Blob = "ok", &value
	case "writer_acquire":
		s.acquireCount++
		response.Status, response.WriterToken = "ok", strings.Repeat("a", 64)
	case "writer_release":
		if !writerHeld || request.WriterToken != strings.Repeat("a", 64) {
			response.Status, response.Error = "rejected", "writer not held"
			return response
		}
		s.releaseCount++
		response.Status = "ok"
	case "enrollment_otp":
		s.otpCount++
		s.credentialOutsideLock = s.credentialOutsideLock || !writerHeld
		response.Status, response.OTP = "ok", "123456"
	default:
		response.Status, response.Error = "rejected", "unknown operation"
	}
	return response
}

func TestWriteAuthorityRPCHandlesShortAndZeroWrites(t *testing.T) {
	short := &shortRPCWriter{maximum: 2}
	if err := writeAuthorityRPC(short, []byte("abcdef")); err != nil || string(short.value) != "abcdef" {
		t.Fatalf("short write = %q %v", short.value, err)
	}
	zero := &shortRPCWriter{zero: true}
	if err := writeAuthorityRPC(zero, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write = %v", err)
	}
}

type shortRPCWriter struct {
	maximum int
	zero    bool
	value   []byte
}

func (w *shortRPCWriter) Write(raw []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	count := min(len(raw), w.maximum)
	w.value = append(w.value, raw[:count]...)
	return count, nil
}
