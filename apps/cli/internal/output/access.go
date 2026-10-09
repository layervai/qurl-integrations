package output

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// This file renders access requests: what a publisher sends to people, the
// pending requests, an approval, a denial, and the people who were approved.
//
// Two rules hold in every rendering here.
//
//   - A requester's name is text that person typed. It proves nothing about
//     who they are. It never reaches a terminal raw: it goes through the same
//     quoting and escaping as a publisher name, and JSON carries it with
//     name_verified false beside it.
//   - A code ties an approval to the screen that asked, and to whoever passed
//     the code on. It does not say who that is: the address of a private
//     resource is safe to send to anyone, so anyone who has it can ask. No
//     listing shows a code, in any output mode: a listing with the codes
//     would let a publisher, or the agent that runs these commands for them,
//     approve from the list, which is approval by name with one more step.
//     Every listing says instead how a person is let in: ask them for the
//     code on their screen, and approve a code only when the person the
//     publisher means to let in gave it to them themselves.

// Fixed customer-facing strings for access requests, registered in
// CustomerMessages.
const (
	// The guidance printed when access requests are turned on, at publish and
	// by `qurl requests <CRID> --on`. It has two openings: the address on the
	// link site when this install knows that site, and the CRID when it does
	// not. Nothing here names a site the install was not told about.
	//
	// msgRequestsApproveRule follows the approve command, the same with both
	// openings. This guidance is the first thing a new owner reads, and an
	// owner can go from it to `qurl approve` without reading a listing or a
	// help text, so it says here from whom to take a code.
	msgRequestsSendAddress    = "People can ask you for access to this resource. Send them this address:"
	msgRequestsSendCRID       = "People can ask you for access to this resource. This install does not know the web address where a CRID is opened for its deployment, so send them the CRID itself:"
	msgRequestsNextStep       = "They ask for access there and get a six-digit code to give you. Approve a code with:"
	msgRequestsNextStepNoSite = "Where they open it, they ask for access and get a six-digit code to give you. Approve a code with:"
	msgRequestsApproveCommand = "qurl approve %s <code>"
	msgRequestsApproveRule    = "Approve a code only when the person you mean to let in gave it to you themselves, in a way you know it is them."
	msgRequestsSafeToSend     = "The address and the CRID are safe to send to anyone: a private resource opens only for you and the people you allow."
	msgRequestsSafeToSendCRID = "The CRID is safe to send to anyone: a private resource opens only for you and the people you allow."
	msgRequestsOn             = "Access requests are on for %s."
	msgRequestsOff            = "Access requests are off for %s. Nobody new can ask for access."
	msgRequestsOffOnePerson   = "1 approved person still has access. See them, or take access away, with `qurl grants %s`."
	msgRequestsOffPeople      = "%d approved people still have access. See them, or take access away, with `qurl grants %s`."
	// msgRequestsOffPeopleUnknown is for an answer that does not say who is
	// approved. It gives no count, and it does not read as "nobody": turning
	// requests off never takes access away from anyone.
	msgRequestsOffPeopleUnknown = "Anyone you approved earlier still has access. See them, or take access away, with `qurl grants %s`."
	msgNoPendingRequests        = "No pending access requests."
	msgNoPendingRequestsForOne  = "No pending access requests for this resource."

	// msgApproveOnlyGivenCodes ends every text listing of requests, and is
	// the approval_rule member of every JSON listing: the reader of JSON is
	// most often an agent, which decides what to approve from that document.
	// A listing shows no code, and this is the sentence that says where the
	// code is and from whom to take one. It says both halves of the rule: a
	// name can be typed by anyone, and a code shows only which screen asked,
	// not who is at it. %s is the CRID of the resource in the listing of one
	// resource, where the command knows it, and the placeholder in the
	// listing of all resources, where each row has its own.
	msgApproveOnlyGivenCodes = "To let one of these people in, ask them for the six-digit code on their screen and run `qurl approve %s <code>`; a name can be typed by anyone, and a code shows only that it came from the screen that asked, so approve a code only when the person you mean to let in gave it to you themselves, in a way you know it is them (in person, on a call, or in a conversation you already have with them)."

	// msgRequestsMayBeMore follows a listing of all resources that the
	// service said may be incomplete. That listing is bounded; the listing
	// of one resource is how to see what that resource has.
	// msgRequestsMayBeMoreForOne is for the listing of one resource, where
	// that advice would point at the command that was just run.
	msgRequestsMayBeMore       = "There may be more requests than are shown here. To see all the requests for one resource, run `qurl requests <CRID>`."
	msgRequestsMayBeMoreForOne = "There may be more requests for this resource than are shown here. A request leaves the list when it is approved, denied or expired; run this command again to see the rest."

	// msgRequesterNoName stands in for a name the requester left empty, and
	// msgRequesterNameUnchecked qualifies a name wherever it stands alone.
	// msgRequesterNameNote says the same in the JSON document of an
	// approval, where no sentence stands beside the name.
	msgRequesterNoName        = "no name given"
	msgRequesterNameUnchecked = "typed by them, not checked"
	msgRequesterNameNote      = "The name was typed by the person who asked. Nobody checked it."

	// The approval document and the denial line.
	msgApproved         = "Approved"
	msgApprovedCanOpen  = "This person can now open %s. To take the access away, run:"
	msgRemoveCommand    = "qurl grants %s --remove %s"
	msgDenied           = "Denied the request with the code %s for %s. No access was given."
	msgDeniedDevice     = "Denied the request from the device id %s for %s. No access was given."
	labelName           = "Name:"
	labelDeviceID       = "Device ID:"
	labelApproved       = "Approved:"
	labelAccessRequests = "Access requests:"
	labelApprovedPeople = "Approved people:"
	msgNoApprovedPeople = "none"
	msgStateOn          = "on"
	msgStateOff         = "off"
	msgRemovePersonHint = "Take one person's access away with `qurl grants %s --remove <device id>`."
	// msgApprovedPeopleNotSaid stands where the count of approved people
	// would be, for a resource whose answer has no list of them. It is not
	// "none": the service did not say who was approved.
	msgApprovedPeopleNotSaid = "not said"
	// msgRemovalKeysNotChanged follows a removal of approved people that
	// stopped, when the same command also named public keys. It is true
	// whether the command stopped at a removal or after the last one.
	msgRemovalKeysNotChanged = "No public key was added or removed: that change comes after the removals, and the command stopped before it."
	// msgRemovalFinishKeys follows a removal of every person after which
	// the change to public keys failed or was not reached. %s is the
	// command that makes that change alone. It can be run whether or not a
	// failed change was made: a key that is already on the list, or already
	// off it, is left as it is.
	msgRemovalFinishKeys = "To make the change to the public keys, run: %s"
)

// requesterName is the only form in which a requester's name sits beside
// other text: quoted and escaped like a publisher name, or the fixed stand-in
// for a name that was left empty.
func (p *Printer) requesterName(name string) string {
	if name == "" {
		return msgRequesterNoName
	}
	return p.publisherName(name)
}

// saidPrivate reports whether the service said that a resource is private. A
// value it did not send is not that.
func saidPrivate(private *bool) bool {
	return private != nil && *private
}

// requestGuidance writes what a publisher sends to people and what happens
// next, for a resource whose access requests are on. address is the
// resource's address on the link site, empty when this install does not know
// that site.
//
// After the approve command it says from whom to take a code, in the same
// sentence with or without an address. It is text only: the JSON documents of
// the commands that print this guidance have no member for it.
//
// It says that the address and the CRID are safe to send to anyone, because
// a private resource opens only for the people the publisher allows. Every
// caller therefore checks saidPrivate first.
func (p *Printer) requestGuidance(ew *errWriter, resourceCRID, address string) {
	opening, sent, next, safe := msgRequestsSendAddress, address, msgRequestsNextStep, msgRequestsSafeToSend
	if address == "" {
		opening, sent, next, safe = msgRequestsSendCRID, resourceCRID, msgRequestsNextStepNoSite, msgRequestsSafeToSendCRID
	}
	ew.printf("%s\n\n  %s\n\n", opening, sent)
	ew.printf("%s\n\n  %s\n\n", next, fmt.Sprintf(msgRequestsApproveCommand, resourceCRID))
	ew.printf("%s\n\n", msgRequestsApproveRule)
	ew.printf("%s\n", safe)
}

// accessRequestJSON is one row of a listing. It has no member for the code
// of a request, and the type it is built from has no code to put in one.
type accessRequestJSON struct {
	// Name is omitted when the requester left it empty. NameVerified is
	// always present and always false: nobody checks a requester's name.
	Name         escapedJSONString `json:"name,omitempty"`
	NameVerified bool              `json:"name_verified"`
	DeviceID     string            `json:"device_id"`
	RequestedAt  *time.Time        `json:"requested_at,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	CRID         string            `json:"crid"`
}

// accessRequestsJSON is the document of both listings. ApprovalRule is always
// present, with an empty listing too, and always the sentence that ends the
// text listing: how a person is let in does not depend on the output mode,
// and the reader of this one is most often an agent. HasMore is always
// present, and true when the service said the listing may be incomplete.
type accessRequestsJSON struct {
	Requests     []accessRequestJSON `json:"requests"`
	ApprovalRule string              `json:"approval_rule"`
	HasMore      bool                `json:"has_more"`
}

// AccessRequests renders pending access requests. resourceCRID is the
// resource the listing is for. It is empty for the listing that covers every
// resource of the owner, where each row names its resource.
//
// No mode shows the code of a request. Text is a table of who asked, from
// which device, when, and until when the request stands, and it ends with
// the line on how a person is let in. An empty listing writes nothing to
// stdout and says so on stderr. --quiet prints what `qurl deny` takes, one
// request per line: the device id, after the CRID when the listing covers
// every resource.
//
// A listing the service said may be incomplete says so after its rows, in
// text, and on stderr with --quiet, whose stdout is values only.
func (p *Printer) AccessRequests(list *qurlapi.AccessRequestList, resourceCRID string) error {
	if list == nil {
		return errors.New("qURL access-request listing is incomplete")
	}
	all := resourceCRID == ""
	requests := list.Requests
	switch {
	case p.format == FormatJSON:
		out := accessRequestsJSON{Requests: make([]accessRequestJSON, 0, len(requests)), ApprovalRule: approvalRule(resourceCRID), HasMore: list.HasMore}
		for index := range requests {
			request := &requests[index]
			out.Requests = append(out.Requests, accessRequestJSON{
				Name: escapedJSONString(request.Name), DeviceID: request.DeviceID,
				RequestedAt: request.RequestedAt, ExpiresAt: request.ExpiresAt, CRID: request.CRID,
			})
		}
		return p.writeJSON(out)
	case p.quiet:
		ew := &errWriter{w: p.out}
		for index := range requests {
			if all {
				ew.printf("%s %s\n", requests[index].CRID, requests[index].DeviceID)
				continue
			}
			ew.printf("%s\n", requests[index].DeviceID)
		}
		if list.HasMore {
			p.Notef("%s", mayBeMore(all))
		}
		return ew.flush(nil)
	}
	if len(requests) == 0 {
		switch {
		case list.HasMore:
			// An empty page of a listing that goes on is not "none".
			p.Notef("%s", mayBeMore(all))
		case all:
			p.Notef("%s", msgNoPendingRequests)
		default:
			p.Notef("%s", msgNoPendingRequestsForOne)
		}
		return nil
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	// Headers stay uncolored: tabwriter counts ANSI escape bytes as cell
	// width, so styled headers would skew every column under them.
	header := "NAME\tDEVICE ID\tREQUESTED\tEXPIRES"
	if all {
		header += "\tCRID"
	}
	ew.printf("%s\n", header)
	for index := range requests {
		request := &requests[index]
		requested, expires := "-", "-"
		if request.RequestedAt != nil {
			requested = p.relativeTime(*request.RequestedAt)
		}
		if request.ExpiresAt != nil {
			expires = p.expiresIn(*request.ExpiresAt)
		}
		ew.printf("%s\t%s\t%s\t%s", p.requesterName(request.Name), request.DeviceID, requested, expires)
		if all {
			ew.printf("\t%s", request.CRID)
		}
		ew.printf("\n")
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	plain := &errWriter{w: p.out}
	if list.HasMore {
		plain.printf("\n%s\n", mayBeMore(all))
	}
	plain.printf("\n%s\n", approvalRule(resourceCRID))
	return plain.flush(nil)
}

// approvalRule is the sentence on how a person is let in, with the command
// that does it. The listing of one resource knows the CRID that command
// takes and writes it. The listing of all resources has one in each row, so
// its sentence has the placeholder.
func approvalRule(resourceCRID string) string {
	if resourceCRID == "" {
		resourceCRID = placeholderCRID
	}
	return fmt.Sprintf(msgApproveOnlyGivenCodes, resourceCRID)
}

// mayBeMore is the line for a listing the service said may be incomplete.
func mayBeMore(all bool) string {
	if all {
		return msgRequestsMayBeMore
	}
	return msgRequestsMayBeMoreForOne
}

// expiresIn writes when a request stops standing, for a table cell: how long
// from now, or that it has expired.
func (p *Printer) expiresIn(t time.Time) string {
	remaining := t.Sub(p.now())
	if remaining <= 0 {
		return expiredLabel
	}
	return "in " + formatDuration(remaining)
}

type accessRequestsSettingJSON struct {
	CRID           string `json:"crid"`
	AccessRequests bool   `json:"access_requests"`
	// ResourceURL is the resource's address on the link site. It is present
	// only when access requests are on and this install knows that site.
	ResourceURL string `json:"resource_url,omitempty"`
}

// AccessRequestsSetting renders the outcome of turning access requests on or
// off. On, it prints what the publisher sends to people; address is the
// resource's address on the link site, empty when this install does not know
// that site. Off, it says how many approved people still have access, read
// from the service's answer. When that answer does not say who is approved,
// no count is printed and nothing reads as "nobody": the line says that
// anyone approved earlier still has access, and where to look.
//
// TODO(upstream-contract): the service sends allowed_passkeys on every
// resource row, as an empty array when nobody is approved, on the answer to
// a change as on a read. The count is read from that member. A missing
// member is read as "the service did not say", which is the line with no
// count.
//
// On, the resource must be one the service said is private: the guidance
// says that its address is safe to send to anyone, which is true of a
// private resource only. The API client fails such an answer before it gets
// here; this is the same rule at the place that prints the sentence.
func (p *Printer) AccessRequestsSetting(resource *qurlapi.ResourceSummary, address string) error {
	if resource == nil || resource.AccessRequests == nil {
		return errors.New("qURL access-request setting is incomplete")
	}
	on := *resource.AccessRequests
	if on && !saidPrivate(resource.Private) {
		return errors.New("qURL access requests are on for a resource that is not known to be private")
	}
	if !on {
		address = ""
	}
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(accessRequestsSettingJSON{CRID: resource.CRID, AccessRequests: on, ResourceURL: address})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, resource.CRID)
		return err
	}
	ew := &errWriter{w: p.out}
	if on {
		ew.printf(msgRequestsOn+"\n\n", resource.CRID)
		p.requestGuidance(ew, resource.CRID, address)
		return ew.flush(nil)
	}
	ew.printf(msgRequestsOff+"\n", resource.CRID)
	switch people := len(resource.AllowedPasskeys); {
	case resource.AllowedPasskeys == nil:
		ew.printf(msgRequestsOffPeopleUnknown+"\n", resource.CRID)
	case people == 1:
		ew.printf(msgRequestsOffOnePerson+"\n", resource.CRID)
	case people > 1:
		ew.printf(msgRequestsOffPeople+"\n", people, resource.CRID)
	}
	return ew.flush(nil)
}

type approvedPersonJSON struct {
	Name         escapedJSONString `json:"name,omitempty"`
	NameVerified bool              `json:"name_verified"`
	DeviceID     string            `json:"device_id"`
	ApprovedAt   *time.Time        `json:"approved_at,omitempty"`
}

func approvedPersonDocument(person *qurlapi.AllowedPasskey) approvedPersonJSON {
	return approvedPersonJSON{Name: escapedJSONString(person.Name), DeviceID: person.DeviceID, ApprovedAt: person.ApprovedAt}
}

// approvedJSON is the document of an approval. NameNote is always present
// and says in words what name_verified: false says as a value: the text
// document has that sentence beside the name, and this one is read by an
// agent that just gave a person access.
type approvedJSON struct {
	CRID     string `json:"crid"`
	Approved bool   `json:"approved"`
	approvedPersonJSON
	NameNote string `json:"name_note"`
}

// Approved renders a completed approval: who now has access, and the exact
// command that takes it away. --quiet prints the device id, which is what
// that command needs.
func (p *Printer) Approved(resourceCRID string, person *qurlapi.AllowedPasskey) error {
	if person == nil {
		return errors.New("qURL approval is incomplete")
	}
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(approvedJSON{CRID: resourceCRID, Approved: true, approvedPersonJSON: approvedPersonDocument(person), NameNote: msgRequesterNameNote})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, person.DeviceID)
		return err
	}
	ew := &errWriter{w: p.out}
	ew.printf("%s\n\n", p.green(msgApproved))
	if ew.err != nil {
		return ew.err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	twe := &errWriter{w: tw}
	name := p.requesterName(person.Name)
	if person.Name != "" {
		name += " (" + msgRequesterNameUnchecked + ")"
	}
	twe.printf("  %s\t%s\n", p.bold(labelName), name)
	twe.printf("  %s\t%s\n", p.bold(labelDeviceID), person.DeviceID)
	if approved := p.createdText(person.ApprovedAt); approved != "" {
		twe.printf("  %s\t%s\n", p.bold(labelApproved), approved)
	}
	if err := twe.flush(tw); err != nil {
		return err
	}
	ew.printf("\n"+msgApprovedCanOpen+"\n\n  %s\n", resourceCRID, fmt.Sprintf(msgRemoveCommand, resourceCRID, person.DeviceID))
	return ew.flush(nil)
}

// deniedJSON is the document of a denial. It names the request the way the
// command named it: by device_id, or by code for a publisher who was given a
// code. Exactly one of the two is present.
type deniedJSON struct {
	CRID     string `json:"crid"`
	DeviceID string `json:"device_id,omitempty"`
	Code     string `json:"code,omitempty"`
	Denied   bool   `json:"denied"`
}

// Denied renders a completed denial. request is what the command named the
// request by: a device id, or a six-digit code. Like a deletion, the text
// confirmation is a status line on stderr; --quiet echoes what was given and
// JSON emits the outcome document. A code is shown only when the publisher
// typed it: a denial by device id never learns one.
func (p *Printer) Denied(resourceCRID, request string) error {
	byDevice := qurlapi.ValidDeviceID(request)
	switch {
	case p.format == FormatJSON && byDevice:
		return p.writeJSON(deniedJSON{CRID: resourceCRID, DeviceID: request, Denied: true})
	case p.format == FormatJSON:
		return p.writeJSON(deniedJSON{CRID: resourceCRID, Code: request, Denied: true})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, request)
		return err
	case byDevice:
		_, err := fmt.Fprintf(p.err, msgDeniedDevice+"\n", request, resourceCRID)
		return err
	}
	_, err := fmt.Fprintf(p.err, msgDenied+"\n", qurlapi.SpacedRequestCode(request), resourceCRID)
	return err
}

// removalOutcomeJSON is what `qurl grants --remove -o json` writes when the
// removal of approved people did not take every person it named off the
// list. Every device id the command named is in exactly one of the three
// arrays, each of which is always present.
type removalOutcomeJSON struct {
	CRID       string   `json:"crid"`
	Removed    []string `json:"removed"`
	NotFound   []string `json:"not_found"`
	NotRemoved []string `json:"not_removed"`
	// PublicKeysChanged is present, and false, only when the command also
	// named public keys and stopped before it reached that change, because
	// a removal failed or the removals were not confirmed: the change comes
	// after the removals, so it was not made. It is not there when the
	// change to public keys was reached and failed, which may have changed
	// a key: PublicKeysCommand without this member is that case.
	PublicKeysChanged *bool `json:"public_keys_changed,omitempty"`
	// PublicKeysCommand is present only when every person was removed and
	// the change to public keys failed or was not reached: the command that
	// makes that change alone, which is what finishes the job.
	PublicKeysCommand string `json:"public_keys_command,omitempty"`
}

// RemovalOutcome writes, in JSON mode only, what happened to each device id
// of a removal that failed. The failure itself is the command's error, with
// its message on stderr and its exit code; this document is what a script or
// an agent reads to learn which people lost access all the same. Text and
// --quiet write nothing here: the message says it.
func (p *Printer) RemovalOutcome(outcome *qurlapi.PasskeyRemovalError) error {
	if outcome == nil || p.format != FormatJSON {
		return nil
	}
	document := removalOutcomeJSON{
		CRID:       outcome.ID,
		Removed:    append([]string{}, outcome.Removed...),
		NotFound:   append([]string{}, outcome.NotFound...),
		NotRemoved: append([]string{}, outcome.NotRemoved...),
	}
	if outcome.KeysNotChanged {
		changed := false
		document.PublicKeysChanged = &changed
	}
	document.PublicKeysCommand = outcome.KeyChangeCommand
	return p.writeJSON(document)
}

// grantsJSON is the `qurl grants` document: the resource status document with
// the two things that decide who else can open a private resource beside its
// device keys. Each is omitted when the service did not say it, like every
// member the service may leave out. approved_people is an array when the
// service sent the list, empty when it said that nobody was approved, and
// the member is not there when the service sent no list: a reader must not
// take "not said" for "nobody". access_requests is there when the service
// said whether people can ask.
type grantsJSON struct {
	AllowedDeviceKeys []string              `json:"allowed_device_keys"`
	ApprovedPeople    *[]approvedPersonJSON `json:"approved_people,omitempty"`
	AccessRequests    *bool                 `json:"access_requests,omitempty"`
	Private           *bool                 `json:"private,omitempty"`
	CRID              string                `json:"crid,omitempty"`
	ResourceID        string                `json:"resource_id"`
	TargetURL         string                `json:"target_url,omitempty"`
	Type              string                `json:"type"`
	Status            string                `json:"status"`
	CreatedAt         *time.Time            `json:"created_at,omitempty"`
	ExpiresAt         *time.Time            `json:"expires_at,omitempty"`
	Publisher         publisherJSON         `json:"publisher"`
}

// Grants renders who can open a resource besides its owner: the allowed
// device keys and the approved people, with whether new people can ask. It is
// the resource status rendering with those rows added, so `qurl grants`
// stays the one place a publisher reads the whole answer.
func (p *Printer) Grants(resource *qurlapi.ResourceSummary) error {
	switch {
	case p.format == FormatJSON:
		// A list the service did not send stays out of the document. A
		// list it sent is an array, empty when nobody was approved.
		var approved *[]approvedPersonJSON
		if resource.AllowedPasskeys != nil {
			people := make([]approvedPersonJSON, 0, len(resource.AllowedPasskeys))
			for index := range resource.AllowedPasskeys {
				people = append(people, approvedPersonDocument(&resource.AllowedPasskeys[index]))
			}
			approved = &people
		}
		return p.writeJSON(grantsJSON{
			AllowedDeviceKeys: append([]string{}, resource.AllowedDeviceKeys...),
			ApprovedPeople:    approved,
			AccessRequests:    resource.AccessRequests,
			Private:           resource.Private,
			CRID:              resource.CRID, ResourceID: resource.ResourceID,
			TargetURL: resource.TargetURL, Type: resource.Type, Status: resource.Status,
			CreatedAt: resource.CreatedAt, ExpiresAt: resource.ExpiresAt,
			Publisher: publisherDocument(resource.Publisher),
		})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, resource.CRID)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	p.resourceStatusRows(ew, resource, true)
	if err := ew.flush(tw); err != nil {
		return err
	}
	if len(resource.AllowedPasskeys) == 0 {
		return nil
	}
	plain := &errWriter{w: p.out}
	plain.printf("\n%s\n", p.bold(labelApprovedPeople))
	if plain.err != nil {
		return plain.err
	}
	people := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	pw := &errWriter{w: people}
	pw.printf("  NAME\tDEVICE ID\tAPPROVED\n")
	for index := range resource.AllowedPasskeys {
		person := &resource.AllowedPasskeys[index]
		approved := p.createdText(person.ApprovedAt)
		if approved == "" {
			approved = "-"
		}
		pw.printf("  %s\t%s\t%s\n", p.requesterName(person.Name), person.DeviceID, approved)
	}
	if err := pw.flush(people); err != nil {
		return err
	}
	plain.printf("\n"+msgRemovePersonHint+"\n", resource.CRID)
	return plain.flush(nil)
}
