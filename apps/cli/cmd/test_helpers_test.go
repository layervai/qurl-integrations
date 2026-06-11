package main

import "testing"

func isolateCLIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_ENDPOINT", "")
	t.Setenv("QURL_PROFILE", "")
}
