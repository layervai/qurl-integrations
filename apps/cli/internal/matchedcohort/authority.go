// Package matchedcohort owns the environment-neutral fixed-canary authority
// used by the sandbox matched-cohort rollout. It contains no AWS client and no
// environment endpoint: the attended orchestration layer supplies a durable
// compare-and-swap store and a closed, signed plan.
package matchedcohort

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	// AuthoritySchema is the only accepted fixed-canary authority schema.
	AuthoritySchema = 1
	// EnvironmentSandbox prevents this package from accepting production authority.
	EnvironmentSandbox = "sandbox"
	// ColorBlue is the blue physical cohort identity.
	ColorBlue = "blue"
	// ColorGreen is the green physical cohort identity.
	ColorGreen = "green"
	// AgentKeySchemaVersion is the required qurl-agent-keys row schema.
	AgentKeySchemaVersion = 2
	// EnrollmentCredentialKindAccount requires an account registration row.
	EnrollmentCredentialKindAccount = "account"
	// RequiredNHPSourceSHA is the reviewed merged native-operation authority.
	RequiredNHPSourceSHA = "a70e5d66dda604459b0a37ed7c634da8c8e46c3d"
	// RequiredQURLGoSourceSHA is the reviewed merged durable-operation SDK.
	RequiredQURLGoSourceSHA = "c92478b3f70ff027fe7bd9c306b7a9fd96553b64"
	labelDirectA            = "direct-a"
	labelDirectB            = "direct-b"
	labelRelayC             = "relay-c"
	labelRelayD             = "relay-d"
	selectorResourceA       = "qurl-tunnel-server-a"
	selectorResourceB       = "qurl-tunnel-server-b"
	selectorResourceC       = "qurl-tunnel-server-c"
)

var (
	hex64Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex40Pattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	identityPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	awsRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[1-9]\d*$`)
	accountPattern   = regexp.MustCompile(`^\d{12}$`)
	// errInvalidAuthority reports any closed-schema or binding violation.
	errInvalidAuthority = errors.New("matched cohort: invalid fixed canary authority")
)

var (
	labels            = []string{labelDirectA, labelDirectB, labelRelayC, labelRelayD}
	selectorResources = []string{selectorResourceA, selectorResourceB, selectorResourceC, selectorResourceA}
)

// Plan is the complete non-secret input for one fixed two-color generation.
// The eight identities are permanent until an explicit rotation activates a
// replacement generation. A normal release reads this authority and never
// creates, deletes, or revokes any identity.
type Plan struct {
	Schema          int            `json:"schema"`
	Environment     string         `json:"environment"`
	GenerationID    string         `json:"generation_id"`
	OwnerSubject    string         `json:"owner_subject"`
	AWSAccountID    string         `json:"aws_account_id"`
	AWSRegion       string         `json:"aws_region"`
	NHPSourceSHA    string         `json:"nhp_source_sha"`
	QURLGoSourceSHA string         `json:"qurl_go_source_sha"`
	Cohorts         []CohortPlan   `json:"cohorts"`
	Identities      []IdentityPlan `json:"identities"`
}

// CohortPlan binds one physical color to its signed runtime and state authority.
type CohortPlan struct {
	Color                 string              `json:"color"`
	ServerASG             string              `json:"server_asg"`
	ACASG                 string              `json:"ac_asg"`
	RelayASG              string              `json:"relay_asg"`
	SessionControlTable   string              `json:"session_control_table"`
	QURLAgentKeysTable    string              `json:"qurl_agent_keys_table"`
	CellID                string              `json:"cell_id"`
	AssignmentGeneration  int64               `json:"assignment_generation"`
	HubHost               string              `json:"hub_host"`
	HubPort               int                 `json:"hub_port"`
	HubServerPublicKeyB64 string              `json:"hub_server_public_key_b64"`
	CellEndpoint          qurl.NHPUDPEndpoint `json:"cell_endpoint"`
}

// FRPSSelector is one exact authenticated placement outcome.
type FRPSSelector struct {
	ResourceID string `json:"resource_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
}

// IdentityPlan is the non-secret immutable identity requested by provisioning.
type IdentityPlan struct {
	Color       string       `json:"color"`
	Label       string       `json:"label"`
	OwnerID     string       `json:"owner_id"`
	AgentID     string       `json:"agent_id"`
	ConnectorID string       `json:"connector_id"`
	Selector    FRPSSelector `json:"frps_selector"`
}

// StateReference binds one immutable secret version without exposing its body.
type StateReference struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
}

// EnrollmentCredentialReceipt is the non-secret authenticated identity of the
// one ordinary key used for explicit generation provisioning. It is committed
// before registration and remains after the run-scoped key is revoked.
type EnrollmentCredentialReceipt struct {
	Schema    int      `json:"schema"`
	OwnerID   string   `json:"owner_id"`
	AuthType  string   `json:"auth_type"`
	KeyID     string   `json:"key_id"`
	Kind      string   `json:"kind"`
	Scopes    []string `json:"scopes"`
	KeyPrefix string   `json:"key_prefix"`
}

// FixedIdentity is one completed permanent canary identity and its state pointers.
type FixedIdentity struct {
	Color                    string         `json:"color"`
	Label                    string         `json:"label"`
	OwnerID                  string         `json:"owner_id"`
	AgentID                  string         `json:"agent_id"`
	AgentPublicKeyB64        string         `json:"agent_public_key_b64"`
	AgentKeySchemaVersion    int            `json:"agent_key_schema_version"`
	EnrollmentCredentialKind string         `json:"enrollment_credential_kind"`
	ConnectorIDClaim         string         `json:"connector_id_claim"`
	DeviceAPIKeyID           string         `json:"device_api_key_id"`
	ConnectorID              string         `json:"connector_id"`
	ResourceID               string         `json:"resource_id"`
	CRID                     string         `json:"crid"`
	ConnectorRoutingID       string         `json:"connector_routing_id"`
	KnockResourceID          string         `json:"knock_resource_id"`
	Selector                 FRPSSelector   `json:"frps_selector"`
	AgentState               StateReference `json:"agent_state"`
	ConnectorState           StateReference `json:"connector_state"`
}

// Authority is the credential-free completed two-color, eight-identity envelope.
type Authority struct {
	Schema                      int             `json:"schema"`
	Environment                 string          `json:"environment"`
	GenerationID                string          `json:"generation_id"`
	OwnerSubject                string          `json:"owner_subject"`
	AWSAccountID                string          `json:"aws_account_id"`
	AWSRegion                   string          `json:"aws_region"`
	NHPSourceSHA                string          `json:"nhp_source_sha"`
	QURLGoSourceSHA             string          `json:"qurl_go_source_sha"`
	EnrollmentCredentialReceipt StateReference  `json:"enrollment_credential_receipt"`
	Cohorts                     []CohortPlan    `json:"cohorts"`
	Identities                  []FixedIdentity `json:"identities"`
}

// ValidatePlan enforces the exact ordered sandbox provisioning input.
func ValidatePlan(plan Plan) error { //nolint:gocognit,gocritic,gocyclo // Closed trust-boundary checks remain explicit.
	if plan.Schema != AuthoritySchema || plan.Environment != EnvironmentSandbox || !hex64Pattern.MatchString(plan.GenerationID) {
		return fmt.Errorf("%w: plan identity", errInvalidAuthority)
	}
	if !validText(plan.OwnerSubject) || !accountPattern.MatchString(plan.AWSAccountID) || !awsRegionPattern.MatchString(plan.AWSRegion) ||
		!hex40Pattern.MatchString(plan.NHPSourceSHA) || plan.NHPSourceSHA != RequiredNHPSourceSHA ||
		!hex40Pattern.MatchString(plan.QURLGoSourceSHA) || plan.QURLGoSourceSHA != RequiredQURLGoSourceSHA {
		return fmt.Errorf("%w: plan owner or AWS authority", errInvalidAuthority)
	}
	if len(plan.Cohorts) != 2 || len(plan.Identities) != 8 {
		return fmt.Errorf("%w: require two cohorts and eight identities", errInvalidAuthority)
	}
	for index, color := range []string{ColorBlue, ColorGreen} {
		cohort := plan.Cohorts[index]
		if cohort.Color != color || !validCohort(cohort) {
			return fmt.Errorf("%w: %s cohort", errInvalidAuthority, color)
		}
		selectors := make(map[string]FRPSSelector, 3)
		for labelIndex, label := range labels {
			identity := plan.Identities[index*len(labels)+labelIndex]
			expectedSelector := selectorResources[labelIndex]
			if identity.Color != color || identity.Label != label || identity.OwnerID != plan.OwnerSubject || !validIdentityPlan(identity) ||
				identity.Selector.ResourceID != expectedSelector {
				return fmt.Errorf("%w: %s %s identity", errInvalidAuthority, color, label)
			}
			if expectedSelector == selectorResourceA {
				if prior, exists := selectors[expectedSelector]; exists && prior != identity.Selector {
					return fmt.Errorf("%w: %s selector a is inconsistent", errInvalidAuthority, color)
				}
			}
			selectors[expectedSelector] = identity.Selector
		}
		if len(selectors) != 3 || selectors[selectorResourceA].Host != selectors[selectorResourceB].Host ||
			selectors[selectorResourceA].Host != selectors[selectorResourceC].Host ||
			selectors[selectorResourceA].Port == selectors[selectorResourceB].Port ||
			selectors[selectorResourceA].Port == selectors[selectorResourceC].Port ||
			selectors[selectorResourceB].Port == selectors[selectorResourceC].Port {
			return fmt.Errorf("%w: %s selector set", errInvalidAuthority, color)
		}
	}
	seenAgents := map[string]struct{}{}
	seenConnectors := map[string]struct{}{}
	for _, identity := range plan.Identities {
		if _, exists := seenAgents[identity.AgentID]; exists {
			return fmt.Errorf("%w: duplicate agent id", errInvalidAuthority)
		}
		seenAgents[identity.AgentID] = struct{}{}
		if _, exists := seenConnectors[identity.ConnectorID]; exists {
			return fmt.Errorf("%w: duplicate connector id", errInvalidAuthority)
		}
		seenConnectors[identity.ConnectorID] = struct{}{}
	}
	return nil
}

// ValidateAuthority enforces the exact completed authority and state references.
func ValidateAuthority(authority Authority) error { //nolint:gocritic // Public validation intentionally snapshots one closed authority.
	plan := Plan{Schema: authority.Schema, Environment: authority.Environment, GenerationID: authority.GenerationID,
		OwnerSubject: authority.OwnerSubject, AWSAccountID: authority.AWSAccountID, AWSRegion: authority.AWSRegion,
		NHPSourceSHA: authority.NHPSourceSHA, QURLGoSourceSHA: authority.QURLGoSourceSHA,
		Cohorts: slices.Clone(authority.Cohorts), Identities: make([]IdentityPlan, len(authority.Identities))}
	for i := range authority.Identities {
		identity := &authority.Identities[i]
		plan.Identities[i] = IdentityPlan{Color: identity.Color, Label: identity.Label, OwnerID: identity.OwnerID,
			AgentID: identity.AgentID, ConnectorID: identity.ConnectorID, Selector: identity.Selector}
	}
	if err := ValidatePlan(plan); err != nil {
		return err
	}
	if err := validateStateReference(authority.EnrollmentCredentialReceipt); err != nil {
		return fmt.Errorf("%w: enrollment credential receipt", errInvalidAuthority)
	}
	for i := range authority.Identities {
		identity := &authority.Identities[i]
		if identity.AgentKeySchemaVersion != AgentKeySchemaVersion || identity.EnrollmentCredentialKind != EnrollmentCredentialKindAccount || identity.ConnectorIDClaim != "" {
			return fmt.Errorf("%w: registered agent row contract", errInvalidAuthority)
		}
		if !validBase64Raw32(identity.AgentPublicKeyB64) || !validText(identity.DeviceAPIKeyID) || !validText(identity.ResourceID) || !validText(identity.CRID) ||
			!validText(identity.ConnectorRoutingID) || !validText(identity.KnockResourceID) {
			return fmt.Errorf("%w: completed identity fields", errInvalidAuthority)
		}
		if err := validateStateReference(identity.AgentState); err != nil {
			return err
		}
		if err := validateStateReference(identity.ConnectorState); err != nil {
			return err
		}
	}
	return nil
}

// CanonicalJSON returns the compact Go-struct encoding used for every digest.
func CanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical authority: %w", err)
	}
	return raw, nil
}

// Digest returns a lowercase SHA-256 digest.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validCohort(cohort CohortPlan) bool { //nolint:gocritic // The closed two-item plan favors value semantics.
	return validText(cohort.ServerASG) && validText(cohort.ACASG) && validText(cohort.RelayASG) &&
		validText(cohort.SessionControlTable) && validText(cohort.QURLAgentKeysTable) && validText(cohort.CellID) &&
		cohort.AssignmentGeneration > 0 && validDNS(cohort.HubHost) && cohort.HubPort == 443 && validBase64Raw32(cohort.HubServerPublicKeyB64) &&
		validDNS(cohort.CellEndpoint.Host) && cohort.CellEndpoint.Port == 443 && validBase64Raw32(cohort.CellEndpoint.ServerPublicKeyB64)
}

func validIdentityPlan(identity IdentityPlan) bool { //nolint:gocritic // The closed eight-item plan favors value semantics.
	return validText(identity.OwnerID) && identityPattern.MatchString(identity.AgentID) && identityPattern.MatchString(identity.ConnectorID) &&
		validText(identity.Selector.ResourceID) && validDNS(identity.Selector.Host) && identity.Selector.Port > 0 && identity.Selector.Port <= 65535
}

func validateStateReference(ref StateReference) error {
	if !validText(ref.Key) || !validText(ref.VersionID) || !hex64Pattern.MatchString(ref.SHA256) {
		return fmt.Errorf("%w: state reference", errInvalidAuthority)
	}
	return nil
}

func validText(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func validDNS(value string) bool {
	return validText(value) && strings.Contains(value, ".") && !strings.ContainsAny(value, "/:@")
}

func validBase64Raw32(value string) bool {
	raw, err := base64.StdEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.StdEncoding.EncodeToString(raw) == value
}
