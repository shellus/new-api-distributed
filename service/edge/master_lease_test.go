package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type masterLeaseTestFixture struct {
	db         *gorm.DB
	now        time.Time
	node       *model.EdgeNode
	credential *model.EdgeNodeCredential
	identity   *model.EdgeControlIdentity
	user       *model.User
	token      *model.Token
	channel    *model.Channel
	snapshot   *model.EdgeCompiledSnapshot
}

func TestMasterLeaseWalletSettlementReplayAndConfirmedClose(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "wallet-close", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-wallet-1", 1_000, 1_000)

	assertMasterLeaseBalances(t, fixture, 4_000, 0, 4_000, 0, 0)
	replayCommand := masterLeaseAcquireCommandForTest(fixture, "lease-wallet-1", 1_000, 1_000, strings.Repeat("a", 64))
	var replayedLease *model.EdgeQuotaLease
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		replayedLease, err = AcquireMasterQuotaLeaseTx(tx, fixture.identity, replayCommand)
		return err
	}))
	assert.Equal(t, lease.ID, replayedLease.ID)
	assertMasterLeaseBalances(t, fixture, 4_000, 0, 4_000, 0, 0)

	block := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
	var ack *dto.EdgeSettlementAckV1
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		ack, err = SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: block, IdempotencyKey: "settle-wallet-1", RequestHash: strings.Repeat("b", 64), Now: fixture.now.Add(time.Minute),
		})
		return err
	}))
	assert.Equal(t, dto.EdgeSettlementAckAcceptedV1, ack.Status)
	assert.Equal(t, int64(1), ack.AckedThroughSequence)
	assertMasterLeaseBalances(t, fixture, 4_000, 120, 4_000, 120, 120)

	var duplicate *dto.EdgeSettlementAckV1
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		duplicate, err = SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: block, IdempotencyKey: "settle-wallet-1", RequestHash: strings.Repeat("b", 64), Now: fixture.now.Add(2 * time.Minute),
		})
		return err
	}))
	assert.Equal(t, dto.EdgeSettlementAckDuplicateV1, duplicate.Status)
	assertMasterLeaseBalances(t, fixture, 4_000, 120, 4_000, 120, 120)

	var closeResponse *dto.EdgeLeaseCloseResponseV1
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		closeResponse, err = CloseMasterQuotaLeaseTx(tx, fixture.identity, MasterLeaseCloseCommand{
			Request: dto.EdgeLeaseCloseRequestV1{
				Meta: masterControlRequestMetaForTest("close-wallet-1"), LeaseID: lease.LeaseUID,
				LeaseVersion: lease.Version, FinalEventSequence: 1,
			},
			Now: fixture.now.Add(3 * time.Minute),
		})
		return err
	}))
	assert.Equal(t, dto.EdgeLeaseStatusClosedV1, closeResponse.Status)
	assert.Equal(t, int64(120), closeResponse.AcceptedQuota)
	assert.Equal(t, int64(880), closeResponse.ReturnedQuota)
	assertMasterLeaseBalances(t, fixture, 4_880, 120, 4_880, 120, 120)

	var storedLease model.EdgeQuotaLease
	require.NoError(t, fixture.db.First(&storedLease, lease.ID).Error)
	assert.Equal(t, model.EdgeQuotaLeaseStatusClosed, storedLease.Status)
	assert.Equal(t, int64(0), storedLease.RemainingQuota())
	var eventCount int64
	var outboxCount int64
	require.NoError(t, fixture.db.Model(&model.EdgeUsageEvent{}).Count(&eventCount).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeConsumeLogOutbox{}).Count(&outboxCount).Error)
	assert.Equal(t, int64(1), eventCount)
	assert.Equal(t, int64(1), outboxCount)
}

func TestMasterSettlementRejectsOutOfOrderCrossNodeAndExcess(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "settlement-guards", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-guards-1", 100, 100)

	tampered := masterSettlementBlockForTest(t, fixture, lease, 1, 100)
	tampered.Events[0].Usage.OpenAIUsage.PromptTokens++
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: tampered, IdempotencyKey: "settle-tampered-1", RequestHash: strings.Repeat("3", 64), Now: fixture.now.Add(time.Minute),
		})
		return settleErr
	})
	require.ErrorIs(t, err, ErrMasterSettlementConflict)
	assertMasterLeaseBalances(t, fixture, 4_900, 0, 4_900, 0, 0)
	var tamperedBlocks int64
	var tamperedEvents int64
	var tamperedOutboxes int64
	require.NoError(t, fixture.db.Model(&model.EdgeSettlementBlock{}).Count(&tamperedBlocks).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeUsageEvent{}).Count(&tamperedEvents).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeConsumeLogOutbox{}).Count(&tamperedOutboxes).Error)
	assert.Zero(t, tamperedBlocks)
	assert.Zero(t, tamperedEvents)
	assert.Zero(t, tamperedOutboxes)

	excess := masterSettlementBlockForTest(t, fixture, lease, 1, 60)
	excess.Events[0].Usage = dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 60, TotalTokens: 60})
	excess.Events[0].Billing.ReservedQuota = 60
	second := excess.Events[0]
	second.EventID = "event-2"
	second.Sequence = 2
	second.ReservationID = "reservation-2"
	second.RequestID = "request-2"
	excess.Events = append(excess.Events, second)
	excess.LastSequence = 2
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &excess))
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: excess, IdempotencyKey: "settle-excess-1", RequestHash: strings.Repeat("c", 64), Now: fixture.now.Add(time.Minute),
		})
		return settleErr
	})
	require.ErrorIs(t, err, ErrMasterLeaseQuotaExceeded)
	assertMasterLeaseBalances(t, fixture, 4_900, 0, 4_900, 0, 0)

	valid := masterSettlementBlockForTest(t, fixture, lease, 1, 100)
	valid.Events[0].Usage = dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 100})
	valid.Events[0].Billing.ReservedQuota = 100
	valid.Events[0].Billing.ChargedQuota = 100
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &valid))
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: valid, IdempotencyKey: "settle-valid-1", RequestHash: strings.Repeat("d", 64), Now: fixture.now.Add(2 * time.Minute),
		})
		return settleErr
	}))

	outOfOrder := masterSettlementBlockForTest(t, fixture, lease, 3, 0)
	outOfOrder.PreviousBlockID = valid.BlockID
	outOfOrder.PreviousBlockDigest = valid.BlockDigest
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &outOfOrder))
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: outOfOrder, IdempotencyKey: "settle-gap-1", RequestHash: strings.Repeat("e", 64), Now: fixture.now.Add(3 * time.Minute),
		})
		return settleErr
	})
	var sequenceErr *SettlementSequenceError
	require.ErrorAs(t, err, &sequenceErr)
	assert.Equal(t, int64(2), sequenceErr.Expected)

	other := newMasterLeaseTestIdentity(t, fixture.db, fixture.now, "edge.other", 10_000)
	crossNode := masterSettlementBlockForTest(t, fixture, lease, 1, 0)
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, other, MasterSettlementCommand{
			Request: crossNode, IdempotencyKey: "settle-cross-node-1", RequestHash: strings.Repeat("f", 64), Now: fixture.now.Add(3 * time.Minute),
		})
		return settleErr
	})
	require.ErrorIs(t, err, ErrMasterSettlementConflict)
}

func TestMasterSettlementRejectsInvalidTimelineWithoutMutation(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "settlement-timeline", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-timeline-1", 1_000, 1_000)
	t.Setenv("EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS", "120")
	t.Setenv("EDGE_MAX_INFLIGHT_REQUEST_SECONDS", "60")

	tests := []struct {
		name   string
		mutate func(*dto.EdgeSettlementBlockRequestV1)
		now    time.Time
	}{
		{
			name: "future event and block",
			mutate: func(block *dto.EdgeSettlementBlockRequestV1) {
				block.Events[0].StartedAtUnixMilli = fixture.now.Add(4 * time.Minute).UnixMilli()
				block.Events[0].FinishedAtUnixMilli = fixture.now.Add(5 * time.Minute).UnixMilli()
				block.CreatedAtUnixMilli = fixture.now.Add(6 * time.Minute).UnixMilli()
			},
			now: fixture.now.Add(time.Minute),
		},
		{
			name: "excessive in-flight duration",
			mutate: func(block *dto.EdgeSettlementBlockRequestV1) {
				block.Events[0].StartedAtUnixMilli = fixture.now.Add(10 * time.Second).UnixMilli()
				block.Events[0].FinishedAtUnixMilli = fixture.now.Add(2 * time.Minute).UnixMilli()
				block.CreatedAtUnixMilli = fixture.now.Add(2*time.Minute + time.Second).UnixMilli()
			},
			now: fixture.now.Add(2*time.Minute + time.Second),
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			block := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
			test.mutate(&block)
			require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &block))
			err := fixture.db.Transaction(func(tx *gorm.DB) error {
				_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
					Request: block, IdempotencyKey: fmt.Sprintf("settle-invalid-timeline-%d", index),
					RequestHash: strings.Repeat(fmt.Sprintf("%x", index+1), 64), Now: test.now,
				})
				return settleErr
			})
			require.ErrorIs(t, err, ErrMasterSettlementConflict)
		})
	}

	assertMasterLeaseBalances(t, fixture, 4_000, 0, 4_000, 0, 0)
	for _, target := range []any{&model.EdgeSettlementBlock{}, &model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}} {
		var count int64
		require.NoError(t, fixture.db.Model(target).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestMasterLeaseAllowsSnapshotContainingUnrelatedTieredPricing(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "mixed-pricing", "wallet_only", 5_000, 5_000, 10_000)
	expression := `v1:tier("base", p + c)`
	modelRatio := 1.0
	updateMasterLeasePricingForTest(t, fixture, func(pricing []dto.EdgePricingPolicyV1) []dto.EdgePricingPolicyV1 {
		return append(pricing, dto.EdgePricingPolicyV1{
			PolicyID: "price-tiered", Version: "v1", Model: "gpt-tiered", BillingMode: dto.EdgeBillingModeTieredExprV1,
			ModelRatio: &modelRatio, BillingExpression: expression, BillingExpressionHash: billingexpr.ExprHashString(expression),
			BillingExpressionVersion: billingexpr.DefaultExprVersion, QuotaPerUnit: 500_000,
		})
	})

	lease := acquireMasterLeaseForTest(t, fixture, "lease-mixed-pricing-1", 1_000, 1_000)
	assert.Equal(t, int64(1_000), lease.GrantedQuota)
}

func TestMasterZeroQuotaLeaseSettlesFreeUsageWithoutBalance(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "free-pricing", "subscription_only", 0, 0, 10_000)
	zero := 0.0
	updateMasterLeasePricingForTest(t, fixture, func(pricing []dto.EdgePricingPolicyV1) []dto.EdgePricingPolicyV1 {
		policy := pricing[0]
		policy.BillingMode = dto.EdgeBillingModeFixedPriceV1
		policy.ModelPrice = &zero
		policy.ModelRatio = nil
		pricing[0] = policy
		return pricing
	})

	lease := acquireMasterLeaseForTest(t, fixture, "lease-free-1", 0, 0)
	assert.Zero(t, lease.GrantedQuota)
	assert.Zero(t, lease.RenewAfterRemainingQuota)
	assertMasterLeaseBalances(t, fixture, 0, 0, 0, 0, 0)
	var funding model.EdgeLeaseFunding
	require.NoError(t, fixture.db.Where("lease_id = ?", lease.ID).First(&funding).Error)
	assert.Zero(t, funding.ReservedQuota)

	block := masterSettlementBlockForTest(t, fixture, lease, 1, 0)
	block.Events[0].Billing.BillingMode = dto.EdgeBillingModeFixedPriceV1
	block.Events[0].Billing.ReservedQuota = 0
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &block))
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: block, IdempotencyKey: "settle-free-1", RequestHash: strings.Repeat("7", 64),
			Now: fixture.now.Add(time.Minute),
		})
		return err
	}))
	assertMasterLeaseBalances(t, fixture, 0, 0, 0, 0, 0)
	var storedUser model.User
	require.NoError(t, fixture.db.First(&storedUser, fixture.user.Id).Error)
	assert.Equal(t, 1, storedUser.RequestCount)

	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := CloseMasterQuotaLeaseTx(tx, fixture.identity, MasterLeaseCloseCommand{
			Request: dto.EdgeLeaseCloseRequestV1{
				Meta: masterControlRequestMetaForTest("close-free-1"), LeaseID: lease.LeaseUID,
				LeaseVersion: lease.Version, FinalEventSequence: 1,
			},
			Now: fixture.now.Add(2 * time.Minute),
		})
		return err
	}))
	var storedLease model.EdgeQuotaLease
	require.NoError(t, fixture.db.First(&storedLease, lease.ID).Error)
	assert.Equal(t, model.EdgeQuotaLeaseStatusClosed, storedLease.Status)
}

func TestMasterSettlementMinimumChargeMirrorsEdgeBillingMode(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "minimum-charge", "wallet_only", 5_000, 5_000, 10_000)
	policies, err := loadMasterSnapshotPoliciesTx(fixture.db, fixture.snapshot.ID, true)
	require.NoError(t, err)
	block := masterSettlementBlockForTest(t, fixture, &model.EdgeQuotaLease{LeaseUID: "lease-formula"}, 1, 0)
	event := block.Events[0]
	event.Usage = dto.NewOpenAIChatBillingUsage(&dto.Usage{CompletionTokens: 1, TotalTokens: 1})
	key := masterPricingKey(event.Billing.PricingPolicyID, event.Billing.PricingPolicyVersion, event.Model)

	t.Run("ratio keeps one quota when non-free weighted usage truncates to zero", func(t *testing.T) {
		policy := policies.pricing[key]
		zero := 0.0
		policy.CompletionRatio = &zero
		policies.pricing[key] = policy
		event.Billing.BillingMode = dto.EdgeBillingModeRatioV1
		quota, _, recomputeErr := recomputeMasterUsageQuota(policies, fixture.user.Id, &event)
		require.NoError(t, recomputeErr)
		assert.Equal(t, int64(1), quota)
	})

	t.Run("fixed price does not invent a minimum charge", func(t *testing.T) {
		policy := policies.pricing[key]
		price := 0.0000001
		policy.BillingMode = dto.EdgeBillingModeFixedPriceV1
		policy.ModelPrice = &price
		policy.ModelRatio = nil
		policies.pricing[key] = policy
		event.Billing.BillingMode = dto.EdgeBillingModeFixedPriceV1
		quota, _, recomputeErr := recomputeMasterUsageQuota(policies, fixture.user.Id, &event)
		require.NoError(t, recomputeErr)
		assert.Zero(t, quota)
	})

	t.Run("zero group ratio remains a free settlement", func(t *testing.T) {
		policy := policies.pricing[key]
		modelRatio := 1.0
		policy.BillingMode = dto.EdgeBillingModeRatioV1
		policy.ModelRatio = &modelRatio
		policy.ModelPrice = nil
		policies.pricing[key] = policy
		group := policies.groups["default"]
		require.NotEmpty(t, group.UsingGroups)
		group.UsingGroups[0].Ratio = 0
		policies.groups["default"] = group
		event.Billing.BillingMode = dto.EdgeBillingModeRatioV1
		event.Billing.GroupRatio = 0
		quota, _, recomputeErr := recomputeMasterUsageQuota(policies, fixture.user.Id, &event)
		require.NoError(t, recomputeErr)
		assert.Zero(t, quota)
	})
}

func TestMasterLeaseSubscriptionReservationAndImmediateClose(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "subscription-close", "subscription_only", 5_000, 5_000, 10_000)
	plan := &model.SubscriptionPlan{
		Title: "Edge", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1,
		TotalAmount: 2_000, Enabled: true,
	}
	require.NoError(t, fixture.db.Create(plan).Error)
	subscription := &model.UserSubscription{
		UserId: fixture.user.Id, PlanId: plan.Id, AmountTotal: 2_000,
		StartTime: fixture.now.Add(-time.Hour).Unix(), EndTime: fixture.now.Add(time.Hour).Unix(),
		Status: "active", AllowWalletOverflow: false,
	}
	require.NoError(t, fixture.db.Create(subscription).Error)

	lease := acquireMasterLeaseForTest(t, fixture, "lease-subscription-1", 1_000, 1_000)
	var afterReserve model.UserSubscription
	require.NoError(t, fixture.db.First(&afterReserve, subscription.Id).Error)
	assert.Equal(t, int64(1_000), afterReserve.AmountUsed)
	assertMasterLeaseBalances(t, fixture, 5_000, 0, 4_000, 0, 0)

	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, closeErr := CloseMasterQuotaLeaseTx(tx, fixture.identity, MasterLeaseCloseCommand{
			Request: dto.EdgeLeaseCloseRequestV1{
				Meta: masterControlRequestMetaForTest("close-subscription-1"), LeaseID: lease.LeaseUID,
				LeaseVersion: lease.Version, FinalEventSequence: 0,
			},
			Now: fixture.now.Add(time.Minute),
		})
		return closeErr
	}))
	require.NoError(t, fixture.db.First(&afterReserve, subscription.Id).Error)
	assert.Equal(t, int64(0), afterReserve.AmountUsed)
	assertMasterLeaseBalances(t, fixture, 5_000, 0, 5_000, 0, 0)
}

func TestMasterLeaseForceCloseForfeitsWithoutRefund(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "force-close", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-force-1", 1_000, 1_000)

	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := ForceCloseMasterQuotaLeaseTx(tx, fixture.node.ID, fixture.node.Generation, lease.LeaseUID, fixture.now.Add(time.Minute))
		return err
	}))
	assertMasterLeaseBalances(t, fixture, 4_000, 1_000, 4_000, 1_000, 0)
	var stored model.EdgeQuotaLease
	require.NoError(t, fixture.db.First(&stored, lease.ID).Error)
	assert.Equal(t, model.EdgeQuotaLeaseStatusForceClosed, stored.Status)
	assert.Equal(t, int64(1_000), stored.ForfeitedQuota)
	assert.Zero(t, stored.ReturnedQuota)
}

func TestMasterLeaseConcurrentRiskBoundDoesNotOverIssue(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "concurrent-risk", "wallet_only", 5_000, 5_000, 1_000)
	commands := []MasterLeaseAcquireCommand{
		masterLeaseAcquireCommandForTest(fixture, "lease-concurrent-a", 1_000, 1_000, strings.Repeat("1", 64)),
		masterLeaseAcquireCommandForTest(fixture, "lease-concurrent-b", 1_000, 1_000, strings.Repeat("2", 64)),
	}
	start := make(chan struct{})
	errorsSeen := make([]error, len(commands))
	var wait sync.WaitGroup
	for i := range commands {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			errorsSeen[index] = fixture.db.Transaction(func(tx *gorm.DB) error {
				_, err := AcquireMasterQuotaLeaseTx(tx, fixture.identity, commands[index])
				return err
			})
		}(i)
	}
	close(start)
	wait.Wait()

	var leaseCount int64
	require.NoError(t, fixture.db.Model(&model.EdgeQuotaLease{}).Count(&leaseCount).Error)
	successCount := int64(0)
	for _, acquireErr := range errorsSeen {
		if acquireErr == nil {
			successCount++
		}
	}
	assert.Equal(t, int64(1), successCount)
	assert.Equal(t, successCount, leaseCount)
	var leases []model.EdgeQuotaLease
	require.NoError(t, fixture.db.Find(&leases).Error)
	var outstanding int64
	for i := range leases {
		outstanding += leases[i].RemainingQuota()
	}
	assert.LessOrEqual(t, outstanding, fixture.node.MaxOutstandingQuota)
}

func newMasterLeaseTestFixture(t *testing.T, name string, preference string, userQuota int, tokenQuota int, nodeRisk int64) *masterLeaseTestFixture {
	t.Helper()
	previousDB := model.DB
	dsn := fmt.Sprintf("file:master-lease-%s?mode=memory&cache=shared", name)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.EdgeNode{}, &model.EdgeNodeCredential{}, &model.User{}, &model.Token{}, &model.Channel{},
		&model.EdgeRequestReceipt{}, &model.EdgeRequestNonceClaim{},
		&model.SubscriptionPlan{}, &model.UserSubscription{},
		&model.EdgeCompiledSnapshot{}, &model.EdgeCompiledSnapshotDataset{}, &model.EdgeCompiledSnapshotPage{},
		&model.EdgeQuotaLease{}, &model.EdgeLeaseFunding{}, &model.EdgeSettlementBlock{},
		&model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}, &model.QuotaData{},
		&model.EdgeQuotaDataEvent{}, &model.EdgeQuotaDataBucket{},
	))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	identity := newMasterLeaseTestIdentity(t, db, now, "edge."+name, nodeRisk)
	user := &model.User{
		Username: "user-" + name, Password: "password", Status: common.UserStatusEnabled,
		Quota: userQuota, Group: "default",
	}
	user.SetSetting(dto.UserSetting{BillingPreference: preference})
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{
		UserId: user.Id, Key: "token-" + name, Status: common.TokenStatusEnabled,
		CreatedTime: now.Unix(), ExpiredTime: -1, RemainQuota: tokenQuota,
		Group: "default",
	}
	require.NoError(t, db.Create(token).Error)
	channel := &model.Channel{
		Type: 1, Key: "upstream", Status: common.ChannelStatusEnabled, Name: "channel-" + name,
		Models: "gpt-test", Group: "default",
	}
	require.NoError(t, db.Create(channel).Error)
	snapshot := createMasterLeaseSnapshotForTest(t, db, now, user, token, channel)
	return &masterLeaseTestFixture{
		db: db, now: now, node: identity.Node, credential: identity.Credential, identity: identity,
		user: user, token: token, channel: channel, snapshot: snapshot,
	}
}

func TestProcessSettlementRejectionRollsBackAccountingAndReplaysReceipt(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "settlement-domain-rejection", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-domain-rejection-1", 1_000, 1_000)

	block := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
	block.Meta.RequestID = "settlement-domain-rejection-1"
	second := block.Events[0]
	second.EventID = "event-domain-rejection-2"
	second.Sequence = 2
	second.ReservationID = "reservation-domain-rejection-2"
	second.RequestID = "request-domain-rejection-2"
	second.Billing.ChargedQuota = 119
	block.Events = append(block.Events, second)
	block.LastSequence = 2
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &block))

	// ExecuteControlMutation revalidates the credential at wall-clock time,
	// while this deterministic accounting fixture settles at a fixed future
	// instant. Make the credential valid across both instants.
	fixture.credential.NotBefore = fixture.now.Add(-time.Hour).Unix()
	fixture.credential.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
	require.NoError(t, fixture.db.Model(&model.EdgeNodeCredential{}).Where("id = ?", fixture.credential.ID).
		Updates(map[string]any{
			"not_before": fixture.credential.NotBefore,
			"expires_at": fixture.credential.ExpiresAt,
		}).Error)

	body, err := common.Marshal(block)
	require.NoError(t, err)
	signedRequest := edgeauth.Request{
		Method: "POST", EscapedPath: "/api/edge/control/v1/settlement-blocks", Body: body,
	}
	requestHash, err := edgeauth.IdempotencySHA256(signedRequest)
	require.NoError(t, err)
	metadata := edgeauth.Metadata{
		Version: edgeauth.VersionV1, NodeID: fixture.node.NodeUID, Generation: fixture.node.Generation,
		KeyID: fixture.credential.CredentialUID, TimestampUnixSeconds: time.Now().Unix(),
		Nonce: "MDEyMzQ1Njc4OWFiY2RlZg", IdempotencyKey: block.Meta.RequestID,
	}
	principal := &ControlPrincipal{
		NodeID: fixture.node.ID, NodeUID: fixture.node.NodeUID, NodeStatus: fixture.node.Status,
		Generation: fixture.node.Generation, CredentialID: fixture.credential.ID,
		CredentialUID: fixture.credential.CredentialUID, CredentialFingerprint: fixture.credential.Fingerprint,
		SignedRequest: &edgeauth.SignedHTTPRequest{Metadata: metadata, Request: signedRequest},
		RawBody:       body, RequestHash: requestHash, NonceHash: edgeauth.BodySHA256([]byte(metadata.Nonce)),
	}

	var leaseBefore model.EdgeQuotaLease
	var fundingBefore model.EdgeLeaseFunding
	var nodeBefore model.EdgeNode
	require.NoError(t, fixture.db.First(&leaseBefore, lease.ID).Error)
	require.NoError(t, fixture.db.Where("lease_id = ?", lease.ID).First(&fundingBefore).Error)
	require.NoError(t, fixture.db.First(&nodeBefore, fixture.node.ID).Error)

	first, err := ProcessSettlementBlock(principal, block, "server-settlement-domain-rejection-1", fixture.now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 409, first.StatusCode)
	assert.False(t, first.Replayed)

	var leaseAfter model.EdgeQuotaLease
	var fundingAfter model.EdgeLeaseFunding
	var nodeAfter model.EdgeNode
	require.NoError(t, fixture.db.First(&leaseAfter, lease.ID).Error)
	require.NoError(t, fixture.db.Where("lease_id = ?", lease.ID).First(&fundingAfter).Error)
	require.NoError(t, fixture.db.First(&nodeAfter, fixture.node.ID).Error)
	assert.Equal(t, leaseBefore.ConsumedQuota, leaseAfter.ConsumedQuota)
	assert.Equal(t, leaseBefore.Version, leaseAfter.Version)
	assert.Equal(t, leaseBefore.Status, leaseAfter.Status)
	assert.Equal(t, fundingBefore.ConsumedQuota, fundingAfter.ConsumedQuota)
	assert.Equal(t, fundingBefore.Status, fundingAfter.Status)
	assert.Equal(t, nodeBefore.LastEventSeq, nodeAfter.LastEventSeq)
	assert.Equal(t, nodeBefore.LastBlockSeq, nodeAfter.LastBlockSeq)
	assertMasterLeaseBalances(t, fixture, 4_000, 0, 4_000, 0, 0)
	for _, target := range []any{&model.EdgeSettlementBlock{}, &model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}} {
		var count int64
		require.NoError(t, fixture.db.Model(target).Count(&count).Error)
		assert.Zero(t, count)
	}

	var receipt model.EdgeRequestReceipt
	require.NoError(t, fixture.db.First(&receipt).Error)
	assert.Equal(t, model.EdgeRequestReceiptStatusRejected, receipt.Status)
	assert.Equal(t, 409, receipt.ResponseStatus)
	assert.Equal(t, string(first.Body), receipt.ResponsePayload)

	retry := cloneControlPrincipalWithNonce(principal, "ZmVkY2JhOTg3NjU0MzIxMA")
	replayed, err := ProcessSettlementBlock(retry, block, "server-settlement-domain-rejection-2", fixture.now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, first.StatusCode, replayed.StatusCode)
	assert.Equal(t, first.Body, replayed.Body)

	var receiptCount int64
	var nonceCount int64
	require.NoError(t, fixture.db.Model(&model.EdgeRequestReceipt{}).Count(&receiptCount).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeRequestNonceClaim{}).Count(&nonceCount).Error)
	assert.Equal(t, int64(1), receiptCount)
	assert.Equal(t, int64(2), nonceCount)
}

func TestMasterSettlementAllowsRequestStartBeforeSynchronousLeaseIssuance(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "settlement-start-before-lease", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-start-before-issue-1", 1_000, 1_000)
	block := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
	block.Events[0].StartedAtUnixMilli = lease.IssuedAtUnixMilli - 2_000
	block.Events[0].FinishedAtUnixMilli = lease.IssuedAtUnixMilli + 1_000
	block.CreatedAtUnixMilli = lease.IssuedAtUnixMilli + 2_000
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &block))

	var ack *dto.EdgeSettlementAckV1
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		ack, err = SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: block, IdempotencyKey: "settlement-start-before-issue-1",
			RequestHash: strings.Repeat("8", 64), Now: fixture.now.Add(time.Minute),
		})
		return err
	}))
	require.NotNil(t, ack)
	assert.Equal(t, dto.EdgeSettlementAckAcceptedV1, ack.Status)
	assertMasterLeaseBalances(t, fixture, 4_000, 120, 4_000, 120, 120)
}

func TestMasterSettlementRejectsRequestCompletedBeforeLeaseIssuance(t *testing.T) {
	fixture := newMasterLeaseTestFixture(t, "settlement-finish-before-lease", "wallet_only", 5_000, 5_000, 10_000)
	lease := acquireMasterLeaseForTest(t, fixture, "lease-finish-before-issue-1", 1_000, 1_000)
	block := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
	block.Events[0].StartedAtUnixMilli = lease.IssuedAtUnixMilli - 2_000
	block.Events[0].FinishedAtUnixMilli = lease.IssuedAtUnixMilli - 1_000
	block.CreatedAtUnixMilli = lease.IssuedAtUnixMilli + 1_000
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &block))

	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, settleErr := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: block, IdempotencyKey: "settlement-finish-before-issue-1",
			RequestHash: strings.Repeat("9", 64), Now: fixture.now.Add(time.Minute),
		})
		return settleErr
	})
	require.ErrorIs(t, err, ErrMasterSettlementConflict)
	assertMasterLeaseBalances(t, fixture, 4_000, 0, 4_000, 0, 0)
	var blockCount int64
	require.NoError(t, fixture.db.Model(&model.EdgeSettlementBlock{}).Count(&blockCount).Error)
	assert.Zero(t, blockCount)
}

func newMasterLeaseTestIdentity(t *testing.T, db *gorm.DB, now time.Time, nodeUID string, nodeRisk int64) *model.EdgeControlIdentity {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	material, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	node := &model.EdgeNode{
		NodeUID: nodeUID, Name: nodeUID, Status: model.EdgeNodeStatusActive, Generation: 1,
		ProtocolVersion: dto.EdgeControlProtocolVersionV1, MaxOutstandingQuota: nodeRisk,
	}
	require.NoError(t, db.Create(node).Error)
	credential := &model.EdgeNodeCredential{
		CredentialUID: "key-" + strings.ReplaceAll(nodeUID, ".", "-"), NodeID: node.ID, Generation: 1,
		VerifyMaterial: material, Status: model.EdgeNodeCredentialStatusActive,
		NotBefore: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	}
	require.NoError(t, db.Create(credential).Error)
	return &model.EdgeControlIdentity{Node: node, Credential: credential}
}

func createMasterLeaseSnapshotForTest(t *testing.T, db *gorm.DB, now time.Time, user *model.User, token *model.Token, channel *model.Channel) *model.EdgeCompiledSnapshot {
	t.Helper()
	snapshot := &model.EdgeCompiledSnapshot{
		SnapshotUID: "snapshot-1", Revision: 1, ProtocolVersion: dto.EdgeControlProtocolVersionV1,
		Status: model.EdgeCompiledSnapshotStatusPublished, CreatedAt: now.Add(-time.Minute).Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), PublishedAt: now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(snapshot).Error)
	modelRatio := 1.0
	completionRatio := 2.0
	cacheRatio := 1.0
	payloads := map[dto.EdgeSnapshotDatasetV1]dto.EdgeSnapshotPagePayloadV1{
		dto.EdgeSnapshotDatasetAuthenticationV1: {Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("a", 64), TokenID: int64(token.Id), UserID: int64(user.Id), Enabled: true, Group: "default",
		}}},
		dto.EdgeSnapshotDatasetUsersV1: {Users: []dto.EdgeUserPolicyV1{{
			UserID: int64(user.Id), Enabled: true, Username: user.Username, DefaultGroup: "default",
		}}},
		dto.EdgeSnapshotDatasetGroupsV1: {Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}}},
		dto.EdgeSnapshotDatasetModelsV1: {Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-test", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1},
			Streaming: true, ChannelIDs: []int64{int64(channel.Id)},
		}}},
		dto.EdgeSnapshotDatasetChannelsV1: {Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: int64(channel.Id), Type: 1, Name: "test", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-test"}, Weight: 1,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}}},
		dto.EdgeSnapshotDatasetPricingV1: {Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: "price-gpt-test", Version: "v1", Model: "gpt-test", BillingMode: dto.EdgeBillingModeRatioV1,
			ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheReadRatio: &cacheRatio,
			CacheCreationRatio: &cacheRatio, CacheCreation1hRatio: &cacheRatio, QuotaPerUnit: 500_000,
		}}},
	}
	for _, datasetName := range []dto.EdgeSnapshotDatasetV1{
		dto.EdgeSnapshotDatasetAuthenticationV1, dto.EdgeSnapshotDatasetUsersV1,
		dto.EdgeSnapshotDatasetGroupsV1, dto.EdgeSnapshotDatasetModelsV1,
		dto.EdgeSnapshotDatasetChannelsV1, dto.EdgeSnapshotDatasetPricingV1,
	} {
		payload := payloads[datasetName]
		require.NoError(t, payload.Validate(datasetName, 1))
		payloadBytes, err := common.Marshal(payload)
		require.NoError(t, err)
		dataset := &model.EdgeCompiledSnapshotDataset{
			SnapshotID: snapshot.ID, Dataset: datasetName, Revision: 1, ItemCount: 1, PageCount: 1,
		}
		require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(dataset).Error)
		page := &model.EdgeCompiledSnapshotPage{DatasetID: dataset.ID, Ordinal: 0, ItemCount: 1, Payload: string(payloadBytes)}
		require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(page).Error)
	}
	return snapshot
}

func updateMasterLeasePricingForTest(
	t *testing.T,
	fixture *masterLeaseTestFixture,
	mutate func([]dto.EdgePricingPolicyV1) []dto.EdgePricingPolicyV1,
) {
	t.Helper()
	require.NotNil(t, fixture)
	require.NotNil(t, mutate)
	var dataset model.EdgeCompiledSnapshotDataset
	require.NoError(t, fixture.db.Where("snapshot_id = ? AND dataset = ?", fixture.snapshot.ID, dto.EdgeSnapshotDatasetPricingV1).
		First(&dataset).Error)
	var page model.EdgeCompiledSnapshotPage
	require.NoError(t, fixture.db.Where("dataset_id = ?", dataset.ID).First(&page).Error)
	var payload dto.EdgeSnapshotPagePayloadV1
	require.NoError(t, common.UnmarshalJsonStr(page.Payload, &payload))
	payload.Pricing = mutate(payload.Pricing)
	require.NoError(t, payload.Validate(dto.EdgeSnapshotDatasetPricingV1, len(payload.Pricing)))
	payloadBytes, err := common.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, fixture.db.Session(&gorm.Session{SkipHooks: true}).Model(&page).
		Updates(map[string]any{"payload": string(payloadBytes), "item_count": len(payload.Pricing)}).Error)
	require.NoError(t, fixture.db.Session(&gorm.Session{SkipHooks: true}).Model(&dataset).
		Update("item_count", len(payload.Pricing)).Error)
}

func acquireMasterLeaseForTest(t *testing.T, fixture *masterLeaseTestFixture, idempotencyKey string, requested int64, minimum int64) *model.EdgeQuotaLease {
	t.Helper()
	command := masterLeaseAcquireCommandForTest(fixture, idempotencyKey, requested, minimum, strings.Repeat("a", 64))
	var lease *model.EdgeQuotaLease
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		lease, err = AcquireMasterQuotaLeaseTx(tx, fixture.identity, command)
		return err
	}))
	require.NotNil(t, lease)
	return lease
}

func masterLeaseAcquireCommandForTest(fixture *masterLeaseTestFixture, idempotencyKey string, requested int64, minimum int64, requestHash string) MasterLeaseAcquireCommand {
	return MasterLeaseAcquireCommand{
		Request: dto.EdgeLeaseAcquireRequestV1{
			Meta:           masterControlRequestMetaForTest(idempotencyKey),
			Subject:        dto.EdgeLeaseSubjectV1{UserID: int64(fixture.user.Id), TokenID: int64(fixture.token.Id)},
			RequestedQuota: requested, MinimumAcceptableQuota: minimum,
			SnapshotID: fixture.snapshot.SnapshotUID, SnapshotRevision: fixture.snapshot.Revision,
		},
		IdempotencyKey: idempotencyKey, RequestHash: requestHash, Now: fixture.now,
		Policy: MasterLeasePolicy{TTL: 10 * time.Minute, MaxLeaseQuota: int64(common.MaxQuota), RenewDivisor: 4},
	}
}

func masterSettlementBlockForTest(t *testing.T, fixture *masterLeaseTestFixture, lease *model.EdgeQuotaLease, sequence int64, charged int64) dto.EdgeSettlementBlockRequestV1 {
	t.Helper()
	status := 200
	startedAt := fixture.now.Add(10 * time.Second).UnixMilli()
	finishedAt := fixture.now.Add(20 * time.Second).UnixMilli()
	request := dto.EdgeSettlementBlockRequestV1{
		Meta:    masterControlRequestMetaForTest(fmt.Sprintf("settlement-%d", sequence)),
		BlockID: fmt.Sprintf("block-%d", sequence), FirstSequence: sequence, LastSequence: sequence,
		CreatedAtUnixMilli: fixture.now.Add(30 * time.Second).UnixMilli(), BlockDigest: strings.Repeat("9", 64),
		Events: []dto.EdgeUsageEventV1{{
			EventID: fmt.Sprintf("event-%d", sequence), Sequence: sequence, LeaseID: lease.LeaseUID,
			ReservationID: fmt.Sprintf("reservation-%d", sequence), RequestID: fmt.Sprintf("request-%d", sequence),
			UserID: int64(fixture.user.Id), TokenID: int64(fixture.token.Id), ChannelID: int64(fixture.channel.Id),
			Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Model: "gpt-test", Group: "default",
			StartedAtUnixMilli: startedAt, FinishedAtUnixMilli: finishedAt,
			Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
			Usage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}),
			Billing: dto.EdgeUsageBillingV1{
				PricingPolicyID: "price-gpt-test", PricingPolicyVersion: "v1", BillingMode: dto.EdgeBillingModeRatioV1,
				GroupRatio: 1, ReservedQuota: 200, ChargedQuota: charged,
			},
		}},
	}
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	return request
}

func masterControlRequestMetaForTest(requestID string) dto.EdgeControlRequestMetaV1 {
	return dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: requestID}
}

func assertMasterLeaseBalances(t *testing.T, fixture *masterLeaseTestFixture, userQuota int, userUsed int, tokenRemain int, tokenUsed int, channelUsed int64) {
	t.Helper()
	var user model.User
	var token model.Token
	var channel model.Channel
	require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
	require.NoError(t, fixture.db.First(&token, fixture.token.Id).Error)
	require.NoError(t, fixture.db.First(&channel, fixture.channel.Id).Error)
	assert.Equal(t, userQuota, user.Quota)
	assert.Equal(t, userUsed, user.UsedQuota)
	assert.Equal(t, tokenRemain, token.RemainQuota)
	assert.Equal(t, tokenUsed, token.UsedQuota)
	assert.Equal(t, channelUsed, channel.UsedQuota)
}
