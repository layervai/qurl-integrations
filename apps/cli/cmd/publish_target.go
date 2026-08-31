package main

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const localPublishIDDomain = "qurl-cli-local-publish-v1"

type publishTargetKind uint8

const (
	publishTargetRemote publishTargetKind = iota
	publishTargetLocal
)

type publishTarget struct {
	kind            publishTargetKind
	original        string
	canonicalOrigin string
	localIP         string
	localPort       int
}

// classifyPublishTarget separates the existing remote publish path from the
// foreground local-Connector path. Classification is syntax-only: it never
// resolves DNS, so a remote hostname cannot become local because of the
// machine's resolver configuration.
func classifyPublishTarget(raw string) (*publishTarget, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, invalidPublishTarget(errors.New("target URL must not be empty"))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalidPublishTarget(fmt.Errorf("parse target URL: %w", err))
	}
	if u.Scheme != httpURLScheme && u.Scheme != httpsURLScheme {
		return nil, invalidPublishTarget(errors.New("target URL must use http or https"))
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, invalidPublishTarget(errors.New("target URL must include a host"))
	}
	if u.User != nil {
		return nil, invalidPublishTarget(errors.New("target URL must not include credentials"))
	}
	if _, err := targetPort(u); err != nil {
		return nil, invalidPublishTarget(err)
	}

	host := u.Hostname()
	ip := net.ParseIP(host)
	isLocalhost := strings.EqualFold(host, "localhost")
	isLoopbackIP := ip != nil && ip.IsLoopback()
	if !isLocalhost && !isLoopbackIP {
		if localLookingHost(host) {
			return nil, invalidPublishTarget(fmt.Errorf("target host %q is not a supported loopback address", host))
		}
		return &publishTarget{kind: publishTargetRemote, original: raw}, nil
	}
	return classifyLocalPublishTarget(raw, u, host, ip, isLocalhost)
}

func classifyLocalPublishTarget(raw string, u *url.URL, host string, ip net.IP, isLocalhost bool) (*publishTarget, error) {
	if u.Scheme != httpURLScheme {
		return nil, invalidPublishTarget(errors.New("local publishing currently supports cleartext http origins only"))
	}
	if u.Path != "" && u.Path != "/" {
		return nil, invalidPublishTarget(errors.New("local publishing accepts an origin only, without a path"))
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, invalidPublishTarget(errors.New("local publishing accepts an origin only, without a query"))
	}
	if u.Fragment != "" || strings.HasSuffix(raw, "#") {
		return nil, invalidPublishTarget(errors.New("local publishing accepts an origin only, without a fragment"))
	}

	port, _ := targetPort(u)
	var forwardIP string
	if isLocalhost {
		forwardIP = "127.0.0.1"
	} else {
		forwardIP = ip.String()
	}
	canonical := "http://" + net.JoinHostPort(forwardIP, strconv.Itoa(port))
	return &publishTarget{
		kind:            publishTargetLocal,
		original:        raw,
		canonicalOrigin: canonical,
		localIP:         forwardIP,
		localPort:       port,
	}, nil
}

func targetPort(u *url.URL) (int, error) {
	raw := u.Port()
	if raw == "" {
		if strings.HasSuffix(u.Host, ":") {
			return 0, errors.New("target URL port must not be empty")
		}
		if u.Scheme == httpsURLScheme {
			return 443, nil
		}
		return 80, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("target URL port %q must be between 1 and 65535", raw)
	}
	return port, nil
}

func localLookingHost(host string) bool {
	lower := strings.ToLower(host)
	return lower == "localhost." || strings.HasSuffix(lower, ".localhost") ||
		strings.HasPrefix(lower, "127.") || strings.HasPrefix(lower, "::1%") ||
		lower == "0.0.0.0" || lower == "::"
}

func invalidPublishTarget(err error) error {
	return exitcode.UsageError(fmt.Errorf("invalid target URL: %w", err))
}

func generatedLocalConnectorID(agentID, canonicalOrigin string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || canonicalOrigin == "" {
		return "", errors.New("cannot derive a local Connector ID without the native agent identity and canonical origin")
	}
	digest := sha256.Sum256([]byte(localPublishIDDomain + "\x00id\x00" + agentID + "\x00" + canonicalOrigin))
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
	return "local-" + suffix, nil
}

// generatedReplacementLocalConnectorID derives the next stable default only
// from a locally accepted binding that an authorized delete retired. It does
// not turn an unexplained authority conflict into replacement permission.
func generatedReplacementLocalConnectorID(connectorID, resourceID string) (string, error) {
	connectorID = strings.TrimSpace(connectorID)
	resourceID = strings.TrimSpace(resourceID)
	if connectorID == "" || resourceID == "" {
		return "", errors.New("cannot derive a replacement Connector ID without the retired Connector and resource identities")
	}
	digest := sha256.Sum256([]byte(localPublishIDDomain + "\x00replacement\x00" + connectorID + "\x00" + resourceID))
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
	return "local-" + suffix, nil
}

const localEnrollmentEntropyBytes = 32

// localEnrollmentIdempotencyKey binds one random, process-local enrollment
// attempt to its Agent and Connector identities. The same attempt reuses the
// key for safe transport retry. A later process must use new entropy because
// the service can retain a successful one-shot credential response for at
// least as long as that credential is valid.
func localEnrollmentIdempotencyKey(agentID, connectorID string, entropy []byte) (string, error) {
	agentID = strings.TrimSpace(agentID)
	connectorID = strings.TrimSpace(connectorID)
	if agentID == "" || connectorID == "" {
		return "", errors.New("cannot derive enrollment idempotency without the native agent and Connector identities")
	}
	if len(entropy) != localEnrollmentEntropyBytes {
		return "", errors.New("qURL Connector enrollment idempotency requires 32 bytes of attempt entropy")
	}
	payload := make([]byte, 0, len(localPublishIDDomain)+len("\x00enrollment\x00")+len(agentID)+len(connectorID)+len(entropy)+2)
	payload = append(payload, localPublishIDDomain+"\x00enrollment\x00"+agentID+"\x00"+connectorID+"\x00"...)
	payload = append(payload, entropy...)
	digest := sha256.Sum256(payload)
	return "qurl-cli-local-publish-" + hex.EncodeToString(digest[:]), nil
}

// TODO(upstream-contract): Keep this local fail-fast check in lockstep with
// qurl-service's Connector slug grammar and qurl-go's validateConnectorSlug.
func validateConnectorID(id string) error {
	if len(id) < 3 || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return invalidConnectorID(id)
	}
	for i := 1; i < len(id)-1; i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return invalidConnectorID(id)
		}
	}
	last := id[len(id)-1]
	if (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		return invalidConnectorID(id)
	}
	return nil
}

func invalidConnectorID(id string) error {
	return exitcode.UsageError(fmt.Errorf("invalid Connector ID %q: use 3-64 lowercase letters, numbers, or hyphens; start with a letter and end with a letter or number", id))
}
