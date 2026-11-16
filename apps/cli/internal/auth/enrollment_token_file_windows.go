//go:build windows

package auth

import "os"

func openExternalEnrollmentTokenNoFollow(path string) (*os.File, error) {
	return openAPIKeyFileNoFollow(path)
}

func validateOpenExternalEnrollmentToken(file *os.File, _ os.FileInfo) error {
	// Same exact-file contract: current owner, protected restrictive ACL,
	// no reparse point and exactly one hard link, checked on the open handle.
	return validateWindowsAPIKeyFile(file)
}

func validExternalEnrollmentTokenMode(os.FileMode) bool { return true }

func validateExternalEnrollmentTokenPlatform() error { return nil }
