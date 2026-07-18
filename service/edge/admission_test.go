package edge

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeRequestPolicyGuardPinsSnapshotUntilBalanceReservation(t *testing.T) {
	_, _ = newEdgeRuntimeTestDB(t, "")
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
		RequestId: "request-guard", UserId: 7, TokenId: 11, OriginModelName: "gpt-test",
		EdgePricingPolicy: &dto.EdgePricingPolicyV1{
			PolicyID: "pricing-runtime", Version: "v1", Model: "gpt-test",
			BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &modelRatio, QuotaPerUnit: common.QuotaPerUnit,
		},
	}
	session, apiErr := BillingSessionFactory(context, 10, relayInfo)
	require.Nil(t, apiErr)
	require.NotNil(t, session)
	assert.Equal(t, edgeRuntimeTestSnapshotID, relayInfo.EdgeSnapshotID)
	assert.Equal(t, edgeRuntimeTestSnapshotRevision, relayInfo.EdgeSnapshotRevision)
	assert.Equal(t, edgeRuntimeTestSnapshotRevision, relayInfo.EdgePricingRevision)
	t.Cleanup(func() { session.Refund(context) })
	require.True(t, edgeDataPlanePolicy.TryLock(), "durable reservation should release the snapshot guard before CPA relay")
	edgeDataPlanePolicy.Unlock()

	session.Refund(context)
	EndEdgeRequest(context)
}

func TestEdgeRequestAdmissionFailureDoesNotLeakPolicyGuard(t *testing.T) {
	resetEdgeAdmissionTestState(t)
	edgeControlReady.Store(false)

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
	edgeBalanceReady.Store(true)
	edgeAccountingReady.Store(true)
	edgeAccountingBlock.Store(false)
	t.Cleanup(func() {
		SetEdgeRequestAdmission(false)
		edgeControlReady.Store(false)
		edgeBalanceReady.Store(false)
		edgeAccountingReady.Store(true)
		edgeAccountingBlock.Store(false)
		edgeAdmission = newEdgeAdmissionGate()
	})
}
