package qurlapi

import "testing"

// A linked device's client is the registered wrapper, which exposes exactly
// Client; the Stripe calls must survive that narrowing.
func TestRegisteredClientKeepsPublisherStripe(t *testing.T) {
	var wrapped Client = &registeredClient{Client: &client{}}
	if _, ok := wrapped.(PublisherStripe); !ok {
		t.Fatal("registered client hides the publisher Stripe calls")
	}
}
