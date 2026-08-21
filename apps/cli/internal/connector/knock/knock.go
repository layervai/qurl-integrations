// Package knock is the per-cycle NHP admission seam between the qURL
// Connector supervisor and qurl-go's native UDP knock runtime.
//
// The heavy lifting — packet construction, assigned-cell resolution, server
// authentication, ACK parsing, and the registered-agent knock/exit
// transactions — lives in the qurl-go SDK. This package owns the thin glue
// around those calls: one process-lifetime runtime key, one immutable RunID
// per supervisor cycle, resource-keyed translation of the SDK's admission
// result, and best-effort session cleanup when a cycle ends.
package knock

import (
	"context"
	"time"
)

// Result is the admission subset the supervisor reads. Decoupling here keeps
// the supervisor independent of qurl-go's native UDP result type.
//
// Wire contract (server → agent → supervisor):
//
//   - ACTokens[resource_id] → the tunnel Login metadata knock token
//   - ResourceHost[resource_id] → the tunnel dial target (canonical host:port)
//
// Both maps are populated per resource by the server-side admission
// controller. Empty values for a configured resource_id are a server-side
// contract violation: in knock-then-login mode the NHP ACK is the source of
// truth for the tunnel dial target, so the consumer must fail closed rather
// than fall back to a static address.
type Result struct {
	// ACTokens is the admission-controller-issued, opaque knock-token map
	// keyed by resource_id. The supervisor requires the entry under its
	// configured knock resource; missing or empty entries are fail-closed
	// contract violations the consumer must refuse to Login on.
	ACTokens map[string]string

	// ResourceHost is the per-resource dial target the server resolved for
	// this knock. Missing or unusable entries are fail-closed contract
	// violations: there is no static-address fallback after a knock.
	ResourceHost map[string]string
}

// Knocker abstracts a single NHP knock against the server-side admission
// controller. The real implementation (Native) wraps qurl-go's native agent
// runtime, which knocks the configured resource through its authenticated
// cell assignment; tests inject a fake that returns canned ACTokens and
// ResourceHost values.
//
// Concurrency: callers serialize Knock calls through the supervisor's
// per-cycle goroutine. Implementations are not required to be
// concurrent-safe — the supervisor never fires two knocks in flight.
//
// Cancellation: Knock must propagate ctx to the native qurl-go call. A caller
// deadline or cancellation bounds the UDP transaction and returns through
// this interface; implementations must not replace it with an
// uninterruptible background context.
type Knocker interface {
	Knock(ctx context.Context) (*Result, error)
}

// CycleKnocker extends Knocker for implementations that correlate the NHP
// admission and the tunnel Login with one caller-owned cycle identifier. The
// supervisor calls BeginCycle exactly once before the first knock of each
// outer cycle, then reads CycleRunID only after that knock succeeds. Physical
// tunnel reconnects inside the same outer cycle call Knock again without
// BeginCycle, so they reuse the exact identifier. After every successful
// BeginCycle, the supervisor calls EndCycle exactly once, even when Knock
// fails or its reply is unusable: the server may have created session state
// before a reply was lost. EndCycle is best-effort cleanup and also runs
// after the tunnel service stops.
//
// The interface is split from Knocker to keep direct test fakes source
// compatible.
type CycleKnocker interface {
	Knocker
	BeginCycle() error
	CycleRunID() string
	EndCycle(ctx context.Context) error
}

// latencyMS converts a time.Duration to float milliseconds for log
// attributes. Derived from Microseconds()/1000 rather than Milliseconds() so
// sub-millisecond timings survive (a fast LAN knock completes in well under a
// millisecond; integer truncation would round those to 0). Keep any future
// latency emit site on this helper so histogram bucketing stays uniform
// across knock and cycle entries.
func latencyMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
