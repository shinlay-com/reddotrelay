package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const secretPrefix = "api_key_"

// GenerateAPIKeySecret returns a 256-bit bearer credential. Only its SHA-256
// digest is persisted; the plaintext is shown once by the creation command.
func GenerateAPIKeySecret() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func HashAPIKeySecret(secret string) ([sha256.Size]byte, error) {
	if !strings.HasPrefix(secret, secretPrefix) {
		return [sha256.Size]byte{}, errors.New("invalid API key format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil || len(raw) != 32 {
		return [sha256.Size]byte{}, errors.New("invalid API key format")
	}
	return sha256.Sum256([]byte(secret)), nil
}

func APIKeyPrefix(secret string) string {
	const visible = 12
	if len(secret) <= visible {
		return secret
	}
	return secret[:visible]
}
