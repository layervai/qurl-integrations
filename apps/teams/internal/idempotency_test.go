package internal

import "testing"

func TestIdempotencyKeyIsDeterministic(t *testing.T) {
	t.Parallel()

	k1 := IdempotencyKey("T1", "C1", "U1", "attempt-1")
	k2 := IdempotencyKey("T1", "C1", "U1", "attempt-1")

	if k1 != k2 {
		t.Fatalf("IdempotencyKey mismatch: %q != %q", k1, k2)
	}
}

func TestIdempotencyKeyPreservesFieldBoundaries(t *testing.T) {
	t.Parallel()

	a := IdempotencyKey("ab", "c", "U1", "attempt-1")
	b := IdempotencyKey("a", "bc", "U1", "attempt-1")

	if a == b {
		t.Fatalf("IdempotencyKey collided across field boundaries: %q", a)
	}
}
