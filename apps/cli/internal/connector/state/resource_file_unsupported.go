//go:build !((linux && !android) || (darwin && !ios)) && !windows

package state

import (
	"errors"
	"os"
)

func openConnectorResourceState(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // unsupported runtime; compilation only.
}

func validateConnectorResourceFile(string, os.FileInfo) error {
	return errors.New("Connector state files are unsupported on this platform")
}

func createConnectorResourceTemp(string) (*os.File, error) {
	return nil, errors.New("Connector state files are unsupported on this platform")
}

func commitConnectorResourceRename(string, string) error {
	return errors.New("Connector state files are unsupported on this platform")
}
