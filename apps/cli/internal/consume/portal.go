package consume

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/layervai/qurl-go/qurl"
)

// Direct access for downloads. A resolved qv2 link carries its credential in
// the URL fragment, which HTTP clients never transmit — a plain GET of the
// link can only ever fetch the in-browser page that consumes the fragment,
// never the content itself. Downloads therefore go through the SDK's
// programmatic opener (qurl.EnterPortalWith): verify the link locally, ask
// the qURL platform for access, then fetch the granted content URL.
//
// The opener needs deployment trust configuration — issuer keys plus a relay
// allowlist or cell catalog. AccessOpener sources it from QURL_DEPLOYMENT
// (the SDK's own operator contract for self-hosted and sandbox deployments),
// read through the CLI's injected environment, falling back to the SDK's
// shipped deployment. A build with neither fails closed with
// ErrAccessNotConfigured rather than saving the wrong bytes.

// credentialFragmentPrefix marks a link whose fragment is an in-link
// credential rather than a page anchor.
//
// TODO(upstream-contract): "qv2." is the pinned wire prefix for
// fragment-credential links (qurl-go internal/qv2 FragmentPrefix). If the
// platform adds another credential-bearing link form, widen NeedsAccessGrant
// in lockstep.
const credentialFragmentPrefix = "qv2."

// NeedsAccessGrant reports whether link carries an in-link credential and so
// must be opened through the platform access flow. A link without one (a
// direct or pre-signed URL) serves its bytes to a plain GET, so the
// downloader fetches it as delivered.
func NeedsAccessGrant(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		// Unparseable links fail loudly at request build; nothing to grant.
		return false
	}
	return strings.HasPrefix(u.Fragment, credentialFragmentPrefix)
}

// Fixed customer-facing messages for the access flow, registered with the
// jargon gate via CustomerMessages. They are self-contained on purpose: the
// SDK's own error text speaks protocol vocabulary, so classifyAccessError
// maps it to these sentinels instead of wrapping it verbatim.
const (
	// MsgAccessNotConfigured reports that no deployment trust settings are
	// available, so direct downloads cannot run at all on this machine.
	MsgAccessNotConfigured = "this machine isn't set up to download qURL content directly — set QURL_DEPLOYMENT to your deployment settings file, or open the CRID in your browser instead (`qurl get <CRID>` on a terminal, without --file)"

	// MsgAccessSettingsMismatch reports settings that don't cover the link
	// the service answered with (typically production settings against a
	// test service, or the reverse).
	MsgAccessSettingsMismatch = "your deployment settings don't match the service this access link came from — check QURL_DEPLOYMENT, or ask whoever runs your qURL deployment"

	// MsgLinkVerification is the fail-closed discard of a link that did not
	// pass its local check; same posture as the CRID verification messages.
	MsgLinkVerification = "the access link failed its safety check, so it was discarded and nothing was downloaded. Try again; if it keeps happening, stop and contact whoever shared the CRID with you"

	// MsgAccessDenied reports an authenticated refusal from the platform.
	MsgAccessDenied = "the service declined to grant access to this content — resolve the CRID again for a fresh link; if it keeps happening, contact whoever shared the CRID with you"

	// MsgAccessBusy reports the platform asking the caller to retry later.
	MsgAccessBusy = "the service is busy right now — try again in a moment"
)

// Access-flow sentinels, each mapped to exactly one exit code in
// internal/exitcode.
var (
	// ErrAccessNotConfigured refuses a direct download with no usable
	// deployment settings (configuration).
	ErrAccessNotConfigured = errors.New(MsgAccessNotConfigured)
	// ErrAccessSettingsMismatch refuses a link the configured settings
	// don't cover (configuration).
	ErrAccessSettingsMismatch = errors.New(MsgAccessSettingsMismatch)
	// ErrLinkVerification discards a link that failed its local check
	// (verification).
	ErrLinkVerification = errors.New(MsgLinkVerification)
	// ErrAccessDenied reports the platform refusing access (forbidden).
	ErrAccessDenied = errors.New(MsgAccessDenied)
	// ErrAccessBusy reports the platform deferring the request
	// (unavailable).
	ErrAccessBusy = errors.New(MsgAccessBusy)
)

// AccessOpener turns a verified link into the reachable content URL by
// asking the qURL platform for access. The zero value works; LookupEnv is
// the CLI's injected environment, so hermetic tests never read the process
// environment.
type AccessOpener struct {
	// LookupEnv resolves QURL_DEPLOYMENT; nil skips the override and uses
	// the SDK's shipped deployment resolution.
	LookupEnv func(string) (string, bool)
}

// Open verifies link, asks the platform for access, and returns the granted
// content URL. Every failure is mapped to a customer-language sentinel; a
// link the platform cannot serve headlessly fails loudly here and nothing
// is ever downloaded in its place.
func (o *AccessOpener) Open(ctx context.Context, link string) (string, error) {
	handle, err := o.enter(ctx, link)
	if err != nil {
		return "", classifyAccessError(err)
	}
	return grantedContentURL(handle.ResourceURL)
}

// grantedContentURL admits only web URLs as download targets: the granted
// URL is about to be fetched, and anything else is the service answering
// outside its contract.
func grantedContentURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%w — it can't be downloaded", ErrUnopenableLink)
	}
	return raw, nil
}

// enter runs the SDK opener with the deployment settings precedence:
// QURL_DEPLOYMENT through the CLI's environment first, then the SDK's own
// resolution (default provider, process QURL_DEPLOYMENT, shipped
// deployment). In production the two views of the environment are the same
// os.LookupEnv; they differ only under test injection.
func (o *AccessOpener) enter(ctx context.Context, link string) (*qurl.ResourceHandle, error) {
	if o.LookupEnv != nil {
		if path, ok := o.LookupEnv(qurl.EnvDeploymentPath); ok && strings.TrimSpace(path) != "" {
			d, err := qurl.LoadDeployment(strings.TrimSpace(path))
			if err != nil {
				// The SDK's file diagnostics name the path and the JSON
				// problem — config-file detail worth keeping.
				return nil, fmt.Errorf("%w (%w)", ErrAccessNotConfigured, err)
			}
			cfg, err := openerConfig(d)
			if err != nil {
				return nil, err
			}
			return qurl.EnterPortalWith(ctx, link, cfg)
		}
	}
	return qurl.EnterPortal(ctx, link)
}

// openerConfig converts a loaded deployment into opener configuration via
// the SDK's exported constructors, failing closed exactly where the SDK
// would: no issuers, or no transport at all, is not a usable deployment.
// Conversion failures carry the CLI's own plain detail — the SDK constructor
// text speaks protocol vocabulary.
func openerConfig(d *qurl.Deployment) (qurl.Config, error) {
	if d == nil || len(d.Issuers) == 0 {
		return qurl.Config{}, fmt.Errorf("%w (the deployment settings file is missing required entries)", ErrAccessNotConfigured)
	}
	derByKID := make(map[string][]byte, len(d.Issuers))
	for _, iss := range d.Issuers {
		der, err := base64.RawURLEncoding.DecodeString(iss.SPKIDERB64)
		if err != nil {
			return qurl.Config{}, fmt.Errorf("%w (deployment settings entry %q has an unusable key)", ErrAccessNotConfigured, iss.Kid)
		}
		derByKID[iss.Kid] = der
	}
	ts, err := qurl.NewTrustStoreFromDER(derByKID)
	if err != nil {
		return qurl.Config{}, fmt.Errorf("%w (deployment settings list an unusable key)", ErrAccessNotConfigured)
	}
	cfg := qurl.Config{TrustStore: ts}
	// Blank entries are dropped rather than handed to the SDK: only real,
	// trimmed hosts belong in the allowlist, and a list that was ONLY
	// blanks must read as no transport at all (fail closed below), not as
	// an allowlist that rejects everything with a worse diagnostic.
	hosts := make([]string, 0, len(d.RelayAllowlist))
	for _, entry := range d.RelayAllowlist {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	if len(hosts) > 0 {
		cfg.RelayAllowlist = qurl.NewRelayAllowlist(hosts)
	}
	if len(d.Cells) > 0 {
		entries := make([]qurl.CellEntry, 0, len(d.Cells))
		for _, cell := range d.Cells {
			entries = append(entries, qurl.CellEntry{
				ServerPublicKeyB64: cell.ServerPublicKeyB64,
				CellID:             cell.CellID,
				Host:               cell.Host,
				Port:               cell.Port,
			})
		}
		catalog, err := qurl.NewCellCatalog(entries)
		if err != nil {
			return qurl.Config{}, fmt.Errorf("%w (deployment settings list an unusable endpoint)", ErrAccessNotConfigured)
		}
		cfg.Cells = catalog
	}
	if cfg.RelayAllowlist == nil && cfg.Cells == nil {
		return qurl.Config{}, fmt.Errorf("%w (the deployment settings file is missing required entries)", ErrAccessNotConfigured)
	}
	return cfg, nil
}

// classifyAccessError maps an SDK opener failure onto the CLI's
// customer-language sentinels. Faults outside the access taxonomy —
// cancellation, network transport — pass through so the exit-code table's
// existing rows keep classifying them.
func classifyAccessError(err error) error {
	var deny *qurl.ServerDenyError
	switch {
	case errors.Is(err, ErrAccessNotConfigured):
		// Already classified by enter/openerConfig.
		return err
	case errors.Is(err, qurl.ErrNotConfigured):
		return ErrAccessNotConfigured
	case errors.Is(err, qurl.ErrUnknownKID), errors.Is(err, qurl.ErrRelayURL):
		return ErrAccessSettingsMismatch
	case errors.Is(err, qurl.ErrSignature),
		errors.Is(err, qurl.ErrStrictParse),
		errors.Is(err, qurl.ErrFragment),
		errors.Is(err, qurl.ErrEncoding),
		errors.Is(err, qurl.ErrKeyLength):
		return ErrLinkVerification
	case errors.As(err, &deny):
		return ErrAccessDenied
	case errors.Is(err, qurl.ErrServerOverloaded):
		return ErrAccessBusy
	default:
		return err
	}
}
