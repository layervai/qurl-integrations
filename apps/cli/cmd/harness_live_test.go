//go:build clisandbox

package main

// liveJourneysBuilt says this test binary carries the live sandbox journeys.
// Only then may an invocation keep the production HTTP client.
const liveJourneysBuilt = true
