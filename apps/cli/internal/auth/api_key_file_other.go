//go:build !((linux && !android) || (darwin && !ios)) && !windows

package auth

import (
	"errors"
	"os"
)

func validAPIKeyEnvironmentFileMode(os.FileMode) bool { return false }

func validateAPIKeyFilePathPlatform(string, os.FileInfo) error {
	return errors.New("API-key file credentials are unsupported on this platform")
}

func validateOpenAPIKeyFilePlatform(*os.File, os.FileInfo) error {
	return errors.New("API-key file credentials are unsupported on this platform")
}

func openAPIKeyFileNoFollow(string) (*os.File, error) {
	return nil, errors.New("API-key file credentials are unsupported on this platform")
}
