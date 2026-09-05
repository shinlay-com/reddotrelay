package sqlite

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrRPCCredentialEncryptionKeyRequired indicates that an administrator tried
// to persist a write-only RPC credential without configuring its local key.
var ErrRPCCredentialEncryptionKeyRequired = errors.New("admin-managed RPC credentials require security.rpc_credentials_key_ref")

// ConfigureRPCSecrets derives a per-database encryption key from an
// operator-supplied secret. The supplied value is never retained.
func (s *Store) ConfigureRPCSecrets(key []byte) error {
	if len(key) == 0 {
		return errors.New("RPC credential encryption key is empty")
	}
	digest := sha256.Sum256(key)
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return err
	}
	sealed, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	s.rpcSecretCipher = sealed
	return nil
}

func (s *Store) sealRPCSecret(value string) ([]byte, error) {
	if value == "" {
		return []byte{}, nil
	}
	if s.rpcSecretCipher == nil {
		return nil, ErrRPCCredentialEncryptionKeyRequired
	}
	nonce := make([]byte, s.rpcSecretCipher.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.rpcSecretCipher.Seal(nonce, nonce, []byte(value), nil), nil
}

func (s *Store) openRPCSecret(value []byte) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	if s.rpcSecretCipher == nil {
		return "", errors.New("RPC credential encryption key is unavailable")
	}
	if len(value) < s.rpcSecretCipher.NonceSize() {
		return "", errors.New("stored RPC credential is invalid")
	}
	plain, err := s.rpcSecretCipher.Open(nil, value[:s.rpcSecretCipher.NonceSize()], value[s.rpcSecretCipher.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt stored RPC credential: %w", err)
	}
	return string(plain), nil
}

// SealBackfillSnapshot protects immutable snapshots with the same local key as
// managed RPC credentials. A marker keeps databases without configured RPC
// secrets backward compatible while avoiding accidental JSON interpretation.
func (s *Store) SealBackfillSnapshot(value []byte) ([]byte, error) {
	if s.rpcSecretCipher == nil {
		return append([]byte{0}, value...), nil
	}
	nonce := make([]byte, s.rpcSecretCipher.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := s.rpcSecretCipher.Seal(nonce, nonce, value, nil)
	return append([]byte{1}, sealed...), nil
}
func (s *Store) OpenBackfillSnapshot(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("backfill snapshot is empty")
	}
	if value[0] == 0 {
		return append([]byte(nil), value[1:]...), nil
	}
	if value[0] != 1 || s.rpcSecretCipher == nil {
		return nil, errors.New("backfill snapshot encryption key is unavailable")
	}
	sealed := value[1:]
	if len(sealed) < s.rpcSecretCipher.NonceSize() {
		return nil, errors.New("backfill snapshot is invalid")
	}
	return s.rpcSecretCipher.Open(nil, sealed[:s.rpcSecretCipher.NonceSize()], sealed[s.rpcSecretCipher.NonceSize():], nil)
}
