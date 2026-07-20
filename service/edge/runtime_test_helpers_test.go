package edge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	edgeRuntimeTestNodeID           = "edge.runtime"
	edgeRuntimeTestNodeGeneration   = int64(1)
	edgeRuntimeTestSnapshotID       = "snapshot-runtime"
	edgeRuntimeTestSnapshotRevision = int64(1)
)

type edgeRuntimeRoundTripper func(*http.Request) (*http.Response, error)

func (f edgeRuntimeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newEdgeRuntimeTestDB(t *testing.T, cpaBaseURL string) (*gorm.DB, time.Time) {
	t.Helper()
	if cpaBaseURL == "" {
		cpaBaseURL = "http://localhost"
	}
	channelConfigDir := t.TempDir()
	t.Setenv("EDGE_CHANNEL_CONFIG_DIR", channelConfigDir)
	require.NoError(t, os.WriteFile(filepath.Join(channelConfigDir, "runtime-cpa.yaml"), []byte(fmt.Sprintf(`name: runtime-cpa
type: openai
base_url: %q
auth: runtime-test-key
`, cpaBaseURL)), 0o600))

	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-runtime.db"))
	require.NoError(t, err)
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Now().UTC().Truncate(time.Millisecond)
	modelPrice := 0.001
	datasets := []dto.EdgeSnapshotDatasetStateV1{
		{Dataset: dto.EdgeSnapshotDatasetAuthenticationV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetUsersV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetGroupsV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetModelsV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetChannelsV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetPricingV1, Revision: edgeRuntimeTestSnapshotRevision},
		{Dataset: dto.EdgeSnapshotDatasetRoutingV1, Revision: edgeRuntimeTestSnapshotRevision},
	}
	require.NoError(t, model.ApplyEdgeLocalSnapshot(db, model.EdgeLocalSnapshotProjectionData{
		State: dto.EdgeSnapshotStateV1{
			SnapshotID: edgeRuntimeTestSnapshotID, Revision: edgeRuntimeTestSnapshotRevision,
			AppliedAtUnixMilli: now.Add(-time.Minute).UnixMilli(), Datasets: datasets,
		},
		Digest: strings.Repeat("a", 64), ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(),
		TokenFingerprint: dto.EdgeTokenFingerprintSchemeV1{
			Algorithm: edgetoken.FingerprintAlgorithm, Version: edgetoken.FingerprintVersion,
		},
		Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("b", 64), TokenID: 11, UserID: 7, Enabled: true, Group: "default",
		}},
		Users: []dto.EdgeUserPolicyV1{{UserID: 7, Enabled: true, Username: "edge-user", DefaultGroup: "default", Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first"}}},
		Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}},
		Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-test", Enabled: true,
			Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1, dto.EdgeEndpointOpenAIResponsesV1},
			Streaming: true, ChannelIDs: []int64{31},
		}},
		Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: 31, Type: 1, Name: "runtime-cpa", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-test"}, Priority: 10, Weight: 100,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}},
		Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: "pricing-runtime", Version: "v1", Model: "gpt-test",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
		}},
		Routing: []dto.EdgeRoutingPolicyV1{{
			ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{Enabled: false, MaxEntries: 1_000, DefaultTTLSeconds: 60},
		}},
	}))
	require.NoError(t, model.ApplyEdgeLocalBalanceDelta(db, dto.EdgeNodeControlConfigV1{
		NodeID: edgeRuntimeTestNodeID, NodeGeneration: edgeRuntimeTestNodeGeneration,
	}, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 1_000_000}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 1_000_000}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 1_000_000, RemainQuota: 1_000_000,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}, now.UnixMilli()))
	return db, now
}

func newEdgeRuntimeTestControlClient(t *testing.T, transport http.RoundTripper) *EdgeControlClient {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	client, err := NewEdgeControlClient(EdgeControlClientConfig{
		MasterURL:       "http://localhost",
		NodeID:          edgeRuntimeTestNodeID,
		NodeGeneration:  edgeRuntimeTestNodeGeneration,
		CredentialKeyID: "credential.runtime",
		CredentialKey:   privateKey,
		Declaration: dto.EdgeNodeDeclarationV1{
			Name: "runtime-edge", PublicURL: "http://localhost", SoftwareVersion: "test",
			StartedAtUnixMilli: time.Now().Add(-time.Minute).UnixMilli(),
			Capabilities: []dto.EdgeEndpointCapabilityV1{
				{Endpoint: dto.EdgeEndpointDataPlaneV1, Streaming: true},
			},
		},
		HTTPClient: &http.Client{Transport: transport}, RequestTimeout: time.Second, MaxResponseBytes: 1 << 20,
	})
	require.NoError(t, err)
	return client
}

func installEdgeRuntimeTestClient(t *testing.T, client *EdgeControlClient) {
	t.Helper()
	previous := activeEdgeControlClient.Swap(client)
	require.Nil(t, previous)
	t.Cleanup(func() { activeEdgeControlClient.CompareAndSwap(client, nil) })
}

func enableEdgeRuntimeServing(t *testing.T) {
	t.Helper()
	edgeAdmission.mu.Lock()
	previousAdmission := edgeAdmission.accepting
	edgeAdmission.accepting = true
	edgeAdmission.mu.Unlock()
	previousReady := edgeControlReady.Load()
	previousAccountingReady := edgeAccountingReady.Load()
	previousAccountingBlock := edgeAccountingBlock.Load()
	previousBalanceReady := edgeBalanceReady.Load()
	previousCircuitOpen := edgeSettlementCircuitOpen.Load()
	edgeControlReady.Store(true)
	edgeAccountingReady.Store(true)
	edgeAccountingBlock.Store(false)
	edgeBalanceReady.Store(true)
	edgeSettlementCircuitOpen.Store(false)
	t.Cleanup(func() {
		edgeAdmission.mu.Lock()
		edgeAdmission.accepting = previousAdmission
		edgeAdmission.mu.Unlock()
		edgeControlReady.Store(previousReady)
		edgeAccountingReady.Store(previousAccountingReady)
		edgeAccountingBlock.Store(previousAccountingBlock)
		edgeBalanceReady.Store(previousBalanceReady)
		edgeSettlementCircuitOpen.Store(previousCircuitOpen)
	})
}

func settleEdgeRuntimeUsage(
	t *testing.T,
	db *gorm.DB,
	reservationID string,
	requestID string,
	reservedQuota int64,
	chargedQuota int64,
	now time.Time,
) *dto.EdgeUsageEventV1 {
	t.Helper()
	_, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: reservationID, RequestID: requestID, UserID: 7, TokenID: 11,
		Quota: reservedQuota, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	status := http.StatusOK
	settled, err := model.SettleEdgeLocalReservation(db, reservationID, dto.EdgeUsageEventV1{
		EventID: "event-" + reservationID, ChannelID: 31,
		Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Model: "gpt-test", Group: "default",
		StartedAtUnixMilli: now.Add(-time.Second).UnixMilli(), FinishedAtUnixMilli: now.UnixMilli(),
		Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "pricing-runtime", PricingPolicyVersion: "v1",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, GroupRatio: 1, ChargedQuota: chargedQuota,
		},
	})
	require.NoError(t, err)
	return settled
}

func edgeRuntimeJSONResponse(t *testing.T, status int, value any) *http.Response {
	t.Helper()
	body, err := common.Marshal(value)
	require.NoError(t, err)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func edgeRuntimeResponseMeta(requestID string) dto.EdgeControlResponseMetaV1 {
	return dto.EdgeControlResponseMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2,
		RequestID:       requestID, ServerRequestID: "server-" + requestID,
		ServerTimeUnixMilli: time.Now().UnixMilli(),
	}
}

func decodeEdgeRuntimeRequest(t *testing.T, request *http.Request, target any) {
	t.Helper()
	require.NoError(t, common.DecodeJsonStrict(request.Body, target))
}

func activeEdgeRuntimeReservation(t *testing.T, db *gorm.DB, reservationID string) *model.EdgeLocalQuotaReservation {
	t.Helper()
	reservation, err := model.GetEdgeLocalReservation(db, reservationID)
	require.NoError(t, err)
	require.Equal(t, model.EdgeLocalReservationStatusActive, reservation.Status)
	return reservation
}
