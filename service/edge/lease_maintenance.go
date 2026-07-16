package edge

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

type edgeLeaseSubjectRuntime struct {
	userID         int
	tokenID        int
	totalRemaining int64
	renewThreshold int64
	latestIssuedAt int64
	latestExpiry   int64
	hasPositive    bool
}

// RunEdgeLeaseMaintenance replenishes current-snapshot leases before their
// aggregate remaining quota reaches the signed renewal threshold or all of
// them approach expiry. It is control-plane-only and never carries a user AI
// request to the master.
func RunEdgeLeaseMaintenance(ctx context.Context) {
	intervalSeconds := common.GetEnvOrDefault("EDGE_LEASE_MAINTENANCE_INTERVAL_SECONDS", 15)
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}
	if intervalSeconds > 300 {
		intervalSeconds = 300
	}
	run := func() {
		if err := maintainEdgeLeases(ctx); err != nil && ctx.Err() == nil {
			common.SysError("edge lease maintenance failed: " + err.Error())
		}
	}
	run()
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func maintainEdgeLeases(ctx context.Context) error {
	if edgeAccountingBlock.Load() {
		return errEdgeAccountingRecoveryBlocked
	}
	if !edgeAccountingReady.Load() {
		if err := RecoverEdgeStagedSettlements(ctx, model.DB); err != nil {
			return err
		}
	}
	if !EdgeServingReady() {
		return nil
	}
	if _, ok := ActiveEdgeControlClient(); !ok {
		return nil
	}
	snapshot, err := model.GetEdgeLocalSnapshotState(model.DB.WithContext(ctx))
	if err != nil || snapshot.SnapshotID == "" {
		return err
	}
	now := time.Now().UTC()
	var leases []model.EdgeLocalQuotaLease
	if err := model.DB.WithContext(ctx).
		Where("status = ? AND snapshot_id = ? AND snapshot_revision = ? AND expires_at_unix_milli > ?",
			dto.EdgeLeaseStatusActiveV1, snapshot.SnapshotID, snapshot.Revision, now.UnixMilli()).
		Order("user_id asc, token_id asc, issued_at_unix_milli asc").Find(&leases).Error; err != nil {
		return err
	}
	subjects := make(map[string]*edgeLeaseSubjectRuntime)
	for _, lease := range leases {
		key := fmt.Sprintf("%d:%d", lease.UserID, lease.TokenID)
		subject := subjects[key]
		if subject == nil {
			subject = &edgeLeaseSubjectRuntime{userID: int(lease.UserID), tokenID: int(lease.TokenID)}
			subjects[key] = subject
		}
		subject.totalRemaining += lease.RemainingQuota
		if lease.GrantedQuota > 0 {
			subject.hasPositive = true
		}
		if lease.RenewAfterRemainingQuota > subject.renewThreshold {
			subject.renewThreshold = lease.RenewAfterRemainingQuota
		}
		if lease.ExpiresAtUnixMilli > subject.latestExpiry ||
			(lease.ExpiresAtUnixMilli == subject.latestExpiry && lease.IssuedAtUnixMilli > subject.latestIssuedAt) {
			subject.latestIssuedAt = lease.IssuedAtUnixMilli
			subject.latestExpiry = lease.ExpiresAtUnixMilli
		}
	}
	renewBeforeSeconds := common.GetEnvOrDefault("EDGE_LEASE_RENEW_BEFORE_SECONDS", 60)
	if renewBeforeSeconds < 1 {
		renewBeforeSeconds = 1
	}
	if renewBeforeSeconds > 3600 {
		renewBeforeSeconds = 3600
	}
	renewWindow := time.Duration(renewBeforeSeconds) * time.Second
	deadline := now.Add(renewWindow).UnixMilli()
	snapshotExpiry, err := model.GetEdgeLocalSnapshotExpiry(model.DB.WithContext(ctx))
	if err != nil {
		return err
	}
	for _, subject := range subjects {
		if !subject.hasPositive {
			continue
		}
		lowBalance := subject.totalRemaining <= subject.renewThreshold
		leaseLifetime := subject.latestExpiry - subject.latestIssuedAt
		// A lease cannot outlive its signed snapshot. Once the snapshot itself
		// enters the renewal window, another lease would receive the same expiry
		// and be closed as superseded on the next pass. A freshly issued lease
		// whose entire lifetime fits inside the window has the same property.
		nearExpiry := subject.latestExpiry <= deadline && snapshotExpiry > deadline &&
			leaseLifetime > renewWindow.Milliseconds()
		if !lowBalance && !nearExpiry {
			continue
		}
		funding := &EdgeLeaseFunding{
			db: model.DB, requestContext: ctx,
			relayInfo: &relaycommon.RelayInfo{UserId: subject.userID, TokenId: subject.tokenID},
		}
		if err := funding.acquireLease(0, true); err != nil {
			return err
		}
	}
	client, ok := ActiveEdgeControlClient()
	if !ok {
		return nil
	}
	return closeSupersededEdgeLeases(ctx, client)
}
