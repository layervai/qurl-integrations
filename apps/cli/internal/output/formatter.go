// Package output provides formatters for CLI output.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Formatter is the interface for output formatters.
type Formatter interface {
	FormatQURL(w io.Writer, qurl *client.QURL) error
	FormatList(w io.Writer, output *client.ListOutput) error
	FormatResolve(w io.Writer, output *client.ResolveOutput) error
}

// TableFormatter outputs human-readable tables.
type TableFormatter struct{}

// FormatQURL formats a single QURL as a key-value table.
func (TableFormatter) FormatQURL(w io.Writer, qurl *client.QURL) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("ID:\t%s\n", qurl.ID)
	wr.printf("Link:\t%s\n", qurl.LinkURL)
	wr.printf("Target:\t%s\n", qurl.TargetURL)
	if qurl.Title != "" {
		wr.printf("Title:\t%s\n", qurl.Title)
	}
	if qurl.Description != "" {
		wr.printf("Description:\t%s\n", qurl.Description)
	}
	if qurl.ExpiresAt != nil {
		wr.printf("Expires:\t%s\n", qurl.ExpiresAt.Format(time.RFC3339))
	}
	wr.printf("Clicks:\t%d\n", qurl.ClickCount)
	if wr.err != nil {
		return wr.err
	}
	return tw.Flush()
}

// FormatList formats a list of QURLs as a columnar table.
func (TableFormatter) FormatList(w io.Writer, output *client.ListOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("ID\tTARGET\tCLICKS\tEXPIRES\n")
	wr.printf("%s\t%s\t%s\t%s\n",
		strings.Repeat("-", 14), strings.Repeat("-", 30),
		strings.Repeat("-", 6), strings.Repeat("-", 10))
	for i := range output.QURLs {
		q := &output.QURLs[i]
		expires := "never"
		if q.ExpiresAt != nil {
			remaining := time.Until(*q.ExpiresAt)
			if remaining > 0 {
				expires = formatDuration(remaining)
			} else {
				expires = "expired"
			}
		}
		target := q.TargetURL
		if len(target) > 30 {
			target = target[:27] + "..."
		}
		wr.printf("%s\t%s\t%d\t%s\n", q.ID, target, q.ClickCount, expires)
	}
	if output.NextCursor != "" {
		wr.printf("\nMore results available. Use --cursor %s\n", output.NextCursor)
	}
	if wr.err != nil {
		return wr.err
	}
	return tw.Flush()
}

// FormatResolve formats a resolve result as a key-value table.
func (TableFormatter) FormatResolve(w io.Writer, output *client.ResolveOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("Target:\t%s\n", output.TargetURL)
	wr.printf("Resource:\t%s\n", output.ResourceID)
	if output.AccessGrant != nil {
		wr.printf("Access:\tgranted for %ds from %s\n",
			output.AccessGrant.ExpiresIn, output.AccessGrant.SrcIP)
	}
	if wr.err != nil {
		return wr.err
	}
	return tw.Flush()
}

// JSONFormatter outputs raw JSON.
type JSONFormatter struct{}

// FormatQURL formats a single QURL as JSON.
func (JSONFormatter) FormatQURL(w io.Writer, qurl *client.QURL) error {
	return writeJSON(w, qurl)
}

// FormatList formats a list of QURLs as JSON.
func (JSONFormatter) FormatList(w io.Writer, output *client.ListOutput) error {
	return writeJSON(w, output)
}

// FormatResolve formats a resolve result as JSON.
func (JSONFormatter) FormatResolve(w io.Writer, output *client.ResolveOutput) error {
	return writeJSON(w, output)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func formatDuration(d time.Duration) string {
	if d > 24*time.Hour {
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	}
	if d > time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// errWriter wraps an io.Writer and stops writing after the first error.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}
