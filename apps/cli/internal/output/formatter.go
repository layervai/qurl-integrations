// Package output provides formatters for CLI output.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Output format constants.
const (
	FormatTable = "table"
	FormatJSON  = "json"
)

// Formatter is the interface for output formatters.
type Formatter interface {
	FormatQURL(w io.Writer, qurl *client.QURL) error
	FormatCreate(w io.Writer, output *client.CreateOutput) error
	FormatList(w io.Writer, output *client.ListOutput) error
	FormatResolve(w io.Writer, output *client.ResolveOutput) error
	FormatMint(w io.Writer, output *client.MintOutput) error
	FormatQuota(w io.Writer, output *client.QuotaOutput) error
}

// --- Table Formatter ---

// TableFormatter outputs human-readable tables with color.
type TableFormatter struct {
	bold  *color.Color
	green *color.Color
	red   *color.Color
	cyan  *color.Color
	dim   *color.Color
}

// NewTableFormatter creates a table formatter with color support.
func NewTableFormatter() TableFormatter {
	return TableFormatter{
		bold:  color.New(color.Bold),
		green: color.New(color.FgGreen),
		red:   color.New(color.FgRed),
		cyan:  color.New(color.FgCyan),
		dim:   color.New(color.Faint),
	}
}

// FormatQURL formats a single qURL as a key-value table.
func (f TableFormatter) FormatQURL(w io.Writer, qurl *client.QURL) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("%s\t%s\n", f.bold.Sprint("ID:"), qurl.ResourceID)
	wr.printf("%s\t%s\n", f.bold.Sprint("Target:"), qurl.TargetURL)
	wr.printf("%s\t%s\n", f.bold.Sprint("Status:"), f.colorStatus(qurl.Status))
	if qurl.Description != "" {
		wr.printf("%s\t%s\n", f.bold.Sprint("Description:"), qurl.Description)
	}
	if len(qurl.Tags) > 0 {
		wr.printf("%s\t%s\n", f.bold.Sprint("Tags:"), strings.Join(qurl.Tags, ", "))
	}
	if qurl.QURLSite != "" {
		wr.printf("%s\t%s\n", f.bold.Sprint("Site:"), qurl.QURLSite)
	}
	if qurl.CustomDomain != nil && *qurl.CustomDomain != "" {
		wr.printf("%s\t%s\n", f.bold.Sprint("Custom domain:"), *qurl.CustomDomain)
	}
	wr.printf("%s\t%s\n", f.bold.Sprint("Created:"), formatRelativeTime(qurl.CreatedAt))
	if qurl.ExpiresAt != nil {
		wr.printf("%s\t%s\n", f.bold.Sprint("Expires:"), formatExpiry(*qurl.ExpiresAt))
	}
	return wr.flush(tw)
}

// FormatCreate formats a create response.
func (f TableFormatter) FormatCreate(w io.Writer, output *client.CreateOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("%s\n\n", f.green.Sprint("qURL created"))
	if output.QurlID != "" {
		wr.printf("%s\t%s\n", f.bold.Sprint("QURL ID:"), output.QurlID)
	}
	wr.printf("%s\t%s\n", f.bold.Sprint("ID:"), output.ResourceID)
	wr.printf("%s\t%s\n", f.bold.Sprint("Link:"), output.QURLLink)
	wr.printf("%s\t%s\n", f.bold.Sprint("Site:"), output.QURLSite)
	if output.Label != "" {
		wr.printf("%s\t%s\n", f.bold.Sprint("Label:"), output.Label)
	}
	if output.ExpiresAt != nil {
		wr.printf("%s\t%s\n", f.bold.Sprint("Expires:"), formatExpiry(*output.ExpiresAt))
	}
	return wr.flush(tw)
}

// FormatList formats a list of qURLs as a columnar table.
func (f TableFormatter) FormatList(w io.Writer, output *client.ListOutput) error {
	if len(output.QURLs) == 0 {
		_, err := fmt.Fprintln(w, f.dim.Sprint("No qURLs found."))
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}

	header := fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
		f.bold.Sprint("ID"), f.bold.Sprint("TARGET"),
		f.bold.Sprint("STATUS"), f.bold.Sprint("CREATED"), f.bold.Sprint("EXPIRES"))
	wr.printf("%s\n", header)
	wr.printf("%s\t%s\t%s\t%s\t%s\n",
		strings.Repeat("─", 14), strings.Repeat("─", 40),
		strings.Repeat("─", 8), strings.Repeat("─", 10), strings.Repeat("─", 10))

	for i := range output.QURLs {
		q := &output.QURLs[i]
		target := q.TargetURL
		if len(target) > 40 {
			target = target[:39] + "…"
		}
		expires := "never"
		if q.ExpiresAt != nil {
			remaining := time.Until(*q.ExpiresAt)
			if remaining > 0 {
				expires = formatDuration(remaining)
			} else {
				expires = f.red.Sprint("expired")
			}
		}
		wr.printf("%s\t%s\t%s\t%s\t%s\n",
			q.ResourceID, target, f.colorStatus(q.Status),
			formatRelativeTime(q.CreatedAt), expires)
	}

	if output.NextCursor != "" {
		wr.printf("\n%s\n", f.cyan.Sprintf("More results available. Use --cursor %s", output.NextCursor))
	}
	return wr.flush(tw)
}

// FormatResolve formats a resolve result as a key-value table.
func (f TableFormatter) FormatResolve(w io.Writer, output *client.ResolveOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("%s\n\n", f.green.Sprint("Access granted"))
	wr.printf("%s\t%s\n", f.bold.Sprint("Target:"), output.TargetURL)
	wr.printf("%s\t%s\n", f.bold.Sprint("Resource:"), output.ResourceID)
	if output.AccessGrant != nil {
		wr.printf("%s\t%ds from %s\n", f.bold.Sprint("Access:"),
			output.AccessGrant.ExpiresIn, output.AccessGrant.SrcIP)
	}
	return wr.flush(tw)
}

// FormatMint formats a mint response.
func (f TableFormatter) FormatMint(w io.Writer, output *client.MintOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("%s\n\n", f.green.Sprint("Link minted"))
	wr.printf("%s\t%s\n", f.bold.Sprint("Link:"), output.QURLLink)
	if output.ExpiresAt != nil {
		wr.printf("%s\t%s\n", f.bold.Sprint("Expires:"), formatExpiry(*output.ExpiresAt))
	}
	return wr.flush(tw)
}

// FormatQuota formats quota information.
func (f TableFormatter) FormatQuota(w io.Writer, output *client.QuotaOutput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	wr := &errWriter{w: tw}
	wr.printf("%s\t%s\n", f.bold.Sprint("Plan:"), strings.ToUpper(output.Plan))

	if output.Usage != nil {
		u := output.Usage
		if output.RateLimits != nil && output.RateLimits.MaxActiveQURLs > 0 {
			wr.printf("%s\t%d / %d\n", f.bold.Sprint("Active qURLs:"),
				u.ActiveQURLs, output.RateLimits.MaxActiveQURLs)
		} else {
			wr.printf("%s\t%d\n", f.bold.Sprint("Active qURLs:"), u.ActiveQURLs)
		}
		wr.printf("%s\t%d\n", f.bold.Sprint("Created (period):"), u.QURLsCreated)
		wr.printf("%s\t%d\n", f.bold.Sprint("Total accesses:"), u.TotalAccesses)
	}

	wr.printf("%s\t%s – %s\n", f.bold.Sprint("Period:"),
		output.PeriodStart.Format("Jan 2"), output.PeriodEnd.Format("Jan 2, 2006"))

	return wr.flush(tw)
}

func (f TableFormatter) colorStatus(status string) string {
	switch status {
	case client.StatusActive:
		return f.green.Sprint(status)
	case client.StatusRevoked:
		return f.red.Sprint(status)
	default:
		return status
	}
}

// --- JSON Formatter ---

// JSONFormatter outputs raw JSON.
type JSONFormatter struct{}

// FormatQURL formats a single qURL as JSON.
func (JSONFormatter) FormatQURL(w io.Writer, qurl *client.QURL) error {
	return writeJSON(w, qurl)
}

// FormatCreate formats a create response as JSON.
func (JSONFormatter) FormatCreate(w io.Writer, output *client.CreateOutput) error {
	return writeJSON(w, output)
}

// FormatList formats a list of qURLs as JSON.
func (JSONFormatter) FormatList(w io.Writer, output *client.ListOutput) error {
	return writeJSON(w, output)
}

// FormatResolve formats a resolve result as JSON.
func (JSONFormatter) FormatResolve(w io.Writer, output *client.ResolveOutput) error {
	return writeJSON(w, output)
}

// FormatMint formats a mint response as JSON.
func (JSONFormatter) FormatMint(w io.Writer, output *client.MintOutput) error {
	return writeJSON(w, output)
}

// FormatQuota formats quota info as JSON.
func (JSONFormatter) FormatQuota(w io.Writer, output *client.QuotaOutput) error {
	return writeJSON(w, output)
}

// --- Helpers ---

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
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func formatRelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func formatExpiry(t time.Time) string {
	remaining := time.Until(t)
	if remaining <= 0 {
		return "expired"
	}
	return fmt.Sprintf("%s (%s)", t.Format(time.RFC3339), formatDuration(remaining))
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

// flush returns the first write error, or flushes the underlying tabwriter.
func (ew *errWriter) flush(tw *tabwriter.Writer) error {
	if ew.err != nil {
		return ew.err
	}
	return tw.Flush()
}
