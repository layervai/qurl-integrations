package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// productionRetainedSessionFetch reuses an already authorized request. It has
// no opener, resolver, cookie jar, or redirect path that can replace its grant.
func productionRetainedSessionFetch(original *http.Request, marker string, denied bool, transport http.RoundTripper) (resultErr error) {
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// A transport failure does not prove an application-session refusal.
	response, err := client.Do(original.Clone(original.Context()))
	if err != nil {
		return errors.New("retained production session request failed")
	}
	defer func() {
		if err := response.Body.Close(); err != nil && resultErr == nil {
			resultErr = errors.New("retained production session response could not be closed")
		}
	}()
	if denied {
		// Accept only explicit refusal, including the protected route's concealed 404.
		if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusGone {
			return fmt.Errorf("replaced production session did not return an access refusal (HTTP %d)", response.StatusCode)
		}
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("original production session did not serve content")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(marker)+1)))
	if err != nil || string(body) != marker {
		return errors.New("original production session returned unexpected content")
	}
	return nil
}
