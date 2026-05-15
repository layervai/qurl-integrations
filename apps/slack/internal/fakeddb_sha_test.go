package internal

// Local sha256 hex helper for fixture rows. Mirrors what
// slackdata.hashBootstrapCode does without forcing an import of an
// unexported helper.

import (
	"crypto/sha256"
	"encoding/hex"
)

func fakeSha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
