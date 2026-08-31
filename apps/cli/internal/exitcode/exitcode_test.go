package exitcode

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/crid"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionconfig"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

// cliSentinels is the authoritative mapping of every CLI-defined sentinel to
// its exit code. TestEverySentinelIsMapped scans the apps/cli source tree
// and fails when a sentinel exists that has no row here — adding an error
// without deciding its exit code is a build failure, not a silent exit 1.
var cliSentinels = map[string]struct {
	err  error
	code int
}{
	"auth.ErrNoCredential":          {auth.ErrNoCredential, Auth},
	"auth.ErrInvalidKey":            {auth.ErrInvalidKey, Auth},
	"auth.ErrCredentialConflict":    {auth.ErrCredentialConflict, Conflict},
	"auth.ErrDeviceAccountConflict": {auth.ErrDeviceAccountConflict, Conflict},
	"config.ErrInvalidProfileName":  {config.ErrInvalidProfileName, Config},
	"config.ErrConfigFile":          {config.ErrConfigFile, Config},
	"config.ErrSecretInConfig":      {config.ErrSecretInConfig, Config},
	"cridux.ErrUnusableID":          {cridux.ErrUnusableID, InvalidInput},
	"cridux.ErrTestIDOnProduction":  {cridux.ErrTestIDOnProduction, Usage},
	"consume.ErrPipedNeedsFile":     {consume.ErrPipedNeedsFile, Usage},
	"consume.ErrFileExists":         {consume.ErrFileExists, Conflict},
	"consume.ErrLinkExpired":        {consume.ErrLinkExpired, NotFound},
	"consume.ErrLinkUnavailable":    {consume.ErrLinkUnavailable, Unavailable},
	"consume.ErrLinkFetch":          {consume.ErrLinkFetch, ServerError},
	"consume.ErrUnopenableLink":     {consume.ErrUnopenableLink, ServerError},

	// Platform access flow (direct downloads through the SDK opener). The
	// two settings sentinels share the Hub triple's Config row; the local
	// link check shares CRID verification's fail-closed row; a platform
	// deny is Forbidden and a platform defer is Unavailable.
	"consume.ErrAccessNotConfigured":        {consume.ErrAccessNotConfigured, Config},
	"consume.ErrAccessSettingsMismatch":     {consume.ErrAccessSettingsMismatch, Config},
	"consume.ErrLinkVerification":           {consume.ErrLinkVerification, VerificationFailed},
	"consume.ErrAccessDenied":               {consume.ErrAccessDenied, Forbidden},
	"consume.ErrAccessBusy":                 {consume.ErrAccessBusy, Unavailable},
	"daemon.ErrAlreadyRunning":              {connectordaemon.ErrAlreadyRunning, Conflict},
	"daemon.ErrResourceGone":                {connectordaemon.ErrResourceGone, NotFound},
	"state.ErrNoDefaultStateDir":            {state.ErrNoDefaultStateDir, Config},
	"state.ErrLocalShareOwnerConflict":      {state.ErrLocalShareOwnerConflict, Conflict},
	"state.ErrLocalShareVersionUnsupported": {state.ErrLocalShareVersionUnsupported, Config},

	// A same-Connector response contradiction is a fail-closed verification
	// failure; a cross-Connector alias is valid identity in conflicting state.
	"state.ErrConnectorResourceVerification":  {state.ErrConnectorResourceVerification, VerificationFailed},
	"state.ErrConnectorResourceStateConflict": {state.ErrConnectorResourceStateConflict, Conflict},
	"state.ErrConnectorResourceRetired":       {state.ErrConnectorResourceRetired, Conflict},
	// The Hub trust triple (or a dark build's absent pin) is configuration
	// even though it lives in the environment.
	"hub.ErrConfig":           {hub.ErrConfig, Config},
	"sessionconfig.ErrConfig": {sessionconfig.ErrConfig, Config},
}

// sdkSentinels pins the mapping for every qurl-go sentinel the CLI can
// surface. The SDK is version-pinned, so this list is fixed per release.
var sdkSentinels = map[string]struct {
	err  error
	code int
}{
	"qurl.ErrTemporaryAccessLinksDisabled":        {qurl.ErrTemporaryAccessLinksDisabled, Unavailable},
	"qurl.ErrNoCRID":                              {qurl.ErrNoCRID, VerificationFailed},
	"qurl.ErrCRIDMismatch":                        {qurl.ErrCRIDMismatch, VerificationFailed},
	"qurl.ErrInvalidClientConfig":                 {qurl.ErrInvalidClientConfig, Config},
	"qurl.ErrInvalidResourceRequest":              {qurl.ErrInvalidResourceRequest, InvalidInput},
	"qurl.ErrInvalidPortalRequest":                {qurl.ErrInvalidPortalRequest, InvalidInput},
	"qurl.ErrInvalidAPIResponse":                  {qurl.ErrInvalidAPIResponse, ServerError},
	"qurl.ErrCredentialStateNotFound":             {qurl.ErrCredentialStateNotFound, Auth},
	"qurl.ErrInsecureCredentialStatePermissions":  {qurl.ErrInsecureCredentialStatePermissions, Auth},
	"qurl.ErrDeviceCredentialMissing":             {qurl.ErrDeviceCredentialMissing, Auth},
	"qurl.ErrCredentialRecoveryRequired":          {qurl.ErrCredentialRecoveryRequired, Auth},
	"qurl.ErrEndpointNoReply":                     {qurl.ErrEndpointNoReply, Unavailable},
	"qurl.ErrInvalidRegisterConfig":               {qurl.ErrInvalidRegisterConfig, Config},
	"qurl.ErrAgentBindingPersistence":             {qurl.ErrAgentBindingPersistence, General},
	"qurl.ErrAgentCompletionCandidatePersistence": {qurl.ErrAgentCompletionCandidatePersistence, General},
	"qurl.ErrAgentSetupLock":                      {qurl.ErrAgentSetupLock, General},
	"qurl.ErrKeyRejected":                         {qurl.ErrKeyRejected, Auth},
	"qurl.ErrBootstrapSetupKeyConsumed":           {qurl.ErrBootstrapSetupKeyConsumed, Auth},
	"qurl.ErrAgentIdentityConflict":               {qurl.ErrAgentIdentityConflict, Conflict},
	"qurl.ErrRegistrationDisabled":                {qurl.ErrRegistrationDisabled, Forbidden},
	"qurl.ErrRegistrationRateLimited":             {qurl.ErrRegistrationRateLimited, RateLimited},
	"qurl.ErrRegistrationRecoveryRequired":        {qurl.ErrRegistrationRecoveryRequired, Unavailable},
	"qurl.ErrAssignmentTicketInvalid":             {qurl.ErrAssignmentTicketInvalid, ServerError},
	"qurl.ErrAssignmentTicketExpired":             {qurl.ErrAssignmentTicketExpired, Unavailable},
	"qurl.ErrRegistrationInvalidInput":            {qurl.ErrRegistrationInvalidInput, InvalidInput},
	"qurl.ErrRegisterReplyMalformed":              {qurl.ErrRegisterReplyMalformed, ServerError},
	"qurl.ErrRegistrationKeyKindDisallowed":       {qurl.ErrRegistrationKeyKindDisallowed, ServerError},
	"qurl.ErrCompletionUnavailable":               {qurl.ErrCompletionUnavailable, Unavailable},
	"qurl.ErrCompletionIdentityRejected":          {qurl.ErrCompletionIdentityRejected, Auth},
	"qurl.ErrDeviceKeyQuotaExceeded":              {qurl.ErrDeviceKeyQuotaExceeded, Forbidden},
	"qurl.ErrCompletionCredentialConflict":        {qurl.ErrCompletionCredentialConflict, Conflict},
	"qurl.ErrCompletionRequestRejected":           {qurl.ErrCompletionRequestRejected, InvalidInput},
	"qurl.ErrCompletionRecoveryRequired":          {qurl.ErrCompletionRecoveryRequired, Unavailable},
	"crid.ErrCharset":                             {crid.ErrCharset, InvalidInput},
	"crid.ErrLength":                              {crid.ErrLength, InvalidInput},
	"crid.ErrChecksum":                            {crid.ErrChecksum, InvalidInput},
	"crid.ErrNonCanonical":                        {crid.ErrNonCanonical, InvalidInput},
	"crid.ErrForbiddenVersion":                    {crid.ErrForbiddenVersion, InvalidInput},

	// The enrollment/assignment taxonomy a local publish can surface.
	// Each choice is argued at its case in connectorSentinelCode.
	// The enrollment token is this surface's credential: refusing it, or the
	// identity it vouches for, is the Auth row.
	"qurl.ErrAssignmentKeyRejected":                        {qurl.ErrAssignmentKeyRejected, Auth},
	"qurl.ErrAssignmentBootstrapConsumed":                  {qurl.ErrAssignmentBootstrapConsumed, Auth},
	"qurl.ErrAssignmentIdentityRejected":                   {qurl.ErrAssignmentIdentityRejected, Auth},
	"qurl.ErrRecoveryCredentialRejected":                   {qurl.ErrRecoveryCredentialRejected, Auth},
	"qurl.ErrCredentialRecoveryIdentityRejected":           {qurl.ErrCredentialRecoveryIdentityRejected, Auth},
	"qurl.ErrCredentialRecoveryExpired":                    {qurl.ErrCredentialRecoveryExpired, Auth},
	"qurl.ErrCredentialRecoveryRevokeRequired":             {qurl.ErrCredentialRecoveryRevokeRequired, Conflict},
	"qurl.ErrCredentialRecoveryCandidateConflict":          {qurl.ErrCredentialRecoveryCandidateConflict, Conflict},
	"qurl.ErrCredentialRecoveryRequestRejected":            {qurl.ErrCredentialRecoveryRequestRejected, InvalidInput},
	"qurl.ErrCredentialRecoveryRateLimited":                {qurl.ErrCredentialRecoveryRateLimited, RateLimited},
	"qurl.ErrCredentialRecoveryUnavailable":                {qurl.ErrCredentialRecoveryUnavailable, Unavailable},
	"qurl.ErrCredentialReplacementUnavailable":             {qurl.ErrCredentialReplacementUnavailable, Unavailable},
	"qurl.ErrCredentialRecoveryAssignmentRequired":         {qurl.ErrCredentialRecoveryAssignmentRequired, Unavailable},
	"qurl.ErrCredentialRecoveryGrantRejected":              {qurl.ErrCredentialRecoveryGrantRejected, Unavailable},
	"qurl.ErrCredentialRecoveryRetryRequired":              {qurl.ErrCredentialRecoveryRetryRequired, Unavailable},
	"qurl.ErrCredentialRecoveredAssignmentRefreshRequired": {qurl.ErrCredentialRecoveredAssignmentRefreshRequired, Unavailable},
	"qurl.ErrCredentialRecoveryInvalidResponse":            {qurl.ErrCredentialRecoveryInvalidResponse, ServerError},
	"qurl.ErrCredentialRecoveryCandidatePersistence":       {qurl.ErrCredentialRecoveryCandidatePersistence, General},
	// The request, not the credential, was rejected — a valid token minted for
	// another Connector lands here.
	"qurl.ErrAssignmentRequestRejected": {qurl.ErrAssignmentRequestRejected, InvalidInput},
	// Standing entitlement refusals: waiting and retyping both change nothing.
	"qurl.ErrAssignmentRegistrationDisabled": {qurl.ErrAssignmentRegistrationDisabled, Forbidden},
	"qurl.ErrAssignmentQuotaExceeded":        {qurl.ErrAssignmentQuotaExceeded, Forbidden},
	// Still rate limited after the SDK's own bounded retries.
	"qurl.ErrAssignmentRateLimited": {qurl.ErrAssignmentRateLimited, RateLimited},
	// The platform is not placing this Connector, or its assignment lapsed.
	"qurl.ErrAssignmentUnavailable":          {qurl.ErrAssignmentUnavailable, Unavailable},
	"qurl.ErrAssignmentReassignmentRequired": {qurl.ErrAssignmentReassignmentRequired, Unavailable},
	"qurl.ErrAssignmentRecoveryRequired":     {qurl.ErrAssignmentRecoveryRequired, Unavailable},
	"qurl.ErrAssignmentLeaseExpired":         {qurl.ErrAssignmentLeaseExpired, Unavailable},
	// An authenticated producer-contract violation.
	"qurl.ErrAssignmentInvalidResponse": {qurl.ErrAssignmentInvalidResponse, ServerError},

	// Native assigned-cell Connector-resource setup taxonomy.
	"qurl.ErrInvalidNativeConnectorResourceRequest":  {qurl.ErrInvalidNativeConnectorResourceRequest, InvalidInput},
	"qurl.ErrConnectorResourceUnavailable":           {qurl.ErrConnectorResourceUnavailable, Unavailable},
	"qurl.ErrConnectorResourceIdentityRejected":      {qurl.ErrConnectorResourceIdentityRejected, Auth},
	"qurl.ErrConnectorResourceEntitlementDenied":     {qurl.ErrConnectorResourceEntitlementDenied, Forbidden},
	"qurl.ErrConnectorResourceIdentityConflict":      {qurl.ErrConnectorResourceIdentityConflict, Conflict},
	"qurl.ErrConnectorResourceQuotaExceeded":         {qurl.ErrConnectorResourceQuotaExceeded, Forbidden},
	"qurl.ErrConnectorResourceRateLimited":           {qurl.ErrConnectorResourceRateLimited, RateLimited},
	"qurl.ErrConnectorResourceRequestRejected":       {qurl.ErrConnectorResourceRequestRejected, InvalidInput},
	"qurl.ErrInvalidNativeConnectorResourceResponse": {qurl.ErrInvalidNativeConnectorResourceResponse, ServerError},
}

// TestSentinelMapping asserts every defined sentinel — CLI and SDK — maps to
// exactly its one expected code, both bare and wrapped.
func TestSentinelMapping(t *testing.T) {
	for name, row := range cliSentinels {
		assertCode(t, name, row.err, row.code)
	}
	for name, row := range sdkSentinels {
		assertCode(t, name, row.err, row.code)
	}
}

func TestRecoveredAssignmentRefreshKeepsUnavailableExitCode(t *testing.T) {
	err := &qurl.CredentialRecoveredAssignmentRefreshRequiredError{
		Cause: errors.Join(&qurl.AssignmentError{Code: "52201"}, qurl.ErrAssignmentIdentityRejected),
	}
	if got := FromError(err); got != Unavailable {
		t.Fatalf("recovered assignment refresh exit code = %d, want %d", got, Unavailable)
	}
}

func TestCredentialRecoveryRetryKeepsLastAuthenticatedExitCode(t *testing.T) {
	last := errors.Join(
		&qurl.CredentialRecoveryError{Code: "52404", Phase: "hub_issue_recovery"},
		qurl.ErrCredentialRecoveryRateLimited,
	)
	err := &qurl.CredentialRecoveryRetryRequiredError{
		Phase: "hub_issue_recovery", Attempts: 3, Elapsed: time.Minute, Last: last,
	}
	if got := FromError(err); got != RateLimited {
		t.Fatalf("credential recovery retry exit code = %d, want %d", got, RateLimited)
	}
}

func assertCode(t *testing.T, name string, err error, want int) {
	t.Helper()
	if got := FromError(err); got != want {
		t.Errorf("%s: FromError = %d, want %d", name, got, want)
	}
	wrapped := fmt.Errorf("outer context: %w", err)
	if got := FromError(wrapped); got != want {
		t.Errorf("%s (wrapped): FromError = %d, want %d", name, got, want)
	}
}

// TestEverySentinelIsMapped scans every non-test Go file under apps/cli for
// exported top-level Err* declarations and fails when one is missing from
// cliSentinels — the "added an error without mapping it" tripwire — or when
// a mapping row refers to a sentinel that no longer exists.
func TestEverySentinelIsMapped(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		pkg := file.Name.Name
		if pkg == "main" {
			pkg = "cmd"
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if strings.HasPrefix(name.Name, "Err") && ast.IsExported(name.Name) {
						found[pkg+"."+name.Name] = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan apps/cli: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("scanned no sentinels; the tripwire would be vacuous")
	}
	for name := range found {
		if _, ok := cliSentinels[name]; !ok {
			t.Errorf("sentinel %s has no exit-code mapping: add it to cliSentinels AND a case in FromError", name)
		}
	}
	for name := range cliSentinels {
		if !found[name] {
			t.Errorf("cliSentinels lists %s but no such sentinel exists in the source tree", name)
		}
	}
}

// TestTypedWrappers pins the CLI wrapper types, including precedence: a
// verification wrapper wins over whatever its cause would map to.
func TestTypedWrappers(t *testing.T) {
	if got := FromError(UsageError(errors.New("bad flag"))); got != Usage {
		t.Errorf("UsageError = %d, want %d", got, Usage)
	}
	if got := FromError(InvalidInputError("unsupported operand", errors.New("service detail"))); got != InvalidInput {
		t.Errorf("InvalidInputError = %d, want %d", got, InvalidInput)
	}
	if got := FromError(NotImplemented("later")); got != General {
		t.Errorf("NotImplemented = %d, want %d", got, General)
	}
	if got := FromError(VerificationError("mismatch", qurl.ErrCRIDMismatch)); got != VerificationFailed {
		t.Errorf("VerificationError = %d, want %d", got, VerificationFailed)
	}
	// Precedence: a held-CRID parse failure inside a verification context is
	// 12, not the bare sentinel's 8.
	if got := FromError(VerificationError("held value invalid", crid.ErrChecksum)); got != VerificationFailed {
		t.Errorf("VerificationError over crid sentinel = %d, want %d", got, VerificationFailed)
	}
	if UsageError(nil) != nil {
		t.Error("UsageError(nil) must be nil")
	}
	if InvalidInputError("bad operand", nil) == nil {
		t.Error("InvalidInputError with a message must not be nil")
	}
}

// TestAPIErrorStatusMapping walks the full status table.
func TestAPIErrorStatusMapping(t *testing.T) {
	cases := map[int]int{
		400: InvalidInput,
		401: Auth,
		403: Forbidden,
		404: NotFound,
		409: Conflict,
		410: NotFound,
		422: InvalidInput,
		429: RateLimited,
		500: ServerError,
		502: ServerError,
		503: Unavailable,
	}
	for status, want := range cases {
		err := error(&qurlapi.Error{StatusCode: status})
		if got := FromError(err); got != want {
			t.Errorf("HTTP %d: FromError = %d, want %d", status, got, want)
		}
	}
}

// TestGoneFamilySharesExitFive pins the pinned-contract decision: the
// platform's three "does not resolve" spellings — 404 (either code), 400
// `revoked`, and 410 `resource_tombstoned` — all exit 5; only messages
// differ.
func TestGoneFamilySharesExitFive(t *testing.T) {
	family := []*qurlapi.Error{
		{StatusCode: 404, Code: "resource_not_found"},
		{StatusCode: 404, Code: "not_found"},
		{StatusCode: 400, Code: "revoked"},
		{StatusCode: 410, Code: "resource_tombstoned"},
	}
	for _, apiErr := range family {
		if got := FromError(apiErr); got != NotFound {
			t.Errorf("HTTP %d code %q: FromError = %d, want %d", apiErr.StatusCode, apiErr.Code, got, NotFound)
		}
	}
	// A plain 400 without the revoked code stays invalid input.
	if got := FromError(&qurlapi.Error{StatusCode: 400, Code: "invalid_request"}); got != InvalidInput {
		t.Errorf("plain 400 = %d, want %d", got, InvalidInput)
	}
}

// TestProcessLevelMappings covers cancellation, timeouts, network failures,
// success, and the unclassified default.
func TestProcessLevelMappings(t *testing.T) {
	if got := FromError(nil); got != Success {
		t.Errorf("nil = %d, want 0", got)
	}
	if got := FromError(context.Canceled); got != Interrupted {
		t.Errorf("canceled = %d, want %d", got, Interrupted)
	}
	if got := FromError(fmt.Errorf("wrap: %w", context.DeadlineExceeded)); got != Unavailable {
		t.Errorf("deadline = %d, want %d", got, Unavailable)
	}
	netErr := &url.Error{Op: "Post", URL: "https://api.layerv.ai", Err: errors.New("connection refused")}
	if got := FromError(netErr); got != Unavailable {
		t.Errorf("network = %d, want %d", got, Unavailable)
	}
	if got := FromError(errors.New("mystery")); got != General {
		t.Errorf("default = %d, want %d", got, General)
	}
}
