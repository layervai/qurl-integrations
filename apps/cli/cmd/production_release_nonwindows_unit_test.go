//go:build clisandbox && cliproduction && !windows

package main

import "testing"

func productionStateDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
