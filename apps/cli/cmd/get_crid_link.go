package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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
// There are two ways to a link.
//
// The link request asks the service for a link for a CRID. The SDK sends it
// through the deployment's relay and returns a link only after it has checked
// the link against the CRID. It has two forms:
//
//   - With the CRID alone. It uses no device identity. The service gives a
//     link for a public resource.
//   - As this device. The SDK follows one rule: a random key first, the
//     device key only after "not found". So a public resource is answered as
//     above, and the device key is used only when the first answer is "not
//     found". The service then also gives a link for a private resource that
//     this device may open: the owner's own device, or a device the owner
//     allowed. The device key is read at that moment and not before: get
//     gives the SDK a function that reads it (opts.deviceKeyOnDemand).
//
// The share request is a request to the qURL API with this device's
// credential. The service answers it on the resource owner's devices and on
// the devices a publisher allowed. Every other device gets "not found". It
// is the path get has always had, and the only one that can carry a share
// option. get has one share option: --session-duration.
//
// Whether this machine can make the link request at all is a fact about its
// deployment settings. get learns it from opts.cridLinkOffered, which needs
// no CRID, sends nothing and creates nothing. It has three answers: the
// request is offered, it is not offered, or the settings are wrong.
//
// "A device with an identity" is a machine that can make the share request
// without creating an identity that belongs to no account
// (hasDeviceIdentity). There are two kinds: a machine that holds device
// state, and a machine that has an account key in its environment and no
// device state yet. "A machine with no identity" has neither. The rules
// below are the same for both kinds of device with an identity, and this is
// intended: a machine with only an account key also makes the link request
// before its share request. It has no device key, so it asks with the CRID
// alone. For a resource it may share, that is one request more than it sent
// before the link request came first, and it waits for the answer for up to
// the short time limit (cridLinkTimeoutBeforeShare).
//
// The rules:
//
//   - A device with an identity, where the request is offered and no share
//     option is set, makes the link request first. A link ends the run, and
//     no share request is sent. Any other answer leads to the share request.
//     In one case the device then makes the link request once more: the
//     first one had no answer when its short time limit ran out, and the
//     share request said "not found". The text below the second table says
//     why. With that request, every case that gave a link before this order
//     existed still gives one, when the service gives the same answers. This
//     order costs requests and time. It does not cost a link.
//   - That device asks as this device. Its key is read only when the SDK
//     needs it: after the first request of the link request was answered
//     "not found". For every other answer the key is not read at all, so a
//     public resource costs no read of the device state. The key is read
//     without changing the device state (opts.readDeviceKey). A read that
//     gives no key is not an error. The device has then asked with the CRID
//     alone, and the answer to that first request, "not found", is the
//     result of the link request. A machine with only an account key has no
//     device state, so its read always gives no key.
//   - When a share option is set, get keeps the order it had before: the
//     share request first, and the link request with the CRID alone only
//     after "not found". Only the share request can carry the option.
//   - A machine with no identity, where the request is offered, asks with the
//     CRID alone, and the answer is final. That includes the answer "this
//     client cannot ask for this CRID". Such a machine does not create an
//     identity to open somebody else's resource.
//   - Where the request is not offered, nothing changes for anyone: get does
//     exactly what it did before the link request existed, and no key is
//     read. For a machine with no identity that includes creating one, as
//     before. This is the case under the deployment every release ships
//     today.
//   - Settings that name a place to send the request and cannot be used are a
//     fault of this install. A machine with no identity is told so, and
//     nothing else happens: no request and no new identity. A device with an
//     identity is told so only after its own share request was answered "not
//     found", because its share request does not depend on those settings.
//   - The rules above give the first link. One run of the command can need a
//     second one: a download asks again when its link expired before any
//     byte was served (a refresh). The third table says how.
//
// The first link. "CRID" is one that passed the local check every command
// applies first; a CRID that fails it is refused before this file runs, on
// every machine, with nothing sent and nothing created. "Key" is what
// opts.readDeviceKey gives when it is called. It is called only when the
// first request of the link request is answered "not found". For every other
// answer the two rows that differ in "key" are the same row, and nothing is
// read.
//
//	identity  share   request      CRID this   key       result
//	          option               client can
//	                               ask for
//	no        either  offered      yes         -         ask with the CRID alone; the answer is final
//	no        either  offered      no          -         refused; nothing sent or created
//	no        either  not offered  either      -         share request, as before
//	no        either  wrong setup  either      -         setup error; nothing sent or created
//	yes       either  not offered  either      not read  share; its answer stands
//	yes       either  wrong setup  either      not read  share; on "not found", setup error
//	yes       set     offered      yes         not read  share; on "not found", ask with the CRID alone
//	yes       set     offered      no          not read  share; its answer stands
//	yes       not set offered      yes         yes       ask as this device; then see the second table
//	yes       not set offered      yes         no        ask with the CRID alone; then see the second table
//	yes       not set offered      no          either    nothing is sent for a link; share; its answer stands
//
// The second table is the last three rows written out: a device with an
// identity that made the link request first.
//
//	answer to the link request        answer to the     result
//	                                  share request
//	link                              (not asked)       the link
//	"not found"                       link              the share link
//	"not found"                       "not found"       "not found", with the hint for a device
//	the short time limit ran out      link              the share link
//	the short time limit ran out      "not found"       the link request once more: see below
//	no answer for another reason      link              the share link
//	no answer for another reason      "not found"       "the service did not answer"
//	another refusal                   link              the share link
//	another refusal                   "not found"       that refusal
//	not sent: this client cannot      link              the share link
//	  ask for the CRID
//	not sent: this client cannot      "not found"       the share request's "not found"
//	  ask for the CRID
//	any answer that is not a link     another failure   the share request's failure
//	interrupted by the user           (not asked)       exit code 130 and no error text
//
// These are the results get gave before, with the two questions in the other
// order. One row is a decision. When the share request says "not found" and
// the link request got no answer, nobody knows whether the resource opens for
// this device. "Not found" would be wrong for every public resource, so get
// says that the service did not answer and that the user can try again later.
//
// The link request that comes before a share request has a shorter time
// limit than the one whose answer is final (cridLinkTimeoutBeforeShare). A
// relay that does not answer must not hold back for long a share request
// that would give the link.
//
// The short limit must not cost a link that the long limit gives. Before the
// link request came first, a device that may not share a public resource got
// "not found" from its share request, and then made one link request with
// the CRID alone and the long limit. A relay that needs more time than the
// short limit, and less than the long one, gave that device its link. So in
// the row "the link request once more", get makes that same request: once,
// with the CRID alone, and with the long limit. It is not made as this
// device, also when the first request was: the share request has already
// said that this device may not open the resource. The answer of this
// request is the result, as it was when the share request came first:
//
//   - A link is used. A refresh of that link asks with the CRID alone: the
//     third row of the refresh table.
//   - "Not found", another refusal, and no answer are told to the user as
//     they are. No third request is made.
//   - "This client cannot ask for the CRID" is not an answer the SDK gives
//     here, because it sent the first request for the same CRID. If it ever
//     does, the share request's "not found" stands.
//   - An interrupt by the user ends the command with exit code 130 and no
//     error text.
//
// "The short time limit ran out" means three things together: the link
// request ended with "the service did not answer", the time of the short
// limit was over, and the command's own context had not ended. So it is not
// this row when the user interrupted the command, and not when the command's
// own context ended. "No answer for another reason" is a service that could
// not be reached, or a relay that answered with an error. That answer came
// inside the short limit, and the long limit would not change it, so the
// request is not made once more.
//
// The row costs time. The command can wait for the short limit, then for the
// share request, then for the long limit, before it says that the service
// did not answer. The order where the share request comes first does not
// wait for the short limit.
//
// The refresh, by the link that is in use when a download asks again:
//
//	link in use came from        refresh
//	the share request            decided again by the first table
//	the link request, and the    decided again by the first table: the link
//	share request was not        request first, and the share request only
//	asked for that link          if it gives no link
//	the link request, and the    the link request with the CRID alone; the
//	share request cannot give    answer is final; no share request
//	this run a link
//
// The share request cannot give a run a link when it already answered "not
// found" for the link in use, or when the machine has no identity to make it
// with. That is the third row, and it is a decision. Sending the share
// request again would name this device to the service once more for nothing.
// And if it then failed in any other way, for example because the service
// was busy for a moment, that failure would end a download that the link
// request was serving. In the second row the share request was never asked,
// so it can still help: a refresh there that gets no link for a moment, for
// example "too many requests", goes on to the share request and the download
// goes on. The first row keeps the rule get always had: a link from the
// share request does not fix the choice.
//
// A refresh that needs the key reads it again. The key is not kept for the
// length of a download: the SDK wipes it when the request has been answered.
//
// getLinkSource holds the memory of one run. Nothing is kept between runs.
//
// `qurl share` does not use this file. It always shares with the device.

// cridLinkTimeoutBeforeShare bounds the link request of a device that makes
// its share request afterwards when no link is given. It covers the whole
// request as this device, which can be two requests and the read of the
// device key between them. The link request whose answer is final keeps the
// longer limit that internal/consume sets.
const cridLinkTimeoutBeforeShare = 10 * time.Second

// timeoutBeforeShare returns the time limit of the link request that comes
// before a share request. It is cridLinkTimeoutBeforeShare, unless a test set
// another limit.
func (opts *globalOpts) timeoutBeforeShare() time.Duration {
	if opts.linkTimeoutBeforeShare > 0 {
		return opts.linkTimeoutBeforeShare
	}
	return cridLinkTimeoutBeforeShare
}

// linkOrigin says where a link of this run came from. A refresh is decided
// by the origin of the link in use: see the third table at the top of this
// file.
type linkOrigin int

const (
	// linkFromShare is a link from the share request.
	linkFromShare linkOrigin = iota
	// linkFromRequest is a link from the link request, for which the share
	// request was not asked.
	linkFromRequest
	// linkFromRequestOnly is a link from the link request in a run where the
	// share request cannot give a link: it answered "not found", or the
	// machine has no identity.
	linkFromRequestOnly
)

// byLinkRequest reports that the link came from the link request, which
// cannot carry a session duration.
func (o linkOrigin) byLinkRequest() bool { return o != linkFromShare }

// errCRIDNotRequestable reports that the SDK will not ask for a link for this
// CRID and sent nothing. errCRIDVersionNotRequestable is the one cause that
// reaches it in practice: a CRID version this client cannot check a link
// against. errDeviceKeyRefused reports that the SDK will not ask as a device
// with what it was given, and sent nothing. None of the three leaves this
// file. The functions below turn the first two into the share request's own
// answer on a device with an identity, and into a refusal on a machine with
// none. The third makes the device ask with the CRID alone.
var (
	errCRIDNotRequestable        = errors.New("no link can be asked for with this CRID alone")
	errCRIDVersionNotRequestable = fmt.Errorf("%w: its version is not one this client can check", errCRIDNotRequestable)
	errDeviceKeyRefused          = errors.New("the device key cannot be used for a link request")
)

// errNoDeviceKey is what the function that reads the device key for the SDK
// returns when the read gave no key. why is the one fixed word for the
// reason. The error does not leave this file, and it holds no path, no other
// error text and no part of a key.
type errNoDeviceKey struct{ why connectorstate.NoDeviceKey }

func (e errNoDeviceKey) Error() string { return "no device key: " + string(e.why) }

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
// It remembers one fact about the run: where the link in use came from. When
// that is the link request, and the share request cannot give this run a
// link, next asks with the CRID alone and sends no share request. The third
// table at the top of this file says why.
//
// The downloader asks for its links one after the other, never at the same
// time, so the memory needs no lock.
type getLinkSource struct {
	opts       *globalOpts
	assessment *cridux.Assessment
	options    qurlapi.ShareOptions
	// inUse is the memory. Only verified sets it.
	inUse linkOrigin
}

// linkSourceForGet returns the link source of one run of get.
func (opts *globalOpts) linkSourceForGet(assessment *cridux.Assessment, options qurlapi.ShareOptions) *getLinkSource {
	return &getLinkSource{opts: opts, assessment: assessment, options: options}
}

// next returns the next link of the run and where it came from. The caller
// verifies the link, and then calls verified.
func (s *getLinkSource) next(ctx context.Context) (*qurlapi.ShareLink, linkOrigin, error) {
	if s.inUse != linkFromRequestOnly {
		return s.opts.linkForGet(ctx, s.assessment, s.options)
	}
	// The identity is looked at only to pick the not-found hint.
	link, err := s.opts.linkByCRIDAlone(ctx, s.assessment.Input, s.opts.hasDeviceIdentity())
	if errors.Is(err, errCRIDNotRequestable) {
		// The SDK asked for this same CRID earlier in the run, so it does not
		// give this answer now. If it ever does, there is no share answer to
		// fall back on, and the error must not leave this file.
		err = refusalForCRIDNotRequestable(err)
	}
	return linkWithOrigin(link, linkFromRequestOnly, err)
}

// verified records that the link next returned passed the link check and is
// now the link in use. origin is what next reported for it.
func (s *getLinkSource) verified(origin linkOrigin) {
	s.inUse = origin
}

// linkWithOrigin is the result of one way to a link: the link and its origin,
// or the error and no origin.
func linkWithOrigin(link *qurlapi.ShareLink, origin linkOrigin, err error) (*qurlapi.ShareLink, linkOrigin, error) {
	if err != nil {
		return nil, linkFromShare, err
	}
	return link, origin, nil
}

// linkForGet returns the link get acts on when the first table decides: the
// first link of a run, and a refresh that is decided again. origin says
// where the link came from.
//
// The caller verifies the link against the CRID and opens it the same way
// whichever path it came from.
func (opts *globalOpts) linkForGet(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (link *qurlapi.ShareLink, origin linkOrigin, err error) {
	if !opts.hasDeviceIdentity() {
		return opts.linkWithNoIdentity(ctx, assessment, options)
	}
	if options != (qurlapi.ShareOptions{}) {
		// Only the share request can carry a share option, so it comes
		// first, as it always did. Whether the link request is offered is
		// looked at only after "not found".
		return opts.linkShareFirst(ctx, assessment, options, opts.cridLinkOffered)
	}
	offered, offerErr := opts.cridLinkOffered()
	if offerErr != nil || !offered {
		// No link request can be made here. This is the code get had before
		// the link request existed, with the answer that was just given. No
		// key is read.
		return opts.linkShareFirst(ctx, assessment, options, func() (bool, error) { return offered, offerErr })
	}
	return opts.linkRequestFirst(ctx, assessment, options)
}

// linkShareFirst is the order get had before the link request came first:
// the share request, and the link request with the CRID alone only after
// "not found". offered answers whether the link request is offered. It is
// called at most once, and only after "not found".
func (opts *globalOpts) linkShareFirst(
	ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions, offered func() (bool, error),
) (*qurlapi.ShareLink, linkOrigin, error) {
	link, shareErr := opts.shareLinkForGet(ctx, assessment, options)
	if shareErr == nil || !shareNotFound(shareErr) {
		return linkWithOrigin(link, linkFromShare, shareErr)
	}

	// The service will not let this device share the CRID. The resource may
	// still be one that opens with the CRID alone.
	canAsk, err := offered()
	switch {
	case err != nil:
		// The settings are wrong, so whether the CRID opens that way is not
		// known. Saying "not found" here would hide the fault.
		return nil, linkFromShare, err
	case !canAsk:
		// Not possible here: the share request's own answer stands, unchanged.
		return nil, linkFromShare, shareErr
	}
	// With the CRID alone, and not as this device: the share request has
	// already said that this device may not open the resource. A request
	// under the device key would name the device again for nothing.
	link, err = opts.linkByCRIDAlone(ctx, assessment.Input, true)
	if errors.Is(err, errCRIDNotRequestable) {
		// Not possible for this CRID: the share request's answer stands too.
		return nil, linkFromShare, shareErr
	}
	// From here the link request's answer is the result. That includes no
	// answer at all, which does not bring back the share request's "not
	// found": see the second table at the top of this file.
	return linkWithOrigin(link, linkFromRequestOnly, err)
}

// linkRequestFirst is linkForGet for a device with an identity, where the
// link request is offered and no share option is set: the link request
// first, and the share request only when it gives no link. In one case the
// link request is then made once more. The second table at the top of this
// file, with the text below it, is this function.
func (opts *globalOpts) linkRequestFirst(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (*qurlapi.ShareLink, linkOrigin, error) {
	link, limitRanOut, requestErr := opts.linkByRequestBeforeShare(ctx, assessment.Input)
	if requestErr == nil {
		// A link ends the run. The share request is not sent.
		return link, linkFromRequest, nil
	}
	if errors.Is(requestErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		// The user interrupted the command. It stops here, and does not go on
		// to a request the user did not wait for.
		return nil, linkFromShare, context.Canceled
	}

	link, shareErr := opts.shareLinkForGet(ctx, assessment, options)
	switch {
	case shareErr == nil || !shareNotFound(shareErr):
		// A link, or a failure that is the share request's own.
		return linkWithOrigin(link, linkFromShare, shareErr)
	case errors.Is(requestErr, errCRIDNotRequestable):
		// Nothing was sent for a link, because this client cannot ask for
		// this CRID. The share request's "not found" is the only answer.
		return nil, linkFromShare, shareErr
	case !limitRanOut:
		// The share request said "not found", so the link request's answer is
		// the result, as it was when the share request came first.
		return nil, linkFromShare, requestErr
	}

	// The share request said "not found", and the link request had no answer
	// when its short limit ran out. The order where the share request comes
	// first makes one link request at this point, with the long limit, and a
	// relay that is slow can still answer it. So get makes that request now:
	// once, with the CRID alone, and with the command's own context, so the
	// only limit is the long one that internal/consume sets. It is not made
	// as this device, for the reason linkShareFirst gives.
	link, err := opts.linkByCRIDAlone(ctx, assessment.Input, true)
	if errors.Is(err, errCRIDNotRequestable) {
		// The SDK sent the first request for this same CRID, so it does not
		// give this answer now. If it ever does, the share request's "not
		// found" stands, as in linkShareFirst, and the error does not leave
		// this file.
		return nil, linkFromShare, shareErr
	}
	// The answer of this request is the result, whatever it is. The share
	// request cannot give this run a link, so the origin is the one a
	// refresh asks with the CRID alone for.
	return linkWithOrigin(link, linkFromRequestOnly, err)
}

// linkByRequestBeforeShare makes the link request of a device that makes its
// share request afterwards when no link is given. It asks as this device.
//
// The device key is not read here. The SDK gets a function that reads it
// (deviceKeyOnDemand) and calls it only when the first request was answered
// "not found". So a request that the first answer settles reads nothing. The
// request has the limit cridLinkTimeoutBeforeShare, and that limit covers the
// read of the key too.
//
// limitRanOut reports that the request had no answer when that short limit
// ran out. It is true only when three things hold together:
//
//   - The result is "the service did not answer". Any other result is an
//     answer, also when it comes at the moment the limit runs out.
//   - The context that carries the short limit ended because its time was
//     over.
//   - The command's own context has not ended. When the user interrupts the
//     command, or the command's own time limit runs out, the command's
//     context ends, and the context of the request ends with it. That is not
//     the short limit running out.
//
// So limitRanOut is false for a service that could not be reached: that
// answer comes while the short limit still has time left. It is true when
// the limit ran out while the key was read: the SDK then reports a request
// that was not sent because its time was over.
//
// TODO(upstream-contract): the first condition rests on the order of the
// cases in consume.ClassifyCRIDLinkError. It tests qurl-go's
// ErrServerOverloaded and *ServerDenyError before the deadline of the
// context, and today qurl-go reports a deadline that ran out as neither. If
// qurl-go ever wraps a deadline in one of the two, the result is no longer
// "the service did not answer", and the request under the full limit is not
// made. TestGetMakesTheLinkRequestOnceMoreThroughTheSDK holds today's
// behavior through the SDK.
func (opts *globalOpts) linkByRequestBeforeShare(ctx context.Context, resourceCRID string) (link *qurlapi.ShareLink, limitRanOut bool, err error) {
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeoutBeforeShare())
	defer cancel()
	ranOut := func(requestErr error) bool {
		return errors.Is(requestErr, consume.ErrCRIDLinkNoAnswer) &&
			errors.Is(requestCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	}
	link, err = opts.linkAsDevice(requestCtx, opts.deviceKeyOnDemand(ctx), resourceCRID)
	if !errors.Is(err, errDeviceKeyRefused) {
		return link, ranOut(err), err
	}
	// The SDK will not ask as a device with what it was given, and sent
	// nothing. That is a device that cannot ask with its key, like one that
	// could not read it.
	if logf := opts.verboseLogger(); logf != nil {
		logf(msgCRIDLinkDeviceKeyNotRead, string(connectorstate.NoDeviceKeyInvalid))
	}
	link, err = opts.linkByCRIDAlone(requestCtx, resourceCRID, true)
	return link, ranOut(err), err
}

// deviceKeyOnDemand returns the function the SDK calls when a request as this
// device needs the device key. The SDK calls it at most once in a request,
// and only after the first request was answered "not found". Until then
// nothing is read: the device state is not opened, and on a machine that
// keeps its state sealed nothing is unsealed.
//
// ctx is the command's own context. The read gets that context, and not the
// one the SDK passes, which carries the short time limit of the request.
// connectorstate.ReadDeviceStaticPrivateKey says why: a read that is given up
// early can break a later step of the same command that opens the state. The
// short limit still ends the request. The SDK does not wait for the read
// when the context of the request ends, and it wipes a key that arrives
// after that.
//
// So a read that is slow can still run when get goes on to its share
// request, and the share request opens the same device state. The two are
// safe at the same time:
//
//   - The read changes nothing and takes no lock. It opens the state with a
//     reader that has no way to write
//     (connectorstate.ReadDeviceStaticPrivateKey): qurl-go's
//     OpenFileAgentStateReadOnly for the plaintext file, and
//     qurl-connector's OpenSDKStateReader for state sealed to the TPM. Both
//     document that open, load and close create no lock and write nothing.
//     The lock of the share request's store is on a file of its own beside
//     the state file, and the read never opens it. So the read cannot hold
//     back the share request, and it cannot make its open, load or save
//     fail.
//   - The share request's store replaces the state file by a rename. The
//     read gets the whole file. If the file was replaced while it was read,
//     the read finds that out and gives no key ("unreadable"). It never
//     returns a part of a file.
//   - Nothing uses what a late read returns. The SDK has stopped waiting,
//     and it wipes the key.
//   - On a machine that seals its state to the TPM, the state is then
//     unsealed twice: for the read, and for the share request. The unseal of
//     the read is work for nothing. Each unseal opens a connection of its
//     own to the TPM, and none of them asks the user anything. A key storage
//     that could ask the user, or call a service, is one the read refuses
//     before it opens the state.
//   - One thing in the process is shared. After a TPM call was given up
//     because its context ended, qurl-connector lets later TPM calls fail at
//     once for some time. That is the reason the read gets the command's
//     context: with the short limit, a run in which the limit ran out during
//     the read could then fail its share request. With the command's
//     context, a TPM call of the read is given up only when the command
//     ends, or when the TPM itself does not answer in time. The share
//     request needs the TPM too, and then reports that fault.
//
// TestGetOpensTheDeviceStateWhileTheKeyIsStillRead and
// TestReadDeviceStaticPrivateKeyNextToTheStateStore run the read next to the
// store, for the plaintext file. No test here runs two unseals on a TPM.
//
// A key the read returns goes to the SDK, which wipes it when the request
// has been answered. A read that gives no key is reported to the SDK as
// errNoDeviceKey, with the one fixed word for the reason.
func (opts *globalOpts) deviceKeyOnDemand(ctx context.Context) qurl.DeviceKeySource {
	return func(context.Context) ([]byte, error) {
		key, why := opts.readDeviceKey(ctx)
		if why != "" {
			return nil, errNoDeviceKey{why: why}
		}
		return key, nil
	}
}

// whyNoDeviceKey returns the one fixed word for why a request as this device
// got no device key. err is an error for which consume.DeviceKeyNotGiven is
// true.
func whyNoDeviceKey(err error) connectorstate.NoDeviceKey {
	var none errNoDeviceKey
	if errors.As(err, &none) {
		return none.why
	}
	// The read gave bytes, and the SDK will not use them as a key.
	return connectorstate.NoDeviceKeyInvalid
}

// readDeviceStaticPrivateKey is the production opts.readDeviceKey: the key
// in the device state of this command's state directory, read without
// changing anything there.
func (opts *globalOpts) readDeviceStaticPrivateKey(ctx context.Context) ([]byte, connectorstate.NoDeviceKey) {
	stateDir, err := opts.resolveShareStateDir("")
	switch {
	case errors.Is(err, connectorstate.ErrNoDefaultStateDir):
		return nil, connectorstate.NoDeviceKeyNoState
	case err != nil:
		return nil, connectorstate.NoDeviceKeyUnreadable
	}
	return connectorstate.ReadDeviceStaticPrivateKey(ctx, stateDir, opts.resolvedSupervision)
}

// linkWithNoIdentity is linkForGet for a machine with no identity: it holds
// no device state and has no account key. A machine that has only an account
// key does not come here. It is a device with an identity, and linkForGet
// sends it to linkShareFirst or to linkRequestFirst.
//
// Where the link request is offered, its answer is final, a refusal
// included, and so is a fault in the settings. In both cases this function
// returns before shareLinkForGet, which is the only way from here to
// newClient, where an identity is created and registered.
//
// Where the request is not offered, nothing was sent and get does what it did
// before that request existed: the share request, which enrolls this machine
// first.
func (opts *globalOpts) linkWithNoIdentity(ctx context.Context, assessment *cridux.Assessment, options qurlapi.ShareOptions) (*qurlapi.ShareLink, linkOrigin, error) {
	offered, err := opts.cridLinkOffered()
	switch {
	case err != nil:
		return nil, linkFromShare, err
	case !offered:
		link, err := opts.shareLinkForGet(ctx, assessment, options)
		return linkWithOrigin(link, linkFromShare, err)
	}
	link, err := opts.linkByCRIDAlone(ctx, assessment.Input, false)
	if errors.Is(err, errCRIDNotRequestable) {
		err = refusalForCRIDNotRequestable(err)
	}
	return linkWithOrigin(link, linkFromRequestOnly, err)
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
// link. When the user asked for a session duration and the link came from
// the link request, it says that the flag was not applied: that request
// cannot carry one, and a requested lifetime is never dropped silently. It
// says so once. A link refreshed in the middle of a download is the same
// case.
func sessionDurationNoteOnce(printer *output.Printer, requested time.Duration) func(byLinkRequest bool) {
	noted := false
	return func(byLinkRequest bool) {
		if !byLinkRequest || requested == 0 || noted {
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
// linkFromLinkRequest says what it returns for each answer.
func (opts *globalOpts) linkByCRIDAlone(ctx context.Context, resourceCRID string, device bool) (*qurlapi.ShareLink, error) {
	issued, err := opts.requestCRIDLink(ctx, resourceCRID)
	return opts.linkFromLinkRequest(resourceCRID, device, issued, err)
}

// linkAsDevice asks for a link as this device and returns it in the shape a
// share answer has. deviceKey reads the device's static private key. The SDK
// calls it only when it needs the key, and wipes the key it returned.
//
// When the SDK asked for the key and got none, the device has asked with the
// CRID alone: the first request is that request, and nothing was sent after
// it. Its answer is the result, as linkByCRIDAlone gives it for a device.
//
// It returns errDeviceKeyRefused when the SDK will not ask as a device with
// what it was given and sent nothing. linkFromLinkRequest says what it
// returns for every other answer.
//
// With --verbose, one line says what was done about the device key. There
// are three, and each is true on every machine:
//
//   - The SDK did not ask for the key, because the first request settled the
//     link request (msgCRIDLinkKeyNotNeeded). The line does not say "as this
//     device". Nothing was read, so it is not known whether this machine has
//     a device key. A machine with only an account key has none.
//   - The SDK asked for the key and got none (msgCRIDLinkDeviceKeyNotRead,
//     with the word for the reason).
//   - The SDK asked for the key, and the read did not say that there is none
//     (msgCRIDLinkAsDevice).
func (opts *globalOpts) linkAsDevice(ctx context.Context, deviceKey qurl.DeviceKeySource, resourceCRID string) (*qurlapi.ShareLink, error) {
	// keyAsked records that the SDK asked for the device key. The SDK asks on
	// a goroutine of its own, and it does not wait for the read when the
	// context ends. So the record is atomic.
	var keyAsked atomic.Bool
	source := deviceKey
	if deviceKey != nil {
		// A missing function stays missing, so the SDK still refuses it.
		source = func(readCtx context.Context) ([]byte, error) {
			keyAsked.Store(true)
			return deviceKey(readCtx)
		}
	}
	issued, err := opts.requestCRIDLinkAsDevice(ctx, source, resourceCRID)
	if consume.DeviceKeyNotGiven(err) {
		if logf := opts.verboseLogger(); logf != nil {
			logf(msgCRIDLinkDeviceKeyNotRead, string(whyNoDeviceKey(err)))
		}
		// No link came with this answer, whatever the SDK returned beside it.
		// An answer that is missing is refused there: no error and no link is
		// not an answer.
		return opts.linkFromLinkRequest(resourceCRID, true, nil, consume.AnswerWithoutDeviceKey(err))
	}
	switch {
	case consume.DeviceKeyRefused(err):
		return nil, errDeviceKeyRefused
	case !consume.CRIDNotRequestable(err):
		if logf := opts.verboseLogger(); logf != nil {
			if keyAsked.Load() {
				// The line does not say that a request was sent under the
				// device key. The time limit can run out while the key is
				// read.
				logf(msgCRIDLinkAsDevice)
			} else {
				logf(msgCRIDLinkKeyNotNeeded)
			}
		}
	}
	return opts.linkFromLinkRequest(resourceCRID, true, issued, err)
}

// linkFromLinkRequest turns the SDK's answer to a link request, in either of
// its two forms, into the link get acts on or the error the user is shown.
// device says whether this machine holds a device identity; it only selects
// the not-found hint.
//
// It returns errCRIDNotRequestable when the SDK will not ask for this CRID
// and sent nothing. Every other failure comes back as one of the CLI's fixed
// messages, and the SDK's own error text does not, with one exception: a
// settings file that cannot be read or parsed. Its message ends with the
// SDK's detail, which names the file the user pointed QURL_DEPLOYMENT at and
// says what is wrong with it. That file is the user's own, and the detail is
// what lets them fix it.
func (opts *globalOpts) linkFromLinkRequest(resourceCRID string, device bool, issued *qurl.CRIDLink, err error) (*qurlapi.ShareLink, error) {
	switch {
	case consume.CRIDNotRequestable(err):
		opts.noteCRIDNotRequestable(err)
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

// noteCRIDNotRequestable writes, as a --verbose diagnostic, why the SDK will
// not ask for a link for the CRID. linkFromLinkRequest keeps only two facts
// of that answer, "the version" or "anything else", and a device with an
// identity then shows the share request's answer, so without this line the
// cause is lost. A cause other than the version means that the SDK's check
// of the CRID and the CLI's own check disagree, and this line is the only
// evidence of it.
//
// The line is fixed text and one word for the class of the cause. It never
// carries the SDK's own error text, which can quote what the user typed.
func (opts *globalOpts) noteCRIDNotRequestable(err error) {
	if logf := opts.verboseLogger(); logf != nil {
		logf(msgCRIDLinkNotSent, consume.CRIDNotRequestableClass(err))
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
