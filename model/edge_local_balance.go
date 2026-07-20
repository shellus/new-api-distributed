package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
)

type EdgeLocalBalanceState struct {
	Revision               int64
	Initialized            bool
	SettlementSequence     int64
	SettlementCircuitOpen  bool
	SettlementCircuitEpoch int64
}

type EdgeLocalBalanceReservationRequest struct {
	ReservationID        string
	RequestID            string
	UserID               int64
	TokenID              int64
	Quota                int64
	SettlementFloorQuota int64
	NowUnixMilli         int64
}

func BindEdgeLocalReservationOwner(db *gorm.DB, reservationID, ownerKind, ownerID string, nowUnixMilli int64) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local reservation owner binding requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return err
	}
	if err := validateEdgeLocalIdentifier(ownerKind); err != nil {
		return err
	}
	if err := validateEdgeLocalIdentifier(ownerID); err != nil {
		return err
	}
	if nowUnixMilli <= 0 {
		return errors.New("edge local reservation owner binding time must be positive")
	}
	result := db.Model(&EdgeLocalQuotaReservation{}).
		Where("reservation_id = ? AND status = ? AND staged_event_payload = '' AND (owner_kind = '' OR (owner_kind = ? AND owner_id = ?))",
			reservationID, EdgeLocalReservationStatusActive, ownerKind, ownerID).
		Updates(map[string]any{"owner_kind": ownerKind, "owner_id": ownerID, "updated_at_unix_milli": nowUnixMilli})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLocalReservationConflict
	}
	return nil
}

func ListActiveEdgeLocalOwnedReservations(db *gorm.DB, ownerKind string) ([]EdgeLocalQuotaReservation, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local owned reservation query requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(ownerKind); err != nil {
		return nil, err
	}
	var reservations []EdgeLocalQuotaReservation
	if err := db.Where("status = ? AND owner_kind = ?", EdgeLocalReservationStatusActive, ownerKind).
		Order("created_at_unix_milli asc, reservation_id asc").Find(&reservations).Error; err != nil {
		return nil, err
	}
	return reservations, nil
}

func GetEdgeLocalBalanceState(db *gorm.DB) (*EdgeLocalBalanceState, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local balance state requires SQLite")
	}
	var control EdgeLocalControlState
	if err := db.First(&control, edgeLocalControlStateID).Error; err != nil {
		return nil, err
	}
	return &EdgeLocalBalanceState{
		Revision: control.BalanceRevision, Initialized: control.BalanceInitialized,
		SettlementSequence:     control.BalanceSettlementSequence,
		SettlementCircuitOpen:  control.SettlementCircuitOpen,
		SettlementCircuitEpoch: control.SettlementCircuitEpoch,
	}, nil
}

func ApplyEdgeLocalControlConfig(db *gorm.DB, control dto.EdgeNodeControlConfigV1, nowUnixMilli int64) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local control application requires SQLite")
	}
	if err := control.Validate(); err != nil {
		return err
	}
	if nowUnixMilli <= 0 {
		return errors.New("edge local control application time must be positive")
	}
	result := db.Model(&EdgeLocalControlState{}).Where("id = ?", edgeLocalControlStateID).Updates(map[string]any{
		"node_id": control.NodeID, "node_generation": control.NodeGeneration,
		"settlement_circuit_open":  control.SettlementCircuitOpen,
		"settlement_circuit_epoch": control.SettlementCircuitEpoch,
		"updated_at_unix_milli":    nowUnixMilli,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLocalAccountingCorruption
	}
	return nil
}

func ApplyEdgeLocalBalanceDelta(db *gorm.DB, node dto.EdgeNodeControlConfigV1, delta dto.EdgeBalanceDeltaV2, nowUnixMilli int64) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local balance application requires SQLite")
	}
	if err := edgeauth.ValidateNodeID(node.NodeID); err != nil {
		return err
	}
	if node.NodeGeneration <= 0 || nowUnixMilli <= 0 {
		return errors.New("edge local balance control identity is invalid")
	}
	if err := delta.Validate(); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if control.BalanceInitialized && delta.Revision == control.BalanceRevision {
			return nil
		}
		if !control.BalanceInitialized && !delta.Full {
			return ErrEdgeLocalSnapshotMismatch
		}
		if control.BalanceInitialized && !delta.Full && delta.BaseRevision != control.BalanceRevision {
			return ErrEdgeLocalSnapshotMismatch
		}
		if delta.Revision <= control.BalanceRevision || delta.SettlementAppliedThroughSequence < control.BalanceSettlementSequence {
			return ErrEdgeLocalSnapshotStale
		}

		if err := clearEdgeLocalSettledOverlayTx(tx, control.BalanceSettlementSequence, delta.SettlementAppliedThroughSequence, nowUnixMilli); err != nil {
			return err
		}
		if delta.Full {
			if err := tx.Model(&EdgeLocalBalanceAccount{}).Where("deleted = ?", false).Updates(map[string]any{
				"deleted": true, "balance_revision": delta.Revision, "updated_at_unix_milli": nowUnixMilli,
			}).Error; err != nil {
				return err
			}
		}
		for _, item := range delta.Wallets {
			if err := applyEdgeLocalBalanceAccountTx(tx, EdgeLocalBalanceAccount{
				AccountType: EdgeBalanceAccountTypeWallet, AccountID: item.UserID, UserID: item.UserID,
				ReplicatedQuota: item.RemainQuota, Deleted: item.Deleted,
				BalanceRevision: delta.Revision, UpdatedAtUnixMilli: nowUnixMilli,
			}); err != nil {
				return err
			}
		}
		for _, item := range delta.Tokens {
			if err := applyEdgeLocalBalanceAccountTx(tx, EdgeLocalBalanceAccount{
				AccountType: EdgeBalanceAccountTypeToken, AccountID: item.TokenID, UserID: item.UserID,
				ReplicatedQuota: item.RemainQuota, UnlimitedQuota: item.UnlimitedQuota, Deleted: item.Deleted,
				BalanceRevision: delta.Revision, UpdatedAtUnixMilli: nowUnixMilli,
			}); err != nil {
				return err
			}
		}
		for _, item := range delta.Subscriptions {
			if err := applyEdgeLocalBalanceAccountTx(tx, EdgeLocalBalanceAccount{
				AccountType: EdgeBalanceAccountTypeSubscription, AccountID: item.SubscriptionID, UserID: item.UserID,
				ReplicatedQuota: item.RemainQuota, UnlimitedQuota: item.UnlimitedQuota, TotalQuota: item.TotalQuota,
				NextResetAtUnixMilli: item.NextResetAtUnixMilli, ExpiresAtUnixMilli: item.ExpiresAtUnixMilli,
				AllowWalletOverflow: item.AllowWalletOverflow, Deleted: item.Deleted,
				BalanceRevision: delta.Revision, UpdatedAtUnixMilli: nowUnixMilli,
			}); err != nil {
				return err
			}
		}
		if err := tx.Where("deleted = ? AND reserved_quota = 0 AND unsettled_quota = 0", true).
			Delete(&EdgeLocalBalanceAccount{}).Error; err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalControlState{}).Where("id = ?", edgeLocalControlStateID).Updates(map[string]any{
			"node_id": node.NodeID, "node_generation": node.NodeGeneration,
			"balance_revision": delta.Revision, "balance_initialized": true,
			"balance_settlement_sequence": delta.SettlementAppliedThroughSequence,
			"updated_at_unix_milli":       nowUnixMilli,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalAccountingCorruption
		}
		return nil
	})
}

func applyEdgeLocalBalanceAccountTx(tx *gorm.DB, incoming EdgeLocalBalanceAccount) error {
	var existing EdgeLocalBalanceAccount
	query := tx.Where("account_type = ? AND account_id = ?", incoming.AccountType, incoming.AccountID).Limit(1).Find(&existing)
	if query.Error != nil {
		return query.Error
	}
	if query.RowsAffected == 0 {
		return tx.Create(&incoming).Error
	}
	return tx.Model(&EdgeLocalBalanceAccount{}).
		Where("account_type = ? AND account_id = ?", incoming.AccountType, incoming.AccountID).
		Updates(map[string]any{
			"user_id": incoming.UserID, "replicated_quota": incoming.ReplicatedQuota,
			"unlimited_quota": incoming.UnlimitedQuota, "total_quota": incoming.TotalQuota,
			"next_reset_at_unix_milli": incoming.NextResetAtUnixMilli,
			"expires_at_unix_milli":    incoming.ExpiresAtUnixMilli,
			"allow_wallet_overflow":    incoming.AllowWalletOverflow, "deleted": incoming.Deleted,
			"balance_revision": incoming.BalanceRevision, "updated_at_unix_milli": incoming.UpdatedAtUnixMilli,
		}).Error
}

func clearEdgeLocalSettledOverlayTx(tx *gorm.DB, afterSequence, throughSequence, nowUnixMilli int64) error {
	if throughSequence <= afterSequence {
		return nil
	}
	var stored []EdgeLocalUsageEvent
	if err := tx.Where("sequence > ? AND sequence <= ?", afterSequence, throughSequence).Order("sequence asc").Find(&stored).Error; err != nil {
		return err
	}
	for i := range stored {
		var event dto.EdgeUsageEventV1
		if err := common.Unmarshal([]byte(stored[i].Payload), &event); err != nil {
			return ErrEdgeLocalAccountingCorruption
		}
		if event.Sequence != stored[i].Sequence {
			return ErrEdgeLocalAccountingCorruption
		}
		accountType := EdgeBalanceAccountTypeWallet
		accountID := event.UserID
		if event.FundingSource == "subscription" {
			accountType = EdgeBalanceAccountTypeSubscription
			accountID = event.UserSubscriptionID
		}
		if err := decreaseEdgeLocalUnsettledTx(tx, accountType, accountID, event.Billing.ChargedQuota, nowUnixMilli); err != nil {
			return err
		}
		if !event.TokenUnlimitedQuota {
			if err := decreaseEdgeLocalUnsettledTx(tx, EdgeBalanceAccountTypeToken, event.TokenID, event.Billing.ChargedQuota, nowUnixMilli); err != nil {
				return err
			}
		}
	}
	return nil
}

func decreaseEdgeLocalUnsettledTx(tx *gorm.DB, accountType EdgeBalanceAccountType, accountID, quota, nowUnixMilli int64) error {
	result := tx.Model(&EdgeLocalBalanceAccount{}).
		Where("account_type = ? AND account_id = ? AND unsettled_quota >= ?", accountType, accountID, quota).
		Updates(map[string]any{
			"unsettled_quota": gorm.Expr("unsettled_quota - ?", quota), "updated_at_unix_milli": nowUnixMilli,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLocalAccountingCorruption
	}
	return nil
}

func ReserveEdgeLocalBalance(db *gorm.DB, request EdgeLocalBalanceReservationRequest) (*EdgeLocalQuotaReservation, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local balance reservation requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(request.ReservationID); err != nil {
		return nil, err
	}
	if err := validateEdgeLocalIdentifier(request.RequestID); err != nil {
		return nil, err
	}
	if request.UserID <= 0 || request.TokenID <= 0 || request.NowUnixMilli <= 0 {
		return nil, errors.New("edge local balance reservation identity is invalid")
	}
	if err := validateEdgeLocalQuota(request.Quota, true); err != nil {
		return nil, err
	}
	if request.SettlementFloorQuota < -int64(common.MaxQuota) || request.SettlementFloorQuota > 0 {
		return nil, errors.New("edge local settlement floor is invalid")
	}

	var reservation *EdgeLocalQuotaReservation
	err := db.Transaction(func(tx *gorm.DB) error {
		var existing EdgeLocalQuotaReservation
		query := tx.Where("reservation_id = ? OR request_id = ?", request.ReservationID, request.RequestID).Limit(1).Find(&existing)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected == 1 {
			if existing.ReservationID == request.ReservationID && existing.RequestID == request.RequestID &&
				existing.UserID == request.UserID && existing.TokenID == request.TokenID && existing.ReservedQuota == request.Quota &&
				existing.SettlementFloorQuota == request.SettlementFloorQuota && existing.FundingAccountType != "" {
				if existing.Status != EdgeLocalReservationStatusActive {
					return ErrEdgeLocalReservationFinalized
				}
				reservation = &existing
				return nil
			}
			return ErrEdgeLocalReservationConflict
		}
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if !control.BalanceInitialized || control.BalanceRevision <= 0 || control.NodeID == "" || control.NodeGeneration <= 0 {
			return ErrEdgeLocalSnapshotMismatch
		}
		var pricing EdgeLocalDatasetState
		if err := tx.Where("dataset = ?", dto.EdgeSnapshotDatasetPricingV1).First(&pricing).Error; err != nil {
			return ErrEdgeLocalSnapshotMismatch
		}
		user, err := GetEdgeLocalUser(tx, request.UserID)
		if err != nil || !user.Enabled {
			return ErrEdgeLocalSnapshotMismatch
		}
		funding, err := selectEdgeLocalFundingAccountTx(tx, request.UserID, common.NormalizeBillingPreference(user.Setting.BillingPreference), request.Quota, request.NowUnixMilli)
		if err != nil {
			return err
		}
		var token EdgeLocalBalanceAccount
		if err := tx.Where("account_type = ? AND account_id = ?", EdgeBalanceAccountTypeToken, request.TokenID).First(&token).Error; err != nil {
			return ErrEdgeLocalQuotaInsufficient
		}
		if token.UserID != request.UserID || token.Deleted {
			return ErrEdgeLocalQuotaInsufficient
		}
		if err := reserveEdgeLocalAccountTx(tx, funding, request.Quota, request.NowUnixMilli); err != nil {
			return err
		}
		if err := reserveEdgeLocalAccountTx(tx, &token, request.Quota, request.NowUnixMilli); err != nil {
			return err
		}
		created := &EdgeLocalQuotaReservation{
			ReservationID: request.ReservationID, RequestID: request.RequestID,
			UserID: request.UserID, TokenID: request.TokenID, Status: EdgeLocalReservationStatusActive,
			FundingAccountType: funding.AccountType, FundingAccountID: funding.AccountID,
			TokenAccountID: token.AccountID, TokenUnlimitedQuota: token.UnlimitedQuota,
			SnapshotID: control.SnapshotID, SnapshotRevision: control.SnapshotRevision,
			PricingRevision: pricing.Revision, BalanceRevision: control.BalanceRevision,
			SettlementFloorQuota: request.SettlementFloorQuota, ReservedQuota: request.Quota,
			CreatedAtUnixMilli: request.NowUnixMilli, UpdatedAtUnixMilli: request.NowUnixMilli,
		}
		if err := tx.Create(created).Error; err != nil {
			return err
		}
		reservation = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

func selectEdgeLocalFundingAccountTx(tx *gorm.DB, userID int64, preference string, quota, nowUnixMilli int64) (*EdgeLocalBalanceAccount, error) {
	var wallet EdgeLocalBalanceAccount
	walletErr := tx.Where("account_type = ? AND account_id = ? AND deleted = ?", EdgeBalanceAccountTypeWallet, userID, false).First(&wallet).Error
	var subscriptions []EdgeLocalBalanceAccount
	if err := tx.Where("account_type = ? AND user_id = ? AND deleted = ? AND expires_at_unix_milli > ? AND (next_reset_at_unix_milli = 0 OR next_reset_at_unix_milli > ?)",
		EdgeBalanceAccountTypeSubscription, userID, false, nowUnixMilli, nowUnixMilli).
		Order("expires_at_unix_milli asc, account_id asc").Find(&subscriptions).Error; err != nil {
		return nil, err
	}
	findSubscription := func() *EdgeLocalBalanceAccount {
		for i := range subscriptions {
			if edgeLocalAccountCanReserve(subscriptions[i], quota) {
				copy := subscriptions[i]
				return &copy
			}
		}
		return nil
	}
	walletAvailable := walletErr == nil && edgeLocalAccountCanReserve(wallet, quota)
	subscription := findSubscription()
	switch preference {
	case "wallet_only":
		if walletAvailable {
			return &wallet, nil
		}
	case "subscription_only":
		if subscription != nil {
			return subscription, nil
		}
	case "wallet_first":
		if walletAvailable {
			return &wallet, nil
		}
		if subscription != nil {
			return subscription, nil
		}
	default:
		if subscription != nil {
			return subscription, nil
		}
		allowWallet := len(subscriptions) == 0
		if len(subscriptions) > 0 {
			allowWallet = true
			for i := range subscriptions {
				if !subscriptions[i].AllowWalletOverflow {
					allowWallet = false
					break
				}
			}
		}
		if allowWallet && walletAvailable {
			return &wallet, nil
		}
	}
	return nil, ErrEdgeLocalQuotaInsufficient
}

func edgeLocalAccountCanReserve(account EdgeLocalBalanceAccount, quota int64) bool {
	if account.Deleted {
		return false
	}
	if account.UnlimitedQuota || quota == 0 {
		return true
	}
	return account.ReplicatedQuota-account.ReservedQuota-account.UnsettledQuota-quota >= 0
}

func reserveEdgeLocalAccountTx(tx *gorm.DB, account *EdgeLocalBalanceAccount, quota, nowUnixMilli int64) error {
	if account == nil || account.Deleted {
		return ErrEdgeLocalQuotaInsufficient
	}
	if account.UnlimitedQuota || quota == 0 {
		return nil
	}
	result := tx.Model(&EdgeLocalBalanceAccount{}).
		Where("account_type = ? AND account_id = ? AND deleted = ? AND unlimited_quota = ? AND replicated_quota - reserved_quota - unsettled_quota - ? >= 0",
			account.AccountType, account.AccountID, false, false, quota).
		Updates(map[string]any{
			"reserved_quota": gorm.Expr("reserved_quota + ?", quota), "updated_at_unix_milli": nowUnixMilli,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLocalQuotaInsufficient
	}
	account.ReservedQuota += quota
	return nil
}

func AdjustEdgeLocalBalanceReservation(db *gorm.DB, reservationID string, targetQuota, nowUnixMilli int64) (*EdgeLocalQuotaReservation, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local balance adjustment requires SQLite")
	}
	if err := validateEdgeLocalQuota(targetQuota, true); err != nil {
		return nil, err
	}
	var adjusted *EdgeLocalQuotaReservation
	err := db.Transaction(func(tx *gorm.DB) error {
		var reservation EdgeLocalQuotaReservation
		if err := tx.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
			return err
		}
		if reservation.Status != EdgeLocalReservationStatusActive || reservation.FundingAccountType == "" {
			return ErrEdgeLocalReservationFinalized
		}
		staged, err := edgeLocalReservationSettlementStaged(reservation)
		if err != nil {
			return err
		}
		if staged {
			return ErrEdgeLocalSettlementStaged
		}
		delta := targetQuota - reservation.ReservedQuota
		if delta != 0 {
			if err := adjustEdgeLocalAccountReservedTx(tx, reservation.FundingAccountType, reservation.FundingAccountID, delta, nowUnixMilli); err != nil {
				return err
			}
			if !reservation.TokenUnlimitedQuota {
				if err := adjustEdgeLocalAccountReservedTx(tx, EdgeBalanceAccountTypeToken, reservation.TokenAccountID, delta, nowUnixMilli); err != nil {
					return err
				}
			}
		}
		reservation.ReservedQuota = targetQuota
		reservation.UpdatedAtUnixMilli = nowUnixMilli
		if err := tx.Model(&EdgeLocalQuotaReservation{}).Where("reservation_id = ? AND status = ?", reservationID, EdgeLocalReservationStatusActive).
			Updates(map[string]any{"reserved_quota": targetQuota, "updated_at_unix_milli": nowUnixMilli}).Error; err != nil {
			return err
		}
		adjusted = &reservation
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adjusted, nil
}

func adjustEdgeLocalAccountReservedTx(tx *gorm.DB, accountType EdgeBalanceAccountType, accountID, delta, nowUnixMilli int64) error {
	if delta == 0 {
		return nil
	}
	query := tx.Model(&EdgeLocalBalanceAccount{}).Where("account_type = ? AND account_id = ? AND unlimited_quota = ?", accountType, accountID, false)
	if delta > 0 {
		query = query.Where("deleted = ? AND replicated_quota - reserved_quota - unsettled_quota - ? >= 0", false, delta)
	} else {
		query = query.Where("reserved_quota >= ?", -delta)
	}
	result := query.Updates(map[string]any{
		"reserved_quota": gorm.Expr("reserved_quota + ?", delta), "updated_at_unix_milli": nowUnixMilli,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		if delta > 0 {
			return ErrEdgeLocalQuotaInsufficient
		}
		return ErrEdgeLocalAccountingCorruption
	}
	return nil
}

func settleStagedEdgeLocalBalanceReservationTx(tx *gorm.DB, reservation *EdgeLocalQuotaReservation, event *dto.EdgeUsageEventV1) error {
	if reservation == nil || event == nil || reservation.FundingAccountType == "" {
		return ErrEdgeLocalAccountingCorruption
	}
	charged := event.Billing.ChargedQuota
	if err := settleEdgeLocalBalanceAccountTx(tx, reservation.FundingAccountType, reservation.FundingAccountID,
		reservation.ReservedQuota, charged, reservation.SettlementFloorQuota, event.FinishedAtUnixMilli); err != nil {
		return err
	}
	if !reservation.TokenUnlimitedQuota {
		if err := settleEdgeLocalBalanceAccountTx(tx, EdgeBalanceAccountTypeToken, reservation.TokenAccountID,
			reservation.ReservedQuota, charged, reservation.SettlementFloorQuota, event.FinishedAtUnixMilli); err != nil {
			return err
		}
	}
	return nil
}

func settleEdgeLocalBalanceAccountTx(tx *gorm.DB, accountType EdgeBalanceAccountType, accountID, reserved, charged, floor, nowUnixMilli int64) error {
	result := tx.Model(&EdgeLocalBalanceAccount{}).
		Where("account_type = ? AND account_id = ? AND reserved_quota >= ? AND replicated_quota - (reserved_quota - ?) - (unsettled_quota + ?) >= ?",
			accountType, accountID, reserved, reserved, charged, floor).
		Updates(map[string]any{
			"reserved_quota":        gorm.Expr("reserved_quota - ?", reserved),
			"unsettled_quota":       gorm.Expr("unsettled_quota + ?", charged),
			"updated_at_unix_milli": nowUnixMilli,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLocalQuotaInsufficient
	}
	return nil
}

func refundEdgeLocalBalanceReservationTx(tx *gorm.DB, reservation EdgeLocalQuotaReservation, nowUnixMilli int64) error {
	if reservation.FundingAccountType == "" {
		return ErrEdgeLocalAccountingCorruption
	}
	if reservation.ReservedQuota > 0 {
		if err := adjustEdgeLocalAccountReservedTx(tx, reservation.FundingAccountType, reservation.FundingAccountID,
			-reservation.ReservedQuota, nowUnixMilli); err != nil {
			return err
		}
		if !reservation.TokenUnlimitedQuota {
			if err := adjustEdgeLocalAccountReservedTx(tx, EdgeBalanceAccountTypeToken, reservation.TokenAccountID,
				-reservation.ReservedQuota, nowUnixMilli); err != nil {
				return err
			}
		}
	}
	return nil
}

func edgeLocalBalanceFundingSource(reservation EdgeLocalQuotaReservation) (string, int64, error) {
	switch reservation.FundingAccountType {
	case EdgeBalanceAccountTypeWallet:
		return "wallet", 0, nil
	case EdgeBalanceAccountTypeSubscription:
		return "subscription", reservation.FundingAccountID, nil
	default:
		return "", 0, fmt.Errorf("unsupported edge funding account type %q", reservation.FundingAccountType)
	}
}
