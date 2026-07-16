package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSnapshotSigningKeyFromEnv(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privateMaterial, err := edgeauth.EncodePrivateKey(privateKey)
	require.NoError(t, err)
	t.Setenv(edgeSnapshotSigningKeyIDEnv, "snapshot-key-1")
	t.Setenv(edgeSnapshotSigningPrivateKeyEnv, privateMaterial)
	t.Setenv(edgeSnapshotSigningNotBeforeEnv, "1784150000")
	t.Setenv(edgeSnapshotSigningExpiresAtEnv, "1784250000")

	key, err := LoadSnapshotSigningKeyFromEnv(time.Unix(1_784_160_000, 0))
	require.NoError(t, err)
	assert.Equal(t, "snapshot-key-1", key.KeyID)
	assert.Equal(t, privateKey, key.PrivateKey)
	assert.Equal(t, privateKey.Public(), key.PublicKey)

	verificationKey, err := key.VerificationKeyV1()
	require.NoError(t, err)
	assert.Equal(t, key.PublicKeyB64, verificationKey.PublicKey)
	assert.Equal(t, int64(1_784_150_000_000), verificationKey.NotBeforeUnixMilli)
	assert.Equal(t, int64(1_784_250_000_000), verificationKey.ExpiresAtUnixMilli)
}

func TestLoadSnapshotSigningKeyFromEnvRejectsMissingInvalidAndExpiredConfiguration(t *testing.T) {
	now := time.Unix(1_784_160_000, 0)
	t.Setenv(edgeSnapshotSigningKeyIDEnv, "snapshot-key-1")
	t.Setenv(edgeSnapshotSigningPrivateKeyEnv, "")
	_, err := LoadSnapshotSigningKeyFromEnv(now)
	require.Error(t, err)

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privateMaterial, err := edgeauth.EncodePrivateKey(privateKey)
	require.NoError(t, err)
	t.Setenv(edgeSnapshotSigningPrivateKeyEnv, privateMaterial)
	t.Setenv(edgeSnapshotSigningNotBeforeEnv, "1784150000")
	t.Setenv(edgeSnapshotSigningExpiresAtEnv, "1784159999")
	_, err = LoadSnapshotSigningKeyFromEnv(now)
	require.Error(t, err)

	t.Setenv(edgeSnapshotSigningNotBeforeEnv, "not-a-timestamp")
	t.Setenv(edgeSnapshotSigningExpiresAtEnv, "1784250000")
	_, err = LoadSnapshotSigningKeyFromEnv(now)
	require.Error(t, err)
}
