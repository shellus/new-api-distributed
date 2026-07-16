package edge

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaintainEdgeLeasesRenewsLowBalanceAndNearExpiry(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	t.Setenv("EDGE_LEASE_REQUEST_QUOTA", "1000")
	t.Setenv("EDGE_LEASE_MINIMUM_QUOTA", "100")
	t.Setenv("EDGE_LEASE_RENEW_BEFORE_SECONDS", "60")
	enableEdgeRuntimeServing(t)

	lowBalance := edgeRuntimeTestLease(now, "lease-low-balance", 7, 11, 1_000)
	nearExpiry := edgeRuntimeTestLease(now, "lease-near-expiry", 8, 12, 1_000)
	nearExpiry.ExpiresAtUnixMilli = now.Add(20 * time.Second).UnixMilli()
	healthy := edgeRuntimeTestLease(now, "lease-healthy", 9, 13, 1_000)
	for _, lease := range []dto.EdgeQuotaLeaseV1{lowBalance, nearExpiry, healthy} {
		require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	}
	require.NoError(t, db.Model(&model.EdgeLocalQuotaLease{}).Where("lease_id = ?", lowBalance.LeaseID).Updates(map[string]any{
		"remaining_quota": 100, "consumed_quota": 900,
	}).Error)

	acquires := make([]dto.EdgeLeaseAcquireRequestV1, 0, 2)
	closed := make([]string, 0, 2)
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/control/v1/lease/acquire":
			var acquire dto.EdgeLeaseAcquireRequestV1
			decodeEdgeRuntimeRequest(t, request, &acquire)
			acquires = append(acquires, acquire)
			lease := edgeRuntimeTestLease(
				now, fmt.Sprintf("lease-renewed-%d", acquire.Subject.UserID),
				acquire.Subject.UserID, acquire.Subject.TokenID, acquire.RequestedQuota,
			)
			lease.SnapshotID = acquire.SnapshotID
			lease.SnapshotRevision = acquire.SnapshotRevision
			lease.PricingRevision = acquire.SnapshotRevision
			lease.IssuedAtUnixMilli = now.UnixMilli()
			lease.ExpiresAtUnixMilli = now.Add(10 * time.Minute).UnixMilli()
			return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseAcquireResponseV1{
				Meta: edgeRuntimeResponseMeta(acquire.Meta.RequestID), Lease: lease,
			}), nil
		case "/control/v1/lease/close":
			var closeRequest dto.EdgeLeaseCloseRequestV1
			decodeEdgeRuntimeRequest(t, request, &closeRequest)
			closed = append(closed, closeRequest.LeaseID)
			stored := requireEdgeRuntimeLease(t, db, closeRequest.LeaseID)
			return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseCloseResponseV1{
				Meta:    edgeRuntimeResponseMeta(closeRequest.Meta.RequestID),
				LeaseID: closeRequest.LeaseID, LeaseVersion: closeRequest.LeaseVersion + 1,
				Status: dto.EdgeLeaseStatusClosedV1, GrantedQuota: stored.GrantedQuota,
				AcceptedQuota: stored.ConsumedQuota, ReturnedQuota: stored.RemainingQuota,
				CloseAfterEventSequence: closeRequest.FinalEventSequence,
			}), nil
		default:
			require.FailNow(t, "unexpected control path", request.URL.Path)
			return nil, nil
		}
	}))
	installEdgeRuntimeTestClient(t, client)

	require.NoError(t, maintainEdgeLeases(context.Background()))
	require.Len(t, acquires, 2)
	byUser := make(map[int64]dto.EdgeLeaseAcquireRequestV1, len(acquires))
	for _, acquire := range acquires {
		byUser[acquire.Subject.UserID] = acquire
	}
	assert.Equal(t, lowBalance.LeaseID, byUser[7].ExistingLeaseID)
	assert.Equal(t, nearExpiry.LeaseID, byUser[8].ExistingLeaseID)
	_, healthyRenewed := byUser[9]
	assert.False(t, healthyRenewed)
	assert.Equal(t, int64(1_000), byUser[7].RequestedQuota)
	assert.Equal(t, int64(100), byUser[7].MinimumAcceptableQuota)

	sort.Strings(closed)
	assert.Equal(t, []string{lowBalance.LeaseID, nearExpiry.LeaseID}, closed)
	assert.Equal(t, dto.EdgeLeaseStatusClosedV1, requireEdgeRuntimeLease(t, db, lowBalance.LeaseID).Status)
	assert.Equal(t, dto.EdgeLeaseStatusClosedV1, requireEdgeRuntimeLease(t, db, nearExpiry.LeaseID).Status)
	assert.Equal(t, dto.EdgeLeaseStatusActiveV1, requireEdgeRuntimeLease(t, db, healthy.LeaseID).Status)
	assert.Equal(t, dto.EdgeLeaseStatusActiveV1, requireEdgeRuntimeLease(t, db, "lease-renewed-7").Status)
	assert.Equal(t, dto.EdgeLeaseStatusActiveV1, requireEdgeRuntimeLease(t, db, "lease-renewed-8").Status)

	require.NoError(t, maintainEdgeLeases(context.Background()))
	assert.Len(t, acquires, 2, "a completed replenishment must be stable on the next maintenance pass")
	assert.Len(t, closed, 2, "the replacement lease must not supersede itself on the next pass")
}

func TestMaintainEdgeLeasesDoesNotChurnAtSnapshotExpiry(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	t.Setenv("EDGE_LEASE_RENEW_BEFORE_SECONDS", "60")
	enableEdgeRuntimeServing(t)

	snapshotExpiry := now.Add(20 * time.Second).UnixMilli()
	require.NoError(t, db.Model(&model.EdgeLocalControlState{}).Where("snapshot_id = ?", edgeRuntimeTestSnapshotID).
		Update("snapshot_expires_at_unix_milli", snapshotExpiry).Error)
	healthy := edgeRuntimeTestLease(now, "lease-snapshot-capped", 7, 11, 1_000)
	healthy.ExpiresAtUnixMilli = snapshotExpiry
	require.NoError(t, model.InstallEdgeLocalLease(db, healthy))

	controlRequests := 0
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		controlRequests++
		require.FailNow(t, "snapshot-capped healthy lease must not be replaced", request.URL.Path)
		return nil, nil
	}))
	installEdgeRuntimeTestClient(t, client)

	for range 3 {
		require.NoError(t, maintainEdgeLeases(context.Background()))
	}
	assert.Zero(t, controlRequests)
	stored := requireEdgeRuntimeLease(t, db, healthy.LeaseID)
	assert.Equal(t, dto.EdgeLeaseStatusActiveV1, stored.Status)
	assert.Equal(t, healthy.GrantedQuota, stored.RemainingQuota)
}

func TestCloseSupersededEdgeLeasesClosesOnlySafeCandidates(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")

	current := edgeRuntimeTestLease(now, "lease-current", 7, 11, 100)
	superseded := edgeRuntimeTestLease(now, "lease-superseded", 8, 12, 100)
	superseded.IssuedAtUnixMilli = now.Add(-10 * time.Minute).UnixMilli()
	superseding := edgeRuntimeTestLease(now, "lease-superseding", 8, 12, 100)
	expired := edgeRuntimeTestLease(now, "lease-expired", 9, 13, 100)
	expired.IssuedAtUnixMilli = now.Add(-20 * time.Minute).UnixMilli()
	expired.ExpiresAtUnixMilli = now.Add(-time.Minute).UnixMilli()
	oldSnapshot := edgeRuntimeTestLease(now, "lease-old-snapshot", 10, 14, 100)
	oldSnapshot.SnapshotID = "snapshot-old"
	exhausted := edgeRuntimeTestLease(now, "lease-exhausted", 11, 15, 100)
	unacknowledged := edgeRuntimeTestLease(now, "lease-unacknowledged", 12, 16, 100)
	unacknowledged.IssuedAtUnixMilli = now.Add(-10 * time.Minute).UnixMilli()
	unacknowledgedNew := edgeRuntimeTestLease(now, "lease-unacknowledged-new", 12, 16, 100)
	reserved := edgeRuntimeTestLease(now, "lease-reserved", 13, 17, 100)
	reserved.IssuedAtUnixMilli = now.Add(-10 * time.Minute).UnixMilli()
	reservedNew := edgeRuntimeTestLease(now, "lease-reserved-new", 13, 17, 100)
	leasing := []dto.EdgeQuotaLeaseV1{
		current, superseded, superseding, expired, oldSnapshot, exhausted,
		unacknowledged, unacknowledgedNew, reserved, reservedNew,
	}
	for _, lease := range leasing {
		require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	}
	require.NoError(t, db.Model(&model.EdgeLocalQuotaLease{}).Where("lease_id = ?", exhausted.LeaseID).Updates(map[string]any{
		"remaining_quota": 0, "consumed_quota": 100,
	}).Error)
	settleEdgeRuntimeUsage(t, db, unacknowledged, "reservation-unacknowledged", "request-unacknowledged", 10, 10, now)
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-still-active", RequestID: "request-still-active", LeaseID: reserved.LeaseID,
		Quota: 10, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)

	closed := make([]string, 0, 4)
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, "/control/v1/lease/close", request.URL.Path)
		var closeRequest dto.EdgeLeaseCloseRequestV1
		decodeEdgeRuntimeRequest(t, request, &closeRequest)
		closed = append(closed, closeRequest.LeaseID)
		stored := requireEdgeRuntimeLease(t, db, closeRequest.LeaseID)
		return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseCloseResponseV1{
			Meta:    edgeRuntimeResponseMeta(closeRequest.Meta.RequestID),
			LeaseID: closeRequest.LeaseID, LeaseVersion: closeRequest.LeaseVersion + 1,
			Status: dto.EdgeLeaseStatusClosedV1, GrantedQuota: stored.GrantedQuota,
			AcceptedQuota: stored.ConsumedQuota, ReturnedQuota: stored.RemainingQuota,
			CloseAfterEventSequence: closeRequest.FinalEventSequence,
		}), nil
	}))

	require.NoError(t, closeSupersededEdgeLeases(context.Background(), client))
	sort.Strings(closed)
	expectedClosed := []string{superseded.LeaseID, expired.LeaseID, oldSnapshot.LeaseID, exhausted.LeaseID}
	sort.Strings(expectedClosed)
	assert.Equal(t, expectedClosed, closed)
	for _, leaseID := range expectedClosed {
		assert.Equal(t, dto.EdgeLeaseStatusClosedV1, requireEdgeRuntimeLease(t, db, leaseID).Status)
	}
	for _, leaseID := range []string{
		current.LeaseID, superseding.LeaseID, unacknowledged.LeaseID,
		unacknowledgedNew.LeaseID, reserved.LeaseID, reservedNew.LeaseID,
	} {
		assert.Equal(t, dto.EdgeLeaseStatusActiveV1, requireEdgeRuntimeLease(t, db, leaseID).Status)
	}
	activeReservation := activeEdgeRuntimeReservation(t, db, "reservation-still-active")
	assert.Equal(t, reserved.LeaseID, activeReservation.LeaseID)
}
