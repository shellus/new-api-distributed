package edge

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainEdgeControlDoesNotCloseLeaseBeforeSettlementAck(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	lease := edgeRuntimeTestLease(now, "lease-drain", 7, 11, 1_000)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	settled := settleEdgeRuntimeUsage(t, db, lease, "reservation-drain", "request-drain", 100, 100, now)

	failSettlement := true
	settlementRequests := make([]dto.EdgeSettlementBlockRequestV1, 0, 2)
	closeCalls := 0
	ackObservedBeforeClose := false
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/control/v1/settlement/block":
			var block dto.EdgeSettlementBlockRequestV1
			decodeEdgeRuntimeRequest(t, request, &block)
			settlementRequests = append(settlementRequests, block)
			if failSettlement {
				return nil, io.ErrUnexpectedEOF
			}
			return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeSettlementBlockResponseV1{
				Meta: edgeRuntimeResponseMeta(block.Meta.RequestID),
				Ack: dto.EdgeSettlementAckV1{
					Status: dto.EdgeSettlementAckAcceptedV1,
					NodeID: edgeRuntimeTestNodeID, NodeGeneration: edgeRuntimeTestNodeGeneration,
					BlockID: block.BlockID, AckedThroughSequence: block.LastSequence,
					NextExpectedSequence: block.LastSequence + 1, AcceptedEventCount: len(block.Events),
					AcknowledgedAtUnixMilli: now.Add(time.Second).UnixMilli(),
				},
			}), nil
		case "/control/v1/lease/close":
			closeCalls++
			var closeRequest dto.EdgeLeaseCloseRequestV1
			decodeEdgeRuntimeRequest(t, request, &closeRequest)
			var event model.EdgeLocalUsageEvent
			require.NoError(t, db.Where("event_id = ?", settled.EventID).First(&event).Error)
			ackObservedBeforeClose = event.Acknowledged
			storedLease := requireEdgeRuntimeLease(t, db, closeRequest.LeaseID)
			return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseCloseResponseV1{
				Meta:    edgeRuntimeResponseMeta(closeRequest.Meta.RequestID),
				LeaseID: closeRequest.LeaseID, LeaseVersion: closeRequest.LeaseVersion + 1,
				Status: dto.EdgeLeaseStatusClosedV1, GrantedQuota: storedLease.GrantedQuota,
				AcceptedQuota: storedLease.ConsumedQuota, ReturnedQuota: storedLease.RemainingQuota,
				CloseAfterEventSequence: closeRequest.FinalEventSequence,
			}), nil
		default:
			require.FailNow(t, "unexpected control path", request.URL.Path)
			return nil, nil
		}
	}))
	installEdgeRuntimeTestClient(t, client)

	err := DrainEdgeControl(context.Background())
	require.Error(t, err)
	assert.Zero(t, closeCalls)
	require.Len(t, settlementRequests, 1)
	var storedEvent model.EdgeLocalUsageEvent
	require.NoError(t, db.Where("event_id = ?", settled.EventID).First(&storedEvent).Error)
	assert.False(t, storedEvent.Acknowledged)
	assert.Equal(t, dto.EdgeLeaseStatusActiveV1, requireEdgeRuntimeLease(t, db, lease.LeaseID).Status)

	failSettlement = false
	require.NoError(t, DrainEdgeControl(context.Background()))
	require.Len(t, settlementRequests, 2)
	assert.Equal(t, settlementRequests[0], settlementRequests[1], "retry must submit the exact durable block")
	assert.Equal(t, 1, closeCalls)
	assert.True(t, ackObservedBeforeClose)
	require.NoError(t, db.Where("event_id = ?", settled.EventID).First(&storedEvent).Error)
	assert.True(t, storedEvent.Acknowledged)
	assert.Equal(t, dto.EdgeLeaseStatusClosedV1, requireEdgeRuntimeLease(t, db, lease.LeaseID).Status)
}
