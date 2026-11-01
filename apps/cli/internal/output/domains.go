package output

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const domainUnknownValue = "unknown"

func domainBool(value *bool) string {
	if value == nil {
		return domainUnknownValue
	}
	return strconv.FormatBool(*value)
}

// DomainPage shows current server readiness without interpreting missing values.
func (p *Printer) DomainPage(page *qurlapi.AccountPage[qurlapi.AccountDomain]) error {
	if p.format == FormatJSON {
		return p.writeJSON(page)
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("DOMAIN\tSTATUS\tREADY FOR QURLS\n")
	}
	for i := range page.Data {
		item := &page.Data[i]
		if p.quiet {
			ew.printf("%s\n", item.Domain)
		} else {
			ew.printf("%s\t%s\t%s\n", item.Domain, strconv.Quote(item.Status), domainBool(item.ReadyForQURLs))
		}
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore && !p.quiet {
		p.Notef("More domains are available; use next_cursor from --output json.")
	}
	return nil
}

// Domain prints the API's current configuration; it never changes local or remote DNS.
func (p *Printer) Domain(item *qurlapi.AccountDomain) error {
	if p.format == FormatJSON {
		return p.writeJSON(item)
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, item.Domain)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("Domain:\t%s\nStatus:\t%s\nReady for qURLs:\t%s\nVerification token:\t%s\n", item.Domain, strconv.Quote(item.Status), domainBool(item.ReadyForQURLs), strconv.Quote(item.VerificationToken))
	if item.TokenExpiresAt != "" {
		ew.printf("Token expires:\t%s\n", strconv.Quote(item.TokenExpiresAt))
	}
	if item.FailureReason != "" {
		ew.printf("Failure reason:\t%s\n", strconv.Quote(item.FailureReason))
	}
	ew.printf("\nDNS TYPE\tNAME\tVALUE\tVERIFIED\n")
	for _, record := range item.DNSRecords {
		ew.printf("%s\t%s\t%s\t%s\n", strconv.Quote(record.Type), strconv.Quote(record.Name), strconv.Quote(record.Value), domainBool(record.Verified))
	}
	return ew.flush(tw)
}

// DomainDeleted reports only a confirmed removal.
func (p *Printer) DomainDeleted(name string) error {
	if p.format == FormatJSON {
		return p.writeJSON(map[string]string{"domain": name, accountStatusField: "deleted"})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, name)
		return err
	}
	_, err := fmt.Fprintf(p.out, "Deleted %s\n", name)
	return err
}

// DomainVerification preserves each actual check, including incomplete verification.
func (p *Printer) DomainVerification(result *qurlapi.DomainVerification) error {
	if p.format == FormatJSON {
		return p.writeJSON(result)
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, result.Status)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("Domain:\t%s\nStatus:\t%s\n\nDNS CHECK\tVERIFIED\tFOUND\tERROR\n", result.Domain, strconv.Quote(result.Status))
	for _, entry := range []struct {
		name  string
		check *qurlapi.DomainDNSCheck
	}{
		{"TXT", &result.Checks.TXT}, {"ACME CNAME", &result.Checks.ACMECNAME}, {"Traffic routing", &result.Checks.TrafficRouting},
	} {
		ew.printf("%s\t%s\t%s\t%s\n", entry.name, domainBool(entry.check.Verified), strconv.Quote(entry.check.Found), strconv.Quote(entry.check.Error))
	}
	return ew.flush(tw)
}
