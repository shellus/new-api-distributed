package edge

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeRequestPolicyGuardPinsSnapshotUntilLeaseReservation(t *testing.T) {
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-admission.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	now := time.Now().UnixMilli()
	require.NoError(t, db.Model(&model.EdgeLocalControlState{}).Where("id = ?", 1).Updates(map[string]any{
		"snapshot_id":                    "snapshot-guard",
		"snapshot_revision":              int64(7),
		"snapshot_applied_at_unix_milli": now,
		"snapshot_expires_at_unix_milli": now + int64(time.Minute/time.Millisecond),
		"token_fingerprint_algorithm":    edgetoken.FingerprintAlgorithm,
		"token_fingerprint_key_id":       "",
		"token_fingerprint_version":      edgetoken.FingerprintVersion,
		"updated_at_unix_milli":          now,
	}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalDatasetState{
		Dataset: dto.EdgeSnapshotDatasetPricingV1, Revision: 7,
	}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalQuotaLease{
		LeaseID: "lease-guard", Version: 1, Status: dto.EdgeLeaseStatusActiveV1,
		NodeID: "node-guard", NodeGeneration: 1, UserID: 1, TokenID: 2,
		GrantedQuota: 100, RemainingQuota: 100, RenewAfterRemainingQuota: 25,
		IssuedAtUnixMilli: now, ExpiresAtUnixMilli: now + int64(time.Minute/time.Millisecond),
		SnapshotID: "snapshot-guard", SnapshotRevision: 7, PricingRevision: 7,
		CreatedAtUnixMilli: now, UpdatedAtUnixMilli: now,
	}).Error)

	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB })
	resetEdgeAdmissionTestState(t)

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	require.True(t, BeginEdgeRequest(context))
	t.Cleanup(func() { EndEdgeRequest(context) })
	if edgeDataPlanePolicy.TryLock() {
		edgeDataPlanePolicy.Unlock()
		t.Fatal("snapshot mutation must wait until pricing and reservation are pinned")
	}

	modelRatio := 1.0
	relayInfo := &relaycommon.RelayInfo{
		RequestId: "request-guard", UserId: 1, TokenId: 2, OriginModelName: "gpt-guard",
		EdgePricingPolicy: &dto.EdgePricingPolicyV1{
			PolicyID: "price-guard", Version: "v1", Model: "gpt-guard",
			BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &modelRatio, QuotaPerUnit: common.QuotaPerUnit,
		},
	}
	session, apiErr := BillingSessionFactory(context, 10, relayInfo)
	require.Nil(t, apiErr)
	require.NotNil(t, session)
	assert.Equal(t, "snapshot-guard", relayInfo.EdgeLeaseSnapshotID)
	assert.Equal(t, int64(7), relayInfo.EdgeLeaseSnapshotRevision)
	assert.Equal(t, int64(7), relayInfo.EdgeLeasePricingRevision)
	t.Cleanup(func() { session.Refund(context) })
	require.True(t, edgeDataPlanePolicy.TryLock(), "durable reservation should release the snapshot guard before CPA relay")
	edgeDataPlanePolicy.Unlock()

	session.Refund(context)
	EndEdgeRequest(context)
}

func TestEdgeRequestAdmissionFailureDoesNotLeakPolicyGuard(t *testing.T) {
	resetEdgeAdmissionTestState(t)
	edgeControlReady.Store(false)
	edgeControlSnapshotExpiry.Store(0)

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.False(t, BeginEdgeRequest(context))
	require.True(t, edgeDataPlanePolicy.TryLock())
	edgeDataPlanePolicy.Unlock()
}

func TestEdgeAccountingBlockOverridesStaleReadyFlag(t *testing.T) {
	resetEdgeAdmissionTestState(t)
	edgeAccountingReady.Store(true)
	edgeAccountingBlock.Store(true)
	assert.False(t, EdgeServingReady())

	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.False(t, BeginEdgeRequest(context))
	require.True(t, edgeDataPlanePolicy.TryLock())
	edgeDataPlanePolicy.Unlock()
}

func resetEdgeAdmissionTestState(t *testing.T) {
	edgeAdmission = newEdgeAdmissionGate()
	SetEdgeRequestAdmission(true)
	edgeControlReady.Store(true)
	edgeControlSnapshotExpiry.Store(time.Now().Add(time.Minute).UnixMilli())
	edgeAccountingReady.Store(true)
	edgeAccountingBlock.Store(false)
	t.Cleanup(func() {
		SetEdgeRequestAdmission(false)
		edgeControlReady.Store(false)
		edgeControlSnapshotExpiry.Store(0)
		edgeAccountingReady.Store(true)
		edgeAccountingBlock.Store(false)
		edgeAdmission = newEdgeAdmissionGate()
	})
}
