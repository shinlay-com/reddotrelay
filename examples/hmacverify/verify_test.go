package hmacverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	secret := []byte("receiver-secret")
	body := []byte(`{"eventId":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}`)
	timestamp := "1787552400"
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	signature := "v1=" + hex.EncodeToString(mac.Sum(nil))
	now := time.Unix(1787552405, 0)
	if err := Verify(secret, timestamp, signature, now, 5*time.Minute, body); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		timestamp, signature string
		body                 []byte
		now                  time.Time
	}{
		"body":      {timestamp, signature, append(append([]byte(nil), body...), ' '), now},
		"signature": {timestamp, "v1=" + string(make([]byte, 64)), body, now},
		"stale":     {timestamp, signature, body, now.Add(10 * time.Minute)},
	} {
		if err := Verify(secret, test.timestamp, test.signature, test.now, 5*time.Minute, test.body); err == nil {
			t.Fatalf("%s input accepted", name)
		}
	}
}
