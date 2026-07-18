package edge

import (
	"context"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainEdgeControlFlushesSettlementWithoutLeaseClose(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	settled := settleEdgeRuntimeUsage(t, db, "reservation-drain", "request-drain", 100, 100, now)
	var settlementRequests int
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "/control/v1/settlement/block", request.URL.Path)
		settlementRequests++
		var block dto.EdgeSettlementBlockRequestV1
		decodeEdgeRuntimeRequest(t, request, &block)
		return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeSettlementBlockResponseV1{
			Meta: edgeRuntimeResponseMeta(block.Meta.RequestID),
			Ack: dto.EdgeSettlementAckV1{
				Status: dto.EdgeSettlementAckAcceptedV1, NodeID: edgeRuntimeTestNodeID,
				NodeGeneration: edgeRuntimeTestNodeGeneration, BlockID: block.BlockID,
				AckedThroughSequence: block.LastSequence, NextExpectedSequence: block.LastSequence + 1,
				AcceptedEventCount: len(block.Events), AcknowledgedAtUnixMilli: now.UnixMilli(),
			},
		}), nil
	}))
	installEdgeRuntimeTestClient(t, client)
	enableEdgeRuntimeServing(t)

	require.NoError(t, DrainEdgeControl(context.Background()))
	assert.Equal(t, 1, settlementRequests)
	state, err := model.GetEdgeLocalSettlementState(db)
	require.NoError(t, err)
	assert.Equal(t, settled.Sequence, state.LastAckedSequence)
}
