package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

var errRetainedSessionTransport = errors.New("retained production session request failed")
var errRetainedSessionRefused = errors.New("original production session did not serve content")

// productionWaitRetainedSession retries transport failures; responses and timeouts are final.
// TODO(upstream-contract): the gateway caches active tunnel routes for at most five seconds.
func productionWaitRetainedSession(ctx context.Context, original *http.Request, marker string, transport http.RoundTripper) error {
	for {
		err := productionRetainedSessionFetch(original.Clone(ctx), marker, transport)
		if !errors.Is(err, errRetainedSessionTransport) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func productionActiveSessionIDs(ctx context.Context, client *qurl.Client, crid string) (map[string]bool, error) {
	ids := map[string]bool{}
	cursor := ""
	for range 10 {
		page, err := client.ListSessions(ctx, crid, cursor)
		if err != nil {
			return nil, errors.New("could not read production fixture sessions")
		}
		for _, session := range page.Data {
			ids[session.SessionID] = true
		}
		if page.Meta.NextCursor == "" {
			return ids, nil
		}
		cursor = page.Meta.NextCursor
	}
	return nil, errors.New("production fixture exceeded session page bound")
}

// productionRetainedSessionFetch reuses an already authorized request. It has
// no opener, resolver, cookie jar, or redirect path that can replace its grant.
// A nil transport opens a new connection; non-nil transports are unit-test fakes.
func productionRetainedSessionFetch(original *http.Request, marker string, transport http.RoundTripper) (resultErr error) {
	if transport == nil {
		fresh, err := productionRetainedSessionTransport(http.DefaultTransport)
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
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return fmt.Errorf("%w (%T)", errRetainedSessionTransport, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil && resultErr == nil {
			resultErr = errors.New("retained production session response could not be closed")
		}
	}()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w (HTTP %d)", errRetainedSessionRefused, response.StatusCode)
		}
		return fmt.Errorf("original production session did not serve content (HTTP %d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(marker)+1)))
	if err != nil || string(body) != marker {
		return errors.New("original production session returned unexpected content")
	}
	return nil
}

func productionRetainedSessionTransport(base http.RoundTripper) (*http.Transport, error) {
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, errors.New("retained production session transport could not be isolated")
	}
	// A Connector restart can close pooled connections; the grant must work on a new one.
	return transport.Clone(), nil
}
