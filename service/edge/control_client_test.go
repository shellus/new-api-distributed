package edge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func TestEdgeControlClientSignsExactJSONBody(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)

	response, err := client.Bootstrap(context.Background(), dto.EdgeBootstrapRequestV1{
		Meta:                      dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "bootstrap-exact-body"},
		SupportedProtocolVersions: []string{dto.EdgeControlProtocolVersionV2, dto.EdgeControlProtocolVersionV1},
		Declaration:               client.Declaration(),
		Snapshot:                  dto.EdgeSnapshotStateV1{},
		Settlement:                dto.EdgeSettlementStateV1{NextEventSequence: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, fixture.manifest.SnapshotID, response.Snapshot.SnapshotID)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	require.NoError(t, handler.err)
	require.Len(t, handler.bootstrapBodies, 1)
	assert.Equal(t, "bootstrap-exact-body", handler.bootstrapRequestIDs[0])
	assert.Equal(t, "identity", handler.bootstrapAcceptEncodings[0])
	var decoded dto.EdgeBootstrapRequestV1
	require.NoError(t, common.DecodeJsonStrict(bytes.NewReader(handler.bootstrapBodies[0]), &decoded))
	canonical, err := common.Marshal(decoded)
	require.NoError(t, err)
	assert.Equal(t, handler.bootstrapBodies[0], canonical)
	assert.Equal(t, decoded.Meta.RequestID, handler.bootstrapRequestIDs[0])
}

func TestEdgeControlBootstrapRejectsTamperedSnapshotPageBeforeApply(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture, tamperFirstPage: true}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	store := newEdgeControlTestStore()

	err := RunEdgeControlLoopsWithDependencies(context.Background(), EdgeControlLoopDependencies{
		Client: client, Store: store, Now: func() time.Time { return fixture.now },
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeControlProtocolViolation)
	assert.Equal(t, 0, store.applyCountValue())
	assert.False(t, EdgeControlReady())
}

func TestEdgeControlBootstrapRetriesPageWithStableIdempotencyAndCancelsCleanly(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture, retryFirstPage: true}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	store := newEdgeControlTestStore()
	ctx, cancel := context.WithCancel(context.Background())
	readyTransitions := make([]bool, 0, 3)

	err := RunEdgeControlLoopsWithDependencies(ctx, EdgeControlLoopDependencies{
		Client: client, Store: store, Now: func() time.Time { return fixture.now },
		Ready: func(ready bool) {
			readyTransitions = append(readyTransitions, ready)
			if ready {
				active, ok := ActiveEdgeControlClient()
				assert.True(t, ok)
				assert.Same(t, client, active)
				cancel()
			}
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, store.applyCountValue())
	assert.Equal(t, []bool{false, true, false}, readyTransitions)
	assert.False(t, EdgeControlReady())
	_, active := ActiveEdgeControlClient()
	assert.False(t, active)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	require.NoError(t, handler.err)
	require.GreaterOrEqual(t, len(handler.firstPageRequestIDs), 2)
	assert.Equal(t, handler.firstPageRequestIDs[0], handler.firstPageRequestIDs[1])
	assert.Equal(t, handler.firstPageBodies[0], handler.firstPageBodies[1])
	require.GreaterOrEqual(t, len(handler.firstPageNonces), 2)
	assert.NotEqual(t, handler.firstPageNonces[0], handler.firstPageNonces[1])
	assert.Equal(t, []string{"", "p1"}, handler.userCursors)
}

func TestEdgeControlReadyFailsClosedAtSnapshotExpiry(t *testing.T) {
	edgeControlSnapshotExpiry.Store(time.Now().Add(time.Minute).UnixMilli())
	edgeControlReady.Store(true)
	t.Cleanup(func() {
		edgeControlReady.Store(false)
		edgeControlSnapshotExpiry.Store(0)
	})
	assert.True(t, EdgeControlReady())

	edgeControlSnapshotExpiry.Store(time.Now().Add(-time.Millisecond).UnixMilli())
	assert.False(t, EdgeControlReady())
}

func TestEdgeControlRestoresUnexpiredPersistedSnapshotOffline(t *testing.T) {
	now := time.Now().UTC()
	store := newEdgeControlTestStore()
	store.state = dto.EdgeSnapshotStateV1{
		SnapshotID: "snapshot-offline", Revision: 9, AppliedAtUnixMilli: now.Add(-time.Minute).UnixMilli(),
		Datasets: make([]dto.EdgeSnapshotDatasetStateV1, 0, len(edgeSnapshotDatasetOrder)),
	}
	for _, dataset := range edgeSnapshotDatasetOrder {
		store.state.Datasets = append(store.state.Datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset, Revision: 9})
	}
	store.expiresAt = now.Add(time.Minute).UnixMilli()
	edgeControlReady.Store(false)
	edgeControlSnapshotExpiry.Store(0)
	t.Cleanup(func() {
		edgeControlReady.Store(false)
		edgeControlSnapshotExpiry.Store(0)
	})

	runner := edgeControlLoop{store: store, now: func() time.Time { return now }}
	var restoreErr error
	require.NotPanics(t, func() {
		restoreErr = runner.restoreLocalReadiness(context.Background())
	})
	require.NoError(t, restoreErr)
	assert.True(t, EdgeControlReady())
	refreshes, installs := store.runtimeRefreshCounts()
	assert.Equal(t, 1, refreshes)
	assert.Equal(t, 1, installs)
}

func TestEdgeControlRestoresPersistedSnapshotDuringBootstrapRetry(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	server := httptest.NewServer(http.NotFoundHandler())
	client := newEdgeControlTestClient(t, fixture, server.URL)
	server.Close()

	store := newEdgeControlTestStore()
	store.state = dto.EdgeSnapshotStateV1{
		SnapshotID: "snapshot-offline-recovery", Revision: 9, AppliedAtUnixMilli: fixture.now.Add(-time.Minute).UnixMilli(),
		Datasets: make([]dto.EdgeSnapshotDatasetStateV1, 0, len(edgeSnapshotDatasetOrder)),
	}
	for _, dataset := range edgeSnapshotDatasetOrder {
		store.state.Datasets = append(store.state.Datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset, Revision: 9})
	}
	store.expiresAt = fixture.now.Add(time.Minute).UnixMilli()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := RunEdgeControlLoopsWithDependencies(ctx, EdgeControlLoopDependencies{
		Client: client, Store: store, Now: func() time.Time { return fixture.now },
		Ready: func(ready bool) {
			if ready {
				cancel()
			}
		},
	})
	require.NoError(t, err)
}

func TestEdgeControlBootstrapReusesAlreadyAppliedSnapshot(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	store := newEdgeControlTestStore()

	runUntilReady := func() {
		ctx, cancel := context.WithCancel(context.Background())
		err := RunEdgeControlLoopsWithDependencies(ctx, EdgeControlLoopDependencies{
			Client: client, Store: store, Now: func() time.Time { return fixture.now },
			Ready: func(ready bool) {
				if ready {
					cancel()
				}
			},
		})
		require.NoError(t, err)
	}
	runUntilReady()
	firstPageCalls := handler.pageCallCount()
	require.Greater(t, firstPageCalls, 0)
	assert.Equal(t, 1, store.applyCountValue())

	runUntilReady()
	assert.Equal(t, firstPageCalls, handler.pageCallCount())
	assert.Equal(t, 1, store.applyCountValue())
}

func TestEdgeHeartbeatOmitsDeprecatedCPAObservations(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	runner := edgeControlLoop{
		client:        client,
		store:         newEdgeControlTestStore(),
		runtimeStatus: func() dto.EdgeRuntimeStatusV1 { return dto.EdgeRuntimeStatusV1{} },
	}

	_, err := runner.heartbeat(context.Background(), fixture.control)
	require.NoError(t, err)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	require.NoError(t, handler.err)
	require.Len(t, handler.heartbeatCPA, 1)
	assert.Empty(t, handler.heartbeatCPA[0])
}

func TestEdgeSnapshotApplyFailsClosedWhenRoutingInstallFails(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	store := newEdgeControlTestStore()
	store.routingInstallErr = errors.New("install routing policy")

	edgeControlReady.Store(true)
	edgeControlSnapshotExpiry.Store(fixture.now.Add(time.Minute).UnixMilli())
	t.Cleanup(func() {
		edgeControlReady.Store(false)
		edgeControlSnapshotExpiry.Store(0)
	})
	setReady := func(ready bool) {
		if !ready {
			edgeControlReady.Store(false)
			edgeControlSnapshotExpiry.Store(0)
			return
		}
		edgeControlReady.Store(true)
	}
	runner := edgeControlLoop{client: client, store: store, setReady: setReady, now: func() time.Time { return fixture.now }}

	changed, err := runner.syncSnapshot(context.Background(), fixture.manifest, fixture.control)
	require.ErrorContains(t, err, "install routing policy")
	assert.False(t, changed)
	assert.Equal(t, 1, store.applyCountValue(), "the failure must cover the apply-succeeded/install-failed transition")
	assert.False(t, EdgeControlReady(), "partially switched policy must not remain request-admissible")
}

func TestEdgeSnapshotApplyActivatesAfterRoutingInstall(t *testing.T) {
	fixture := newEdgeControlTransportFixture(t)
	handler := &edgeControlTestHandler{t: t, fixture: fixture}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newEdgeControlTestClient(t, fixture, server.URL)
	store := newEdgeControlTestStore()
	readyTransitions := make([]bool, 0, 2)

	edgeControlReady.Store(true)
	edgeControlSnapshotExpiry.Store(fixture.now.Add(time.Minute).UnixMilli())
	t.Cleanup(func() {
		edgeControlReady.Store(false)
		edgeControlSnapshotExpiry.Store(0)
	})
	setReady := func(ready bool) {
		readyTransitions = append(readyTransitions, ready)
		if !ready {
			edgeControlReady.Store(false)
			edgeControlSnapshotExpiry.Store(0)
			return
		}
		edgeControlReady.Store(true)
	}
	runner := edgeControlLoop{
		client: client, store: store, setReady: setReady, now: func() time.Time { return fixture.now },
	}

	changed, err := runner.syncSnapshot(context.Background(), fixture.manifest, fixture.control)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, []bool{false, true}, readyTransitions)
	assert.True(t, EdgeControlReady())
}

func TestEdgeRoutingInstallRejectsPlaintextTokenAffinityMatcher(t *testing.T) {
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-routing.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	routing := dto.EdgeRoutingPolicyV1{ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{
		Enabled: true, MaxEntries: 100, DefaultTTLSeconds: 60,
		Rules: []dto.EdgeChannelAffinityRuleV1{{
			Name: "plaintext token matcher", ModelRegex: []string{"^gpt-5\\.4$"},
			KeySources: []dto.EdgeChannelAffinityKeySourceV1{{
				Type: dto.EdgeChannelAffinityKeySourceContextStringV1, Key: "token_key",
			}},
			ValueRegex: "^tokenSecret", TTLSeconds: 60,
		}},
	}}
	payload, err := common.Marshal(routing)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.EdgeLocalRoutingProjection{ID: 1, Payload: string(payload)}).Error)

	err = (&edgeControlGormStore{db: db}).InstallRoutingPolicy(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeControlProtocolViolation)
}

type edgeControlTransportFixture struct {
	now                time.Time
	nodePrivateKey     ed25519.PrivateKey
	nodePublicKey      ed25519.PublicKey
	snapshotPrivateKey ed25519.PrivateKey
	control            dto.EdgeNodeControlConfigV1
	manifest           dto.EdgeSnapshotManifestV1
	pages              map[dto.EdgeSnapshotDatasetV1][]dto.EdgeSnapshotPagePayloadV1
}

func newEdgeControlTransportFixture(t *testing.T) *edgeControlTransportFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	nodeSeed := sha256.Sum256([]byte("edge-control-client-node-test-key"))
	snapshotSeed := sha256.Sum256([]byte("edge-control-client-snapshot-test-key"))
	nodePrivateKey := ed25519.NewKeyFromSeed(nodeSeed[:])
	snapshotPrivateKey := ed25519.NewKeyFromSeed(snapshotSeed[:])
	snapshotPublicKey := snapshotPrivateKey.Public().(ed25519.PublicKey)
	encodedSnapshotPublicKey, err := edgeauth.EncodePublicKey(snapshotPublicKey)
	require.NoError(t, err)

	modelPrice := 0.001
	pages := map[dto.EdgeSnapshotDatasetV1][]dto.EdgeSnapshotPagePayloadV1{
		dto.EdgeSnapshotDatasetAuthenticationV1: {{Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("b", 64), TokenID: 11, UserID: 7, Enabled: true, Group: "default",
		}}}},
		dto.EdgeSnapshotDatasetUsersV1: {
			{Users: []dto.EdgeUserPolicyV1{{UserID: 7, Enabled: true, Username: "edge-user-7", DefaultGroup: "default", Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first"}}}},
			{Users: []dto.EdgeUserPolicyV1{{UserID: 8, Enabled: true, Username: "edge-user-8", DefaultGroup: "default", Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first"}}}},
		},
		dto.EdgeSnapshotDatasetGroupsV1: {{Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}}}},
		dto.EdgeSnapshotDatasetModelsV1: {{Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-4o-mini", Enabled: true,
			Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1, dto.EdgeEndpointOpenAIResponsesV1},
			Streaming: true, ChannelIDs: []int64{31},
		}}}},
		dto.EdgeSnapshotDatasetChannelsV1: {{Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: 31, Type: 1, Name: "edge-channel", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-4o-mini"}, Priority: 10, Weight: 100,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}}}},
		dto.EdgeSnapshotDatasetPricingV1: {{Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: "pricing-1", Version: "v1", Model: "gpt-4o-mini",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
		}}}},
		dto.EdgeSnapshotDatasetRoutingV1: {{Routing: []dto.EdgeRoutingPolicyV1{{
			ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{Enabled: false, MaxEntries: 1000, DefaultTTLSeconds: 60},
		}}}},
	}

	manifest := dto.EdgeSnapshotManifestV1{
		SnapshotID: "snapshot-control-test", Revision: 1,
		CreatedAtUnixMilli: now.Add(-time.Minute).UnixMilli(), ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(),
		HashAlgorithm: edgesnapshot.HashAlgorithm,
		TokenFingerprint: dto.EdgeTokenFingerprintSchemeV1{
			Algorithm: edgetoken.FingerprintAlgorithm, Version: edgetoken.FingerprintVersion,
		},
		Datasets: make([]dto.EdgeSnapshotDatasetManifestV1, 0, len(edgeSnapshotDatasetOrder)),
	}
	primitiveManifests := make([]edgesnapshot.DatasetManifest, 0, len(edgeSnapshotDatasetOrder))
	for _, dataset := range edgeSnapshotDatasetOrder {
		payloads := pages[dataset]
		pageDigests := make([]string, 0, len(payloads))
		var itemCount int64
		for _, payload := range payloads {
			count := edgeControlTestPayloadCount(dataset, payload)
			require.NoError(t, payload.Validate(dataset, count))
			_, digest, err := edgesnapshot.MarshalPagePayload(payload)
			require.NoError(t, err)
			pageDigests = append(pageDigests, digest)
			itemCount += int64(count)
		}
		datasetDigest, err := edgesnapshot.AggregatePageDigests(pageDigests)
		require.NoError(t, err)
		primitive := edgesnapshot.DatasetManifest{
			SnapshotID: manifest.SnapshotID, Dataset: string(dataset), Revision: 1,
			ItemCount: itemCount, PageCount: len(payloads), PayloadDigest: datasetDigest,
		}
		signature, err := edgesnapshot.SignDatasetManifest(snapshotPrivateKey, primitive)
		require.NoError(t, err)
		manifest.Datasets = append(manifest.Datasets, dto.EdgeSnapshotDatasetManifestV1{
			Dataset: dataset, Revision: 1, ItemCount: itemCount, PageCount: len(payloads), Digest: datasetDigest,
			DetachedSignature: dto.EdgeDetachedContentSignatureV1{
				Algorithm: edgesnapshot.SignatureAlgorithm, KeyID: "snapshot.test", PayloadDigest: datasetDigest, Value: signature,
			},
		})
		primitiveManifests = append(primitiveManifests, primitive)
	}
	manifest.Digest, err = edgesnapshot.AggregateDatasetManifests(manifest.SnapshotID, manifest.Revision, primitiveManifests)
	require.NoError(t, err)
	require.NoError(t, manifest.Validate())

	control := dto.EdgeNodeControlConfigV1{
		NodeID: "edge.test", NodeGeneration: 1, Enabled: true,
		HeartbeatIntervalSeconds: 1, SnapshotPollIntervalSeconds: 1, SnapshotPageLimit: 1,
		SettlementMaxEvents: 100, SettlementMaxDelaySeconds: 1, ClockSkewToleranceSeconds: 60,
		SnapshotVerificationKeys: []dto.EdgeSnapshotVerificationKeyV1{{
			KeyID: "snapshot.test", Algorithm: edgeauth.Algorithm, PublicKey: encodedSnapshotPublicKey,
			NotBeforeUnixMilli: now.Add(-time.Hour).UnixMilli(), ExpiresAtUnixMilli: now.Add(2 * time.Hour).UnixMilli(),
		}},
	}
	require.NoError(t, control.Validate())
	return &edgeControlTransportFixture{
		now: now, nodePrivateKey: nodePrivateKey, nodePublicKey: nodePrivateKey.Public().(ed25519.PublicKey),
		snapshotPrivateKey: snapshotPrivateKey, control: control, manifest: manifest, pages: pages,
	}
}

func newEdgeControlTestClient(t *testing.T, fixture *edgeControlTransportFixture, masterURL string) *EdgeControlClient {
	t.Helper()
	client, err := NewEdgeControlClient(EdgeControlClientConfig{
		MasterURL: masterURL, NodeID: fixture.control.NodeID, NodeGeneration: fixture.control.NodeGeneration,
		CredentialKeyID: "credential.test", CredentialKey: fixture.nodePrivateKey,
		Declaration: dto.EdgeNodeDeclarationV1{
			Name: "edge-test", PublicURL: masterURL, SoftwareVersion: "test",
			StartedAtUnixMilli: fixture.now.Add(-time.Minute).UnixMilli(),
			Capabilities: []dto.EdgeEndpointCapabilityV1{
				{Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Streaming: true},
				{Endpoint: dto.EdgeEndpointOpenAIResponsesV1, Streaming: true},
			},
		},
		RequestTimeout: time.Second, MaxResponseBytes: 1 << 20, Now: func() time.Time { return fixture.now },
	})
	require.NoError(t, err)
	return client
}

type edgeControlTestHandler struct {
	t                        *testing.T
	fixture                  *edgeControlTransportFixture
	tamperFirstPage          bool
	retryFirstPage           bool
	mu                       sync.Mutex
	err                      error
	pageCalls                int
	retriedFirstPage         bool
	bootstrapBodies          [][]byte
	bootstrapRequestIDs      []string
	bootstrapAcceptEncodings []string
	firstPageBodies          [][]byte
	firstPageRequestIDs      []string
	firstPageNonces          []string
	userCursors              []string
	heartbeatCPA             [][]dto.EdgeCPAStatusV1
}

func (h *edgeControlTestHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		h.fail(writer, err)
		return
	}
	signed, err := edgeauth.ParseHTTPRequest(request, body)
	if err != nil {
		h.fail(writer, err)
		return
	}
	if err := signed.Verify(h.fixture.nodePublicKey, edgeauth.VerifyOptions{Now: h.fixture.now, MaxClockSkew: time.Minute}); err != nil {
		h.fail(writer, err)
		return
	}
	var envelope struct {
		Meta dto.EdgeControlRequestMetaV1 `json:"meta"`
	}
	if err := common.Unmarshal(body, &envelope); err != nil {
		h.fail(writer, err)
		return
	}
	if envelope.Meta.RequestID != signed.Metadata.IdempotencyKey {
		h.fail(writer, errors.New("body request_id differs from signed idempotency key"))
		return
	}

	switch request.URL.Path {
	case "/control/v1/bootstrap":
		h.mu.Lock()
		h.bootstrapBodies = append(h.bootstrapBodies, append([]byte(nil), body...))
		h.bootstrapRequestIDs = append(h.bootstrapRequestIDs, signed.Metadata.IdempotencyKey)
		h.bootstrapAcceptEncodings = append(h.bootstrapAcceptEncodings, request.Header.Get("Accept-Encoding"))
		h.mu.Unlock()
		h.writeJSON(writer, dto.EdgeBootstrapResponseV1{
			Meta: h.responseMeta(envelope.Meta.RequestID), Control: h.fixture.control, Snapshot: h.fixture.manifest,
		})
	case "/control/v1/snapshot/page":
		var pageRequest dto.EdgeSnapshotPageRequestV1
		if err := common.DecodeJsonStrict(bytes.NewReader(body), &pageRequest); err != nil {
			h.fail(writer, err)
			return
		}
		ordinal := 0
		if pageRequest.Cursor != "" {
			if _, err := fmt.Sscanf(pageRequest.Cursor, "p%d", &ordinal); err != nil {
				h.fail(writer, err)
				return
			}
		}
		h.mu.Lock()
		h.pageCalls++
		isFirstPage := pageRequest.Dataset == dto.EdgeSnapshotDatasetAuthenticationV1 && pageRequest.Cursor == ""
		if isFirstPage {
			h.firstPageBodies = append(h.firstPageBodies, append([]byte(nil), body...))
			h.firstPageRequestIDs = append(h.firstPageRequestIDs, signed.Metadata.IdempotencyKey)
			h.firstPageNonces = append(h.firstPageNonces, signed.Metadata.Nonce)
		}
		if pageRequest.Dataset == dto.EdgeSnapshotDatasetUsersV1 {
			h.userCursors = append(h.userCursors, pageRequest.Cursor)
		}
		shouldRetry := h.retryFirstPage && isFirstPage && !h.retriedFirstPage
		if shouldRetry {
			h.retriedFirstPage = true
		}
		h.mu.Unlock()
		if shouldRetry {
			retryAfter := int64(1)
			h.writeStatusJSON(writer, http.StatusServiceUnavailable, dto.EdgeControlErrorResponseV1{
				Meta: h.responseMeta(envelope.Meta.RequestID),
				Error: dto.EdgeControlErrorV1{
					Code: dto.EdgeControlErrorCodeTemporarilyUnavailableV1, Message: "retry", Retryable: true,
					RetryAfterSeconds: &retryAfter,
				},
			})
			return
		}
		payloads := h.fixture.pages[pageRequest.Dataset]
		if ordinal < 0 || ordinal >= len(payloads) {
			h.fail(writer, errors.New("page ordinal out of range"))
			return
		}
		payload := payloads[ordinal]
		_, digest, err := edgesnapshot.MarshalPagePayload(payload)
		if err != nil {
			h.fail(writer, err)
			return
		}
		if h.tamperFirstPage && isFirstPage {
			payload.Authentication = append([]dto.EdgeTokenAuthRecordV1(nil), payload.Authentication...)
			payload.Authentication[0].TokenFingerprint = strings.Repeat("c", 64)
		}
		nextCursor := ""
		if ordinal+1 < len(payloads) {
			nextCursor = fmt.Sprintf("p%d", ordinal+1)
		}
		datasetManifest := edgeControlTestDatasetManifest(h.fixture.manifest, pageRequest.Dataset)
		h.writeJSON(writer, dto.EdgeSnapshotPageResponseV1{
			Meta: h.responseMeta(envelope.Meta.RequestID), SnapshotID: h.fixture.manifest.SnapshotID,
			Dataset: pageRequest.Dataset, Revision: datasetManifest.Revision, Cursor: pageRequest.Cursor,
			NextCursor: nextCursor, ItemCount: edgeControlTestPayloadCount(pageRequest.Dataset, payload),
			Digest: digest, Payload: payload,
		})
	case "/control/v1/heartbeat":
		var heartbeatRequest dto.EdgeHeartbeatRequestV1
		if err := common.DecodeJsonStrict(bytes.NewReader(body), &heartbeatRequest); err != nil {
			h.fail(writer, err)
			return
		}
		h.mu.Lock()
		h.heartbeatCPA = append(h.heartbeatCPA, append([]dto.EdgeCPAStatusV1(nil), heartbeatRequest.CPA...))
		h.mu.Unlock()
		h.writeJSON(writer, dto.EdgeHeartbeatResponseV1{
			Meta: h.responseMeta(envelope.Meta.RequestID), Control: h.fixture.control,
		})
	default:
		h.fail(writer, fmt.Errorf("unexpected request path %s", request.URL.Path))
	}
}

func (h *edgeControlTestHandler) responseMeta(requestID string) dto.EdgeControlResponseMetaV1 {
	return dto.EdgeControlResponseMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: requestID,
		ServerRequestID: "server-" + requestID, ServerTimeUnixMilli: h.fixture.now.UnixMilli(),
	}
}

func (h *edgeControlTestHandler) writeJSON(writer http.ResponseWriter, value any) {
	h.writeStatusJSON(writer, http.StatusOK, value)
}

func (h *edgeControlTestHandler) writeStatusJSON(writer http.ResponseWriter, status int, value any) {
	body, err := common.Marshal(value)
	if err != nil {
		h.fail(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func (h *edgeControlTestHandler) fail(writer http.ResponseWriter, err error) {
	h.mu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.mu.Unlock()
	h.t.Errorf("edge control test handler: %v", err)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusInternalServerError)
	_, _ = writer.Write([]byte(`{"error":"test handler failure"}`))
}

func (h *edgeControlTestHandler) pageCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pageCalls
}

type edgeControlTestStore struct {
	mu                  sync.Mutex
	state               dto.EdgeSnapshotStateV1
	expiresAt           int64
	applyCount          int
	channelRefreshCount int
	routingInstallCount int
	routingInstallErr   error
}

func newEdgeControlTestStore() *edgeControlTestStore {
	return &edgeControlTestStore{}
}

func (s *edgeControlTestStore) SnapshotState(context.Context) (*dto.EdgeSnapshotStateV1, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	state.Datasets = append([]dto.EdgeSnapshotDatasetStateV1(nil), s.state.Datasets...)
	return &state, nil
}

func (s *edgeControlTestStore) SnapshotExpiry(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt, nil
}

func (s *edgeControlTestStore) SettlementState(context.Context) (*dto.EdgeSettlementStateV1, error) {
	return &dto.EdgeSettlementStateV1{NextEventSequence: 1}, nil
}

func (s *edgeControlTestStore) LeaseRuntimeStates(context.Context) ([]dto.EdgeLeaseRuntimeStateV1, error) {
	return nil, nil
}

func (s *edgeControlTestStore) ApplySnapshot(_ context.Context, snapshot model.EdgeLocalSnapshotProjectionData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = snapshot.State
	s.expiresAt = snapshot.ExpiresAtUnixMilli
	s.applyCount++
	return nil
}

func (s *edgeControlTestStore) RefreshChannelRuntime(context.Context) error {
	s.mu.Lock()
	s.channelRefreshCount++
	s.mu.Unlock()
	return nil
}

func (s *edgeControlTestStore) InstallRoutingPolicy(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routingInstallCount++
	return s.routingInstallErr
}

func (s *edgeControlTestStore) PendingSettlementBlock(context.Context) (*dto.EdgeSettlementBlockRequestV1, error) {
	return nil, model.ErrEdgeLocalNoPendingUsageEvents
}

func (s *edgeControlTestStore) BuildSettlementBlock(context.Context, dto.EdgeControlRequestMetaV1, string, int, int64) (*dto.EdgeSettlementBlockRequestV1, error) {
	return nil, model.ErrEdgeLocalNoPendingUsageEvents
}

func (s *edgeControlTestStore) AcknowledgeSettlement(context.Context, dto.EdgeSettlementAckV1) error {
	return nil
}

func (s *edgeControlTestStore) applyCountValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyCount
}

func (s *edgeControlTestStore) runtimeRefreshCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelRefreshCount, s.routingInstallCount
}

func edgeControlTestPayloadCount(dataset dto.EdgeSnapshotDatasetV1, payload dto.EdgeSnapshotPagePayloadV1) int {
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		return len(payload.Authentication)
	case dto.EdgeSnapshotDatasetUsersV1:
		return len(payload.Users)
	case dto.EdgeSnapshotDatasetGroupsV1:
		return len(payload.Groups)
	case dto.EdgeSnapshotDatasetModelsV1:
		return len(payload.Models)
	case dto.EdgeSnapshotDatasetChannelsV1:
		return len(payload.Channels)
	case dto.EdgeSnapshotDatasetPricingV1:
		return len(payload.Pricing)
	case dto.EdgeSnapshotDatasetRoutingV1:
		return len(payload.Routing)
	default:
		return 0
	}
}

func edgeControlTestDatasetManifest(manifest dto.EdgeSnapshotManifestV1, dataset dto.EdgeSnapshotDatasetV1) dto.EdgeSnapshotDatasetManifestV1 {
	for _, candidate := range manifest.Datasets {
		if candidate.Dataset == dataset {
			return candidate
		}
	}
	return dto.EdgeSnapshotDatasetManifestV1{}
}
