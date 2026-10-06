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
//   - What ties an approval to a person is the code, which only the person
//     who asked was shown. Every text listing of requests ends with the line
//     that says so, because the publisher, or the agent that runs these
//     commands for them, decides what to approve from this output.

// Fixed customer-facing strings for access requests, registered in
// CustomerMessages.
const (
	// The guidance printed when access requests are turned on, at publish and
	// by `qurl requests <CRID> --on`. It has two openings: the address on the
	// link site when this install knows that site, and the CRID when it does
	// not. Nothing here names a site the install was not told about.
	msgRequestsSendAddress     = "People can ask you for access to this resource. Send them this address:"
	msgRequestsSendCRID        = "People can ask you for access to this resource. This install does not know the web address where a CRID is opened for its deployment, so send them the CRID itself:"
	msgRequestsNextStep        = "They ask for access there and get a six-digit code to give you. Approve a code with:"
	msgRequestsNextStepNoSite  = "Where they open it, they ask for access and get a six-digit code to give you. Approve a code with:"
	msgRequestsApproveCommand  = "qurl approve %s <code>"
	msgRequestsSafeToSend      = "The address and the CRID are safe to send to anyone: a private resource opens only for you and the people you allow."
	msgRequestsSafeToSendCRID  = "The CRID is safe to send to anyone: a private resource opens only for you and the people you allow."
	msgRequestsOn              = "Access requests are on for %s."
	msgRequestsOff             = "Access requests are off for %s. Nobody new can ask for access."
	msgRequestsOffOnePerson    = "1 approved person still has access. See them, or take the access away, with `qurl grants %s`."
	msgRequestsOffPeople       = "%d approved people still have access. See them, or take access away, with `qurl grants %s`."
	msgNoPendingRequests       = "No pending access requests."
	msgNoPendingRequestsForOne = "No pending access requests for this resource."

	// msgApproveOnlyGivenCodes ends every text listing of requests.
	msgApproveOnlyGivenCodes = "Approve a code only when the person gave it to you themselves; a name can be typed by anyone."

	// msgRequesterNoName stands in for a name the requester left empty, and
	// msgRequesterNameUnchecked qualifies a name wherever it stands alone.
	msgRequesterNoName        = "no name given"
	msgRequesterNameUnchecked = "typed by them, not checked"

	// The approval document and the denial line.
	msgApproved         = "Approved"
	msgApprovedCanOpen  = "This person can now open %s. To take the access away, run:"
	msgRemoveCommand    = "qurl grants %s --remove %s"
	msgDenied           = "Denied the request with the code %s for %s. No access was given."
	labelName           = "Name:"
	labelDeviceID       = "Device ID:"
	labelApproved       = "Approved:"
	labelAccessRequests = "Access requests:"
	labelApprovedPeople = "Approved people:"
	msgNoApprovedPeople = "none"
	msgStateOn          = "on"
	msgStateOff         = "off"
	msgRemovePersonHint = "Take one person's access away with `qurl grants %s --remove <device id>`."
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

// spacedCode writes a six-digit request code as two groups of three, the
// form it is read aloud in. Anything else is returned as it is.
func spacedCode(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[:3] + " " + code[3:]
}

// requestGuidance writes what a publisher sends to people and what happens
// next, for a resource whose access requests are on. address is the
// resource's address on the link site, empty when this install does not know
// that site.
func (p *Printer) requestGuidance(ew *errWriter, resourceCRID, address string) {
	opening, sent, next, safe := msgRequestsSendAddress, address, msgRequestsNextStep, msgRequestsSafeToSend
	if address == "" {
		opening, sent, next, safe = msgRequestsSendCRID, resourceCRID, msgRequestsNextStepNoSite, msgRequestsSafeToSendCRID
	}
	ew.printf("%s\n\n  %s\n\n", opening, sent)
	ew.printf("%s\n\n  %s\n\n", next, fmt.Sprintf(msgRequestsApproveCommand, resourceCRID))
	ew.printf("%s\n", safe)
}

type accessRequestJSON struct {
	Code string `json:"code"`
	// Name is omitted when the requester left it empty. NameVerified is
	// always present and always false: nobody checks a requester's name.
	Name         escapedJSONString `json:"name,omitempty"`
	NameVerified bool              `json:"name_verified"`
	DeviceID     string            `json:"device_id"`
	RequestedAt  *time.Time        `json:"requested_at,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	CRID         string            `json:"crid"`
}

type accessRequestsJSON struct {
	Requests []accessRequestJSON `json:"requests"`
}

// AccessRequests renders pending access requests. all says that the listing
// covers every resource of the owner, so each row names its resource.
//
// Text is a table that ends with the line on what a code and a name are
// worth. An empty listing writes nothing to stdout and says so on stderr.
// --quiet prints what `qurl approve` takes, one request per line: the code,
// after the CRID when the listing covers every resource.
func (p *Printer) AccessRequests(requests []qurlapi.AccessRequest, all bool) error {
	switch {
	case p.format == FormatJSON:
		out := accessRequestsJSON{Requests: make([]accessRequestJSON, 0, len(requests))}
		for index := range requests {
			request := &requests[index]
			out.Requests = append(out.Requests, accessRequestJSON{
				Code: request.Code, Name: escapedJSONString(request.Name), DeviceID: request.DeviceID,
				RequestedAt: request.RequestedAt, ExpiresAt: request.ExpiresAt, CRID: request.CRID,
			})
		}
		return p.writeJSON(out)
	case p.quiet:
		ew := &errWriter{w: p.out}
		for index := range requests {
			if all {
				ew.printf("%s %s\n", requests[index].CRID, requests[index].Code)
				continue
			}
			ew.printf("%s\n", requests[index].Code)
		}
		return ew.flush(nil)
	}
	if len(requests) == 0 {
		if all {
			p.Notef("%s", msgNoPendingRequests)
		} else {
			p.Notef("%s", msgNoPendingRequestsForOne)
		}
		return nil
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	// Headers stay uncolored: tabwriter counts ANSI escape bytes as cell
	// width, so styled headers would skew every column under them.
	header := "CODE\tNAME\tDEVICE ID\tREQUESTED"
	if all {
		header += "\tCRID"
	}
	ew.printf("%s\n", header)
	for index := range requests {
		request := &requests[index]
		requested := "-"
		if request.RequestedAt != nil {
			requested = p.relativeTime(*request.RequestedAt)
		}
		ew.printf("%s\t%s\t%s\t%s", spacedCode(request.Code), p.requesterName(request.Name), request.DeviceID, requested)
		if all {
			ew.printf("\t%s", request.CRID)
		}
		ew.printf("\n")
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	_, err := fmt.Fprintf(p.out, "\n%s\n", msgApproveOnlyGivenCodes)
	return err
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
// from the service's answer.
func (p *Printer) AccessRequestsSetting(resource *qurlapi.ResourceSummary, address string) error {
	if resource == nil || resource.AccessRequests == nil {
		return errors.New("qURL access-request setting is incomplete")
	}
	on := *resource.AccessRequests
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

type approvedJSON struct {
	CRID     string `json:"crid"`
	Approved bool   `json:"approved"`
	approvedPersonJSON
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
		return p.writeJSON(approvedJSON{CRID: resourceCRID, Approved: true, approvedPersonJSON: approvedPersonDocument(person)})
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

type deniedJSON struct {
	CRID   string `json:"crid"`
	Code   string `json:"code"`
	Denied bool   `json:"denied"`
}

// Denied renders a completed denial. Like a deletion, the text confirmation
// is a status line on stderr; --quiet echoes the code and JSON emits the
// outcome document.
func (p *Printer) Denied(resourceCRID, code string) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(deniedJSON{CRID: resourceCRID, Code: code, Denied: true})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, code)
		return err
	}
	_, err := fmt.Fprintf(p.err, msgDenied+"\n", spacedCode(code), resourceCRID)
	return err
}

// grantsJSON is the `qurl grants` document: the resource status document with
// the two things that decide who else can open a private resource beside its
// device keys. approved_people is always an array, empty when nobody was
// approved; access_requests is omitted when the service did not say.
type grantsJSON struct {
	AllowedDeviceKeys []string             `json:"allowed_device_keys"`
	ApprovedPeople    []approvedPersonJSON `json:"approved_people"`
	AccessRequests    *bool                `json:"access_requests,omitempty"`
	Private           *bool                `json:"private,omitempty"`
	CRID              string               `json:"crid,omitempty"`
	ResourceID        string               `json:"resource_id"`
	TargetURL         string               `json:"target_url,omitempty"`
	Type              string               `json:"type"`
	Status            string               `json:"status"`
	CreatedAt         *time.Time           `json:"created_at,omitempty"`
	ExpiresAt         *time.Time           `json:"expires_at,omitempty"`
	Publisher         publisherJSON        `json:"publisher"`
}

// Grants renders who can open a resource besides its owner: the allowed
// device keys and the approved people, with whether new people can ask. It is
// the resource status rendering with those rows added, so `qurl grants`
// stays the one place a publisher reads the whole answer.
func (p *Printer) Grants(resource *qurlapi.ResourceSummary) error {
	switch {
	case p.format == FormatJSON:
		people := make([]approvedPersonJSON, 0, len(resource.AllowedPasskeys))
		for index := range resource.AllowedPasskeys {
			people = append(people, approvedPersonDocument(&resource.AllowedPasskeys[index]))
		}
		return p.writeJSON(grantsJSON{
			AllowedDeviceKeys: append([]string{}, resource.AllowedDeviceKeys...),
			ApprovedPeople:    people,
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
