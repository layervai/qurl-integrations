//go:build !((linux && !android) || (darwin && !ios))

package state

// deviceKeyReadSupported is false on every platform but Linux and macOS,
// Windows included. qurl-go documents its read-only open of the state for
// those two only, and no test here shows that the open changes nothing on
// another platform. ReadDeviceStaticPrivateKey returns no key there, and the
// caller goes on without it.
//
// TODO(upstream-contract): make this true for Windows when qurl-go documents
// its read-only open for Windows and a test on Windows shows that the open
// leaves the state directory as it was.
const deviceKeyReadSupported = false
