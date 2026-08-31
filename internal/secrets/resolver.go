package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxSecretBytes = 64 << 10

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Resolver loads secret values at their point of use. It never caches values,
// so operators can rotate an environment variable or mounted secret file
// without writing the resolved value to RedDotRelay's database.
type Resolver struct {
	lookupEnv func(string) (string, bool)
	readFile  func(string) ([]byte, error)
}

func New() *Resolver {
	return &Resolver{lookupEnv: os.LookupEnv, readFile: os.ReadFile}
}

func IsReference(value string) bool {
	return strings.HasPrefix(value, "env://") || strings.HasPrefix(value, "file://")
}

func ValidateReference(reference string) error {
	_, _, err := parseReference(reference)
	return err
}

func (r *Resolver) Resolve(ctx context.Context, reference string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	provider, key, err := parseReference(reference)
	if err != nil {
		return "", err
	}
	var value string
	switch provider {
	case "env":
		resolved, ok := r.lookupEnv(key)
		if !ok {
			return "", errors.New("environment secret is unavailable")
		}
		value = resolved
	case "file":
		data, readErr := r.readFile(key)
		if readErr != nil {
			return "", errors.New("file secret is unavailable")
		}
		if len(data) > maxSecretBytes {
			return "", errors.New("file secret exceeds size limit")
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if value == "" {
		return "", errors.New("resolved secret is empty")
	}
	if len(value) > maxSecretBytes {
		return "", errors.New("resolved secret exceeds size limit")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("resolved secret contains invalid data")
	}
	return value, nil
}

func parseReference(reference string) (string, string, error) {
	switch {
	case strings.HasPrefix(reference, "env://"):
		key := strings.TrimPrefix(reference, "env://")
		if !environmentName.MatchString(key) {
			return "", "", errors.New("env secret reference must contain a valid environment variable name")
		}
		return "env", key, nil
	case strings.HasPrefix(reference, "file://"):
		key := strings.TrimPrefix(reference, "file://")
		if len(key) >= 3 && key[0] == '/' && key[2] == ':' {
			key = key[1:]
		}
		if key == "" || (!filepath.IsAbs(key) && !strings.HasPrefix(key, "/")) {
			return "", "", errors.New("file secret reference must contain an absolute path")
		}
		return "file", filepath.Clean(key), nil
	default:
		return "", "", fmt.Errorf("unsupported secret reference provider")
	}
}
