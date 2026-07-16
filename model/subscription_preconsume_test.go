package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdjustAndRefundSubscriptionPreConsumeKeepsRecordAndQuotaAtomic(t *testing.T) {
	truncateTables(t)

	subscription := &UserSubscription{
		Id:          9801,
		UserId:      981,
		AmountTotal: 1_000,
		AmountUsed:  100,
		Status:      "active",
	}
	require.NoError(t, DB.Create(subscription).Error)
	record := &SubscriptionPreConsumeRecord{
		RequestId:          "subscription-adjust-1",
		UserId:             subscription.UserId,
		UserSubscriptionId: subscription.Id,
		PreConsumed:        100,
		Status:             "consumed",
	}
	require.NoError(t, DB.Create(record).Error)

	require.NoError(t, AdjustSubscriptionPreConsume(record.RequestId, 50))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 150, 150, "consumed")

	require.NoError(t, AdjustSubscriptionPreConsume(record.RequestId, -50))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 100, 100, "consumed")

	require.NoError(t, RefundSubscriptionPreConsume(record.RequestId))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 0, 100, "refunded")

	// The record status makes the refund idempotent; a retry cannot credit the
	// subscription twice.
	require.NoError(t, RefundSubscriptionPreConsume(record.RequestId))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 0, 100, "refunded")
}

func TestAdjustSubscriptionPreConsumeRejectsBoundsWithoutPartialUpdate(t *testing.T) {
	truncateTables(t)

	subscription := &UserSubscription{
		Id:          9821,
		UserId:      982,
		AmountTotal: 200,
		AmountUsed:  100,
		Status:      "active",
	}
	require.NoError(t, DB.Create(subscription).Error)
	record := &SubscriptionPreConsumeRecord{
		RequestId:          "subscription-adjust-2",
		UserId:             subscription.UserId,
		UserSubscriptionId: subscription.Id,
		PreConsumed:        100,
		Status:             "consumed",
	}
	require.NoError(t, DB.Create(record).Error)

	require.Error(t, AdjustSubscriptionPreConsume(record.RequestId, 101))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 100, 100, "consumed")

	require.Error(t, AdjustSubscriptionPreConsume(record.RequestId, -101))
	assertSubscriptionPreConsumeState(t, subscription.Id, record.RequestId, 100, 100, "consumed")
}

func assertSubscriptionPreConsumeState(t *testing.T, subscriptionID int, requestID string, used int64, reserved int64, status string) {
	t.Helper()
	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, used, subscription.AmountUsed)

	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&record).Error)
	assert.Equal(t, reserved, record.PreConsumed)
	assert.Equal(t, status, record.Status)
}
