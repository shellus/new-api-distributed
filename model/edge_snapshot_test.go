package model

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesnapshot"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type edgeCompiledSnapshotTestGraph struct {
	Snapshot *EdgeCompiledSnapshot
	Datasets map[dto.EdgeSnapshotDatasetV1]*EdgeCompiledSnapshotDataset
	Pages    map[dto.EdgeSnapshotDatasetV1]*EdgeCompiledSnapshotPage
}

func TestEdgeCompiledSnapshotPublishRetiresPreviousAtomically(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800000000)
	first := createEdgeCompiledSnapshotTestGraph(t, "snapshot-1", 1, createdAt, createdAt+3600, 0x31)

	published, err := PublishEdgeCompiledSnapshot(first.Snapshot.ID, createdAt+1)
	require.NoError(t, err)
	assert.True(t, published)

	second := createEdgeCompiledSnapshotTestGraph(t, "snapshot-2", 2, createdAt+2, createdAt+3602, 0x32)
	missingPage := *second.Pages[dto.EdgeSnapshotDatasetAuthenticationV1]
	require.NoError(t, DB.Delete(&missingPage).Error)

	published, err = PublishEdgeCompiledSnapshot(second.Snapshot.ID, createdAt+3)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotIncomplete)
	assert.False(t, published)

	var firstAfterFailure EdgeCompiledSnapshot
	require.NoError(t, DB.First(&firstAfterFailure, first.Snapshot.ID).Error)
	assert.Equal(t, EdgeCompiledSnapshotStatusPublished, firstAfterFailure.Status)
	var secondAfterFailure EdgeCompiledSnapshot
	require.NoError(t, DB.First(&secondAfterFailure, second.Snapshot.ID).Error)
	assert.Equal(t, EdgeCompiledSnapshotStatusDraft, secondAfterFailure.Status)

	missingPage.ID = 0
	require.NoError(t, DB.Create(&missingPage).Error)
	published, err = PublishEdgeCompiledSnapshot(second.Snapshot.ID, createdAt+3)
	require.NoError(t, err)
	assert.True(t, published)

	var snapshots []EdgeCompiledSnapshot
	require.NoError(t, DB.Order("revision ASC").Find(&snapshots).Error)
	require.Len(t, snapshots, 2)
	assert.Equal(t, EdgeCompiledSnapshotStatusRetired, snapshots[0].Status)
	assert.Equal(t, createdAt+3, snapshots[0].RetiredAt)
	assert.Equal(t, EdgeCompiledSnapshotStatusPublished, snapshots[1].Status)
	assert.Equal(t, createdAt+3, snapshots[1].PublishedAt)

	var publishedCount int64
	require.NoError(t, DB.Model(&EdgeCompiledSnapshot{}).
		Where("status = ?", EdgeCompiledSnapshotStatusPublished).
		Count(&publishedCount).Error)
	assert.Equal(t, int64(1), publishedCount)
}

func TestEdgeCompiledSnapshotPublishedAndRetiredContentIsImmutable(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800001000)
	graph := createEdgeCompiledSnapshotTestGraph(t, "snapshot-immutable", 10, createdAt, createdAt+3600, 0x41)
	published, err := PublishEdgeCompiledSnapshot(graph.Snapshot.ID, createdAt+1)
	require.NoError(t, err)
	assert.True(t, published)

	var snapshot EdgeCompiledSnapshot
	require.NoError(t, DB.First(&snapshot, graph.Snapshot.ID).Error)
	snapshot.Digest = strings.Repeat("b", 64)
	err = DB.Save(&snapshot).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotImmutable)

	var dataset EdgeCompiledSnapshotDataset
	require.NoError(t, DB.First(&dataset, graph.Datasets[dto.EdgeSnapshotDatasetUsersV1].ID).Error)
	dataset.ItemCount++
	err = DB.Save(&dataset).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotImmutable)

	var page EdgeCompiledSnapshotPage
	require.NoError(t, DB.First(&page, graph.Pages[dto.EdgeSnapshotDatasetModelsV1].ID).Error)
	page.Payload = `{}`
	err = DB.Save(&page).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotImmutable)
	err = DB.Delete(&page).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotImmutable)

	retired, err := RetireEdgeCompiledSnapshot(graph.Snapshot.ID, createdAt+2)
	require.NoError(t, err)
	assert.True(t, retired)
	require.NoError(t, DB.First(&snapshot, graph.Snapshot.ID).Error)
	snapshot.SigningKeyID = "different-key"
	err = DB.Save(&snapshot).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotImmutable)
}

func TestEdgeCompiledSnapshotPersistenceEnforcesStableUniqueIdentities(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800001500)
	graph := createEdgeCompiledSnapshotTestGraph(t, "snapshot-unique", 15, createdAt, createdAt+3600, 0x49)

	duplicateUID := *graph.Snapshot
	duplicateUID.ID = 0
	err := DB.Create(&duplicateUID).Error
	require.Error(t, err)

	duplicateRevision := *graph.Snapshot
	duplicateRevision.ID = 0
	duplicateRevision.SnapshotUID = "snapshot-unique-other"
	err = DB.Create(&duplicateRevision).Error
	require.Error(t, err)

	duplicateDataset := *graph.Datasets[dto.EdgeSnapshotDatasetGroupsV1]
	duplicateDataset.ID = 0
	err = DB.Create(&duplicateDataset).Error
	require.Error(t, err)

	duplicatePage := *graph.Pages[dto.EdgeSnapshotDatasetGroupsV1]
	duplicatePage.ID = 0
	err = DB.Create(&duplicatePage).Error
	require.Error(t, err)
}

func TestEdgeCompiledSnapshotManifestUsesProtocolOrderAndPublicSigningKey(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800002000)
	graph := createEdgeCompiledSnapshotTestGraph(t, "snapshot-manifest", 20, createdAt, createdAt+3600, 0x51)
	published, err := PublishEdgeCompiledSnapshot(graph.Snapshot.ID, createdAt+1)
	require.NoError(t, err)
	assert.True(t, published)

	bundle, err := GetLatestPublishedEdgeCompiledSnapshotManifest(createdAt + 2)
	require.NoError(t, err)
	assert.Equal(t, graph.Snapshot.SnapshotUID, bundle.Manifest.SnapshotID)
	assert.Equal(t, graph.Snapshot.Revision, bundle.Manifest.Revision)
	assert.Equal(t, graph.Snapshot.CreatedAt*1000, bundle.Manifest.CreatedAtUnixMilli)
	assert.Equal(t, graph.Snapshot.ExpiresAt*1000, bundle.Manifest.ExpiresAtUnixMilli)
	assert.Equal(t, graph.Snapshot.SigningKeyID, bundle.VerificationKey.KeyID)
	assert.Equal(t, graph.Snapshot.SigningPublicKey, bundle.VerificationKey.PublicKey)
	assert.Equal(t, graph.Snapshot.SigningKeyNotBefore*1000, bundle.VerificationKey.NotBeforeUnixMilli)
	assert.Equal(t, graph.Snapshot.SigningKeyExpiresAt*1000, bundle.VerificationKey.ExpiresAtUnixMilli)
	assert.NoError(t, bundle.VerificationKey.Validate())
	require.Len(t, bundle.Manifest.Datasets, len(edgeCompiledSnapshotDatasetOrder))

	publicKey, err := edgeauth.ParsePublicKey(bundle.VerificationKey.PublicKey)
	require.NoError(t, err)
	for i, expectedDataset := range edgeCompiledSnapshotDatasetOrder {
		actual := bundle.Manifest.Datasets[i]
		assert.Equal(t, expectedDataset, actual.Dataset)
		assert.Equal(t, graph.Snapshot.SigningKeyID, actual.DetachedSignature.KeyID)
		assert.Equal(t, actual.Digest, actual.DetachedSignature.PayloadDigest)
		assert.NoError(t, edgesnapshot.VerifyDatasetManifest(publicKey, edgesnapshot.DatasetManifest{
			SnapshotID:    bundle.Manifest.SnapshotID,
			Dataset:       string(actual.Dataset),
			Revision:      actual.Revision,
			ItemCount:     actual.ItemCount,
			PageCount:     actual.PageCount,
			PayloadDigest: actual.Digest,
		}, actual.DetachedSignature.Value))
	}
	assert.Equal(t, edgetoken.FingerprintAlgorithm, bundle.Manifest.TokenFingerprint.Algorithm)
	assert.Equal(t, edgetoken.FingerprintVersion, bundle.Manifest.TokenFingerprint.Version)
	assert.Empty(t, bundle.Manifest.TokenFingerprint.KeyID)
}

func TestEdgeCompiledSnapshotPageLookupKeepsRetiredSnapshotReadableUntilExpiry(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800003000)
	graph := createEdgeCompiledSnapshotTestGraph(t, "snapshot-page", 30, createdAt, createdAt+100, 0x61)

	_, err := GetPublishedEdgeCompiledSnapshotPage(
		graph.Snapshot.SnapshotUID,
		dto.EdgeSnapshotDatasetChannelsV1,
		0,
		createdAt+1,
	)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	published, err := PublishEdgeCompiledSnapshot(graph.Snapshot.ID, createdAt+1)
	require.NoError(t, err)
	assert.True(t, published)
	page, err := GetPublishedEdgeCompiledSnapshotPage(
		graph.Snapshot.SnapshotUID,
		dto.EdgeSnapshotDatasetChannelsV1,
		0,
		createdAt+2,
	)
	require.NoError(t, err)
	expected := graph.Pages[dto.EdgeSnapshotDatasetChannelsV1]
	assert.Equal(t, expected.Payload, page.Payload)
	assert.Equal(t, expected.Digest, page.Digest)
	assert.Equal(t, expected.ItemCount, page.ItemCount)

	_, err = GetPublishedEdgeCompiledSnapshotPage(
		graph.Snapshot.SnapshotUID,
		dto.EdgeSnapshotDatasetChannelsV1,
		1,
		createdAt+2,
	)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = GetPublishedEdgeCompiledSnapshotPage(
		graph.Snapshot.SnapshotUID,
		dto.EdgeSnapshotDatasetChannelsV1,
		0,
		graph.Snapshot.ExpiresAt,
	)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = GetLatestPublishedEdgeCompiledSnapshotManifest(graph.Snapshot.ExpiresAt)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	retired, err := RetireEdgeCompiledSnapshot(graph.Snapshot.ID, createdAt+3)
	require.NoError(t, err)
	assert.True(t, retired)
	retiredManifest, err := GetEdgeCompiledSnapshotManifest(graph.Snapshot.SnapshotUID, createdAt+4)
	require.NoError(t, err)
	assert.Equal(t, graph.Snapshot.SnapshotUID, retiredManifest.Manifest.SnapshotID)
	retiredPage, err := GetPublishedEdgeCompiledSnapshotPage(
		graph.Snapshot.SnapshotUID,
		dto.EdgeSnapshotDatasetChannelsV1,
		0,
		createdAt+4,
	)
	require.NoError(t, err)
	assert.Equal(t, expected.Digest, retiredPage.Digest)

	_, err = GetEdgeCompiledSnapshotManifest(graph.Snapshot.SnapshotUID, graph.Snapshot.ExpiresAt)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestEdgeCompiledSnapshotRejectsInvalidBoundsEnumsAndSignatures(t *testing.T) {
	truncateTables(t)
	createdAt := int64(1800004000)
	graph := createEdgeCompiledSnapshotTestGraph(t, "snapshot-expired", 40, createdAt, createdAt+10, 0x71)
	published, err := PublishEdgeCompiledSnapshot(graph.Snapshot.ID, graph.Snapshot.ExpiresAt)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotExpired)
	assert.False(t, published)

	digestMismatch := createEdgeCompiledSnapshotTestGraph(t, "snapshot-digest-mismatch", 42, createdAt+20, createdAt+200, 0x73)
	digestMismatch.Snapshot.Digest = strings.Repeat("f", 64)
	require.NoError(t, DB.Save(digestMismatch.Snapshot).Error)
	published, err = PublishEdgeCompiledSnapshot(digestMismatch.Snapshot.ID, createdAt+21)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotDigestMismatch)
	assert.False(t, published)

	publicKey, _ := edgeCompiledSnapshotTestKeyPair(t, 0x72)
	invalidKeyRange := EdgeCompiledSnapshot{
		SnapshotUID:               "snapshot-invalid-key-range",
		Revision:                  41,
		ProtocolVersion:           dto.EdgeControlProtocolVersionV1,
		HashAlgorithm:             edgesnapshot.HashAlgorithm,
		Digest:                    strings.Repeat("a", 64),
		TokenFingerprintAlgorithm: edgetoken.FingerprintAlgorithm,
		TokenFingerprintVersion:   edgetoken.FingerprintVersion,
		SigningAlgorithm:          edgesnapshot.SignatureAlgorithm,
		SigningKeyID:              "snapshot-key-41",
		SigningPublicKey:          publicKey,
		SigningKeyNotBefore:       createdAt,
		SigningKeyExpiresAt:       createdAt + 5,
		CreatedAt:                 createdAt,
		ExpiresAt:                 createdAt + 10,
		UpdatedAt:                 createdAt,
	}
	err = DB.Create(&invalidKeyRange).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "covered by the signing key")

	invalidDataset := *graph.Datasets[dto.EdgeSnapshotDatasetAuthenticationV1]
	invalidDataset.ID = 0
	invalidDataset.Dataset = dto.EdgeSnapshotDatasetV1("unknown")
	invalidDataset.SnapshotID = graph.Snapshot.ID
	err = DB.Create(&invalidDataset).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid edge snapshot dataset")

	invalidSignature := *graph.Datasets[dto.EdgeSnapshotDatasetAuthenticationV1]
	invalidSignature.ID = 0
	invalidSignature.Dataset = dto.EdgeSnapshotDatasetUsersV1
	invalidSignature.Signature = strings.Repeat("A", 88)
	err = DB.Create(&invalidSignature).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signature")

	canonical, digest, err := edgesnapshot.MarshalPagePayload(map[string]any{"items": []int{1}})
	require.NoError(t, err)
	invalidPage := EdgeCompiledSnapshotPage{
		DatasetID: graph.Datasets[dto.EdgeSnapshotDatasetAuthenticationV1].ID,
		Ordinal:   2,
		ItemCount: 1,
		Digest:    digest,
		Payload:   " " + string(canonical),
	}
	err = DB.Create(&invalidPage).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotDigestMismatch)

	unknownPayload, unknownDigest, err := edgesnapshot.MarshalPagePayload(map[string]any{
		"authentication": []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("2", 64),
			TokenID:          2,
			UserID:           2,
			Enabled:          true,
		}},
		"unexpected": true,
	})
	require.NoError(t, err)
	unknownFieldPage := EdgeCompiledSnapshotPage{
		DatasetID: graph.Datasets[dto.EdgeSnapshotDatasetAuthenticationV1].ID,
		Ordinal:   2,
		ItemCount: 1,
		Digest:    unknownDigest,
		Payload:   string(unknownPayload),
	}
	err = DB.Create(&unknownFieldPage).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeCompiledSnapshotDigestMismatch)
}

func createEdgeCompiledSnapshotTestGraph(
	t *testing.T,
	snapshotUID string,
	revision int64,
	createdAt int64,
	expiresAt int64,
	seed byte,
) *edgeCompiledSnapshotTestGraph {
	t.Helper()
	publicKey, privateKey := edgeCompiledSnapshotTestKeyPair(t, seed)
	signingKeyID := "snapshot-key-" + strings.TrimPrefix(snapshotUID, "snapshot-")

	type datasetBuild struct {
		dataset    dto.EdgeSnapshotDatasetV1
		payload    string
		pageDigest string
		digest     string
		signature  string
	}
	built := make([]datasetBuild, 0, len(edgeCompiledSnapshotDatasetOrder))
	datasetManifests := make([]edgesnapshot.DatasetManifest, 0, len(edgeCompiledSnapshotDatasetOrder))
	for _, dataset := range edgeCompiledSnapshotDatasetOrder {
		payload, pageDigest, err := edgesnapshot.MarshalPagePayload(edgeCompiledSnapshotTestPayload(dataset))
		require.NoError(t, err)
		digest, err := edgesnapshot.AggregatePageDigests([]string{pageDigest})
		require.NoError(t, err)
		manifest := edgesnapshot.DatasetManifest{
			SnapshotID:    snapshotUID,
			Dataset:       string(dataset),
			Revision:      revision,
			ItemCount:     1,
			PageCount:     1,
			PayloadDigest: digest,
		}
		signature, err := edgesnapshot.SignDatasetManifest(privateKey, manifest)
		require.NoError(t, err)
		built = append(built, datasetBuild{
			dataset:    dataset,
			payload:    string(payload),
			pageDigest: pageDigest,
			digest:     digest,
			signature:  signature,
		})
		datasetManifests = append(datasetManifests, manifest)
	}
	snapshotDigest, err := edgesnapshot.AggregateDatasetManifests(snapshotUID, revision, datasetManifests)
	require.NoError(t, err)
	snapshot := &EdgeCompiledSnapshot{
		SnapshotUID:               snapshotUID,
		Revision:                  revision,
		ProtocolVersion:           dto.EdgeControlProtocolVersionV1,
		HashAlgorithm:             edgesnapshot.HashAlgorithm,
		Digest:                    snapshotDigest,
		TokenFingerprintAlgorithm: edgetoken.FingerprintAlgorithm,
		TokenFingerprintVersion:   edgetoken.FingerprintVersion,
		SigningAlgorithm:          edgesnapshot.SignatureAlgorithm,
		SigningKeyID:              signingKeyID,
		SigningPublicKey:          publicKey,
		SigningKeyNotBefore:       createdAt - 60,
		SigningKeyExpiresAt:       expiresAt + 60,
		CreatedAt:                 createdAt,
		ExpiresAt:                 expiresAt,
		UpdatedAt:                 createdAt,
	}
	require.NoError(t, DB.Create(snapshot).Error)

	graph := &edgeCompiledSnapshotTestGraph{
		Snapshot: snapshot,
		Datasets: make(map[dto.EdgeSnapshotDatasetV1]*EdgeCompiledSnapshotDataset, len(built)),
		Pages:    make(map[dto.EdgeSnapshotDatasetV1]*EdgeCompiledSnapshotPage, len(built)),
	}
	for i := range built {
		dataset := &EdgeCompiledSnapshotDataset{
			SnapshotID:   snapshot.ID,
			Dataset:      built[i].dataset,
			Revision:     revision,
			ItemCount:    1,
			PageCount:    1,
			Digest:       built[i].digest,
			Signature:    built[i].signature,
			SigningKeyID: signingKeyID,
		}
		require.NoError(t, DB.Create(dataset).Error)
		page := &EdgeCompiledSnapshotPage{
			DatasetID: dataset.ID,
			Ordinal:   0,
			ItemCount: 1,
			Digest:    built[i].pageDigest,
			Payload:   built[i].payload,
		}
		require.NoError(t, DB.Create(page).Error)
		graph.Datasets[built[i].dataset] = dataset
		graph.Pages[built[i].dataset] = page
	}
	return graph
}

func edgeCompiledSnapshotTestKeyPair(t *testing.T, seed byte) (string, ed25519.PrivateKey) {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	publicKey, err := edgeauth.EncodePublicKey(privateKey.Public().(ed25519.PublicKey))
	require.NoError(t, err)
	return publicKey, privateKey
}

func edgeCompiledSnapshotTestPayload(dataset dto.EdgeSnapshotDatasetV1) dto.EdgeSnapshotPagePayloadV1 {
	payload := dto.EdgeSnapshotPagePayloadV1{}
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		payload.Authentication = []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("1", 64),
			TokenID:          1,
			UserID:           1,
			Enabled:          true,
		}}
	case dto.EdgeSnapshotDatasetUsersV1:
		payload.Users = []dto.EdgeUserPolicyV1{{
			UserID:       1,
			Enabled:      true,
			Username:     "edge-user",
			DefaultGroup: "default",
			Setting:      dto.EdgeUserSettingV1{BillingPreference: "subscription_first"},
		}}
	case dto.EdgeSnapshotDatasetGroupsV1:
		payload.Groups = []dto.EdgeGroupPolicyV1{{
			UserGroup: "default",
			UsingGroups: []dto.EdgeUsingGroupPolicyV1{{
				Group:   "default",
				Enabled: true,
				Ratio:   1,
			}},
		}}
	case dto.EdgeSnapshotDatasetModelsV1:
		payload.Models = []dto.EdgeModelPolicyV1{{
			Model:      "gpt-test",
			Enabled:    true,
			Endpoints:  []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1},
			Streaming:  true,
			ChannelIDs: []int64{1},
		}}
	case dto.EdgeSnapshotDatasetChannelsV1:
		payload.Channels = []dto.EdgeChannelProjectionV1{{
			ChannelID:    1,
			Type:         1,
			Name:         "local-cpa",
			Enabled:      true,
			Groups:       []string{"default"},
			Models:       []string{"gpt-test"},
			Priority:     1,
			Weight:       1,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}}
	case dto.EdgeSnapshotDatasetPricingV1:
		ratio := 1.0
		payload.Pricing = []dto.EdgePricingPolicyV1{{
			PolicyID:     "price-gpt-test",
			Version:      "v1",
			Model:        "gpt-test",
			BillingMode:  dto.EdgeBillingModeRatioV1,
			ModelRatio:   &ratio,
			QuotaPerUnit: 500000,
		}}
	case dto.EdgeSnapshotDatasetRoutingV1:
		payload.Routing = []dto.EdgeRoutingPolicyV1{{
			ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{
				Enabled:           false,
				MaxEntries:        1024,
				DefaultTTLSeconds: 300,
			},
		}}
	}
	return payload
}
