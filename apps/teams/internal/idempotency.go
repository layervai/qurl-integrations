package internal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
)

// IdempotencyKey derives a deterministic 64-hex Idempotency-Key for a
// Teams-originated request from the four scope-distinguishing fields the bot
// receives. The same tuple always hashes to the same value, so qURL can dedupe
// connector bootstrap-key retries triggered by request replay.
func IdempotencyKey(tenantID, scopeID, userID, attemptID string) string {
	return hashIdempotencyFields(tenantID, scopeID, userID, attemptID)
}

func hashIdempotencyFields(fields ...string) string {
	h := sha256.New()
	for _, field := range fields {
		writeLengthPrefixed(h, field)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeLengthPrefixed(w io.Writer, s string) {
	if uint64(len(s)) > math.MaxUint32 {
		panic(fmt.Sprintf("writeLengthPrefixed: field of %d bytes exceeds uint32 max", len(s)))
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(s)))
	_, _ = w.Write(buf[:])
	_, _ = w.Write([]byte(s))
}
