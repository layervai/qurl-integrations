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
// A nil transport opens a new connection; tests can supply an explicit transport.
func productionRetainedSessionFetch(original *http.Request, marker string, transport http.RoundTripper) (resultErr error) {
	if transport == nil {
		fresh, err := productionRetainedSessionCloneTransport(http.DefaultTransport)
		if err != nil {
			return err
		}
		defer fresh.CloseIdleConnections()
		transport = fresh
	}
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

// Clone an explicit base so unit tests never replace the process-wide transport.
func productionRetainedSessionCloneTransport(transport http.RoundTripper) (*http.Transport, error) {
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, errors.New("retained production session transport could not be isolated")
	}
	return base.Clone(), nil
}
