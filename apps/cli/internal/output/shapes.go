package output

import (
	"fmt"
	"text/tabwriter"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Repo-owned JSON projections. These structs — not SDK types — are the
// `-o json` contract; each field is deliberately tagged so upstream renames
// cannot silently change the CLI's output.

type publishJSON struct {
	CRID       string     `json:"crid,omitempty"`
	ResourceID string     `json:"resource_id"`
	TargetURL  string     `json:"target_url"`
	Status     string     `json:"status,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// FoundExisting mirrors the text-mode already-published note for
	// scripts. Always emitted — explicit false on a fresh publish — so
	// scripts can rely on the key being present, including against
	// platform versions that do not send the flag yet.
	FoundExisting bool `json:"found_existing"`
}

type resolveJSON struct {
	QURL             string     `json:"qurl"`
	CRID             string     `json:"crid,omitempty"`
	Type             string     `json:"type,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	ExpiresInSeconds int        `json:"expires_in_seconds,omitempty"`
	SingleUse        bool       `json:"single_use,omitempty"`
}

type listItemJSON struct {
	CRID       string     `json:"crid,omitempty"`
	ResourceID string     `json:"resource_id"`
	TargetURL  string     `json:"target_url,omitempty"`
	Status     string     `json:"status,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type listJSON struct {
	Resources []listItemJSON `json:"resources"`
	// HasMore — not next_cursor presence — is the pagination terminator
	// (the platform legitimately serves short and even zero-item pages with
	// has_more=true). false is the terminal signal, so the field is always
	// emitted rather than omitempty'd away.
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type deleteJSON struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	// AlreadyGone is emitted only for the idempotent re-delete, so the
	// common case's document (and its golden) stays unchanged.
	AlreadyGone bool `json:"already_gone,omitempty"`
}

type downloadJSON struct {
	CRID string `json:"crid,omitempty"`
	File string `json:"file"`
	// Bytes is the payload size actually written.
	Bytes int64 `json:"bytes"`
}

// listCRIDWidth is the middle-ellipsis budget for the text CRID column; JSON
// and --quiet always carry the full value.
const (
	listCRIDWidth   = 24
	listTargetWidth = 40
)

// Publish renders a publish result. Text mode prints the CRID last, alone on
// its line, so it is the easiest thing to select and copy. Publishing an
// already-published URL returns the existing resource, and the rendering
// says so: the text document itself carries the story (headline plus note),
// while --quiet and JSON keep their stdout documents unchanged and note the
// replay on stderr.
func (p *Printer) Publish(res *qurlapi.Published) error {
	if res.FoundExisting && (p.format == FormatJSON || p.quiet) {
		p.Notef(msgAlreadyPublished)
	}
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(publishJSON{
			CRID:          res.CRID,
			ResourceID:    res.ResourceID,
			TargetURL:     res.TargetURL,
			Status:        res.Status,
			CreatedAt:     res.CreatedAt,
			ExpiresAt:     res.ExpiresAt,
			FoundExisting: res.FoundExisting,
		})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, primaryID(res.CRID, res.ResourceID))
		return err
	default:
		return p.publishText(res)
	}
}

// publishText renders the publish document. The raw platform resource id is
// deliberately absent: the CRID is the customer-facing identity, and nothing
// in the CLI accepts a resource id as input. JSON keeps the raw field, and
// --quiet falls back to it only when no CRID was minted (primaryID).
func (p *Printer) publishText(res *qurlapi.Published) error {
	headline := "Published"
	if res.FoundExisting {
		headline = "Already published"
	}
	ew := &errWriter{w: p.out}
	ew.printf("%s\n\n", p.green(headline))
	if ew.err != nil {
		return ew.err
	}

	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	twe := &errWriter{w: tw}
	twe.printf("  %s\t%s\n", p.bold("Target:"), res.TargetURL)
	if res.Status != "" {
		twe.printf("  %s\t%s\n", p.bold("Status:"), res.Status)
	}
	if res.CreatedAt != nil {
		twe.printf("  %s\t%s\n", p.bold("Created:"), p.relativeTime(*res.CreatedAt))
	}
	if res.ExpiresAt != nil {
		twe.printf("  %s\t%s\n", p.bold("Expires:"), p.formatExpiry(*res.ExpiresAt))
	}
	if err := twe.flush(tw); err != nil {
		return err
	}

	if res.FoundExisting {
		ew.printf("\n%s\n", p.dim(msgPublishFoundExisting))
	}
	if res.CRID != "" {
		ew.printf("\n%s %s\n", p.bold("CRID:"), res.CRID)
	}
	return ew.flush(nil)
}

// Resolve renders a minted temporary access link. Piped stdout gets the bare
// link and nothing else, so `curl "$(qurl resolve <CRID>)"` works; a TTY
// gets the link plus its expiry on stderr-free stdout decoration.
func (p *Printer) Resolve(res *qurlapi.Resolved) error {
	if p.format == FormatJSON {
		out := resolveJSON{
			QURL:             res.QURL,
			CRID:             res.CRID,
			Type:             res.Type,
			ExpiresInSeconds: res.ExpiresInSeconds,
			SingleUse:        res.SingleUse,
		}
		if !res.ExpiresAt.IsZero() {
			t := res.ExpiresAt
			out.ExpiresAt = &t
		}
		return p.writeJSON(out)
	}
	if p.quiet || !p.outTTY {
		_, err := fmt.Fprintln(p.out, res.QURL)
		return err
	}
	ew := &errWriter{w: p.out}
	ew.printf("%s\n", res.QURL)
	if line := p.resolveDetail(res); line != "" {
		ew.printf("\n%s\n", p.dim("  "+line))
	}
	return ew.flush(nil)
}

func (p *Printer) resolveDetail(res *qurlapi.Resolved) string {
	var expiry string
	switch {
	case res.ExpiresInSeconds > 0:
		expiry = "Expires in " + formatDuration(time.Duration(res.ExpiresInSeconds)*time.Second)
	case !res.ExpiresAt.IsZero():
		expiry = "Expires " + p.formatExpiry(res.ExpiresAt)
	}
	switch {
	case expiry != "" && res.SingleUse:
		return expiry + " (single use)"
	case expiry != "":
		return expiry
	case res.SingleUse:
		return "Single use"
	default:
		return ""
	}
}

// List renders one page of resources. The text table middle-ellipsizes the
// CRID column; --quiet and JSON always carry full identifiers. An empty page
// writes nothing to stdout — zero rows means zero data lines.
func (p *Printer) List(page *qurlapi.ResourcePage) error {
	switch {
	case p.format == FormatJSON:
		out := listJSON{Resources: make([]listItemJSON, 0, len(page.Items)), HasMore: page.HasMore, NextCursor: page.NextCursor}
		for _, item := range page.Items {
			out.Resources = append(out.Resources, listItemJSON{
				CRID:       item.CRID,
				ResourceID: item.ResourceID,
				TargetURL:  item.TargetURL,
				Status:     item.Status,
				CreatedAt:  item.CreatedAt,
				ExpiresAt:  item.ExpiresAt,
			})
		}
		return p.writeJSON(out)
	case p.quiet:
		ew := &errWriter{w: p.out}
		for _, item := range page.Items {
			ew.printf("%s\n", primaryID(item.CRID, item.ResourceID))
		}
		return ew.flush(nil)
	default:
		return p.listText(page)
	}
}

func (p *Printer) listText(page *qurlapi.ResourcePage) error {
	if len(page.Items) == 0 {
		// Zero items is only "nothing found" when the server says the listing
		// is complete: post-filtering legitimately produces empty pages with
		// more behind them.
		if page.HasMore {
			p.notefMore(page.NextCursor)
		} else {
			p.Notef("No resources found.")
		}
		return nil
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	// Headers stay uncolored: tabwriter counts ANSI escape bytes as cell
	// width, so styled headers would skew every column under them.
	ew.printf("CRID\tTARGET\tSTATUS\tCREATED\tEXPIRES\n")
	for i := range page.Items {
		item := &page.Items[i]
		ew.printf("%s\t%s\t%s\t%s\t%s\n",
			p.middleEllipsis(primaryID(item.CRID, item.ResourceID), listCRIDWidth),
			p.truncateEnd(item.TargetURL, listTargetWidth),
			item.Status,
			p.listCreated(item.CreatedAt),
			p.listExpires(item.ExpiresAt))
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore {
		p.notefMore(page.NextCursor)
	}
	return nil
}

func (p *Printer) notefMore(cursor string) {
	if cursor != "" {
		p.Notef("More results available: add --cursor %s", cursor)
		return
	}
	p.Notef("More results available.")
}

func (p *Printer) listCreated(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return p.relativeTime(*t)
}

func (p *Printer) listExpires(t *time.Time) string {
	if t == nil {
		return "never"
	}
	remaining := t.Sub(p.now())
	if remaining <= 0 {
		return expiredLabel
	}
	return "in " + formatDuration(remaining)
}

// Delete renders a delete outcome. The text confirmation is a status message
// for humans and goes to stderr; --quiet echoes the identifier to stdout so
// scripts can pipeline it; JSON emits the outcome document. When the resource
// was already gone the caller has said so on stderr, so the text confirmation
// is suppressed rather than contradicting it; JSON reports the same fact as a
// field instead.
func (p *Printer) Delete(id string, alreadyGone bool) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(deleteJSON{ID: id, Deleted: true, AlreadyGone: alreadyGone})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, id)
		return err
	case alreadyGone:
		return nil
	default:
		_, err := fmt.Fprintf(p.err, "Deleted %s.\n", id)
		return err
	}
}

// Downloaded renders a completed --file download. The file itself is the
// data, so the text confirmation is a status message for humans and goes to
// stderr; --quiet echoes the destination path to stdout for pipelines; JSON
// emits the outcome document.
func (p *Printer) Downloaded(crid, path string, bytes int64) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(downloadJSON{CRID: crid, File: path, Bytes: bytes})
	case p.quiet:
		_, err := fmt.Fprintln(p.out, path)
		return err
	default:
		// Best-effort like every stderr status line: the file is already in
		// place, so a broken stderr must not turn success into failure.
		_, _ = fmt.Fprintf(p.err, msgSavedTo+"\n", path, bytes)
		return nil
	}
}

// primaryID prefers the CRID and falls back to the resource ID for rows the
// service has not (yet) minted a CRID for.
func primaryID(crid, resourceID string) string {
	if crid != "" {
		return crid
	}
	return resourceID
}
