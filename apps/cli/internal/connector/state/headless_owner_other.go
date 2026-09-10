//go:build !unix && !windows

package state

import "os"

// Headless container/runtime releases target Unix and Windows. On another
// platform the ownership shape is unavailable, so bearer credentials fail
// closed while the non-secret config reader remains usable.
func pinnedFileWritableByAnotherUser(*os.File, os.FileInfo) (bool, error) { return false, nil }

func sensitiveFileReadableByProcess(os.FileInfo) bool { return false }

func validatePinnedFileParent(string) error { return nil }
