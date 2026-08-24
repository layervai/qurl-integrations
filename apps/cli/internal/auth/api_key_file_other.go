//go:build !((linux && !android) || (darwin && !ios))

package auth

import (
	"errors"
	"os"
)

func validateAPIKeyFilePlatform(os.FileInfo) error {
	return errors.New("API-key file credentials are unsupported on this platform")
}

func openAPIKeyFileNoFollow(string) (*os.File, error) {
	return nil, errors.New("API-key file credentials are unsupported on this platform")
}
