package edge

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	edgeAccountingMaintenanceInterval               = 15 * time.Second
	defaultEdgeLocalAccountingRetentionEvents int64 = 10_000
	minEdgeLocalAccountingRetentionEvents     int64 = 1_000
	maxEdgeLocalAccountingRetentionEvents     int64 = 1_000_000
	defaultEdgeLocalAccountingPruneBatchSize        = 100
	maxEdgeLocalAccountingPruneBatchSize            = 5_000
)

// RunEdgeAccountingMaintenance retries only durable staged settlements. It
// never acquires remote funding or mutates policy state.
func RunEdgeAccountingMaintenance(ctx context.Context) {
	retentionEvents := int64(common.GetEnvOrDefault("EDGE_LOCAL_ACCOUNTING_RETENTION_EVENTS", int(defaultEdgeLocalAccountingRetentionEvents)))
	if retentionEvents < minEdgeLocalAccountingRetentionEvents {
		retentionEvents = minEdgeLocalAccountingRetentionEvents
	}
	if retentionEvents > maxEdgeLocalAccountingRetentionEvents {
		retentionEvents = maxEdgeLocalAccountingRetentionEvents
	}
	pruneBatchSize := common.GetEnvOrDefault("EDGE_LOCAL_ACCOUNTING_PRUNE_BATCH_SIZE", defaultEdgeLocalAccountingPruneBatchSize)
	if pruneBatchSize < 1 {
		pruneBatchSize = 1
	}
	if pruneBatchSize > maxEdgeLocalAccountingPruneBatchSize {
		pruneBatchSize = maxEdgeLocalAccountingPruneBatchSize
	}

	run := func(pruneHistory bool) {
		if edgeAccountingBlock.Load() {
			return
		}
		if !edgeAccountingReady.Load() {
			if err := RecoverEdgeStagedSettlements(ctx, model.DB); err != nil && ctx.Err() == nil {
				common.SysError("edge accounting maintenance failed: " + err.Error())
				return
			}
		}
		if !edgeAccountingReady.Load() || ctx.Err() != nil {
			return
		}
		if err := ReconcileEdgeAccountingQuarantine(ctx, model.DB); err != nil {
			if ctx.Err() == nil {
				common.SysError("edge accounting quarantine reconciliation failed: " + err.Error())
			}
			return
		}
		if !edgeAccountingReady.Load() {
			return
		}
		if !pruneHistory {
			return
		}
		result, err := model.PruneEdgeLocalAccountingHistory(model.DB.WithContext(ctx), retentionEvents, pruneBatchSize)
		if err != nil {
			if ctx.Err() == nil {
				common.SysError("edge accounting history pruning failed: " + err.Error())
			}
			return
		}
		if result.DeletedRows() > 0 {
			common.SysLog(fmt.Sprintf(
				"edge accounting history pruned through sequence %d: reservations=%d usage=%d outbox=%d blocks=%d",
				result.ThroughSequence, result.Reservations, result.UsageEvents, result.OutboxEntries, result.SettlementBlocks,
			))
		}
	}
	// Startup already performs recovery and rebuilds quarantine state. Delay
	// history pruning until the first maintenance tick so initial control-plane
	// state and user traffic do not queue behind a background cleanup write.
	run(false)
	ticker := time.NewTicker(edgeAccountingMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run(true)
		}
	}
}
