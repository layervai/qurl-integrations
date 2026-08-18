// Package exitcode is the CLI's single exit-code authority.
//
// The table below is the design doc's §16.5 contract plus the CRID
// verification code (12) and the conventional SIGINT code (130). Every error
// the CLI can surface maps to exactly one code, and it maps in exactly one
// place: FromError. Commands never call os.Exit and never pick numbers; they
// return errors, and main exits with FromError's answer.
//
// The mapping is guarded two ways in exitcode_test.go: a table-driven test
// pins every defined sentinel to its code, and a source scan fails the build
// when a new Err* sentinel appears anywhere under apps/cli without a row in
// that table.
package exitcode

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/layervai/qurl-go/crid"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

// The exit-code table (§16.5 plus 12 and 130).
const (
	// Success: the command did what was asked.
	Success = 0
	// General: an unclassified failure, including features not yet available
	// in this build.
	General = 1
	// Usage: the command line itself was wrong (flags, arguments, missing
	// confirmation).
	Usage = 2
	// Config: configuration files or profiles are invalid.
	Config = 3
	// Auth: no credential, an implausible credential, or the service
	// rejected the credential (HTTP 401).
	Auth = 4
	// NotFound: the resource does not exist or is retired (HTTP 404/410).
	NotFound = 5
	// Forbidden: the credential lacks permission (HTTP 403).
	Forbidden = 6
	// Conflict: the request conflicts with current state (HTTP 409).
	Conflict = 7
	// InvalidInput: an operand or request the service (or the local gate,
	// for inputs that can never be valid) rejected as invalid.
	InvalidInput = 8
	// RateLimited: still rate limited after the transport's bounded retries
	// (HTTP 429).
	RateLimited = 9
	// ServerError: the service failed (HTTP 5xx other than 503) or answered
	// outside its contract.
	ServerError = 10
	// Unavailable: the service cannot be reached or is not serving this
	// surface (HTTP 503, network failures, timeouts).
	Unavailable = 11
	// VerificationFailed: the response failed CRID-anchored verification.
	// Nothing was emitted; treat as tampering, not transience.
	VerificationFailed = 12
	// Interrupted: the run was canceled (SIGINT convention 128+2).
	Interrupted = 130
)

// usageError marks a command-line usage failure (exit 2).
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// UsageError wraps err as a usage failure. The message is surfaced verbatim.
func UsageError(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// notImplementedError marks a feature absent from this build (exit 1, by the
// table's General row: the command is valid, the capability just is not
// shipped yet).
type notImplementedError struct{ msg string }

func (e *notImplementedError) Error() string { return e.msg }

// NotImplemented returns the typed error for a stubbed capability.
func NotImplemented(msg string) error {
	return &notImplementedError{msg: msg}
}

// verificationError marks a fail-closed verification outcome (exit 12). The
// cause chain stays reachable so tests can match the SDK sentinels.
type verificationError struct {
	msg string
	err error
}

func (e *verificationError) Error() string { return e.msg }
func (e *verificationError) Unwrap() error { return e.err }

// VerificationError wraps cause as a fail-closed verification failure with a
// customer-facing message.
func VerificationError(msg string, cause error) error {
	return &verificationError{msg: msg, err: cause}
}

// FromError maps any error the CLI can return onto exactly one exit code.
// nil maps to Success.
func FromError(err error) int {
	if err == nil {
		return Success
	}

	// CLI-typed wrappers first: they were chosen deliberately at the site
	// that best knows the failure, including verification wrappers whose
	// cause chain may contain sentinels that would otherwise map elsewhere.
	var usage *usageError
	if errors.As(err, &usage) {
		return Usage
	}
	var notImpl *notImplementedError
	if errors.As(err, &notImpl) {
		return General
	}
	var verification *verificationError
	if errors.As(err, &verification) {
		return VerificationFailed
	}

	switch {
	case errors.Is(err, context.Canceled):
		return Interrupted
	case errors.Is(err, context.DeadlineExceeded):
		return Unavailable
	case errors.Is(err, qurl.ErrTemporaryAccessLinksDisabled):
		return Unavailable
	case errors.Is(err, qurl.ErrNoCRID), errors.Is(err, qurl.ErrCRIDMismatch):
		return VerificationFailed
	}

	if code, ok := cliSentinelCode(err); ok {
		return code
	}

	if code, ok := apiErrorCode(err); ok {
		return code
	}

	switch {
	case errors.Is(err, qurl.ErrInvalidClientConfig):
		return Config
	case errors.Is(err, qurl.ErrCredentialStateNotFound),
		errors.Is(err, qurl.ErrInsecureCredentialStatePermissions):
		return Auth
	case errors.Is(err, qurl.ErrInvalidResourceRequest),
		errors.Is(err, qurl.ErrInvalidPortalRequest):
		return InvalidInput
	case errors.Is(err, qurl.ErrInvalidAPIResponse):
		return ServerError
	case isLocalCRIDReject(err):
		return InvalidInput
	case isNetworkError(err):
		return Unavailable
	default:
		return General
	}
}

// cliSentinelCode maps the CLI's own sentinel families — operand
// assessment, credentials, configuration, and the consume layer — onto
// their exit codes.
func cliSentinelCode(err error) (int, bool) {
	switch {
	case errors.Is(err, cridux.ErrTestIDOnProduction):
		return Usage, true
	case errors.Is(err, cridux.ErrUnusableID):
		return InvalidInput, true
	case errors.Is(err, consume.ErrPipedNeedsFile):
		// The invocation is wrong for its context (piped stdout, no --file):
		// a usage failure, remedied on the command line.
		return Usage, true
	case errors.Is(err, consume.ErrFileExists):
		// Overwrite refusal is the Conflict row of the §16.5 table: the
		// command and operand are both valid — the request conflicts with
		// state that already exists at the destination, exactly the shape
		// the HTTP 409 row describes, and --force is the caller's explicit
		// resolution. (Not InvalidInput: the path is a perfectly good path.)
		return Conflict, true
	case errors.Is(err, consume.ErrLinkExpired):
		// Expiry that survived the one automatic refresh joins the
		// platform's gone family: the link does not resolve to content now.
		return NotFound, true
	case errors.Is(err, consume.ErrLinkFetch):
		// A freshly minted, verified link that then refuses to serve is the
		// service answering outside its contract.
		return ServerError, true
	case errors.Is(err, consume.ErrUnopenableLink):
		// A verified answer whose link is not a web URL is likewise the
		// service outside its contract — never handed to a launcher.
		return ServerError, true
	case errors.Is(err, auth.ErrNoCredential), errors.Is(err, auth.ErrInvalidKey):
		return Auth, true
	case errors.Is(err, config.ErrInvalidProfileName),
		errors.Is(err, config.ErrConfigFile),
		errors.Is(err, config.ErrSecretInConfig):
		return Config, true
	default:
		return 0, false
	}
}

// apiErrorCode maps a typed qURL API error by status class, with the pinned
// code-level exceptions: the platform's "gone" family — 404 (both code
// spellings), 400 `revoked`, and 410 `resource_tombstoned` — all mean the
// resource does not resolve and share the not-found exit code; only the
// stderr message distinguishes them.
func apiErrorCode(err error) (int, bool) {
	var apiErr *qurlapi.Error
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	switch {
	case apiErr.StatusCode == 400 && strings.EqualFold(apiErr.Code, "revoked"):
		return NotFound, true
	case apiErr.StatusCode == 401:
		return Auth, true
	case apiErr.StatusCode == 403:
		return Forbidden, true
	case apiErr.StatusCode == 404 || apiErr.StatusCode == 410:
		return NotFound, true
	case apiErr.StatusCode == 409:
		return Conflict, true
	case apiErr.StatusCode == 429:
		return RateLimited, true
	case apiErr.StatusCode == 503:
		return Unavailable, true
	case apiErr.StatusCode >= 500:
		return ServerError, true
	default:
		return InvalidInput, true
	}
}

// isLocalCRIDReject matches the crid package's closed reject vocabulary.
// These reach FromError only outside a verification context (verification
// wrapping is matched earlier and wins).
func isLocalCRIDReject(err error) bool {
	return errors.Is(err, crid.ErrCharset) ||
		errors.Is(err, crid.ErrLength) ||
		errors.Is(err, crid.ErrChecksum) ||
		errors.Is(err, crid.ErrNonCanonical) ||
		errors.Is(err, crid.ErrForbiddenVersion)
}

// isNetworkError matches transport-level failures: DNS, refused connections,
// timeouts surfaced as *url.Error by net/http.
func isNetworkError(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}
