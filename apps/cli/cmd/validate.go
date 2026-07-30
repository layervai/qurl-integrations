package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// durationPattern matches Go-style durations and day suffixes (e.g., "1h", "24h", "7d", "30m").
var durationPattern = regexp.MustCompile(`^(\d+)([smhd])$`)

// validateURL checks that the target URL is a valid HTTP(S) URL.
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL scheme %q: must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("invalid URL: missing host")
	}
	return nil
}

// validateDuration checks that the duration string matches expected patterns.
func validateDuration(d string) error {
	if d == "" {
		return nil // optional
	}
	m := durationPattern.FindStringSubmatch(d)
	if m == nil {
		return fmt.Errorf("invalid duration %q: use format like 30m, 1h, 24h, 7d", d)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid duration %q: must be greater than zero", d)
	}
	return nil
}

// validateResourceID checks only presence. qurl-service owns the opaque resource
// ID syntax; duplicating its former r_ prefix contract here broke the public-key
// REST-ID cutover before the request could reach the authoritative validator.
func validateResourceID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("resource ID is required")
	}
	return nil
}

// validateAccessToken checks that the token has the expected prefix.
func validateAccessToken(token string) error {
	if !strings.HasPrefix(token, "at_") {
		return fmt.Errorf("invalid access token %q: expected prefix \"at_\"", token)
	}
	if len(token) < 6 {
		return fmt.Errorf("invalid access token %q: too short", token)
	}
	return nil
}
