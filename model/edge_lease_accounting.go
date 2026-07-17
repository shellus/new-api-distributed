package model

import (
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

var (
	ErrEdgeLeaseUserUnavailable         = errors.New("edge lease user is unavailable")
	ErrEdgeLeaseTokenUnavailable        = errors.New("edge lease token is unavailable")
	ErrEdgeLeaseWalletQuotaInsufficient = errors.New("edge lease wallet quota is insufficient")
	ErrEdgeLeaseTokenQuotaInsufficient  = errors.New("edge lease token quota is insufficient")
	ErrEdgeLeaseSubscriptionUnavailable = errors.New("edge lease subscription quota is unavailable")
	ErrEdgeLeaseQuotaCounterOverflow    = errors.New("edge lease quota counter would overflow")
)

// LockEdgeLeaseSubjectTx serializes authoritative reservations for one user
// and token. The token must belong to the user; status, expiry and snapshot
// policy checks remain service-layer decisions.
func LockEdgeLeaseSubjectTx(tx *gorm.DB, userID int, tokenID int) (*User, *Token, error) {
	if tx == nil {
		return nil, nil, errors.New("database is nil")
	}
	if userID <= 0 || tokenID <= 0 {
		return nil, nil, errors.New("invalid edge lease subject")
	}
	var user User
	if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
		return nil, nil, err
	}
	var token Token
	if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
		return nil, nil, err
	}
	return &user, &token, nil
}

func ReserveEdgeLeaseWalletTx(tx *gorm.DB, userID int, quota int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if userID <= 0 || quota <= 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease wallet reservation")
	}
	result := tx.Model(&User{}).
		Where("id = ? AND status = ? AND quota >= ?", userID, common.UserStatusEnabled, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLeaseWalletQuotaInsufficient
	}
	return nil
}

func RefundEdgeLeaseWalletTx(tx *gorm.DB, userID int, quota int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if userID <= 0 || quota < 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease wallet refund")
	}
	if quota == 0 {
		return nil
	}
	result := tx.Unscoped().Model(&User{}).
		Where("id = ? AND quota <= ?", userID, int64(common.MaxQuota)-quota).
		Update("quota", gorm.Expr("quota + ?", quota))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}
	return nil
}

func ReserveEdgeLeaseTokenTx(tx *gorm.DB, tokenID int, quota int64, unlimited bool) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if tokenID <= 0 || quota <= 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease token reservation")
	}
	if unlimited {
		return nil
	}
	result := tx.Model(&Token{}).
		Where("id = ? AND status = ? AND unlimited_quota = ? AND remain_quota >= ?", tokenID, common.TokenStatusEnabled, false, quota).
		Update("remain_quota", gorm.Expr("remain_quota - ?", quota))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLeaseTokenQuotaInsufficient
	}
	return nil
}

func RefundEdgeLeaseTokenTx(tx *gorm.DB, tokenID int, quota int64, unlimited bool) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if tokenID <= 0 || quota < 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease token refund")
	}
	if quota == 0 || unlimited {
		return nil
	}
	result := tx.Unscoped().Model(&Token{}).
		Where("id = ? AND unlimited_quota = ? AND remain_quota <= ?", tokenID, false, int64(common.MaxQuota)-quota).
		Update("remain_quota", gorm.Expr("remain_quota + ?", quota))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}
	return nil
}

// LockEdgeLeaseSubscriptionsTx returns active subscriptions in the same order
// as ordinary subscription pre-consume. Due reset periods are applied inside
// the caller's transaction before capacities are inspected.
func LockEdgeLeaseSubscriptionsTx(tx *gorm.DB, userID int, now int64) ([]UserSubscription, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if userID <= 0 || now <= 0 {
		return nil, errors.New("invalid edge lease subscription query")
	}
	var subscriptions []UserSubscription
	if err := lockForUpdate(tx).
		Where("user_id = ? AND status = ? AND end_time > ?", userID, "active", now).
		Order("end_time asc, id asc").
		Find(&subscriptions).Error; err != nil {
		return nil, err
	}
	for i := range subscriptions {
		plan, err := getSubscriptionPlanByIdTx(tx, subscriptions[i].PlanId)
		if err != nil {
			return nil, err
		}
		if err := maybeResetUserSubscriptionWithPlanTx(tx, &subscriptions[i], plan, now); err != nil {
			return nil, err
		}
	}
	return subscriptions, nil
}

func EdgeLeaseSubscriptionAvailableQuota(subscription *UserSubscription) int64 {
	if subscription == nil || subscription.Status != "active" {
		return 0
	}
	if subscription.AmountTotal == 0 {
		return int64(common.MaxQuota)
	}
	available := subscription.AmountTotal - subscription.AmountUsed
	if available < 0 {
		return 0
	}
	if available > int64(common.MaxQuota) {
		return int64(common.MaxQuota)
	}
	return available
}

func ReserveEdgeLeaseSubscriptionTx(tx *gorm.DB, subscriptionID int, now int64, quota int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if subscriptionID <= 0 || now <= 0 || quota <= 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease subscription reservation")
	}
	result := tx.Model(&UserSubscription{}).
		Where("id = ? AND status = ? AND end_time > ? AND (amount_total = 0 OR amount_used <= amount_total - ?)",
			subscriptionID, "active", now, quota).
		Updates(map[string]any{
			"amount_used": gorm.Expr("amount_used + ?", quota),
			"updated_at":  common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeLeaseSubscriptionUnavailable
	}
	return nil
}

func RefundEdgeLeaseSubscriptionTx(tx *gorm.DB, subscriptionID int, quota int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if subscriptionID <= 0 || quota < 0 || quota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease subscription refund")
	}
	if quota == 0 {
		return nil
	}
	result := tx.Model(&UserSubscription{}).
		Where("id = ? AND amount_used >= ?", subscriptionID, quota).
		Updates(map[string]any{
			"amount_used": gorm.Expr("amount_used - ?", quota),
			"updated_at":  common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%w: subscription reservation is smaller than refund", ErrEdgeLeaseSubscriptionUnavailable)
	}
	return nil
}

// AddEdgeLeaseSettlementStatsTx updates only consumption statistics. Wallet,
// subscription and token remain_quota were already reserved at lease issue and
// must not be deducted a second time here.
func AddEdgeLeaseSettlementStatsTx(tx *gorm.DB, userID int, tokenID int, channelID int, quota int64, accessedAt int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if userID <= 0 || tokenID <= 0 || channelID <= 0 || quota < 0 || quota > int64(common.MaxQuota) || accessedAt <= 0 {
		return errors.New("invalid edge lease settlement statistics")
	}
	userResult := tx.Unscoped().Model(&User{}).
		Where("id = ? AND used_quota <= ? AND request_count < ?", userID, int64(math.MaxInt64)-quota, int64(math.MaxInt64)).
		Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + 1"),
		})
	if userResult.Error != nil {
		return userResult.Error
	}
	if userResult.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}

	tokenResult := tx.Unscoped().Model(&Token{}).
		Where("id = ? AND used_quota <= ?", tokenID, int64(math.MaxInt64)-quota).
		Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": accessedAt,
		})
	if tokenResult.Error != nil {
		return tokenResult.Error
	}
	if tokenResult.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}

	channelResult := tx.Model(&Channel{}).
		Where("id = ? AND used_quota <= ?", channelID, int64(math.MaxInt64)-quota).
		Update("used_quota", gorm.Expr("used_quota + ?", quota))
	if channelResult.Error != nil {
		return channelResult.Error
	}
	if channelResult.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}
	return nil
}

// AddEdgeLeaseForfeitureStatsTx accounts quota deliberately not returned by a
// force close. It has no channel or request-count attribution because no
// durable usage event was reported for the forfeited remainder.
func AddEdgeLeaseForfeitureStatsTx(tx *gorm.DB, userID int, tokenID int, quota int64, accessedAt int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if userID <= 0 || tokenID <= 0 || quota < 0 || quota > int64(common.MaxQuota) || accessedAt <= 0 {
		return errors.New("invalid edge lease forfeiture statistics")
	}
	if quota == 0 {
		return nil
	}
	userResult := tx.Unscoped().Model(&User{}).
		Where("id = ? AND used_quota <= ?", userID, int64(math.MaxInt64)-quota).
		Update("used_quota", gorm.Expr("used_quota + ?", quota))
	if userResult.Error != nil {
		return userResult.Error
	}
	if userResult.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}
	tokenResult := tx.Unscoped().Model(&Token{}).
		Where("id = ? AND used_quota <= ?", tokenID, int64(math.MaxInt64)-quota).
		Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": accessedAt,
		})
	if tokenResult.Error != nil {
		return tokenResult.Error
	}
	if tokenResult.RowsAffected != 1 {
		return ErrEdgeLeaseQuotaCounterOverflow
	}
	return nil
}
