//go:build !windows

package state

// unixModeRule reports that the connector enforces its directory mode rule
// on this platform, so ExplainUnsafeDirectory can act on its refusals.
const unixModeRule = true
