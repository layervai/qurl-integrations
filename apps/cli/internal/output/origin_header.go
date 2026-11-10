package output

import (
	"errors"
	"fmt"
	"text/tabwriter"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Fixed customer-facing origin header strings, registered in CustomerMessages.
const (
	labelOriginHeader = "Origin header:"
	// msgOriginHeaderNone is the row value after a removal.
	msgOriginHeaderNone = "none"
	// msgOriginHeaderSaved and msgOriginHeaderRemoved confirm a change on
	// stderr. Neither names the value: the service never returns it.
	msgOriginHeaderSaved   = "Origin header saved. qURL adds it to every request it forwards to the target."
	msgOriginHeaderRemoved = "Origin header removed. qURL no longer adds a header to requests it forwards to the target."
)

type originHeaderJSON struct {
	CRID       string `json:"crid,omitempty"`
	ResourceID string `json:"resource_id"`
	TargetURL  string `json:"target_url,omitempty"`
	// OriginHeaderName is empty, and so absent, after a removal.
	OriginHeaderName string `json:"origin_header_name,omitempty"`
}

// OriginHeader renders a resource after `qurl origin-header set` or `clear`:
// the header name, never its value. --quiet prints the bare name, or nothing
// when none is set.
func (p *Printer) OriginHeader(resource *qurlapi.ResourceSummary) error {
	if resource == nil {
		return errors.New("qURL resource answer is empty")
	}
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(originHeaderJSON{
			CRID: resource.CRID, ResourceID: resource.ResourceID, TargetURL: resource.TargetURL,
			OriginHeaderName: resource.OriginHeaderName,
		})
	case p.quiet:
		if resource.OriginHeaderName == "" {
			return nil
		}
		_, err := fmt.Fprintln(p.out, resource.OriginHeaderName)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("%s\t%s\n", p.bold(labelCRID), resource.CRID)
	if resource.TargetURL != "" {
		ew.printf("%s\t%s\n", p.bold("Target:"), resource.TargetURL)
	}
	name := resource.OriginHeaderName
	if name == "" {
		name = msgOriginHeaderNone
	}
	ew.printf("%s\t%s\n", p.bold(labelOriginHeader), name)
	if err := ew.flush(tw); err != nil {
		return err
	}
	if resource.OriginHeaderName == "" {
		p.Notef("%s", msgOriginHeaderRemoved)
	} else {
		p.Notef("%s", msgOriginHeaderSaved)
	}
	return nil
}
