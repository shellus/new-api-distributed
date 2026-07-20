package edge

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesnapshot"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessBootstrapPersistsDeclarationAndReplaysExactResponse(t *testing.T) {
	db, principal := newControlMutationFixture(t)
	now := time.Now().Truncate(time.Second)
	require.NoError(t, db.Model(&model.EdgeNode{}).Where("id = ?", principal.NodeID).Update("protocol_version", dto.EdgeControlProtocolVersionV1).Error)
	bundle := createPublishedControlSnapshot(t, now)
	request := controlBootstrapRequest(now, principal.SignedRequest.Metadata.IdempotencyKey)

	response, err := ProcessBootstrap(principal, request, "server-bootstrap-1", now)
	require.NoError(t, err)
	assert.Equal(t, 200, response.StatusCode)
	var body dto.EdgeBootstrapResponseV1
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Equal(t, bundle.Manifest.SnapshotID, body.Snapshot.SnapshotID)
	assert.Equal(t, principal.NodeUID, body.Control.NodeID)
	assert.True(t, body.Control.Enabled)

	var node model.EdgeNode
	require.NoError(t, db.First(&node, principal.NodeID).Error)
	assert.Equal(t, request.Declaration.PublicURL, node.DeclaredPublicURL)
	assert.Equal(t, request.Declaration.SoftwareVersion, node.SoftwareVersion)
	assert.Equal(t, dto.EdgeControlProtocolVersionV2, node.ProtocolVersion)
	assert.Equal(t, now.Unix(), node.LastSeenAt)

	retry := cloneControlPrincipalWithNonce(principal, "YWJjZGVmMDEyMzQ1Njc4OQ")
	replayed, err := ProcessBootstrap(retry, request, "server-bootstrap-2", now.Add(time.Second))
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, response.Body, replayed.Body)
}

func TestProcessBootstrapPersistsCorrelationRejection(t *testing.T) {
	_, principal := newControlMutationFixture(t)
	now := time.Now().Truncate(time.Second)
	request := controlBootstrapRequest(now, "different-request")

	response, err := ProcessBootstrap(principal, request, "server-bootstrap-invalid", now)
	require.NoError(t, err)
	assert.Equal(t, 400, response.StatusCode)
	var body dto.EdgeControlErrorResponseV1
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Equal(t, dto.EdgeControlErrorCodeInvalidRequestV1, body.Error.Code)

	retry := cloneControlPrincipalWithNonce(principal, "YWJjZGVmOTg3NjU0MzIxMA")
	replayed, err := ProcessBootstrap(retry, request, "server-bootstrap-invalid-2", now.Add(time.Second))
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, response.Body, replayed.Body)
}

func TestProcessBootstrapRejectsV1OnlyEdgeWithExpectedV2(t *testing.T) {
	_, principal := newControlMutationFixture(t)
	now := time.Now().Truncate(time.Second)
	request := controlBootstrapRequest(now, principal.SignedRequest.Metadata.IdempotencyKey)
	request.Meta.ProtocolVersion = dto.EdgeControlProtocolVersionV1
	request.SupportedProtocolVersions = []string{dto.EdgeControlProtocolVersionV1}

	response, err := ProcessBootstrap(principal, request, "server-bootstrap-v1", now)
	require.NoError(t, err)
	assert.Equal(t, 426, response.StatusCode)
	var body dto.EdgeControlErrorResponseV1
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Equal(t, dto.EdgeControlErrorCodeUnsupportedProtocolV1, body.Error.Code)
	require.NotNil(t, body.Error.Expected)
	assert.Equal(t, []string{dto.EdgeControlProtocolVersionV2}, body.Error.Expected.ProtocolVersions)
}

func TestProcessHeartbeatStoresTypedObservationAndOmitsUnchangedSnapshot(t *testing.T) {
	db, principal := newControlMutationFixture(t)
	now := time.Now().Truncate(time.Second)
	bundle := createPublishedControlSnapshot(t, now)
	principal = cloneControlPrincipalForRequest(principal, "heartbeat-1", "YWJjZGVmMDEyMzQ1Njc4OA")

	datasetStates := make([]dto.EdgeSnapshotDatasetStateV1, 0, len(bundle.Manifest.Datasets))
	for _, dataset := range bundle.Manifest.Datasets {
		datasetStates = append(datasetStates, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset.Dataset, Revision: dataset.Revision})
	}
	request := dto.EdgeHeartbeatRequestV1{
		Meta:        dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "heartbeat-1"},
		Declaration: controlDeclaration(now),
		Snapshot: dto.EdgeSnapshotStateV1{
			SnapshotID:         bundle.Manifest.SnapshotID,
			Revision:           bundle.Manifest.Revision,
			AppliedAtUnixMilli: now.Add(-time.Second).UnixMilli(),
			Datasets:           datasetStates,
		},
		Settlement: dto.EdgeSettlementStateV1{NextEventSequence: 1},
		Runtime:    dto.EdgeRuntimeStatusV1{UptimeSeconds: 60, InFlightRequests: 2, RecentRequestCount: 4},
		CPA: []dto.EdgeCPAStatusV1{{
			LocalService:        dto.EdgeLocalServiceCPAPro20x4V1,
			Healthy:             true,
			LatencyMilliseconds: 12,
			AvailableModels:     []string{"gpt-test"},
			CheckedAtUnixMilli:  now.UnixMilli(),
		}},
	}

	response, err := ProcessHeartbeat(principal, request, "server-heartbeat-1", now)
	require.NoError(t, err)
	assert.Equal(t, 200, response.StatusCode)
	var body dto.EdgeHeartbeatResponseV1
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Nil(t, body.Snapshot)
	require.NotNil(t, body.BalanceDelta)
	assert.True(t, body.BalanceDelta.Full)
	assert.Equal(t, int64(1), body.BalanceDelta.Revision)

	var heartbeat model.EdgeNodeHeartbeat
	require.NoError(t, db.Where("node_id = ?", principal.NodeID).First(&heartbeat).Error)
	assert.Equal(t, bundle.Manifest.SnapshotID, heartbeat.SnapshotUID)
	assert.Equal(t, int64(2), heartbeat.InFlightRequests)
	assert.Zero(t, heartbeat.BalanceRevision)
	assert.NotContains(t, heartbeat.CPAPayload, "http://")

	principal = cloneControlPrincipalForRequest(principal, "heartbeat-2", "YWJjZGVmMDEyMzQ1Njc4Ng")
	request.Meta.RequestID = "heartbeat-2"
	request.BalanceRevision = body.BalanceDelta.Revision
	response, err = ProcessHeartbeat(principal, request, "server-heartbeat-2", now.Add(time.Second))
	require.NoError(t, err)
	body = dto.EdgeHeartbeatResponseV1{}
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Nil(t, body.BalanceDelta)
}

func TestProcessSnapshotPageReturnsCanonicalPageAndStableCursor(t *testing.T) {
	_, principal := newControlMutationFixture(t)
	now := time.Now().Truncate(time.Second)
	bundle := createPublishedControlSnapshot(t, now)
	principal = cloneControlPrincipalForRequest(principal, "page-1", "YWJjZGVmMDEyMzQ1Njc4Nw")
	request := dto.EdgeSnapshotPageRequestV1{
		Meta:       dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "page-1"},
		SnapshotID: bundle.Manifest.SnapshotID,
		Dataset:    dto.EdgeSnapshotDatasetAuthenticationV1,
		Limit:      500,
	}

	response, err := ProcessSnapshotPage(principal, request, "server-page-1", now)
	require.NoError(t, err)
	assert.Equal(t, 200, response.StatusCode)
	var body dto.EdgeSnapshotPageResponseV1
	require.NoError(t, common.Unmarshal(response.Body, &body))
	assert.Equal(t, request.SnapshotID, body.SnapshotID)
	assert.Len(t, body.Payload.Authentication, 1)
	assert.Empty(t, body.NextCursor)
	assert.NoError(t, body.Validate())
}

func createPublishedControlSnapshot(t *testing.T, now time.Time) *model.EdgeCompiledSnapshotManifest {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x45}, ed25519.SeedSize))
	publicKey, err := edgeauth.EncodePublicKey(privateKey.Public().(ed25519.PublicKey))
	require.NoError(t, err)
	snapshotUID := "snapshot-control-1"
	revision := int64(1)
	datasets := []dto.EdgeSnapshotDatasetV1{
		dto.EdgeSnapshotDatasetAuthenticationV1,
		dto.EdgeSnapshotDatasetUsersV1,
		dto.EdgeSnapshotDatasetGroupsV1,
		dto.EdgeSnapshotDatasetModelsV1,
		dto.EdgeSnapshotDatasetChannelsV1,
		dto.EdgeSnapshotDatasetPricingV1,
		dto.EdgeSnapshotDatasetRoutingV1,
	}
	type builtDataset struct {
		dataset       dto.EdgeSnapshotDatasetV1
		payload       string
		pageDigest    string
		datasetDigest string
		signature     string
	}
	built := make([]builtDataset, 0, len(datasets))
	manifests := make([]edgesnapshot.DatasetManifest, 0, len(datasets))
	for _, dataset := range datasets {
		canonical, pageDigest, err := edgesnapshot.MarshalPagePayload(controlSnapshotPayload(dataset))
		require.NoError(t, err)
		datasetDigest, err := edgesnapshot.AggregatePageDigests([]string{pageDigest})
		require.NoError(t, err)
		manifest := edgesnapshot.DatasetManifest{
			SnapshotID:    snapshotUID,
			Dataset:       string(dataset),
			Revision:      revision,
			ItemCount:     1,
			PageCount:     1,
			PayloadDigest: datasetDigest,
		}
		signature, err := edgesnapshot.SignDatasetManifest(privateKey, manifest)
		require.NoError(t, err)
		built = append(built, builtDataset{dataset: dataset, payload: string(canonical), pageDigest: pageDigest, datasetDigest: datasetDigest, signature: signature})
		manifests = append(manifests, manifest)
	}
	topDigest, err := edgesnapshot.AggregateDatasetManifests(snapshotUID, revision, manifests)
	require.NoError(t, err)
	createdAt := now.Add(-5 * time.Second).Unix()
	snapshot := &model.EdgeCompiledSnapshot{
		SnapshotUID:               snapshotUID,
		Revision:                  revision,
		ProtocolVersion:           dto.EdgeControlProtocolVersionV1,
		HashAlgorithm:             edgesnapshot.HashAlgorithm,
		Digest:                    topDigest,
		TokenFingerprintAlgorithm: edgetoken.FingerprintAlgorithm,
		TokenFingerprintVersion:   edgetoken.FingerprintVersion,
		SigningAlgorithm:          edgesnapshot.SignatureAlgorithm,
		SigningKeyID:              "snapshot-key-control-1",
		SigningPublicKey:          publicKey,
		SigningKeyNotBefore:       createdAt - 60,
		SigningKeyExpiresAt:       now.Add(2 * time.Hour).Unix(),
		CreatedAt:                 createdAt,
		ExpiresAt:                 now.Add(time.Hour).Unix(),
		UpdatedAt:                 createdAt,
	}
	require.NoError(t, model.DB.Create(snapshot).Error)
	for _, item := range built {
		dataset := &model.EdgeCompiledSnapshotDataset{
			SnapshotID:   snapshot.ID,
			Dataset:      item.dataset,
			Revision:     revision,
			ItemCount:    1,
			PageCount:    1,
			Digest:       item.datasetDigest,
			Signature:    item.signature,
			SigningKeyID: snapshot.SigningKeyID,
		}
		require.NoError(t, model.DB.Create(dataset).Error)
		require.NoError(t, model.DB.Create(&model.EdgeCompiledSnapshotPage{
			DatasetID: dataset.ID,
			Ordinal:   0,
			ItemCount: 1,
			Digest:    item.pageDigest,
			Payload:   item.payload,
		}).Error)
	}
	published, err := model.PublishEdgeCompiledSnapshot(snapshot.ID, now.Add(-4*time.Second).Unix())
	require.NoError(t, err)
	assert.True(t, published)
	bundle, err := model.GetLatestPublishedEdgeCompiledSnapshotManifest(now.Unix())
	require.NoError(t, err)
	return bundle
}

func controlBootstrapRequest(now time.Time, requestID string) dto.EdgeBootstrapRequestV1 {
	return dto.EdgeBootstrapRequestV1{
		Meta:                      dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: requestID},
		SupportedProtocolVersions: []string{dto.EdgeControlProtocolVersionV2, dto.EdgeControlProtocolVersionV1},
		Declaration:               controlDeclaration(now),
		Settlement:                dto.EdgeSettlementStateV1{NextEventSequence: 1},
	}
}

func controlDeclaration(now time.Time) dto.EdgeNodeDeclarationV1 {
	return dto.EdgeNodeDeclarationV1{
		Name:               "Control Edge",
		Region:             "test-region",
		PublicURL:          "https://edge.example.com",
		SoftwareVersion:    "test-version",
		StartedAtUnixMilli: now.Add(-time.Minute).UnixMilli(),
		Capabilities: []dto.EdgeEndpointCapabilityV1{
			{Endpoint: dto.EdgeEndpointDataPlaneV1, Streaming: true},
		},
	}
}

func cloneControlPrincipalWithNonce(principal *ControlPrincipal, nonce string) *ControlPrincipal {
	cloned := *principal
	signed := *principal.SignedRequest
	signed.Metadata = principal.SignedRequest.Metadata
	signed.Metadata.Nonce = nonce
	cloned.SignedRequest = &signed
	cloned.NonceHash = edgeauth.BodySHA256([]byte(nonce))
	return &cloned
}

func cloneControlPrincipalForRequest(principal *ControlPrincipal, requestID string, nonce string) *ControlPrincipal {
	cloned := cloneControlPrincipalWithNonce(principal, nonce)
	cloned.SignedRequest.Metadata.IdempotencyKey = requestID
	return cloned
}

func controlSnapshotPayload(dataset dto.EdgeSnapshotDatasetV1) dto.EdgeSnapshotPagePayloadV1 {
	payload := dto.EdgeSnapshotPagePayloadV1{}
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		payload.Authentication = []dto.EdgeTokenAuthRecordV1{{TokenFingerprint: strings.Repeat("1", 64), TokenID: 1, UserID: 1, Enabled: true}}
	case dto.EdgeSnapshotDatasetUsersV1:
		payload.Users = []dto.EdgeUserPolicyV1{{UserID: 1, Enabled: true, Username: "edge-user", DefaultGroup: "default", Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first"}}}
	case dto.EdgeSnapshotDatasetGroupsV1:
		payload.Groups = []dto.EdgeGroupPolicyV1{{UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}}}}
	case dto.EdgeSnapshotDatasetModelsV1:
		payload.Models = []dto.EdgeModelPolicyV1{{Model: "gpt-test", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1}, Streaming: true, ChannelIDs: []int64{1}}}
	case dto.EdgeSnapshotDatasetChannelsV1:
		payload.Channels = []dto.EdgeChannelProjectionV1{{ChannelID: 1, Type: 1, Name: "local-cpa", Enabled: true, Groups: []string{"default"}, Models: []string{"gpt-test"}, Priority: 1, Weight: 1, LocalService: dto.EdgeLocalServiceCPAPro20x4V1}}
	case dto.EdgeSnapshotDatasetPricingV1:
		ratio := 1.0
		payload.Pricing = []dto.EdgePricingPolicyV1{{PolicyID: "price-gpt-test", Version: "v1", Model: "gpt-test", BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &ratio, QuotaPerUnit: 500000}}
	case dto.EdgeSnapshotDatasetRoutingV1:
		payload.Routing = []dto.EdgeRoutingPolicyV1{{ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{Enabled: false, MaxEntries: 1024, DefaultTTLSeconds: 300}}}
	default:
		panic(fmt.Sprintf("unsupported test dataset %q", dataset))
	}
	return payload
}
