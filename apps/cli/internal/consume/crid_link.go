package consume

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/layervai/qurl-go/crid"
	"github.com/layervai/qurl-go/qurl"
)

// A link from a CRID alone. A device that is neither the resource owner's nor
// one the publisher allowed cannot mint a share link. The SDK can still ask
// the service for a link with only the CRID (qurl.RequestCRIDLink). That
// request uses no device identity and creates none, and the SDK returns a
// link only after it has checked the link against the CRID that was asked
// for.
//
// A deployment has to offer the request. The deployment a release ships does
// not name where to send it. CRIDLinkOffered asks the SDK that one question,
// with no CRID and without sending anything, so a caller knows whether this
// machine can ask at all before it decides anything else. When the request is
// not offered the caller behaves as it did before the request existed.
//
// The answer depends on the deployment only. Whether the SDK will ask for one
// particular CRID is a second question, which CRIDNotRequestable answers from
// the request's own error.

// cridLinkRequestTimeout bounds one request. The SDK sends it with a client
// that has no timeout of its own, so without this bound a service that never
// answers would hold the command until the user interrupts it. The value is
// the bound one qURL API attempt has.
const cridLinkRequestTimeout = 30 * time.Second

// Fixed customer-facing messages for a link requested with only a CRID,
// registered with the jargon gate via CustomerMessages. The SDK's own error
// text is written for SDK callers and can name addresses, so
// ClassifyCRIDLinkError maps every failure to one of these and never wraps
// that text.
const (
	// MsgCRIDNotFound is the one answer for a CRID the service will not give
	// this device a link for. The service does not say why, and neither does
	// this message; the renderer adds a hint that lists every cause.
	MsgCRIDNotFound = "this CRID was not found, or this device is not allowed to open it"

	// MsgCRIDLinkUnavailable reports that the service cannot issue a link at
	// the moment.
	MsgCRIDLinkUnavailable = "the service can't give a link for this CRID right now — try again later"

	// MsgCRIDLinkNoAnswer reports a request that got no usable answer: the
	// service could not be reached, or it did not answer in time.
	MsgCRIDLinkNoAnswer = "the service did not answer — check your network connection, then try again later"

	// MsgCRIDLinkRateLimited reports too many requests.
	MsgCRIDLinkRateLimited = "there are too many requests for links right now — try again later"

	// MsgCRIDPublisherOffline reports a resource whose publisher is not
	// serving it at the moment.
	MsgCRIDPublisherOffline = "the publisher of this resource is offline — try again later"

	// MsgCRIDResourceClosed reports a resource that was closed.
	MsgCRIDResourceClosed = "this resource was closed and can no longer be opened"

	// MsgCRIDLinkRequestRejected reports that the service refused the request
	// itself, which only a newer CLI can change.
	MsgCRIDLinkRequestRejected = "the service did not accept this request — update qurl, then try again"

	// MsgCRIDLinkRefused is the discard of an answer that failed a check: a
	// link that is not for this CRID or not from this deployment, or a reply
	// that cannot be read as an answer. Same posture as MsgLinkVerification.
	MsgCRIDLinkRefused = "the link for this CRID was refused because the service's answer failed its safety check. Nothing was opened or downloaded. Try again; if it keeps happening, stop and contact whoever shared the CRID with you"

	// msgCRIDLinkEntryUnusable is the detail added to MsgAccessNotConfigured
	// when the deployment settings name where to ask for a link with only a
	// CRID and that entry cannot be used. It names the entry by the key it
	// has in the settings file, so the person who fixes the file can find it.
	msgCRIDLinkEntryUnusable = `the settings name a "crid_link" entry that cannot be used`

	// msgCRIDLinkClientNeedsSettings is the detail added to
	// MsgAccessNotConfigured when RequestCRIDLink was given an HTTP client of
	// its own and no settings file to use it with. Only a test can cause it:
	// production never sets that client.
	msgCRIDLinkClientNeedsSettings = "an HTTP client was set for the request with only a CRID, and it is used only with a settings file"
)

// Sentinels for a link requested with only a CRID, each mapped to exactly one
// exit code in internal/exitcode.
var (
	// ErrCRIDNotFound is the service's one answer for an unknown CRID and for
	// a CRID this device may not open (not found).
	ErrCRIDNotFound = errors.New(MsgCRIDNotFound)
	// ErrCRIDNotFoundNoDevice is ErrCRIDNotFound on a machine that has no
	// device identity. The answer and its text are the same; only the hint
	// differs, because that machine has no public key to send a publisher
	// yet. It matches ErrCRIDNotFound.
	ErrCRIDNotFoundNoDevice = fmt.Errorf("%w", ErrCRIDNotFound)
	// ErrCRIDLinkUnavailable reports that no link can be issued right now
	// (unavailable).
	ErrCRIDLinkUnavailable = errors.New(MsgCRIDLinkUnavailable)
	// ErrCRIDLinkNoAnswer reports a request that got no usable answer
	// (unavailable).
	ErrCRIDLinkNoAnswer = errors.New(MsgCRIDLinkNoAnswer)
	// ErrCRIDLinkRateLimited reports too many requests (rate limited).
	ErrCRIDLinkRateLimited = errors.New(MsgCRIDLinkRateLimited)
	// ErrCRIDPublisherOffline reports a publisher that is offline
	// (unavailable).
	ErrCRIDPublisherOffline = errors.New(MsgCRIDPublisherOffline)
	// ErrCRIDResourceClosed reports a closed resource (the platform's gone
	// family).
	ErrCRIDResourceClosed = errors.New(MsgCRIDResourceClosed)
	// ErrCRIDLinkRequestRejected reports a request the service refused as
	// invalid (invalid input).
	ErrCRIDLinkRequestRejected = errors.New(MsgCRIDLinkRequestRejected)
	// ErrCRIDLinkRefused discards an answer that failed a check
	// (verification).
	ErrCRIDLinkRefused = errors.New(MsgCRIDLinkRefused)
)

// errCRIDLinkSetup reports deployment settings that name where to ask for a
// link with only a CRID, and name it in a way that cannot be used. That is a
// fault in how this machine is set up, so it is ErrAccessNotConfigured with
// the detail that says which entry. It is not "the request is not offered":
// someone meant to offer it here, and falling back in silence would hide the
// mistake from them.
var errCRIDLinkSetup = fmt.Errorf("%w (%s)", ErrAccessNotConfigured, msgCRIDLinkEntryUnusable)

// errCRIDLinkClientNeedsSettings reports a programming mistake: the opener
// carries an HTTP client for the link request, and no settings file to use
// it with. Without a settings file the request goes through the SDK's own
// resolution, which takes no client from here. So the request would leave
// through the SDK's default client, which is exactly what a test sets the
// client to prevent. RequestCRIDLink refuses and sends nothing.
//
// It is ErrAccessNotConfigured with a fixed detail, like errCRIDLinkSetup, so
// ClassifyCRIDLinkError keeps it as it is and no SDK text is involved.
var errCRIDLinkClientNeedsSettings = fmt.Errorf("%w (%s)", ErrAccessNotConfigured, msgCRIDLinkClientNeedsSettings)

// CRIDLinkOffered reports whether this machine can ask for a link with only
// a CRID at all. It needs no CRID. It sends nothing and creates nothing: it
// reads the deployment settings the way RequestCRIDLink does and lets the SDK
// check them (qurl.CheckCRIDLinkConfig).
//
// There are three answers:
//
//   - true: a request can be sent. It does not mean the service will give a
//     link.
//   - false with no error: the deployment names no place to send the request.
//     It is not offered here. This is the answer under the deployment every
//     release ships today, and the caller then behaves as it did before the
//     request existed.
//   - false with an error: this machine is set up wrongly, and the error is
//     ErrAccessNotConfigured. Either the settings file cannot be used at all,
//     or the settings name a place to send the request that cannot be used.
//     The caller reports it and does nothing else.
//
// TODO(upstream-contract): qurl-go tells "names no endpoint" from "names one
// that cannot be used" by qurl.ErrCRIDLinkMisconfigured, which also matches
// qurl.ErrCRIDLinkNotConfigured. The narrower error is tested first here. If
// qurl-go ever reports an unusable endpoint without it, that endpoint reads
// as "not offered" again.
func (o *AccessOpener) CRIDLinkOffered() (bool, error) {
	err := o.checkCRIDLinkConfig()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrAccessNotConfigured):
		// Already classified by loadDeployment or cridLinkConfig: the
		// settings file.
		return false, err
	case errors.Is(err, qurl.ErrCRIDLinkMisconfigured):
		return false, errCRIDLinkSetup
	case errors.Is(err, qurl.ErrCRIDLinkNotConfigured):
		return false, nil
	default:
		// The SDK has no deployment settings it can use. Its own text names
		// file paths, so only the fixed message is kept.
		return false, ErrAccessNotConfigured
	}
}

// checkCRIDLinkConfig is the SDK's answer to "could a request be sent from
// here", for the deployment settings RequestCRIDLink would use.
func (o *AccessOpener) checkCRIDLinkConfig() error {
	d, configured, err := o.loadDeployment()
	if err != nil {
		return err
	}
	if !configured {
		return qurl.CheckCRIDLinkConfig()
	}
	cfg, err := cridLinkConfig(&d)
	if err != nil {
		return err
	}
	return qurl.CheckCRIDLinkConfigWith(cfg)
}

// RequestCRIDLink asks the service for a link to the resource resourceCRID
// names, with only the CRID. It reads deployment settings the way Grant does:
// QURL_DEPLOYMENT through the CLI's environment first, then the SDK's own
// resolution. It returns the SDK's answer unchanged, so the caller can tell
// "the SDK will not ask for this CRID" (CRIDNotRequestable) from every other
// failure (ClassifyCRIDLinkError). It reports two failures itself, both as
// ErrAccessNotConfigured: a settings file that cannot be used, and an opener
// with CRIDLinkHTTPClient set and no settings file, which is a mistake only a
// test can make (errCRIDLinkClientNeedsSettings).
//
// Callers ask CRIDLinkOffered first. It reads the same settings, so a request
// made after it said true is sent, unless the CRID is one the SDK will not
// ask for.
//
// No user agent is sent: the SDK's own resolution cannot set one, and what is
// sent must not depend on where the settings came from.
func (o *AccessOpener) RequestCRIDLink(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
	ctx, cancel := context.WithTimeout(ctx, cridLinkRequestTimeout)
	defer cancel()

	d, configured, err := o.loadDeployment()
	if err != nil {
		return nil, err
	}
	if !configured {
		if o.CRIDLinkHTTPClient != nil {
			// The SDK's own resolution cannot be given this client. Refuse
			// before the SDK is called, so the client is never dropped in
			// silence.
			return nil, errCRIDLinkClientNeedsSettings
		}
		return qurl.RequestCRIDLink(ctx, resourceCRID)
	}
	cfg, err := cridLinkConfig(&d)
	if err != nil {
		return nil, err
	}
	cfg.HTTPClient = o.CRIDLinkHTTPClient
	return qurl.RequestCRIDLinkWith(ctx, resourceCRID, cfg)
}

// cridLinkConfig is openerConfig plus the deployment's CRID link endpoint.
// The endpoint's values are copied as written: the SDK checks them against
// this same config on every request and in qurl.CheckCRIDLinkConfigWith. It
// answers qurl.ErrCRIDLinkNotConfigured for a file that names none, and
// qurl.ErrCRIDLinkMisconfigured for one it cannot use.
func cridLinkConfig(d *qurl.Deployment) (qurl.Config, error) {
	cfg, err := openerConfig(d)
	if err != nil {
		return qurl.Config{}, err
	}
	if d.CRIDLink != nil {
		cfg.CRIDLink = &qurl.CRIDLinkConfig{
			RelayURL:   d.CRIDLink.RelayURL,
			LinkOrigin: d.CRIDLink.LinkOrigin,
		}
	}
	return cfg, nil
}

// CRIDNotRequestable reports whether err says the SDK will not ask for a link
// for this CRID, whatever the deployment is. It could not check a link
// against the CRID: the CRID has a version this SDK does not know resources
// to carry. Nothing was sent.
//
// It says nothing about the deployment. CRIDLinkOffered answers that, and a
// caller asks it first, because the SDK checks the CRID before it reads any
// settings and this answer would otherwise hide the other one.
//
// What it means is the caller's decision. A device that already got its
// answer from the share request keeps that answer. A machine that has no
// other way to open the CRID tells the user so, and stops.
//
// TODO(upstream-contract): qurl-go returns qurl.ErrInvalidResourceRequest
// from RequestCRIDLink only before it sends anything. If it ever returns that
// error after sending a request, this must change with it.
func CRIDNotRequestable(err error) bool {
	return errors.Is(err, qurl.ErrInvalidResourceRequest)
}

// CRIDNotRequestableClass names why the SDK will not ask for a link for a
// CRID, for a diagnostic line. err is an error for which CRIDNotRequestable
// is true.
//
// The result is one word from the fixed list below, chosen by the sentinel
// err matches. It is never the SDK's own text, because that text can quote
// what the user typed: for a character outside the CRID alphabet it gives
// the byte and its position, and for a wrong length it gives the length.
//
// "unsupported_version" is the cause that reaches a caller in practice: a
// well-formed CRID whose version this SDK cannot check a link against. The
// next five are the classes of the local CRID check, under the names the
// public conformance vectors give them. A caller that sees one of those has
// found the SDK's check and the CLI's own check in disagreement, since the
// CLI refuses such a CRID before it asks. "other" is a cause this list does
// not know.
//
// TODO(upstream-contract): qurl-go refuses a CRID for a link request with
// qurl.ErrUnsupportedCRIDVersion or with exactly one of the five sentinels of
// its crid package. A cause it adds reads as "other" here until it is listed.
func CRIDNotRequestableClass(err error) string {
	switch {
	case errors.Is(err, qurl.ErrUnsupportedCRIDVersion):
		return "unsupported_version"
	case errors.Is(err, crid.ErrCharset):
		return "charset"
	case errors.Is(err, crid.ErrLength):
		return "length"
	case errors.Is(err, crid.ErrChecksum):
		return "checksum"
	case errors.Is(err, crid.ErrNonCanonical):
		return "non_canonical"
	case errors.Is(err, crid.ErrForbiddenVersion):
		return "version"
	default:
		return "other"
	}
}

// maxRefusalCodeDigits bounds a code CRIDLinkRefusalCode returns.
//
// Every code the platform defines today has five decimal digits, the six of
// this request among them. internal/output has a second bound for a code of
// the service, connectorResourceCodeDigits, and it admits exactly five. This
// one is looser on purpose, because the two codes are not the same kind:
//
//   - The code of a Connector resource answer comes from a closed list.
//     qurl-go accepts seven codes there, each of five digits, and refuses a
//     reply that carries any other. The CLI prints that code in the error
//     message itself.
//   - The code of a refused link request is open. The public conformance
//     vectors define six codes and say that any other code is a general
//     server error. qurl-go checks only the form of such a code: decimal
//     digits with no leading zero, of any length. It hands on a code it has
//     never seen, because that is what a service answers when it does not
//     serve this request. The CLI prints it only with --verbose.
//
// A bound of five here would drop the diagnostic line for a code of another
// length, which is a code nobody expected, and so the case the line is for.
// Digits cannot carry anything else the answer held, so this bound has one
// job: it keeps the line short.
//
// TODO(upstream-contract): both facts are qurl-go's. If it ever bounds the
// length of a refusal code for this request, use its bound here.
const maxRefusalCodeDigits = 16

// CRIDLinkRefusalCode returns the code the service refused a link request
// with, for a diagnostic line. ok is false when err carries no such code.
//
// The code is the one piece of the service's answer a diagnostic may show. It
// is returned only as a short run of decimal digits, so nothing else the
// answer held can ride along with it.
func CRIDLinkRefusalCode(err error) (code string, ok bool) {
	var deny *qurl.ServerDenyError
	if !errors.As(err, &deny) || deny.ErrCode == "" || len(deny.ErrCode) > maxRefusalCodeDigits {
		return "", false
	}
	for i := 0; i < len(deny.ErrCode); i++ {
		if deny.ErrCode[i] < '0' || deny.ErrCode[i] > '9' {
			return "", false
		}
	}
	return deny.ErrCode, true
}

// ClassifyCRIDLinkError maps a failed RequestCRIDLink onto the CLI's
// customer-language sentinels. The mapping is closed: an error it does not
// know is treated as an answer that failed its check, so nothing the SDK or
// the service says can reach the terminal or be acted on. deviceIdentity says
// whether this machine holds a device identity; it selects the not-found
// hint and nothing else.
//
// The order matters. A refused link also matches the sentinels of the check
// it failed, and every typed refusal is also a *qurl.ServerDenyError, so the
// specific cases come first.
//
// A refusal code outside the six this request defines reads as "cannot give
// a link right now". The service answers with a general code when the part of
// it that was asked does not serve this request yet, or was rolled back to a
// version that does not. Waiting is the remedy for both, and a newer CLI is
// not: the code says nothing about this client. qurl-go keeps such a code
// apart from its own "unavailable" error, because the request's contract
// does. The CLI gives the user one piece of advice for both, and --verbose
// shows the code (CRIDLinkRefusalCode).
//
// Callers pass an err for which CRIDNotRequestable is false.
//
// TODO(upstream-contract): the two answers about the endpoint are matched by
// qurl-go's own sentinels, the ones CRIDLinkOffered matches, and not by
// qurl.ErrNotConfigured, which qurl-go documents that both wrap. So neither
// case changes if that wrapping does. What they still depend on is the same
// as in CRIDLinkOffered: qurl.ErrCRIDLinkMisconfigured also matches
// qurl.ErrCRIDLinkNotConfigured, so it is tested first, and an unusable
// endpoint that qurl-go ever reports without it reads as "names none".
func ClassifyCRIDLinkError(err error, deviceIdentity bool) error {
	var deny *qurl.ServerDenyError
	var relay *qurl.RelayError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		// The user interrupted the command. Keep only the cancellation: the
		// SDK text beside it names the address the request was sent to.
		return context.Canceled
	case errors.Is(err, ErrAccessNotConfigured):
		// Already classified by RequestCRIDLink: the settings file.
		return err
	case errors.Is(err, qurl.ErrCRIDLinkMisconfigured):
		// The settings name a place to send the request that cannot be used.
		// CRIDLinkOffered reports the same fault before any request.
		return errCRIDLinkSetup
	case errors.Is(err, qurl.ErrCRIDLinkNotConfigured):
		// The settings name no place to send the request, although
		// CRIDLinkOffered said they did: they changed after the check. The
		// machine's settings are the fault. Nothing was sent, so this is
		// never an answer of the service, and it must not read as one that
		// failed its check.
		return ErrAccessNotConfigured
	case errors.Is(err, qurl.ErrCRIDLinkRejected), errors.Is(err, qurl.ErrCRIDLinkProtocol):
		return ErrCRIDLinkRefused
	case errors.Is(err, qurl.ErrCRIDLinkNotFound):
		if !deviceIdentity {
			return ErrCRIDNotFoundNoDevice
		}
		return ErrCRIDNotFound
	case errors.Is(err, qurl.ErrCRIDLinkUnavailable):
		return ErrCRIDLinkUnavailable
	case errors.Is(err, qurl.ErrCRIDLinkRateLimited):
		return ErrCRIDLinkRateLimited
	case errors.Is(err, qurl.ErrCRIDResourceOffline):
		return ErrCRIDPublisherOffline
	case errors.Is(err, qurl.ErrCRIDResourceClosed):
		return ErrCRIDResourceClosed
	case errors.Is(err, qurl.ErrInvalidCRIDLinkRequest):
		return ErrCRIDLinkRequestRejected
	case errors.Is(err, qurl.ErrServerOverloaded):
		return ErrAccessBusy
	case errors.As(err, &deny):
		// A refusal code this request does not define: a general code.
		return ErrCRIDLinkUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &relay):
		// No answer in time, or none at all.
		return ErrCRIDLinkNoAnswer
	case errors.Is(err, qurl.ErrNotConfigured):
		// The SDK has no deployment settings to check any link with. The
		// machine's settings are the fault here too.
		return ErrAccessNotConfigured
	default:
		// This includes a reply that does not prove where it came from.
		return ErrCRIDLinkRefused
	}
}
