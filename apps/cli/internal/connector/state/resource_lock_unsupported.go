//go:build !((linux && !android) || (darwin && !ios)) && !windows

package state

import (
	"context"
	"errors"
	"os"
)

func acquireConnectorResourcesLock(context.Context, string) (func() error, error) {
	return nil, errors.New("Connector resource state locking is unsupported on this platform")
}

func connectorResourceOwnerOK(os.FileInfo) bool { return false }
