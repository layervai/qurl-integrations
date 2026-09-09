//go:build !unix

package auth

import (
	"errors"
	"os"
)

func openExternalEnrollmentTokenNoFollow(string) (*os.File, error) {
	return nil, errors.New("external enrollment token files are unsupported on this platform")
}

func validateOpenExternalEnrollmentToken(*os.File, os.FileInfo) error {
	return errors.New("external enrollment token files are unsupported on this platform")
}
