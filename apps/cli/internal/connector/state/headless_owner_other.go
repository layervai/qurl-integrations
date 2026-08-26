//go:build !unix

package state

import "os"

// Headless container/runtime releases target Unix. On another platform the
// ownership shape is unavailable, so bearer credentials fail closed while the
// non-secret config reader remains usable.
func sensitiveFileReadableByProcess(os.FileInfo) bool { return false }
