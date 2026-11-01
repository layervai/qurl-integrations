package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/layervai/qurl-go/qurl"
)

// AccountDomain carries the service's current DNS instructions and readiness.
// TODO(upstream-contract): qurl-service api/openapi.yaml owns domain status, DNS and enterprise entitlement.
type AccountDomain struct {
	Domain            string            `json:"domain"`
	Status            string            `json:"status"`
	VerificationToken string            `json:"verification_token"`
	TokenExpiresAt    string            `json:"token_expires_at,omitempty"`
	ACMECNAMETarget   string            `json:"acme_cname_target,omitempty"`
	ReadyForQURLs     *bool             `json:"ready_for_qurls,omitempty"`
	DNSRecords        []DomainDNSRecord `json:"dns_records"`
	CreatedAt         string            `json:"created_at,omitempty"`
	VerifiedAt        string            `json:"verified_at,omitempty"`
	ActivatedAt       string            `json:"activated_at,omitempty"`
	FailureReason     string            `json:"failure_reason,omitempty"`
}

// DomainDNSRecord is an instruction returned by the API, never a local DNS write.
type DomainDNSRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Verified *bool  `json:"verified,omitempty"`
}

// DomainDNSCheck retains a missing check as unknown instead of inventing failure.
type DomainDNSCheck struct {
	Verified *bool  `json:"verified"`
	Error    string `json:"error,omitempty"`
	Found    string `json:"found,omitempty"`
}

// DomainVerification is a single server-side DNS check, without polling.
type DomainVerification struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
	Checks struct {
		TXT            DomainDNSCheck `json:"txt"`
		ACMECNAME      DomainDNSCheck `json:"acme_cname"`
		TrafficRouting DomainDNSCheck `json:"traffic_routing"`
	} `json:"checks"`
}

var accountDomainStatusPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var accountDomainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// NormalizeAccountDomain matches server normalization and refuses URI/path syntax.
func NormalizeAccountDomain(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if len(name) > 253 || !accountDomainPattern.MatchString(name) {
		return "", fmt.Errorf("%w: supply a DNS hostname without a URL, port, wildcard or trailing dot", qurl.ErrInvalidResourceRequest)
	}
	return name, nil
}

func validAccountDomain(item *AccountDomain) bool {
	name, err := NormalizeAccountDomain(item.Domain)
	if err != nil || name != item.Domain || !accountDomainStatusPattern.MatchString(item.Status) || item.VerificationToken == "" || len(item.DNSRecords) == 0 {
		return false
	}
	for _, record := range item.DNSRecords {
		if record.Type == "" || record.Name == "" || record.Value == "" {
			return false
		}
	}
	return true
}

func decodeAccountDomain(reply *restReply, status int, name string) (*AccountDomain, error) {
	if reply.status != status {
		return nil, reply.problem()
	}
	var wire struct {
		Data AccountDomain `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || !validAccountDomain(&wire.Data) || wire.Data.Domain != name {
		return nil, fmt.Errorf("%w: incomplete or mismatched domain result", qurl.ErrInvalidAPIResponse)
	}
	return &wire.Data, nil
}

// ListDomains reads one bounded account page, including its DNS configuration.
func (c *client) ListDomains(ctx context.Context, limit int, cursor string) (*AccountPage[AccountDomain], error) {
	if err := ValidateAccountPage(limit); err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, accountListPath("/v1/domains", limit, cursor), nil)
	if err != nil {
		return nil, err
	}
	page, err := decodeAccountPage[AccountDomain](reply, limit)
	if err != nil {
		return nil, err
	}
	for i := range page.Data {
		if !validAccountDomain(&page.Data[i]) {
			return nil, fmt.Errorf("%w: incomplete domain metadata", qurl.ErrInvalidAPIResponse)
		}
	}
	return page, nil
}

// GetAccountDomain returns current DNS instructions; expired tokens remain visible.
func (c *client) GetAccountDomain(ctx context.Context, raw string) (*AccountDomain, error) {
	name, err := NormalizeAccountDomain(raw)
	if err != nil {
		return nil, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/domains/"+name, nil)
	if err != nil {
		return nil, err
	}
	return decodeAccountDomain(reply, http.StatusOK, name)
}

// RegisterAccountDomain makes one request; enterprise entitlement stays server-owned.
func (c *client) RegisterAccountDomain(ctx context.Context, raw string) (*AccountDomain, error) {
	name, err := NormalizeAccountDomain(raw)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/domains", map[string]string{"domain": name})
	if err != nil {
		return nil, err
	}
	return decodeAccountDomain(reply, http.StatusCreated, name)
}

// DeleteAccountDomain removes one named account domain without automatic retry.
func (c *client) DeleteAccountDomain(ctx context.Context, raw string) error {
	name, err := NormalizeAccountDomain(raw)
	if err != nil {
		return err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, "/v1/domains/"+name, nil)
	if err != nil {
		return err
	}
	if reply.status != http.StatusNoContent {
		return reply.problem()
	}
	return nil
}

// VerifyAccountDomain checks DNS once and preserves incomplete verification results.
func (c *client) VerifyAccountDomain(ctx context.Context, raw string) (*DomainVerification, error) {
	name, err := NormalizeAccountDomain(raw)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/domains/"+name+"/verify", nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var wire struct {
		Data DomainVerification `json:"data"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || wire.Data.Domain != name || !accountDomainStatusPattern.MatchString(wire.Data.Status) || wire.Data.Checks.TXT.Verified == nil || wire.Data.Checks.ACMECNAME.Verified == nil || wire.Data.Checks.TrafficRouting.Verified == nil {
		return nil, fmt.Errorf("%w: incomplete or mismatched DNS verification result", qurl.ErrInvalidAPIResponse)
	}
	return &wire.Data, nil
}

// RegenerateDomainToken returns the latest DNS token after one replacement request.
func (c *client) RegenerateDomainToken(ctx context.Context, raw string) (*AccountDomain, error) {
	name, err := NormalizeAccountDomain(raw)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/domains/"+name+"/regenerate-token", nil)
	if err != nil {
		return nil, err
	}
	return decodeAccountDomain(reply, http.StatusOK, name)
}
