package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// This file is the one place `qurl get` decides where its link comes from.
//
// The share request is the path get has always had. It uses this device's
// identity, and the service answers it on the resource owner's devices and on
// the devices a publisher allowed. Every other device gets "not found".
//
// The second path asks the service for a link with only the CRID. It uses no
// device identity. The SDK makes the request and returns a link only after it
// has checked the link against the CRID.
//
// Whether this machine can ask with the CRID alone at all is a fact about its
// deployment settings. get learns it from opts.cridLinkOffered, which needs no
// CRID, sends nothing and creates nothing. It has three answers: the request
// is offered, it is not offered, or the settings are wrong.
//
// The rules:
//
//   - A device with an identity uses the share request first. Only when the
//     service answers that request "not found" does get look at the second
//     path.
//   - A machine with no identity, where the request is offered, asks with the
//     CRID alone, and the answer is final. That includes the answer "this
//     client cannot ask for this CRID". Such a machine does not create an
//     identity to open somebody else's resource.
//   - Where the request is not offered, nothing changes for anyone: get does
//     exactly what it did before that request existed. For a machine with no
//     identity that includes creating one, as before. This is the case under
//     the deployment every release ships today.
//   - Settings that name a place to send the request and cannot be used are a
//     fault of this install. get reports it and does nothing else: no request
//     and no new identity. A device with an identity is told so only after
//     its own share request was answered "not found", because its share
//     request does not depend on those settings.
//   - The rules above give the first link. One run of the command can need a
//     second one: a download asks again when its link expired before any
//     byte was served (a refresh). Once a link given for the CRID alone has
//     passed the link check and is in use, the refresh asks with the CRID
//     alone too, and that answer is final. It sends no share request and
//     does not ask again whether the request is offered. See the second
//     table.
//
// The whole table. "CRID" is one that passed the local check every command
// applies first; a CRID that fails it is refused before this file runs, on
// every machine, with nothing sent and nothing created.
//
//	identity  request       CRID this client   result
//	                        can ask for
//	no        offered       yes                ask; the answer is final
//	no        offered       no                 refused; nothing sent or created
//	no        not offered   either             share request, as before
//	no        wrong setup   either             setup error; nothing sent or created
//	yes       offered       yes                share; on "not found", ask
//	yes       offered       yes, and that      share; on "not found", ask; then
//	                        request gets       "the service did not answer",
//	                        no answer          and not "not found"
//	yes       offered       no                 share; its answer stands
//	yes       not offered   either             share; its answer stands
//	yes       wrong setup   either             share; on "not found", setup error
//
// The row with no answer is a decision. When the share request says "not
// found" and the request with the CRID alone times out or cannot reach the
// service, nobody knows whether the resource opens with the CRID alone. "Not
// found" would be wrong for every public resource, so get says that the
// service did not answer and that the user can try again later.
//
// The refresh, by the link that is in use when a download asks again:
//
//	link in use came from   refresh
//	the share request       decided again by the table above
//	the request with the    ask with the CRID alone; the answer is final;
//	CRID alone              no share request
//
// The second row is a decision too. The share request cannot give that run
// a link: it already answered "not found", or the machine has no identity to
// make it with. Sending it again would name this device to the service once
// more for nothing. And if it then failed in any other way, for example
// because the service was busy for a moment, that failure would end a
// download that links for the CRID alone were serving. The first row keeps
// the rule get always had: a link from the share request does not fix the
// choice, so when the share request says "not found" at the refresh, get
// asks with the CRID alone.
//
// getLinkSource holds that memory for one run. Nothing is kept between runs.
//
// `qurl share` does not use this file. It always shares with the device.

// errCRIDNotRequestable reports that the SDK will not ask for a link for this
// CRID and sent nothing. errCRIDVersionNotRequestable is the one cause that
// reaches it in practice: a CRID version this client cannot check a link
// against. Neither leaves this file. linkForGet turns them into the share
// request's own answer on a device with an identity, and into a refusal on a
// machine with none.
var (
	errCRIDNotRequestable        = errors.New("no link can be asked for with this CRID alone")
	errCRIDVersionNotRequestable = fmt.Errorf("%w: its version is not one this client can check", errCRIDNotRequestable)
)

// hasDeviceIdentity reports whether this machine can make the share request
// without creating a new device identity that belongs to no account.
//
// It is true for a machine that already holds device state. It is also true
// for a machine with an account key in its environment: that key is the
// owner's own instruction to enroll this machine under their account, and an
// owner's fresh machine must keep opening the owner's resources.
//
// It is false only for a machine with neither. Opening the device client
// there would create an identity and register it, which a person who was only
// handed a CRID did not ask for.
//
// The check reads nothing but two file names and the environment, and it
// creates nothing. When it cannot tell, it answers true, so the doubt is
// reported by the share path exactly as before.
//
// TODO(upstream-contract): "holds device state" is the presence of one of the
// two state files qurl-connector writes (connectorstate.EnvelopePresent). A
// third file name would read as "no identity" here until it is added there.
func (opts *globalOpts) hasDeviceIdentity() bool {
	if accountKeyConfigured(opts.lookupEnv) {
		return true
	}
	stateDir, err := opts.resolveShareStateDir("")
	if errors.Is(err, connectorstate.ErrNoDefaultStateDir) {
		// No place for device state exists on this host, so none is held.
		return false
	}
	if err != nil {
		return true
	}
	return connectorstate.EnvelopePresent(stateDir)
}

// getLinkSource gives one run of get its links: the first one, and the one a
// download asks for when its link expired before any byte was served.
//
// It remembers one fact about the run: a link given for the CRID alone
// passed the link check and is in use. From then on next asks with the CRID
// alone and sends no share request. The table at the top of this file says
// why.
//
// The downloader asks for its links one after the other, never at the same
// time, so the memory needs no lock.
type getLinkSource struct {
	opts       *globalOpts
	assessment *cridux.Assessment
	options    qurlapi.ShareOptions
	// cridAlone is the memory. Only verified sets it.
	cridAlone bool
}

// linkSourceForGet returns the link source of one run of get.
func (opts *globalOpts) linkSourceForGet(assessment *cridux.Assessment, options qurlapi.ShareOptions) *getLinkSource {
	return &getLinkSource{opts: opts, assessment: assessment, options: options}
}

// next returns the next link of the run and reports whether it was given for
// the CRID alone. The caller verifies the link, and then calls verified.
func (s *getLinkSource) next(ctx context.Context) (link *qurlapi.ShareLink, byCRIDAlone bool, err error) {
	if !s.cridAlone {
		return s.opts.linkForGet(ctx, s.assessment, s.options)
	}
	// The identity is looked at only to pick the not-found hint.
	link, err = s.opts.linkByCRIDAlone(ctx, s.assessment.Input, s.opts.hasDeviceIdentity())
	if errors.Is(err, errCRIDNotRequestable) {
		// The SDK asked for this same CRID earlier in the run, so it does not
		// give this answer now. If it ever does, there is no share answer to
		// fall back on, and the error must not leave this file.
		err = refusalForCRIDNotRequestable(err)
	}
	return link, err == nil, err
}

// verified records that the link next returned passed the link check and is
// now the link in use. byCRIDAlone is what next reported for it.
func (s *getLinkSource) verified(byCRIDAlone bool) {
	if byCRIDAlone {
		s.cridAlone = true
	}
}

// linkForGet returns the link get acts on when the run has not yet used a
// link given for the CRID alone: the first link, and a refresh after a link
// from the share request. byCRIDAlone reports that the link came from the
// request that uses only the CRID, which cannot carry a session duration.
//
// The caller verifies the link against the CRID and opens it the same way
// whichever path it came from.
func (opts *globalOpts) linkForGet(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (link *qurlapi.ShareLink, byCRIDAlone bool, err error) {
	if !opts.hasDeviceIdentity() {
		return opts.linkWithNoIdentity(ctx, assessment, options)
	}

	link, shareErr := opts.shareLinkForGet(ctx, assessment, options)
	if shareErr == nil || !shareNotFound(shareErr) {
		return link, false, shareErr
	}

	// The service will not let this device share the CRID. The resource may
	// still be one that opens with the CRID alone.
	offered, err := opts.cridLinkOffered()
	switch {
	case err != nil:
		// The settings are wrong, so whether the CRID opens that way is not
		// known. Saying "not found" here would hide the fault.
		return nil, false, err
	case !offered:
		// Not possible here: the share request's own answer stands, unchanged.
		return nil, false, shareErr
	}
	link, err = opts.linkByCRIDAlone(ctx, assessment.Input, true)
	if errors.Is(err, errCRIDNotRequestable) {
		// Not possible for this CRID: the share request's answer stands too.
		return nil, false, shareErr
	}
	// From here the second request's answer is the result. That includes no
	// answer at all, which does not bring back the share request's "not
	// found": see the table at the top of this file.
	return link, err == nil, err
}

// linkWithNoIdentity is linkForGet for a machine that holds no device
// identity and has no account key.
//
// Where the request with only the CRID is offered, its answer is final, a
// refusal included, and so is a fault in the settings. In both cases this
// function returns before shareLinkForGet, which is the only way from here to
// newClient, where an identity is created and registered.
//
// Where the request is not offered, nothing was sent and get does what it did
// before that request existed: the share request, which enrolls this machine
// first.
func (opts *globalOpts) linkWithNoIdentity(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (link *qurlapi.ShareLink, byCRIDAlone bool, err error) {
	offered, err := opts.cridLinkOffered()
	switch {
	case err != nil:
		return nil, false, err
	case !offered:
		link, err = opts.shareLinkForGet(ctx, assessment, options)
		return link, false, err
	}
	link, err = opts.linkByCRIDAlone(ctx, assessment.Input, false)
	if errors.Is(err, errCRIDNotRequestable) {
		err = refusalForCRIDNotRequestable(err)
	}
	return link, err == nil, err
}

// shareLinkForGet is the path get has always had: the share request with
// this device's identity, then the check that the answer names the CRID that
// was asked for.
func (opts *globalOpts) shareLinkForGet(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (*qurlapi.ShareLink, error) {
	link, err := opts.shareResource(ctx, assessment.Input, options)
	if err := verifyShareLink(assessment, link, err); err != nil {
		return nil, err
	}
	return link, nil
}

// refusalForCRIDNotRequestable is what a machine with no identity is told
// when the SDK will not ask for a link for the CRID. That machine has no
// other way to open it: the share request would first create an identity,
// and a person who was only handed a CRID did not ask for one. So the
// refusal is final.
//
// A CRID version this client cannot check gets the message the link check
// has always had for it. Any other cause is a CRID the SDK calls invalid,
// which the local check refuses before this file runs; it gets that check's
// own answer here, so the two cannot disagree.
func refusalForCRIDNotRequestable(err error) error {
	if errors.Is(err, errCRIDVersionNotRequestable) {
		return consume.ErrUnsupportedCRIDVersion
	}
	return errValidCRIDRequired()
}

// sessionDurationNoteOnce returns the function get calls with each verified
// link. When the user asked for a session duration and the link was given for
// the CRID alone, it says that the flag was not applied: that request cannot
// carry one, and a requested lifetime is never dropped silently. It says so
// once. A link refreshed in the middle of a download is the same case.
func sessionDurationNoteOnce(printer *output.Printer, requested time.Duration) func(byCRIDAlone bool) {
	noted := false
	return func(byCRIDAlone bool) {
		if !byCRIDAlone || requested == 0 || noted {
			return
		}
		noted = true
		printer.Notef(msgSessionDurationNotApplied)
	}
}

// shareNotFound reports whether err is the share request's not-found answer,
// the one answer a device that is neither the owner's nor allowed receives.
func shareNotFound(err error) bool {
	var apiErr *qurlapi.Error
	return errors.As(err, &apiErr) && apiErr.ShareNotFound()
}

// linkByCRIDAlone asks for a link with only the CRID and returns it in the
// shape a share answer has, so everything after it is the code get already
// had. device says whether this machine holds a device identity; it only
// selects the not-found hint.
//
// It returns errCRIDNotRequestable when the SDK will not ask for this CRID
// and sent nothing. Every other failure comes back as one of the CLI's fixed
// messages; the SDK's own error text never does.
func (opts *globalOpts) linkByCRIDAlone(ctx context.Context, resourceCRID string, device bool) (*qurlapi.ShareLink, error) {
	issued, err := opts.requestCRIDLink(ctx, resourceCRID)
	switch {
	case consume.CRIDNotRequestable(err):
		if errors.Is(err, qurl.ErrUnsupportedCRIDVersion) {
			return nil, errCRIDVersionNotRequestable
		}
		return nil, errCRIDNotRequestable
	case err != nil:
		opts.noteCRIDLinkRefusalCode(err)
		return nil, consume.ClassifyCRIDLinkError(err, device)
	case issued == nil || issued.Link == "":
		// No error and no link is not an answer the SDK gives. Fail closed.
		return nil, consume.ErrCRIDLinkRefused
	}
	return shareLinkFromCRIDLink(resourceCRID, issued), nil
}

// noteCRIDLinkRefusalCode writes the code the service refused a link request
// with as a --verbose diagnostic. The message the user gets never carries it:
// several codes share one message, and the code is what tells them apart for
// whoever looks into a failure.
func (opts *globalOpts) noteCRIDLinkRefusalCode(err error) {
	logf := opts.verboseLogger()
	if logf == nil {
		return
	}
	if code, ok := consume.CRIDLinkRefusalCode(err); ok {
		logf(msgCRIDLinkRefusalCode, code)
	}
}

// shareLinkFromCRIDLink copies a link the SDK issued for resourceCRID into the
// CLI's share result.
//
// CRID is the CRID that was asked for: the SDK returns a link only after it
// has bound the link to that CRID. Everything else the service sent beside
// the link is display-only and not covered by that check, exactly like the
// same fields of a share answer. The fields are copied one by one, never by a
// type conversion, so a field the SDK adds later cannot reach the publisher
// rendering unreviewed.
//
// A link asked for this way reports no link type, no lifetime in seconds and
// no single-use flag, so those stay zero. Its id is not kept: the share path
// does not show one either.
//
// TODO(upstream-contract): Publisher.Verified is copied as qurl-go reports
// it. qurl-go documents the flag as the service's statement: it is true only
// when the service explicitly reported a verified publisher, in a reply that
// the SDK accepted as authenticated to the server key of the deployment
// settings. The name beside it is the publisher's own choice, and the SDK
// call that sets a name cannot ask for verification. qurl-go also documents
// that the flag is not covered by the check of the link against the CRID. So
// this code shows the flag exactly as the share path shows the same flag of
// a share answer, and it means no more here than it does there. If qurl-go
// ever fills the flag from anything but that reply, this copy must report
// every publisher as unverified.
func shareLinkFromCRIDLink(resourceCRID string, issued *qurl.CRIDLink) *qurlapi.ShareLink {
	link := &qurlapi.ShareLink{
		QURL:      issued.Link,
		CRID:      resourceCRID,
		ExpiresAt: issued.ExpiresAt,
		Publisher: qurlapi.Publisher{Name: issued.Publisher.Name, Verified: issued.Publisher.Verified},
	}
	if issued.ResourceCreatedAt != nil && !issued.ResourceCreatedAt.IsZero() {
		createdAt := *issued.ResourceCreatedAt
		link.ResourceCreatedAt = &createdAt
	}
	return link
}
