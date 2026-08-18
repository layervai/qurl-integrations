package agent

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ResourceSDKOrigin derives the origin the qurl-go resource client should use
// from a versioned qURL API base URL (for example https://api.example.layerv.ai/v1
// becomes https://api.example.layerv.ai). The SDK appends its own versioned
// paths, so handing it the versioned base would double the version segment.
// Userinfo, query, and fragment components are rejected rather than silently
// stripped: an origin is a trust anchor, and any decoration on it is a
// misconfiguration.
func ResourceSDKOrigin(versionedBase string) (string, error) {
	u, err := url.Parse(versionedBase)
	if err != nil {
		return "", fmt.Errorf("parse qURL resource API URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", errors.New("qURL resource API URL must be an absolute http or https URL with a host")
	}
	// url.Parse does not distinguish an absent fragment delimiter from an
	// empty one, so the literal check keeps https://host/v1# fail closed as
	// well.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(versionedBase, "#") || u.User != nil {
		return "", errors.New("qURL resource API URL must not contain userinfo, query, or fragment")
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	u.Path = path
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}
