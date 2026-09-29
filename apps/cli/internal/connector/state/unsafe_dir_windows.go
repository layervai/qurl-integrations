//go:build windows

package state

// unixModeRule is false on Windows: the connector's mode rule is Unix only,
// and Go synthesizes Windows directory modes, so a re-inspection proves nothing.
const unixModeRule = false
