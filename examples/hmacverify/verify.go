// Package hmacverify demonstrates verification of RedDotRelay webhook requests.
package hmacverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Verify authenticates the exact body bytes and rejects timestamps outside the
// permitted clock skew. Call it before decoding JSON or performing side effects.
func Verify(secret []byte, timestampHeader, signatureHeader string, now time.Time, maxSkew time.Duration, body []byte) error {
	if len(secret) == 0 || maxSkew <= 0 {
		return errors.New("verification is not configured")
	}
	seconds, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != timestampHeader {
		return errors.New("invalid RedDotRelay timestamp")
	}
	observed := time.Unix(seconds, 0)
	delta := now.Sub(observed)
	if delta < 0 {
		delta = -delta
	}
	if delta > maxSkew {
		return errors.New("stale RedDotRelay timestamp")
	}
	if !strings.HasPrefix(signatureHeader, "v1=") {
		return errors.New("unsupported RedDotRelay signature version")
	}
	presented, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, "v1="))
	if err != nil || len(presented) != sha256.Size {
		return errors.New("invalid RedDotRelay signature")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestampHeader + "."))
	_, _ = mac.Write(body)
	if !hmac.Equal(presented, mac.Sum(nil)) {
		return errors.New("invalid RedDotRelay signature")
	}
	return nil
}
