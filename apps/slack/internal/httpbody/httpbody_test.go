package httpbody

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

// TestReadResponseBodyAcceptsBodyUpToLimit is the off-by-one guard in the direction
// that fails silently. A body that exactly fills the ceiling is valid, and rejecting it
// — which reading limit bytes, or comparing with >=, would do — surfaces as a JSON
// parse error on a response that was fine — on the Lambda's hot path as much as in a
// smoke command no CI run exercises.
func TestReadResponseBodyAcceptsBodyUpToLimit(t *testing.T) {
	const limit = 64
	for _, size := range []int{0, limit - 1, limit} {
		want := strings.Repeat("a", size)
		raw, err := ReadResponseBody("auth.test", strings.NewReader(want), limit)
		if err != nil {
			t.Fatalf("ReadResponseBody(_, %d bytes, %d) = %v, want nil", size, limit, err)
		}
		if string(raw) != want {
			t.Fatalf("ReadResponseBody(_, %d bytes, %d) = %d bytes, want %d", size, limit, len(raw), size)
		}
	}
}

// TestReadResponseBodyRejectsOneByteOverLimit is the same guard in the other direction,
// and pins the text alongside it: both smoke commands and all six Lambda call sites
// printed exactly this message before it was hoisted, so the sentinel's own wording
// must not leak into it.
func TestReadResponseBodyRejectsOneByteOverLimit(t *testing.T) {
	const limit = 64
	raw, err := ReadResponseBody("auth.test", strings.NewReader(strings.Repeat("a", limit+1)), limit)
	if err == nil {
		t.Fatal("ReadResponseBody = nil error, want an oversize refusal")
	}
	if raw != nil {
		t.Fatalf("raw = %d bytes, want none alongside the error", len(raw))
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ReadResponseBody = %v, want errors.Is ErrResponseTooLarge", err)
	}
	if want := "auth.test response exceeded 64 bytes"; err.Error() != want {
		t.Fatalf("ReadResponseBody = %q, want %q", err.Error(), want)
	}
}

// TestReadResponseBodyDrainsOversizeBody pins the two reads an oversize response costs:
// limit+1 to detect it, then DrainResponseBody's own limit+1. Returning the refusal
// without the drain leaves the connection unreusable, which is what that function is for.
func TestReadResponseBodyDrainsOversizeBody(t *testing.T) {
	const (
		limit = 16
		total = 4096
	)
	src := strings.NewReader(strings.Repeat("a", total))
	if _, err := ReadResponseBody("conversations.history", src, limit); err == nil {
		t.Fatal("ReadResponseBody = nil error, want an oversize refusal")
	}
	if got, want := total-src.Len(), 2*(limit+1); got != want {
		t.Fatalf("consumed %d bytes, want %d", got, want)
	}
}

// TestReadResponseBodyWrapsReadError keeps a failed read distinguishable from an
// oversize one. slack-dm-smoke records a different result code for each and tells them
// apart with errors.Is, so a read failure matching the sentinel would be mislabeled.
func TestReadResponseBodyWrapsReadError(t *testing.T) {
	cause := errors.New("connection reset")
	raw, err := ReadResponseBody("auth.test", iotest.ErrReader(cause), 64)
	if raw != nil {
		t.Fatalf("raw = %d bytes, want none alongside the error", len(raw))
	}
	if !errors.Is(err, cause) {
		t.Fatalf("ReadResponseBody = %v, want errors.Is %v", err, cause)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ReadResponseBody = %v, want a read failure, not ErrResponseTooLarge", err)
	}
	if want := "auth.test response read: connection reset"; err.Error() != want {
		t.Fatalf("ReadResponseBody = %q, want %q", err.Error(), want)
	}
}

// TestReadResponseBodyClampsNegativeLimit mirrors DrainResponseBody's clamp. Without it
// io.LimitReader treats a negative count as immediate EOF, so a nonsensical ceiling
// would return an empty body as valid rather than refusing it.
func TestReadResponseBodyClampsNegativeLimit(t *testing.T) {
	_, err := ReadResponseBody("auth.test", strings.NewReader("body"), -5)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ReadResponseBody(_, _, -5) = %v, want errors.Is ErrResponseTooLarge", err)
	}
	if want := "auth.test response exceeded 0 bytes"; err.Error() != want {
		t.Fatalf("ReadResponseBody(_, _, -5) = %q, want %q", err.Error(), want)
	}
}

// TestDrainResponseBodyHonoursCallerLimit is the regression guard for the hazard that
// motivated the limit parameter: when this function was duplicated per command it read
// whichever maxSlackResponseBytes sat in the same file, so the same body drained a 64x
// different budget depending on which copy ran.
func TestDrainResponseBodyHonoursCallerLimit(t *testing.T) {
	const total = 4096
	for _, limit := range []int64{0, 16, 1024} {
		src := strings.NewReader(strings.Repeat("a", total))
		DrainResponseBody(src, limit)
		if got := int64(total - src.Len()); got != limit+1 {
			t.Fatalf("DrainResponseBody(_, %d) consumed %d bytes, want %d", limit, got, limit+1)
		}
	}
	// A negative limit is clamped to zero, so it drains the same one byte a zero limit
	// does — not the zero bytes io.LimitReader would read from a negative count.
	src := strings.NewReader(strings.Repeat("a", total))
	DrainResponseBody(src, -5)
	if got := total - src.Len(); got != 1 {
		t.Fatalf("DrainResponseBody(_, -5) consumed %d bytes, want 1", got)
	}
}

func TestDrainResponseBodyStopsAtShortBody(t *testing.T) {
	src := strings.NewReader("short")
	DrainResponseBody(src, 1<<20)
	if src.Len() != 0 {
		t.Fatalf("unread = %d, want 0", src.Len())
	}
}
