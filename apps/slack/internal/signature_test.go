package internal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"
)

func validSigAndHeaders(t *testing.T, secret string, body []byte, ts time.Time) (sig, tsHeader string) {
	t.Helper()
	tsHeader = strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(slackSignatureVersion + ":" + tsHeader + ":"))
	mac.Write(body)
	sig = slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	return sig, tsHeader
}

func TestVerifySlackSignature_Valid(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := []byte("hello")
	sig, ts := validSigAndHeaders(t, "secret", body, now)

	if err := verifySlackSignature("secret", body, sig, ts, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifySlackSignature_BoundaryCases(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := []byte("hello")
	sig, ts := validSigAndHeaders(t, "secret", body, now)

	cases := []struct {
		name string
		want error
		call func() error
	}{
		{
			name: "empty secret fails closed",
			want: errSlackSigningSecretEmpty,
			call: func() error { return verifySlackSignature("", body, sig, ts, now) },
		},
		{
			name: "missing signature header",
			want: errSlackSignatureMissing,
			call: func() error { return verifySlackSignature("secret", body, "", ts, now) },
		},
		{
			name: "missing timestamp header",
			want: errSlackSignatureMissing,
			call: func() error { return verifySlackSignature("secret", body, sig, "", now) },
		},
		{
			name: "malformed signature (no v0= prefix)",
			want: errSlackSignatureMalformed,
			call: func() error { return verifySlackSignature("secret", body, "deadbeef", ts, now) },
		},
		{
			name: "malformed signature (non-hex)",
			want: errSlackSignatureMalformed,
			call: func() error { return verifySlackSignature("secret", body, "v0=notvalidhex", ts, now) },
		},
		{
			name: "malformed timestamp",
			want: errSlackTimestampMalformed,
			call: func() error { return verifySlackSignature("secret", body, sig, "not-a-timestamp", now) },
		},
		{
			name: "timestamp in the past outside skew",
			want: errSlackTimestampStale,
			call: func() error { return verifySlackSignature("secret", body, sig, ts, now.Add(10*time.Minute)) },
		},
		{
			name: "timestamp in the future outside skew",
			want: errSlackTimestampStale,
			call: func() error { return verifySlackSignature("secret", body, sig, ts, now.Add(-10*time.Minute)) },
		},
		{
			name: "tampered body",
			want: errSlackSignatureMismatch,
			call: func() error { return verifySlackSignature("secret", []byte("tampered"), sig, ts, now) },
		},
		{
			name: "wrong secret",
			want: errSlackSignatureMismatch,
			call: func() error { return verifySlackSignature("different-secret", body, sig, ts, now) },
		},
		{
			// Guard rails the defense-in-depth against a math.MinInt64
			// timestamp that would wrap the subtraction/abs chain.
			name: "negative timestamp fails stale",
			want: errSlackTimestampStale,
			call: func() error { return verifySlackSignature("secret", body, sig, "-9223372036854775808", now) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if tc.want == nil && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifySlackSignature_SkewBoundary(t *testing.T) {
	// The skew check should accept a signature exactly at the boundary and
	// reject one past it, so replays aren't off-by-one-ing their way in.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := []byte("body")
	sig, ts := validSigAndHeaders(t, "secret", body, now)

	if err := verifySlackSignature("secret", body, sig, ts, now.Add(slackTimestampSkew)); err != nil {
		t.Errorf("signature at exact skew boundary rejected: %v", err)
	}

	if err := verifySlackSignature("secret", body, sig, ts, now.Add(slackTimestampSkew+time.Second)); !errors.Is(err, errSlackTimestampStale) {
		t.Errorf("signature one second past skew boundary: got %v, want %v", err, errSlackTimestampStale)
	}
}
