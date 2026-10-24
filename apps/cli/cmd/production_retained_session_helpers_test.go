package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// productionRetainedSessionFetch reuses an already authorized request. It has
// no opener, resolver, cookie jar, or redirect path that can replace its grant.
func productionRetainedSessionFetch(original *http.Request, marker string, transport http.RoundTripper) (resultErr error) {
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Transport errors stay sanitized and cannot pass the content check.
	response, err := client.Do(original.Clone(original.Context()))
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return errors.New("retained production session request timed out")
		}
		return errors.New("retained production session request failed")
	}
	defer func() {
		if err := response.Body.Close(); err != nil && resultErr == nil {
			resultErr = errors.New("retained production session response could not be closed")
		}
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("original production session did not serve content (HTTP %d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(marker)+1)))
	if err != nil || string(body) != marker {
		return errors.New("original production session returned unexpected content")
	}
	return nil
}
