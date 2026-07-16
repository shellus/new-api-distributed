package edge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var edgeLeaseCloseMu sync.Mutex

// DrainEdgeControl uploads every durable usage event and then closes leases
// that have no in-flight reservation. Failures leave the SQLite state intact
// for the next start.
func DrainEdgeControl(ctx context.Context) error {
	client, ok := ActiveEdgeControlClient()
	if !ok {
		return errors.New("edge control client is unavailable")
	}
	maxEvents := 100
	if control, exists := ActiveEdgeControlConfig(); exists && control.SettlementMaxEvents > 0 {
		maxEvents = control.SettlementMaxEvents
	}
	return DrainEdgeControlWithClient(ctx, client, maxEvents)
}

// DrainEdgeControlWithClient lets the application retain the immutable control
// client across background-loop shutdown, so periodic settlement and lease
// maintenance are fully stopped before the final drain starts.
func DrainEdgeControlWithClient(ctx context.Context, client *EdgeControlClient, maxEvents int) error {
	if ctx == nil {
		return errors.New("edge control drain context is nil")
	}
	if client == nil {
		return errors.New("edge control client is unavailable")
	}
	if maxEvents <= 0 {
		maxEvents = 100
	}
	if err := flushAllEdgeSettlements(ctx, client, maxEvents); err != nil {
		return err
	}
	return closeSettledEdgeLeases(ctx, client)
}

func flushAllEdgeSettlements(ctx context.Context, client *EdgeControlClient, maxEvents int) error {
	edgeSettlementUploadMu.Lock()
	defer edgeSettlementUploadMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		block, err := model.GetEdgeLocalPendingSettlementBlock(model.DB.WithContext(ctx))
		if errors.Is(err, gorm.ErrRecordNotFound) {
			meta, metaErr := client.NewRequestMeta("settlement")
			if metaErr != nil {
				return metaErr
			}
			block, err = model.BuildEdgeLocalSettlementBlock(
				model.DB.WithContext(ctx), meta, "block-"+uuid.NewString(), maxEvents, time.Now().UTC().UnixMilli(),
			)
		}
		if errors.Is(err, model.ErrEdgeLocalNoPendingUsageEvents) {
			return nil
		}
		if err != nil {
			return err
		}
		response, err := client.SubmitSettlement(ctx, *block)
		if err != nil {
			return err
		}
		if err := model.AcknowledgeEdgeLocalSettlementBlock(model.DB.WithContext(ctx), response.Ack); err != nil {
			return err
		}
	}
}

func closeSettledEdgeLeases(ctx context.Context, client *EdgeControlClient) error {
	edgeLeaseCloseMu.Lock()
	defer edgeLeaseCloseMu.Unlock()
	var leases []model.EdgeLocalQuotaLease
	if err := model.DB.WithContext(ctx).
		Where("status = ? AND reserved_quota = ?", dto.EdgeLeaseStatusActiveV1, 0).
		Order("issued_at_unix_milli asc, lease_id asc").Find(&leases).Error; err != nil {
		return err
	}
	return closeEdgeLeaseList(ctx, client, leases)
}

func closeSupersededEdgeLeases(ctx context.Context, client *EdgeControlClient) error {
	edgeLeaseCloseMu.Lock()
	defer edgeLeaseCloseMu.Unlock()
	snapshot, err := model.GetEdgeLocalSnapshotState(model.DB.WithContext(ctx))
	if err != nil {
		return err
	}
	var active []model.EdgeLocalQuotaLease
	if err := model.DB.WithContext(ctx).
		Where("status = ? AND reserved_quota = ?", dto.EdgeLeaseStatusActiveV1, 0).
		Order("user_id asc, token_id asc, issued_at_unix_milli desc, lease_id asc").Find(&active).Error; err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	newestCurrent := make(map[string]string)
	for _, lease := range active {
		if lease.SnapshotID != snapshot.SnapshotID || lease.SnapshotRevision != snapshot.Revision || lease.ExpiresAtUnixMilli <= now {
			continue
		}
		key := fmt.Sprintf("%d:%d", lease.UserID, lease.TokenID)
		if _, exists := newestCurrent[key]; !exists {
			newestCurrent[key] = lease.LeaseID
		}
	}
	eligible := make([]model.EdgeLocalQuotaLease, 0, len(active))
	for _, lease := range active {
		key := fmt.Sprintf("%d:%d", lease.UserID, lease.TokenID)
		if lease.RemainingQuota == 0 || lease.ExpiresAtUnixMilli <= now ||
			lease.SnapshotID != snapshot.SnapshotID || lease.SnapshotRevision != snapshot.Revision ||
			(newestCurrent[key] != "" && newestCurrent[key] != lease.LeaseID) {
			eligible = append(eligible, lease)
		}
	}
	return closeEdgeLeaseList(ctx, client, eligible)
}

func closeEdgeLeaseList(ctx context.Context, client *EdgeControlClient, leases []model.EdgeLocalQuotaLease) error {
	var closeErrors []error
	for _, lease := range leases {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(closeErrors, err)...)
		}
		var unacknowledged int64
		if err := model.DB.WithContext(ctx).Model(&model.EdgeLocalUsageEvent{}).
			Where("lease_id = ? AND acknowledged = ?", lease.LeaseID, false).Count(&unacknowledged).Error; err != nil {
			closeErrors = append(closeErrors, err)
			continue
		}
		if unacknowledged != 0 {
			continue
		}
		var finalSequence int64
		if err := model.DB.WithContext(ctx).Model(&model.EdgeLocalUsageEvent{}).
			Where("lease_id = ?", lease.LeaseID).Select("COALESCE(MAX(sequence), 0)").Scan(&finalSequence).Error; err != nil {
			closeErrors = append(closeErrors, err)
			continue
		}
		response, err := client.CloseLease(ctx, dto.EdgeLeaseCloseRequestV1{
			LeaseID: lease.LeaseID, LeaseVersion: lease.Version, FinalEventSequence: finalSequence,
		})
		if err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close lease %s: %w", lease.LeaseID, err))
			continue
		}
		if response.LeaseID != lease.LeaseID || response.GrantedQuota != lease.GrantedQuota ||
			response.AcceptedQuota != lease.ConsumedQuota || response.ReturnedQuota != lease.RemainingQuota {
			closeErrors = append(closeErrors, fmt.Errorf("close lease %s: authoritative accounting does not match local state", lease.LeaseID))
			continue
		}
		updates := map[string]any{"version": response.LeaseVersion, "status": response.Status, "updated_at_unix_milli": time.Now().UnixMilli()}
		result := model.DB.WithContext(ctx).Model(&model.EdgeLocalQuotaLease{}).
			Where("lease_id = ? AND version = ? AND status = ?", lease.LeaseID, lease.Version, dto.EdgeLeaseStatusActiveV1).
			Updates(updates)
		if result.Error != nil {
			closeErrors = append(closeErrors, result.Error)
		} else if result.RowsAffected != 1 {
			closeErrors = append(closeErrors, fmt.Errorf("close lease %s: local version changed", lease.LeaseID))
		}
	}
	return errors.Join(closeErrors...)
}
