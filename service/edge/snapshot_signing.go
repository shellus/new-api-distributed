package edge

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
)

const (
	edgeSnapshotSigningKeyIDEnv      = "EDGE_SNAPSHOT_SIGNING_KEY_ID"
	edgeSnapshotSigningPrivateKeyEnv = "EDGE_SNAPSHOT_SIGNING_PRIVATE_KEY"
	edgeSnapshotSigningNotBeforeEnv  = "EDGE_SNAPSHOT_SIGNING_NOT_BEFORE_UNIX"
	edgeSnapshotSigningExpiresAtEnv  = "EDGE_SNAPSHOT_SIGNING_EXPIRES_AT_UNIX"
	defaultSnapshotKeyNotBeforeUnix  = int64(946684800)  // 2000-01-01
	defaultSnapshotKeyExpiresAtUnix  = int64(4102444800) // 2100-01-01
)

type SnapshotSigningKey struct {
	KeyID        string
	PrivateKey   ed25519.PrivateKey
	PublicKey    ed25519.PublicKey
	PublicKeyB64 string
	NotBefore    int64
	ExpiresAt    int64
}

func LoadSnapshotSigningKeyFromEnv(now time.Time) (*SnapshotSigningKey, error) {
	keyID := strings.TrimSpace(os.Getenv(edgeSnapshotSigningKeyIDEnv))
	if err := edgeauth.ValidateKeyID(keyID); err != nil {
		return nil, fmt.Errorf("%s: %w", edgeSnapshotSigningKeyIDEnv, err)
	}
	privateMaterial := strings.TrimSpace(os.Getenv(edgeSnapshotSigningPrivateKeyEnv))
	if privateMaterial == "" {
		return nil, errors.New(edgeSnapshotSigningPrivateKeyEnv + " is required")
	}
	privateKey, err := edgeauth.ParsePrivateKey(privateMaterial)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", edgeSnapshotSigningPrivateKeyEnv, err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	publicMaterial, err := edgeauth.EncodePublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	notBefore, err := parseSnapshotSigningTimestamp(edgeSnapshotSigningNotBeforeEnv, defaultSnapshotKeyNotBeforeUnix)
	if err != nil {
		return nil, err
	}
	expiresAt, err := parseSnapshotSigningTimestamp(edgeSnapshotSigningExpiresAtEnv, defaultSnapshotKeyExpiresAtUnix)
	if err != nil {
		return nil, err
	}
	if expiresAt <= notBefore {
		return nil, errors.New("edge snapshot signing key expiry must be after not-before")
	}
	if now.Unix() < notBefore || now.Unix() >= expiresAt {
		return nil, errors.New("edge snapshot signing key is outside its validity window")
	}

	return &SnapshotSigningKey{
		KeyID:        keyID,
		PrivateKey:   privateKey,
		PublicKey:    publicKey,
		PublicKeyB64: publicMaterial,
		NotBefore:    notBefore,
		ExpiresAt:    expiresAt,
	}, nil
}

func (k *SnapshotSigningKey) VerificationKeyV1() (dto.EdgeSnapshotVerificationKeyV1, error) {
	if k == nil {
		return dto.EdgeSnapshotVerificationKeyV1{}, errors.New("edge snapshot signing key is nil")
	}
	verificationKey := dto.EdgeSnapshotVerificationKeyV1{
		KeyID:              k.KeyID,
		Algorithm:          edgeauth.Algorithm,
		PublicKey:          k.PublicKeyB64,
		NotBeforeUnixMilli: time.Unix(k.NotBefore, 0).UnixMilli(),
		ExpiresAtUnixMilli: time.Unix(k.ExpiresAt, 0).UnixMilli(),
	}
	if err := verificationKey.Validate(); err != nil {
		return dto.EdgeSnapshotVerificationKeyV1{}, err
	}
	return verificationKey, nil
}

func parseSnapshotSigningTimestamp(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Unix timestamp", name)
	}
	return parsed, nil
}
