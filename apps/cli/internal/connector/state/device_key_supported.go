//go:build (linux && !android) || (darwin && !ios)

package state

// deviceKeyReadSupported reports that ReadDeviceStaticPrivateKey can open
// device state on this platform. qurl-go documents and tests its read-only
// open of the state for Linux and macOS.
const deviceKeyReadSupported = true
