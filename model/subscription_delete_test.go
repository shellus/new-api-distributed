package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminDeleteUserSubscriptionRetainsCancelledAccountingRow(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	subscription := &UserSubscription{
		Id:          9901,
		UserId:      991,
		PlanId:      992,
		AmountTotal: 1_000,
		AmountUsed:  375,
		StartTime:   now - 3_600,
		EndTime:     now + 3_600,
		Status:      "active",
		Source:      "admin",
	}
	require.NoError(t, DB.Create(subscription).Error)

	_, err := AdminDeleteUserSubscription(subscription.Id)
	require.NoError(t, err)

	var retained UserSubscription
	require.NoError(t, DB.First(&retained, subscription.Id).Error)
	assert.Equal(t, "cancelled", retained.Status)
	assert.Equal(t, int64(375), retained.AmountUsed)
	assert.LessOrEqual(t, retained.EndTime, GetDBTimestamp())
}

func TestEdgeSettlementChargesRetainedCancelledSubscription(t *testing.T) {
	truncateTables(t)

	user := &User{Id: 993, Username: "retained-subscription-user", Quota: 1_000}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{Id: 994, UserId: user.Id, Key: "retained-subscription-token", RemainQuota: 1_000}
	require.NoError(t, DB.Create(token).Error)

	_, err := ApplyEdgeSettlementChargeTx(DB, user.Id, token.Id, "subscription", 995, false, 25)
	require.ErrorIs(t, err, ErrEdgeSettlementSubscriptionUnavailable)

	subscription := &UserSubscription{
		Id:          995,
		UserId:      user.Id,
		AmountTotal: 1_000,
		AmountUsed:  375,
		Status:      "cancelled",
	}
	require.NoError(t, DB.Create(subscription).Error)

	result, err := ApplyEdgeSettlementChargeTx(DB, user.Id, token.Id, "subscription", subscription.Id, false, 25)
	require.NoError(t, err)
	assert.Equal(t, int64(400), result.SubscriptionUsed)

	var retained UserSubscription
	require.NoError(t, DB.First(&retained, subscription.Id).Error)
	assert.Equal(t, "cancelled", retained.Status)
	assert.Equal(t, int64(400), retained.AmountUsed)
}
