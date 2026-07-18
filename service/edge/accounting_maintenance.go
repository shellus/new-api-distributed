package edge

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const edgeAccountingMaintenanceInterval = 15 * time.Second

// RunEdgeAccountingMaintenance retries only durable staged settlements. It
// never acquires remote funding or mutates policy state.
func RunEdgeAccountingMaintenance(ctx context.Context) {
	run := func() {
		if edgeAccountingBlock.Load() || edgeAccountingReady.Load() {
			return
		}
		if err := RecoverEdgeStagedSettlements(ctx, model.DB); err != nil && ctx.Err() == nil {
			common.SysError("edge accounting maintenance failed: " + err.Error())
		}
	}
	run()
	ticker := time.NewTicker(edgeAccountingMaintenanceInterval)
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
