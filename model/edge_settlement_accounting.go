package model

import (
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var (
	ErrEdgeSettlementSubscriptionUnavailable = errors.New("edge settlement subscription is unavailable")
)

// EdgeSettlementStateConflict describes a verified edge event that cannot be
// applied to the current authoritative business state. It is intentionally
// distinct from database/integrity errors so the caller can skip only this
// event without advancing past an untrusted or uncommitted block.
type EdgeSettlementStateConflict struct {
	Code  string
	Cause error
}

func (e *EdgeSettlementStateConflict) Error() string {
	if e == nil {
		return "edge settlement authoritative state conflict"
	}
	if e.Cause == nil {
		return fmt.Sprintf("edge settlement authoritative state conflict: %s", e.Code)
	}
	return fmt.Sprintf("edge settlement authoritative state conflict: %s: %v", e.Code, e.Cause)
}

func (e *EdgeSettlementStateConflict) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewEdgeSettlementStateConflict(code string, cause error) error {
	return &EdgeSettlementStateConflict{Code: code, Cause: cause}
}

func EdgeSettlementStateConflictCode(err error) (string, bool) {
	var conflict *EdgeSettlementStateConflict
	if !errors.As(err, &conflict) || conflict == nil || conflict.Code == "" {
		return "", false
	}
	return conflict.Code, true
}

type EdgeSettlementChargeResult struct {
	SubscriptionID        int
	SubscriptionPlanID    int
	SubscriptionPlanTitle string
	SubscriptionTotal     int64
	SubscriptionUsed      int64
}

func ApplyEdgeSettlementChargeTx(
	tx *gorm.DB,
	userID int,
	tokenID int,
	fundingSource string,
	userSubscriptionID int,
	tokenUnlimitedQuota bool,
	quota int64,
) (*EdgeSettlementChargeResult, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if userID <= 0 || tokenID <= 0 || quota < 0 || quota > int64(common.MaxQuota) {
		return nil, errors.New("invalid edge settlement charge")
	}
	var user User
	if err := lockForUpdate(tx.Unscoped()).First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, NewEdgeSettlementStateConflict("current_user_missing", err)
		}
		return nil, err
	}
	var token Token
	if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, NewEdgeSettlementStateConflict("current_token_missing_or_reassigned", err)
		}
		return nil, err
	}
	if token.UnlimitedQuota != tokenUnlimitedQuota {
		return nil, NewEdgeSettlementStateConflict(
			"current_token_quota_mode_changed",
			errors.New("edge settlement token quota mode conflicts with authoritative state"),
		)
	}
	result := &EdgeSettlementChargeResult{SubscriptionID: userSubscriptionID}
	var subscription *UserSubscription
	switch fundingSource {
	case "wallet":
		if userSubscriptionID != 0 {
			return nil, errors.New("wallet edge settlement contains a subscription ID")
		}
	case "subscription":
		if userSubscriptionID <= 0 {
			return nil, errors.New("subscription edge settlement is missing its subscription ID")
		}
		stored := &UserSubscription{}
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", userSubscriptionID, userID).First(stored).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, NewEdgeSettlementStateConflict(
					"current_subscription_missing_or_reassigned",
					fmt.Errorf("%w: user_id=%d subscription_id=%d", ErrEdgeSettlementSubscriptionUnavailable, userID, userSubscriptionID),
				)
			}
			return nil, err
		}
		subscription = stored
		result.SubscriptionPlanID = stored.PlanId
		result.SubscriptionTotal = stored.AmountTotal
		if stored.PlanId > 0 {
			var plan SubscriptionPlan
			query := tx.Select("id", "title").Limit(1).Find(&plan, stored.PlanId)
			if query.Error != nil {
				return nil, query.Error
			}
			if query.RowsAffected == 1 {
				result.SubscriptionPlanTitle = plan.Title
			}
		}
	default:
		return nil, errors.New("invalid edge settlement funding source")
	}

	if quota > 0 {
		switch fundingSource {
		case "wallet":
			updated, clamp := common.QuotaFromDecimalChecked(decimal.NewFromInt(int64(user.Quota)).Sub(decimal.NewFromInt(quota)))
			if clamp != nil {
				return nil, NewEdgeSettlementStateConflict("current_wallet_quota_out_of_range", clamp)
			}
			user.Quota = updated
			if err := tx.Unscoped().Save(&user).Error; err != nil {
				return nil, err
			}
		case "subscription":
			updated, clamp := common.QuotaFromDecimalChecked(decimal.NewFromInt(subscription.AmountUsed).Add(decimal.NewFromInt(quota)))
			if clamp != nil {
				return nil, NewEdgeSettlementStateConflict("current_subscription_quota_out_of_range", clamp)
			}
			subscription.AmountUsed = int64(updated)
			subscription.UpdatedAt = common.GetTimestamp()
			if err := tx.Save(subscription).Error; err != nil {
				return nil, err
			}
		}
		if !tokenUnlimitedQuota {
			updated, clamp := common.QuotaFromDecimalChecked(decimal.NewFromInt(int64(token.RemainQuota)).Sub(decimal.NewFromInt(quota)))
			if clamp != nil {
				return nil, NewEdgeSettlementStateConflict("current_token_quota_out_of_range", clamp)
			}
			token.RemainQuota = updated
			if err := tx.Unscoped().Save(&token).Error; err != nil {
				return nil, err
			}
		}
	}
	if subscription != nil {
		result.SubscriptionUsed = subscription.AmountUsed
	}
	return result, nil
}

// AddEdgeSettlementStatsTx updates consumption statistics after authoritative
// wallet/subscription and token balances have been charged in the same transaction.
func AddEdgeSettlementStatsTx(tx *gorm.DB, userID int, tokenID int, channelID int, quota int64, accessedAt int64) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if userID <= 0 || tokenID <= 0 || channelID <= 0 || quota < 0 || quota > int64(common.MaxQuota) || accessedAt <= 0 {
		return errors.New("invalid edge settlement statistics")
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
		return NewEdgeSettlementStateConflict("user_statistics_unavailable", nil)
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
		return NewEdgeSettlementStateConflict("token_statistics_unavailable", nil)
	}

	channelResult := tx.Model(&Channel{}).
		Where("id = ? AND used_quota <= ?", channelID, int64(math.MaxInt64)-quota).
		Update("used_quota", gorm.Expr("used_quota + ?", quota))
	if channelResult.Error != nil {
		return channelResult.Error
	}
	if channelResult.RowsAffected != 1 {
		return NewEdgeSettlementStateConflict("channel_statistics_unavailable", nil)
	}
	return nil
}
