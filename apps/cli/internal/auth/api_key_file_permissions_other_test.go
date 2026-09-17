//go:build !unix && !windows

package auth

import "testing"

func protectAPIKeyTestFile(*testing.T, string) {}
