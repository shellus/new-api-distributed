package edge

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DrainEdgeControl uploads every durable usage event. Balance replication has
// no remote lease to close or return.
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
	return flushAllEdgeSettlements(ctx, client, maxEvents)
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
