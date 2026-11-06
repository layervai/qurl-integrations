package internal

import (
	"errors"
	"net/url"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/nethost"
)

const connectorAPIVersionPath = "/v1"

var (
	errConnectorAPIURLMissing = errors.New("QURL_API_URL is missing")
	errConnectorAPIURLInvalid = errors.New("QURL_API_URL is invalid")
)

// ConnectorURLFailure identifies a URL shape without exposing input or credentials.
type ConnectorURLFailure uint8

// Connector URL failures keep startup diagnostics separate from URL validation.
const (
	connectorURLValid ConnectorURLFailure = iota
	ConnectorURLMissing
	ConnectorURLOrigin
	ConnectorURLCredentials
	ConnectorURLQuery
	ConnectorURLFragment
	ConnectorURLVersion
	ConnectorURLPath
	ConnectorURLInvalid
)

func (reason ConnectorURLFailure) Error() string {
	if reason == ConnectorURLMissing {
		return errConnectorAPIURLMissing.Error()
	}
	return errConnectorAPIURLInvalid.Error()
}

// ParseConnectorEndpoint validates the operator's origin and returns its API base.
func ParseConnectorEndpoint(raw string) (endpoint, apiURL string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", ConnectorURLMissing
	}
	endpoint = strings.TrimRight(trimmed, "/")
	if endpoint == "" {
		return "", "", ConnectorURLOrigin
	}
	if reason := connectorURLShape(endpoint, true); reason != connectorURLValid {
		return "", "", reason
	}
	return endpoint, endpoint + connectorAPIVersionPath, nil
}

// ValidateConnectorAPIURL keeps the renderer's sentinel error contract.
func ValidateConnectorAPIURL(raw string) error {
	reason := connectorURLShape(strings.TrimSpace(raw), false)
	if reason == connectorURLValid {
		return nil
	}
	if reason == ConnectorURLMissing {
		return errConnectorAPIURLMissing
	}
	return errConnectorAPIURLInvalid
}

func connectorURLShape(raw string, origin bool) ConnectorURLFailure {
	if raw == "" {
		return ConnectorURLMissing
	}
	parse := url.ParseRequestURI
	if origin {
		parse = url.Parse
	}
	parsed, err := parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return ConnectorURLInvalid
	}
	if parsed.User != nil {
		return ConnectorURLCredentials
	}
	if parsed.RawQuery != "" {
		return ConnectorURLQuery
	}
	if parsed.Fragment != "" {
		return ConnectorURLFragment
	}
	if origin {
		if strings.EqualFold(strings.Trim(parsed.Path, "/"), "v1") {
			return ConnectorURLVersion
		}
		if parsed.Path != "" {
			return ConnectorURLPath
		}
		// Preserve rejection of empty query/fragment delimiters after appending /v1.
		if parsed.ForceQuery || strings.Contains(raw, "#") {
			return ConnectorURLInvalid
		}
		if _, err := url.ParseRequestURI(raw + connectorAPIVersionPath); err != nil {
			return ConnectorURLInvalid
		}
	} else if strings.TrimRight(parsed.Path, "/") != connectorAPIVersionPath {
		return ConnectorURLPath
	}
	if parsed.Scheme == resourceExposeSchemeHTTPS {
		return connectorURLValid
	}
	if parsed.Scheme != resourceExposeSchemeHTTP || !nethost.IsLoopback(parsed.Hostname()) {
		return ConnectorURLInvalid
	}
	return connectorURLValid
}
