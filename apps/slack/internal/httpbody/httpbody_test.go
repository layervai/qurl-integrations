package httpbody

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

// TestReadResponseBodyAcceptsBodyUpToLimit is the >= guard: a body that exactly fills
// the ceiling is valid, and comparing with >= would refuse it as though it had
// overflowed. It deliberately does NOT cover the other half of the off-by-one — with the
// read shortened to limit bytes every size below still passes, because that mutation
// only misreads bodies larger than the ceiling. The two oversize tests below are what
// kill that one.
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

// TestReadResponseBodyClampsNegativeLimit pins what the clamp actually does, which is
// the opposite of what mirroring DrainResponseBody's reasoning would suggest. Unclamped,
// io.LimitReader reads nothing from a negative count and the comparison then refuses
// every body — the empty one included — with the negative digits in the operator's
// message. Clamped, a negative ceiling behaves exactly as a zero one does, so both halves
// are asserted: empty is valid, anything longer is refused at zero.
func TestReadResponseBodyClampsNegativeLimit(t *testing.T) {
	raw, err := ReadResponseBody("auth.test", strings.NewReader(""), -5)
	if err != nil || len(raw) != 0 {
		t.Fatalf("ReadResponseBody(_, empty, -5) = %q, %v, want empty body and nil", raw, err)
	}
	_, err = ReadResponseBody("auth.test", strings.NewReader("body"), -5)
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
