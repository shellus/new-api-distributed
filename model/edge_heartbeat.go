package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"gorm.io/gorm"
)

// EdgeNodeHeartbeat is the latest master-side observation reported by one
// authenticated edge generation. It stores only the typed control DTO; CPA
// addresses, credentials and other deployment secrets cannot enter it.
type EdgeNodeHeartbeat struct {
	ID                int64  `json:"id" gorm:"primaryKey"`
	NodeID            int64  `json:"node_id" gorm:"not null;uniqueIndex"`
	Generation        int64  `json:"generation" gorm:"type:bigint;not null;index"`
	SnapshotUID       string `json:"snapshot_uid" gorm:"type:varchar(64);not null"`
	SnapshotRevision  int64  `json:"snapshot_revision" gorm:"type:bigint;not null"`
	LastAckedSequence int64  `json:"last_acked_sequence" gorm:"type:bigint;not null"`
	BalanceRevision   int64  `json:"balance_revision" gorm:"type:bigint;not null"`
	PendingEventCount int64  `json:"pending_event_count" gorm:"type:bigint;not null"`
	PendingBlockCount int64  `json:"pending_block_count" gorm:"type:bigint;not null"`
	InFlightRequests  int64  `json:"in_flight_requests" gorm:"type:bigint;not null"`
	SnapshotPayload   string `json:"snapshot_payload" gorm:"type:text;not null"`
	SettlementPayload string `json:"settlement_payload" gorm:"type:text;not null"`
	LeasesPayload     string `json:"leases_payload" gorm:"type:text;not null"`
	RuntimePayload    string `json:"runtime_payload" gorm:"type:text;not null"`
	CPAPayload        string `json:"cpa_payload" gorm:"type:text;not null"`
	ObservedAt        int64  `json:"observed_at" gorm:"type:bigint;not null;index"`
	CreatedAt         int64  `json:"created_at" gorm:"type:bigint;not null"`
	UpdatedAt         int64  `json:"updated_at" gorm:"type:bigint;not null;index"`
}

type EdgeNodeHeartbeatObservation struct {
	Snapshot        dto.EdgeSnapshotStateV1
	Settlement      dto.EdgeSettlementStateV1
	BalanceRevision int64
	Leases          []dto.EdgeLeaseRuntimeStateV1
	Runtime         dto.EdgeRuntimeStatusV1
	CPA             []dto.EdgeCPAStatusV1
	ObservedAt      int64
}

func UpsertEdgeNodeHeartbeatTx(tx *gorm.DB, nodeID int64, generation int64, observation EdgeNodeHeartbeatObservation) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if nodeID <= 0 || generation <= 0 {
		return errors.New("invalid edge heartbeat identity")
	}
	if err := observation.Snapshot.Validate(); err != nil {
		return err
	}
	if err := observation.Settlement.Validate(); err != nil {
		return err
	}
	if observation.BalanceRevision < 0 {
		return errors.New("edge heartbeat balance revision must not be negative")
	}
	if err := observation.Runtime.Validate(); err != nil {
		return err
	}
	for i := range observation.Leases {
		if err := observation.Leases[i].Validate(); err != nil {
			return err
		}
	}
	for i := range observation.CPA {
		if err := observation.CPA[i].Validate(); err != nil {
			return err
		}
	}
	if observation.ObservedAt <= 0 {
		observation.ObservedAt = common.GetTimestamp()
	}

	snapshotPayload, err := common.Marshal(observation.Snapshot)
	if err != nil {
		return err
	}
	settlementPayload, err := common.Marshal(observation.Settlement)
	if err != nil {
		return err
	}
	leasesPayload, err := common.Marshal(observation.Leases)
	if err != nil {
		return err
	}
	runtimePayload, err := common.Marshal(observation.Runtime)
	if err != nil {
		return err
	}
	cpaPayload, err := common.Marshal(observation.CPA)
	if err != nil {
		return err
	}

	values := map[string]any{
		"generation":          generation,
		"snapshot_uid":        observation.Snapshot.SnapshotID,
		"snapshot_revision":   observation.Snapshot.Revision,
		"last_acked_sequence": observation.Settlement.LastAckedSequence,
		"balance_revision":    observation.BalanceRevision,
		"pending_event_count": observation.Settlement.PendingEventCount,
		"pending_block_count": observation.Settlement.PendingBlockCount,
		"in_flight_requests":  observation.Runtime.InFlightRequests,
		"snapshot_payload":    string(snapshotPayload),
		"settlement_payload":  string(settlementPayload),
		"leases_payload":      string(leasesPayload),
		"runtime_payload":     string(runtimePayload),
		"cpa_payload":         string(cpaPayload),
		"observed_at":         observation.ObservedAt,
		"updated_at":          observation.ObservedAt,
	}
	var heartbeat EdgeNodeHeartbeat
	query := tx.Where("node_id = ?", nodeID).Limit(1).Find(&heartbeat)
	if query.Error != nil {
		return query.Error
	}
	if query.RowsAffected == 0 {
		heartbeat = EdgeNodeHeartbeat{
			NodeID:            nodeID,
			Generation:        generation,
			SnapshotUID:       observation.Snapshot.SnapshotID,
			SnapshotRevision:  observation.Snapshot.Revision,
			LastAckedSequence: observation.Settlement.LastAckedSequence,
			BalanceRevision:   observation.BalanceRevision,
			PendingEventCount: observation.Settlement.PendingEventCount,
			PendingBlockCount: observation.Settlement.PendingBlockCount,
			InFlightRequests:  observation.Runtime.InFlightRequests,
			SnapshotPayload:   string(snapshotPayload),
			SettlementPayload: string(settlementPayload),
			LeasesPayload:     string(leasesPayload),
			RuntimePayload:    string(runtimePayload),
			CPAPayload:        string(cpaPayload),
			ObservedAt:        observation.ObservedAt,
			CreatedAt:         observation.ObservedAt,
			UpdatedAt:         observation.ObservedAt,
		}
		return tx.Create(&heartbeat).Error
	}
	return tx.Model(&EdgeNodeHeartbeat{}).Where("id = ?", heartbeat.ID).Updates(values).Error
}
