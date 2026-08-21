//go:build !((linux && !android) || (darwin && !ios))

package state

import "os"

func openConnectorResourceState(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // Unsupported runtime; compilation only.
}
