//go:build !clisandbox

package main

// liveJourneysBuilt says this test binary carries the live sandbox journeys.
// It does not, so no invocation can keep the production HTTP client, whatever
// its options say.
const liveJourneysBuilt = false
