// Package edgetoken defines the token normalization and fingerprint contract
// shared by master snapshot compilation and edge-local authentication.
package edgetoken

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	FingerprintAlgorithm = "sha256"
	FingerprintVersion   = 1
	MaxTokenKeyLength    = 128
)

var (
	ErrTokenMissing       = errors.New("token is missing")
	ErrTokenMalformed     = errors.New("token is malformed")
	ErrChannelSuffix      = errors.New("channel selection suffix is not allowed on edge")
	ErrInvalidFingerprint = errors.New("invalid token fingerprint")
)

type ParsedAuthorization struct {
	Key                  string
	ChannelSuffixPresent bool
	ChannelSuffix        string
}

// ParseAuthorization applies the token syntax already exposed by New API:
// an optional Bearer scheme, an optional sk- display prefix and an optional
// dash-separated channel-selection suffix. Edge callers must reject the
// suffix because the local authorization snapshot deliberately has no role.
func ParseAuthorization(value string) (*ParsedAuthorization, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, ErrTokenMissing
	}
	if strings.EqualFold(value, "bearer") {
		return nil, ErrTokenMissing
	}

	if separator := strings.IndexByte(value, ' '); separator >= 0 {
		scheme := value[:separator]
		if !strings.EqualFold(scheme, "bearer") {
			return nil, fmt.Errorf("%w: unsupported authorization scheme", ErrTokenMalformed)
		}
		value = strings.TrimSpace(value[separator+1:])
		if value == "" {
			return nil, ErrTokenMissing
		}
	}

	value = strings.TrimPrefix(value, "sk-")
	parts := strings.Split(value, "-")
	if err := validateStoredKey(parts[0]); err != nil {
		return nil, err
	}

	parsed := &ParsedAuthorization{Key: parts[0]}
	if len(parts) > 1 {
		parsed.ChannelSuffixPresent = true
		parsed.ChannelSuffix = strings.Join(parts[1:], "-")
	}
	return parsed, nil
}

// FingerprintAuthorization returns the canonical lookup key used by an edge.
func FingerprintAuthorization(value string) (string, error) {
	parsed, err := ParseAuthorization(value)
	if err != nil {
		return "", err
	}
	if parsed.ChannelSuffixPresent {
		return "", ErrChannelSuffix
	}
	return FingerprintStoredKey(parsed.Key)
}

// FingerprintStoredKey compiles a master-side Token.Key into a lowercase
// SHA-256 digest. New API token keys are high-entropy random values, so this
// non-keyed index is not a password hashing mechanism and must never be used
// for low-entropy credentials.
func FingerprintStoredKey(key string) (string, error) {
	if err := validateStoredKey(key); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:]), nil
}

func ValidateFingerprint(fingerprint string) error {
	if len(fingerprint) != sha256.Size*2 {
		return ErrInvalidFingerprint
	}
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(fingerprint) != fingerprint {
		return ErrInvalidFingerprint
	}
	return nil
}

func validateStoredKey(key string) error {
	if len(key) == 0 {
		return ErrTokenMissing
	}
	if len(key) > MaxTokenKeyLength {
		return fmt.Errorf("%w: token key exceeds %d bytes", ErrTokenMalformed, MaxTokenKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e || key[i] == '-' {
			return fmt.Errorf("%w: token key contains an unsupported character", ErrTokenMalformed)
		}
	}
	return nil
}
