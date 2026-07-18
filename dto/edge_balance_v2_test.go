package dto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeBalanceDeltaV2Validate(t *testing.T) {
	valid := EdgeBalanceDeltaV2{
		Dataset:                          EdgeBalanceDatasetBalancesV2,
		BaseRevision:                     0,
		Revision:                         1,
		Full:                             true,
		SettlementAppliedThroughSequence: 3,
		Wallets: []EdgeWalletBalanceV2{
			{UserID: 1, RemainQuota: 100},
			{UserID: 2, RemainQuota: -10},
		},
		Tokens: []EdgeTokenBalanceV2{
			{TokenID: 10, UserID: 1, RemainQuota: 50},
			{TokenID: 11, UserID: 2, UnlimitedQuota: true},
		},
		Subscriptions: []EdgeSubscriptionBalanceV2{{
			SubscriptionID:       20,
			UserID:               1,
			TotalQuota:           1000,
			RemainQuota:          750,
			NextResetAtUnixMilli: 1_800_000_000_000,
			ExpiresAtUnixMilli:   1_900_000_000_000,
			AllowWalletOverflow:  true,
		}},
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeBalanceDeltaV2)
	}{
		{name: "invalid dataset", mutate: func(value *EdgeBalanceDeltaV2) { value.Dataset = "quota" }},
		{name: "negative base revision", mutate: func(value *EdgeBalanceDeltaV2) { value.BaseRevision = -1 }},
		{name: "non advancing revision", mutate: func(value *EdgeBalanceDeltaV2) { value.Revision = value.BaseRevision }},
		{name: "negative settlement sequence", mutate: func(value *EdgeBalanceDeltaV2) { value.SettlementAppliedThroughSequence = -1 }},
		{name: "full tombstone", mutate: func(value *EdgeBalanceDeltaV2) { value.Wallets[0].Deleted = true; value.Wallets[0].RemainQuota = 0 }},
		{name: "unordered wallets", mutate: func(value *EdgeBalanceDeltaV2) {
			value.Wallets[0], value.Wallets[1] = value.Wallets[1], value.Wallets[0]
		}},
		{name: "duplicate token", mutate: func(value *EdgeBalanceDeltaV2) { value.Tokens = append(value.Tokens, value.Tokens[1]) }},
		{name: "unlimited token quota", mutate: func(value *EdgeBalanceDeltaV2) { value.Tokens[1].RemainQuota = 1 }},
		{name: "subscription reset after expiry", mutate: func(value *EdgeBalanceDeltaV2) {
			value.Subscriptions[0].NextResetAtUnixMilli = value.Subscriptions[0].ExpiresAtUnixMilli + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := valid
			value.Wallets = append([]EdgeWalletBalanceV2(nil), valid.Wallets...)
			value.Tokens = append([]EdgeTokenBalanceV2(nil), valid.Tokens...)
			value.Subscriptions = append([]EdgeSubscriptionBalanceV2(nil), valid.Subscriptions...)
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	incremental := EdgeBalanceDeltaV2{
		Dataset:      EdgeBalanceDatasetBalancesV2,
		BaseRevision: 4,
		Revision:     5,
		Wallets:      []EdgeWalletBalanceV2{{UserID: 3, Deleted: true}},
		Tokens:       []EdgeTokenBalanceV2{{TokenID: 7, UserID: 3, Deleted: true}},
		Subscriptions: []EdgeSubscriptionBalanceV2{{
			SubscriptionID: 9,
			UserID:         3,
			Deleted:        true,
		}},
	}
	assert.NoError(t, incremental.Validate())
}

func TestEdgeControlMetaAcceptsV2AndRejectsUnknownProtocol(t *testing.T) {
	request := EdgeControlRequestMetaV1{ProtocolVersion: EdgeControlProtocolVersionV2, RequestID: "request-v2"}
	response := EdgeControlResponseMetaV1{
		ProtocolVersion:     EdgeControlProtocolVersionV2,
		RequestID:           "request-v2",
		ServerRequestID:     "server-v2",
		ServerTimeUnixMilli: 1_800_000_000_000,
	}
	require.NoError(t, request.Validate())
	require.NoError(t, response.Validate())

	request.ProtocolVersion = "edge-control.v3"
	response.ProtocolVersion = "edge-control.v3"
	assert.Error(t, request.Validate())
	assert.Error(t, response.Validate())
}
