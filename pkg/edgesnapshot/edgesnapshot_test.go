package edgesnapshot

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testPagePayload struct {
	Authentication []testTokenRecord `json:"authentication"`
	Metadata       map[string]string `json:"metadata"`
}

type testTokenRecord struct {
	TokenFingerprint string `json:"token_fingerprint"`
	Enabled          bool   `json:"enabled"`
}

func TestMarshalPagePayloadUsesDeterministicCanonicalJSON(t *testing.T) {
	payload := testPagePayload{
		Authentication: []testTokenRecord{
			{TokenFingerprint: "fingerprint-1", Enabled: true},
		},
		Metadata: map[string]string{
			"z": "last",
			"a": "first",
		},
	}

	canonical, digest, err := MarshalPagePayload(payload)
	require.NoError(t, err)
	assert.Equal(t, `{"authentication":[{"token_fingerprint":"fingerprint-1","enabled":true}],"metadata":{"a":"first","z":"last"}}`, string(canonical))
	assert.Equal(t, "a5a6532e048caf83bc89ecd7b78f57fed4099efc5aa40c8655c360aeccee9c0c", digest)
	assert.Len(t, digest, 64)
	assert.Equal(t, strings.ToLower(digest), digest)

	repeatedCanonical, repeatedDigest, err := MarshalPagePayload(payload)
	require.NoError(t, err)
	assert.Equal(t, canonical, repeatedCanonical)
	assert.Equal(t, digest, repeatedDigest)

	payload.Authentication[0].Enabled = false
	_, changedDigest, err := MarshalPagePayload(payload)
	require.NoError(t, err)
	assert.NotEqual(t, digest, changedDigest)
}

func TestMarshalPagePayloadRejectsUnsupportedValue(t *testing.T) {
	_, _, err := MarshalPagePayload(make(chan int))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestAggregatePageDigestsBindsZeroBasedOrdinalOrder(t *testing.T) {
	digests := []string{
		strings.Repeat("1", 64),
		strings.Repeat("2", 64),
		strings.Repeat("3", 64),
	}

	aggregate, err := AggregatePageDigests(digests)
	require.NoError(t, err)
	assert.Equal(t, "261fc4b4e7103052f48c2ebff433f2695b7c673b5e662a97a297cccd3912367d", aggregate)

	repeated, err := AggregatePageDigests(digests)
	require.NoError(t, err)
	assert.Equal(t, aggregate, repeated)

	reordered, err := AggregatePageDigests([]string{digests[1], digests[0], digests[2]})
	require.NoError(t, err)
	assert.NotEqual(t, aggregate, reordered)

	empty, err := AggregatePageDigests(nil)
	require.NoError(t, err)
	assert.Len(t, empty, 64)
	assert.NotEqual(t, aggregate, empty)
}

func TestAggregatePageDigestsRejectsNonCanonicalDigest(t *testing.T) {
	for _, digest := range []string{
		strings.Repeat("a", 63),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
	} {
		t.Run(digest[:8], func(t *testing.T) {
			_, err := AggregatePageDigests([]string{strings.Repeat("0", 64), digest})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidDigest)
		})
	}
}

func TestAggregateDatasetManifestsBindsSnapshotAndProtocolOrder(t *testing.T) {
	datasets := []DatasetManifest{
		{SnapshotID: "snapshot-1", Dataset: "authentication", Revision: 7, ItemCount: 3, PageCount: 1, PayloadDigest: strings.Repeat("1", 64)},
		{SnapshotID: "snapshot-1", Dataset: "users", Revision: 7, ItemCount: 2, PageCount: 1, PayloadDigest: strings.Repeat("2", 64)},
		{SnapshotID: "snapshot-1", Dataset: "pricing", Revision: 8, ItemCount: 4, PageCount: 2, PayloadDigest: strings.Repeat("3", 64)},
	}
	digest, err := AggregateDatasetManifests("snapshot-1", 9, datasets)
	require.NoError(t, err)
	assert.Equal(t, "fe40bc655bd06ace41e67e47c04298b633a5a878e56f2c1bd308ab62e70482f1", digest)

	reordered := []DatasetManifest{datasets[1], datasets[0], datasets[2]}
	reorderedDigest, err := AggregateDatasetManifests("snapshot-1", 9, reordered)
	require.NoError(t, err)
	assert.NotEqual(t, digest, reorderedDigest)

	changedRevision, err := AggregateDatasetManifests("snapshot-1", 10, datasets)
	require.NoError(t, err)
	assert.NotEqual(t, digest, changedRevision)

	transplanted := append([]DatasetManifest(nil), datasets...)
	transplanted[0].SnapshotID = "snapshot-2"
	_, err = AggregateDatasetManifests("snapshot-1", 9, transplanted)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = AggregateDatasetManifests("snapshot-1", 9, []DatasetManifest{datasets[0], datasets[0]})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestDatasetManifestCanonicalAndSignatureAreDeterministic(t *testing.T) {
	publicKey, privateKey := testKeyPair()
	manifest := testManifest()

	canonical, err := CanonicalDatasetManifest(manifest)
	require.NoError(t, err)
	assert.Equal(t, `NEWAPI-EDGE-SNAPSHOT-DATASET-MANIFEST-ED25519-V1
snapshot-id:snapshot-test-7
dataset:authentication
revision:7
item-count:42
page-count:3
payload-digest:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, string(canonical))

	signature, err := SignDatasetManifest(privateKey, manifest)
	require.NoError(t, err)
	assert.Equal(t, "hvh+TItmBtrTPHUt2MjsuP+F6doLaRwlrpQz/q71Y/KXewGImRyBbxKsYFmtRdOMnS7i9LEYWKrZS+RciRFrCg==", signature)
	assert.NoError(t, VerifyDatasetManifest(publicKey, manifest, signature))

	repeatedSignature, err := SignDatasetManifest(privateKey, manifest)
	require.NoError(t, err)
	assert.Equal(t, signature, repeatedSignature)
}

func TestVerifyDatasetManifestRejectsEveryFieldTampering(t *testing.T) {
	publicKey, privateKey := testKeyPair()
	manifest := testManifest()
	signature, err := SignDatasetManifest(privateKey, manifest)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*DatasetManifest)
	}{
		{name: "snapshot ID", mutate: func(value *DatasetManifest) { value.SnapshotID = "snapshot-test-8" }},
		{name: "dataset", mutate: func(value *DatasetManifest) { value.Dataset = "users" }},
		{name: "revision", mutate: func(value *DatasetManifest) { value.Revision++ }},
		{name: "item count", mutate: func(value *DatasetManifest) { value.ItemCount++ }},
		{name: "page count", mutate: func(value *DatasetManifest) { value.PageCount++ }},
		{name: "payload digest", mutate: func(value *DatasetManifest) { value.PayloadDigest = strings.Repeat("b", 64) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := manifest
			test.mutate(&changed)
			err := VerifyDatasetManifest(publicKey, changed, signature)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidSignature)
		})
	}
}

func TestDatasetManifestRejectsInvalidDigestAndFields(t *testing.T) {
	base := testManifest()
	tests := []struct {
		name    string
		mutate  func(*DatasetManifest)
		wantErr error
	}{
		{name: "uppercase digest", mutate: func(value *DatasetManifest) { value.PayloadDigest = strings.Repeat("A", 64) }, wantErr: ErrInvalidDigest},
		{name: "short digest", mutate: func(value *DatasetManifest) { value.PayloadDigest = strings.Repeat("a", 63) }, wantErr: ErrInvalidDigest},
		{name: "non hex digest", mutate: func(value *DatasetManifest) { value.PayloadDigest = strings.Repeat("z", 64) }, wantErr: ErrInvalidDigest},
		{name: "empty snapshot ID", mutate: func(value *DatasetManifest) { value.SnapshotID = "" }, wantErr: ErrInvalidInput},
		{name: "control character", mutate: func(value *DatasetManifest) { value.Dataset = "users\nrevision:9" }, wantErr: ErrInvalidInput},
		{name: "zero revision", mutate: func(value *DatasetManifest) { value.Revision = 0 }, wantErr: ErrInvalidInput},
		{name: "negative item count", mutate: func(value *DatasetManifest) { value.ItemCount = -1 }, wantErr: ErrInvalidInput},
		{name: "negative page count", mutate: func(value *DatasetManifest) { value.PageCount = -1 }, wantErr: ErrInvalidInput},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			test.mutate(&manifest)
			_, err := CanonicalDatasetManifest(manifest)
			require.Error(t, err)
			assert.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestDatasetManifestRejectsWrongKeysAndSignatures(t *testing.T) {
	publicKey, privateKey := testKeyPair()
	manifest := testManifest()
	signature, err := SignDatasetManifest(privateKey, manifest)
	require.NoError(t, err)

	_, err = SignDatasetManifest(ed25519.PrivateKey(make([]byte, ed25519.PrivateKeySize-1)), manifest)
	require.Error(t, err)
	assert.ErrorIs(t, err, edgeauth.ErrInvalidPrivateKey)

	err = VerifyDatasetManifest(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize-1)), manifest, signature)
	require.Error(t, err)
	assert.ErrorIs(t, err, edgeauth.ErrInvalidPublicKey)

	wrongPrivateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))
	wrongPublicKey := wrongPrivateKey.Public().(ed25519.PublicKey)
	err = VerifyDatasetManifest(wrongPublicKey, manifest, signature)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSignature)

	for _, invalidSignature := range []string{
		"not-base64",
		signature[:len(signature)-2],
		"-" + signature[1:],
	} {
		err = VerifyDatasetManifest(publicKey, manifest, invalidSignature)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	}

	tamperedSignature := signature
	replacement := byte('A')
	if tamperedSignature[0] == replacement {
		replacement = 'B'
	}
	tamperedSignature = string(replacement) + tamperedSignature[1:]
	err = VerifyDatasetManifest(publicKey, manifest, tamperedSignature)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSignature)
}

func testKeyPair() (ed25519.PublicKey, ed25519.PrivateKey) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	return privateKey.Public().(ed25519.PublicKey), privateKey
}

func testManifest() DatasetManifest {
	return DatasetManifest{
		SnapshotID:    "snapshot-test-7",
		Dataset:       "authentication",
		Revision:      7,
		ItemCount:     42,
		PageCount:     3,
		PayloadDigest: strings.Repeat("a", 64),
	}
}
