package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedSubscriptionRenewPlan(t *testing.T, plan *SubscriptionPlan) {
	t.Helper()
	require.NoError(t, DB.Create(plan).Error)
}

func seedSubscriptionRenewSub(t *testing.T, sub *UserSubscription) {
	t.Helper()
	require.NoError(t, DB.Create(sub).Error)
}

func TestAdminRenewUserSubscriptionExtendsSameRowWhenNearExpiry(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:               9701,
		Title:            "Renewable",
		DurationUnit:     SubscriptionDurationDay,
		DurationValue:    14,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	seedSubscriptionRenewPlan(t, plan)

	sub := &UserSubscription{
		Id:            9801,
		UserId:        9802,
		PlanId:        plan.Id,
		AmountTotal:   1000,
		AmountUsed:    275,
		StartTime:     now - 8*24*3600,
		EndTime:       now + 6*24*3600,
		Status:        "active",
		NextResetTime: now + 3600,
	}
	seedSubscriptionRenewSub(t, sub)

	result, err := AdminRenewUserSubscription(sub.Id, 7*24*3600)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Renewed)
	assert.Equal(t, sub.Id, result.Subscription.Id)
	assert.Equal(t, int64(275), result.Subscription.AmountUsed)
	assert.Equal(t, sub.NextResetTime, result.Subscription.NextResetTime)
	assert.GreaterOrEqual(t, result.Subscription.EndTime, now+14*24*3600)

	var stored UserSubscription
	require.NoError(t, DB.First(&stored, sub.Id).Error)
	assert.Equal(t, sub.Id, stored.Id)
	assert.Equal(t, result.Subscription.EndTime, stored.EndTime)
}

func TestAdminRenewUserSubscriptionIsIdempotentAboveThreshold(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:            9711,
		Title:         "Not Yet Due",
		DurationUnit:  SubscriptionDurationDay,
		DurationValue: 14,
	}
	seedSubscriptionRenewPlan(t, plan)
	sub := &UserSubscription{
		Id:        9811,
		UserId:    9812,
		PlanId:    plan.Id,
		StartTime: now,
		EndTime:   now + 8*24*3600,
		Status:    "active",
	}
	seedSubscriptionRenewSub(t, sub)

	result, err := AdminRenewUserSubscription(sub.Id, 7*24*3600)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Renewed)
	assert.Equal(t, sub.EndTime, result.Subscription.EndTime)
}

func TestAdminRenewUserSubscriptionRejectsExpiredRow(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{Id: 9721, Title: "Expired", DurationUnit: SubscriptionDurationDay, DurationValue: 14}
	seedSubscriptionRenewPlan(t, plan)
	seedSubscriptionRenewSub(t, &UserSubscription{
		Id:        9821,
		UserId:    9822,
		PlanId:    plan.Id,
		StartTime: now - 2*24*3600,
		EndTime:   now - 1,
		Status:    "active",
	})

	_, err := AdminRenewUserSubscription(9821, 7*24*3600)

	assert.ErrorIs(t, err, ErrUserSubscriptionNotActive)
}
