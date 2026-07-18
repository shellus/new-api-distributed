package model

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"gorm.io/gorm"
)

// EdgeBalanceReplicationState stores the last confirmed and at most one
// pending canonical balance vector for one edge node. JSON is stored as TEXT
// so the schema remains identical on SQLite, MySQL and PostgreSQL.
type EdgeBalanceReplicationState struct {
	ID                          int64  `json:"id" gorm:"primaryKey"`
	NodeID                      int64  `json:"node_id" gorm:"not null;uniqueIndex"`
	NodeGeneration              int64  `json:"node_generation" gorm:"type:bigint;not null;index"`
	ConfirmedRevision           int64  `json:"confirmed_revision" gorm:"type:bigint;not null"`
	ConfirmedVectorPayload      string `json:"confirmed_vector_payload" gorm:"type:text;not null"`
	ConfirmedSettlementSequence int64  `json:"confirmed_settlement_sequence" gorm:"type:bigint;not null"`
	PendingRevision             int64  `json:"pending_revision" gorm:"type:bigint;not null"`
	PendingVectorPayload        string `json:"pending_vector_payload" gorm:"type:text;not null"`
	PendingDeltaPayload         string `json:"pending_delta_payload" gorm:"type:text;not null"`
	PendingSettlementSequence   int64  `json:"pending_settlement_sequence" gorm:"type:bigint;not null"`
	CreatedAt                   int64  `json:"created_at" gorm:"type:bigint;not null"`
	UpdatedAt                   int64  `json:"updated_at" gorm:"type:bigint;not null;index"`
}

type edgeBalanceVectorV2 struct {
	Wallets       []dto.EdgeWalletBalanceV2       `json:"wallets"`
	Tokens        []dto.EdgeTokenBalanceV2        `json:"tokens"`
	Subscriptions []dto.EdgeSubscriptionBalanceV2 `json:"subscriptions"`
}

// PrepareEdgeBalanceDeltaTx advances the per-node confirmed/pending state
// machine and returns the exact full/delta payload that the heartbeat must
// deliver. The caller must hold the node row lock for cross-dialect
// serialization; this function additionally locks an existing replication row.
func PrepareEdgeBalanceDeltaTx(
	tx *gorm.DB,
	nodeID int64,
	nodeGeneration int64,
	requestRevision int64,
	settlementSequence int64,
	now time.Time,
) (*dto.EdgeBalanceDeltaV2, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if nodeID <= 0 || nodeGeneration <= 0 || requestRevision < 0 || settlementSequence < 0 {
		return nil, errors.New("invalid edge balance replication input")
	}
	if now.IsZero() {
		now = time.Now()
	}

	var state EdgeBalanceReplicationState
	query := lockForUpdate(tx).Where("node_id = ?", nodeID).Limit(1).Find(&state)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected == 0 {
		state = EdgeBalanceReplicationState{
			NodeID:         nodeID,
			NodeGeneration: nodeGeneration,
			CreatedAt:      now.Unix(),
			UpdatedAt:      now.Unix(),
		}
		if err := tx.Create(&state).Error; err != nil {
			return nil, err
		}
	}

	nextFrom := max(requestRevision, state.ConfirmedRevision, state.PendingRevision)
	forceFull := false
	if state.NodeGeneration != nodeGeneration {
		forceFull = true
		state.NodeGeneration = nodeGeneration
		state.ConfirmedRevision = requestRevision
		state.ConfirmedVectorPayload = ""
		state.ConfirmedSettlementSequence = 0
		state.PendingRevision = 0
		state.PendingVectorPayload = ""
		state.PendingDeltaPayload = ""
		state.PendingSettlementSequence = 0
	}

	if !forceFull && state.PendingRevision > 0 && requestRevision == state.PendingRevision {
		state.ConfirmedRevision = state.PendingRevision
		state.ConfirmedVectorPayload = state.PendingVectorPayload
		state.ConfirmedSettlementSequence = state.PendingSettlementSequence
		state.PendingRevision = 0
		state.PendingVectorPayload = ""
		state.PendingDeltaPayload = ""
		state.PendingSettlementSequence = 0
		state.UpdatedAt = now.Unix()
		if err := tx.Model(&EdgeBalanceReplicationState{}).Where("id = ?", state.ID).Updates(map[string]any{
			"node_generation":               state.NodeGeneration,
			"confirmed_revision":            state.ConfirmedRevision,
			"confirmed_vector_payload":      state.ConfirmedVectorPayload,
			"confirmed_settlement_sequence": state.ConfirmedSettlementSequence,
			"pending_revision":              int64(0),
			"pending_vector_payload":        "",
			"pending_delta_payload":         "",
			"pending_settlement_sequence":   int64(0),
			"updated_at":                    state.UpdatedAt,
		}).Error; err != nil {
			return nil, err
		}
	} else if !forceFull && state.PendingRevision > 0 && requestRevision == state.ConfirmedRevision {
		return decodePendingEdgeBalanceDelta(&state)
	}

	if !forceFull && state.PendingRevision == 0 && requestRevision != state.ConfirmedRevision {
		forceFull = true
	}
	if !forceFull && requestRevision == 0 && state.ConfirmedRevision > 0 {
		forceFull = true
	}
	if !forceFull && state.PendingRevision > 0 {
		forceFull = true
	}

	target, err := readAuthoritativeEdgeBalanceVectorTx(tx, now)
	if err != nil {
		return nil, err
	}
	targetPayload, err := common.Marshal(target)
	if err != nil {
		return nil, err
	}

	baseRevision := state.ConfirmedRevision
	if forceFull {
		baseRevision = requestRevision
	}
	if nextFrom == math.MaxInt64 {
		return nil, errors.New("edge balance revision exhausted")
	}

	var delta dto.EdgeBalanceDeltaV2
	if forceFull || state.ConfirmedRevision == 0 || state.ConfirmedVectorPayload == "" {
		delta = dto.EdgeBalanceDeltaV2{
			Dataset:                          dto.EdgeBalanceDatasetBalancesV2,
			BaseRevision:                     baseRevision,
			Revision:                         nextFrom + 1,
			Full:                             true,
			SettlementAppliedThroughSequence: settlementSequence,
			Wallets:                          target.Wallets,
			Tokens:                           target.Tokens,
			Subscriptions:                    target.Subscriptions,
		}
	} else {
		var confirmed edgeBalanceVectorV2
		if err := common.UnmarshalJsonStr(state.ConfirmedVectorPayload, &confirmed); err != nil {
			forceFull = true
			delta = dto.EdgeBalanceDeltaV2{
				Dataset:                          dto.EdgeBalanceDatasetBalancesV2,
				BaseRevision:                     requestRevision,
				Revision:                         nextFrom + 1,
				Full:                             true,
				SettlementAppliedThroughSequence: settlementSequence,
				Wallets:                          target.Wallets,
				Tokens:                           target.Tokens,
				Subscriptions:                    target.Subscriptions,
			}
		} else {
			delta = diffEdgeBalanceVectors(confirmed, target)
			delta.Dataset = dto.EdgeBalanceDatasetBalancesV2
			delta.BaseRevision = state.ConfirmedRevision
			delta.Revision = state.ConfirmedRevision + 1
			delta.SettlementAppliedThroughSequence = settlementSequence
			if len(delta.Wallets) == 0 && len(delta.Tokens) == 0 && len(delta.Subscriptions) == 0 &&
				settlementSequence == state.ConfirmedSettlementSequence {
				return nil, nil
			}
		}
	}
	if err := delta.Validate(); err != nil {
		return nil, err
	}
	deltaPayload, err := common.Marshal(delta)
	if err != nil {
		return nil, err
	}
	state.PendingRevision = delta.Revision
	state.PendingVectorPayload = string(targetPayload)
	state.PendingDeltaPayload = string(deltaPayload)
	state.PendingSettlementSequence = settlementSequence
	state.UpdatedAt = now.Unix()
	if err := tx.Model(&EdgeBalanceReplicationState{}).Where("id = ?", state.ID).Updates(map[string]any{
		"node_generation":               nodeGeneration,
		"confirmed_revision":            state.ConfirmedRevision,
		"confirmed_vector_payload":      state.ConfirmedVectorPayload,
		"confirmed_settlement_sequence": state.ConfirmedSettlementSequence,
		"pending_revision":              state.PendingRevision,
		"pending_vector_payload":        state.PendingVectorPayload,
		"pending_delta_payload":         state.PendingDeltaPayload,
		"pending_settlement_sequence":   state.PendingSettlementSequence,
		"updated_at":                    state.UpdatedAt,
	}).Error; err != nil {
		return nil, err
	}
	return &delta, nil
}

func decodePendingEdgeBalanceDelta(state *EdgeBalanceReplicationState) (*dto.EdgeBalanceDeltaV2, error) {
	if state == nil || state.PendingRevision <= 0 || state.PendingDeltaPayload == "" {
		return nil, errors.New("edge balance pending state is incomplete")
	}
	var delta dto.EdgeBalanceDeltaV2
	if err := common.UnmarshalJsonStr(state.PendingDeltaPayload, &delta); err != nil {
		return nil, err
	}
	if err := delta.Validate(); err != nil {
		return nil, err
	}
	return &delta, nil
}

func readAuthoritativeEdgeBalanceVectorTx(tx *gorm.DB, now time.Time) (edgeBalanceVectorV2, error) {
	vector := edgeBalanceVectorV2{
		Wallets:       []dto.EdgeWalletBalanceV2{},
		Tokens:        []dto.EdgeTokenBalanceV2{},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{},
	}
	var users []User
	if err := tx.Select("id", "quota").Order("id asc").Find(&users).Error; err != nil {
		return vector, err
	}
	for i := range users {
		quota := int64(users[i].Quota)
		if err := validateAuthoritativeEdgeBalanceQuota("user quota", quota); err != nil {
			return vector, fmt.Errorf("user %d: %w", users[i].Id, err)
		}
		vector.Wallets = append(vector.Wallets, dto.EdgeWalletBalanceV2{UserID: int64(users[i].Id), RemainQuota: quota})
	}

	var tokens []Token
	if err := tx.Select("id", "user_id", "remain_quota", "unlimited_quota").Order("id asc").Find(&tokens).Error; err != nil {
		return vector, err
	}
	for i := range tokens {
		quota := int64(tokens[i].RemainQuota)
		if tokens[i].UnlimitedQuota {
			quota = 0
		} else if err := validateAuthoritativeEdgeBalanceQuota("token quota", quota); err != nil {
			return vector, fmt.Errorf("token %d: %w", tokens[i].Id, err)
		}
		vector.Tokens = append(vector.Tokens, dto.EdgeTokenBalanceV2{
			TokenID:        int64(tokens[i].Id),
			UserID:         int64(tokens[i].UserId),
			RemainQuota:    quota,
			UnlimitedQuota: tokens[i].UnlimitedQuota,
		})
	}

	var subscriptions []UserSubscription
	if err := tx.Select("id", "user_id", "amount_total", "amount_used", "next_reset_time", "end_time", "allow_wallet_overflow").
		Where("status = ? AND end_time > ?", "active", now.Unix()).
		Order("id asc").Find(&subscriptions).Error; err != nil {
		return vector, err
	}
	for i := range subscriptions {
		subscription := subscriptions[i]
		if subscription.AmountTotal < 0 || subscription.AmountUsed < 0 {
			return vector, fmt.Errorf("subscription %d quotas must not be negative", subscription.Id)
		}
		expiresAt, err := edgeBalanceUnixMilli(subscription.EndTime)
		if err != nil {
			return vector, fmt.Errorf("subscription %d expiry: %w", subscription.Id, err)
		}
		nextResetAt, err := edgeBalanceUnixMilli(subscription.NextResetTime)
		if err != nil {
			return vector, fmt.Errorf("subscription %d reset: %w", subscription.Id, err)
		}
		item := dto.EdgeSubscriptionBalanceV2{
			SubscriptionID:       int64(subscription.Id),
			UserID:               int64(subscription.UserId),
			NextResetAtUnixMilli: nextResetAt,
			ExpiresAtUnixMilli:   expiresAt,
			AllowWalletOverflow:  subscription.AllowWalletOverflow,
		}
		if subscription.AmountTotal == 0 {
			item.UnlimitedQuota = true
		} else {
			if err := validateAuthoritativeEdgeBalanceQuota("subscription total quota", subscription.AmountTotal); err != nil {
				return vector, fmt.Errorf("subscription %d total quota: %w", subscription.Id, err)
			}
			item.TotalQuota = subscription.AmountTotal
			item.RemainQuota = subscription.AmountTotal - subscription.AmountUsed
			if err := validateAuthoritativeEdgeBalanceQuota("subscription remain quota", item.RemainQuota); err != nil {
				return vector, fmt.Errorf("subscription %d: %w", subscription.Id, err)
			}
		}
		vector.Subscriptions = append(vector.Subscriptions, item)
	}
	if len(vector.Wallets) == 0 {
		vector.Wallets = nil
	}
	if len(vector.Tokens) == 0 {
		vector.Tokens = nil
	}
	if len(vector.Subscriptions) == 0 {
		vector.Subscriptions = nil
	}
	return vector, nil
}

func validateAuthoritativeEdgeBalanceQuota(field string, value int64) error {
	if value < -int64(common.MaxQuota) || value > int64(common.MaxQuota) {
		return fmt.Errorf("%s is outside [%d,%d]", field, -common.MaxQuota, common.MaxQuota)
	}
	return nil
}

func edgeBalanceUnixMilli(seconds int64) (int64, error) {
	if seconds == 0 {
		return 0, nil
	}
	if seconds < 0 || seconds > math.MaxInt64/1000 {
		return 0, errors.New("Unix seconds cannot be represented as milliseconds")
	}
	return seconds * 1000, nil
}

func diffEdgeBalanceVectors(confirmed edgeBalanceVectorV2, target edgeBalanceVectorV2) dto.EdgeBalanceDeltaV2 {
	delta := dto.EdgeBalanceDeltaV2{}
	confirmedWallets := make(map[int64]dto.EdgeWalletBalanceV2, len(confirmed.Wallets))
	for _, item := range confirmed.Wallets {
		confirmedWallets[item.UserID] = item
	}
	for _, item := range target.Wallets {
		if previous, exists := confirmedWallets[item.UserID]; !exists || previous != item {
			delta.Wallets = append(delta.Wallets, item)
		}
		delete(confirmedWallets, item.UserID)
	}
	for _, item := range confirmed.Wallets {
		if _, exists := confirmedWallets[item.UserID]; exists {
			delta.Wallets = append(delta.Wallets, dto.EdgeWalletBalanceV2{UserID: item.UserID, Deleted: true})
		}
	}

	confirmedTokens := make(map[int64]dto.EdgeTokenBalanceV2, len(confirmed.Tokens))
	for _, item := range confirmed.Tokens {
		confirmedTokens[item.TokenID] = item
	}
	for _, item := range target.Tokens {
		if previous, exists := confirmedTokens[item.TokenID]; !exists || previous != item {
			delta.Tokens = append(delta.Tokens, item)
		}
		delete(confirmedTokens, item.TokenID)
	}
	for _, item := range confirmed.Tokens {
		if _, exists := confirmedTokens[item.TokenID]; exists {
			delta.Tokens = append(delta.Tokens, dto.EdgeTokenBalanceV2{TokenID: item.TokenID, UserID: item.UserID, Deleted: true})
		}
	}

	confirmedSubscriptions := make(map[int64]dto.EdgeSubscriptionBalanceV2, len(confirmed.Subscriptions))
	for _, item := range confirmed.Subscriptions {
		confirmedSubscriptions[item.SubscriptionID] = item
	}
	for _, item := range target.Subscriptions {
		if previous, exists := confirmedSubscriptions[item.SubscriptionID]; !exists || previous != item {
			delta.Subscriptions = append(delta.Subscriptions, item)
		}
		delete(confirmedSubscriptions, item.SubscriptionID)
	}
	for _, item := range confirmed.Subscriptions {
		if _, exists := confirmedSubscriptions[item.SubscriptionID]; exists {
			delta.Subscriptions = append(delta.Subscriptions, dto.EdgeSubscriptionBalanceV2{
				SubscriptionID: item.SubscriptionID,
				UserID:         item.UserID,
				Deleted:        true,
			})
		}
	}
	sort.Slice(delta.Wallets, func(i, j int) bool { return delta.Wallets[i].UserID < delta.Wallets[j].UserID })
	sort.Slice(delta.Tokens, func(i, j int) bool { return delta.Tokens[i].TokenID < delta.Tokens[j].TokenID })
	sort.Slice(delta.Subscriptions, func(i, j int) bool {
		return delta.Subscriptions[i].SubscriptionID < delta.Subscriptions[j].SubscriptionID
	})
	return delta
}
