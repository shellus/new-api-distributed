package service

import (
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestBillingSessionWithCustomFundingSettlesZeroDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	tokenAccounting := &recordingTokenAccounting{}
	relayInfo := &relaycommon.RelayInfo{UserId: 1, TokenId: 2}

	session, apiErr := NewBillingSessionWithFunding(context, relayInfo, 100, funding, tokenAccounting)
	require.Nil(t, apiErr)
	assert.Equal(t, []int{100}, funding.preConsumed)
	assert.Equal(t, BillingSourceEdgeBalance, relayInfo.BillingSource)
	assert.Equal(t, 100, session.GetPreConsumedQuota())

	require.NoError(t, session.Settle(100))
	assert.Equal(t, []int{0}, funding.settled)
	assert.Equal(t, []int{0}, tokenAccounting.settled)
	require.NoError(t, session.Settle(100))
	assert.Equal(t, []int{0}, funding.settled)
	assert.Equal(t, []int{0}, tokenAccounting.settled)
}

func TestBillingSessionReserveUsesGenericFundingAndRollsBackOnTokenFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	tokenAccounting := &recordingTokenAccounting{reserveErr: errors.New("token reserve failed")}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{UserId: 1, TokenId: 2},
		100,
		funding,
		tokenAccounting,
	)
	require.Nil(t, apiErr)

	err := session.Reserve(150)
	require.Error(t, err)
	assert.Equal(t, []int{50, -50}, funding.reserved)
	assert.Equal(t, 100, session.GetPreConsumedQuota())
}

func TestBillingSessionNeedsRefundUsesFundingAndTokenInterfaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance, refunded: make(chan struct{}, 1)}
	tokenAccounting := &recordingTokenAccounting{refunded: make(chan struct{}, 1)}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{UserId: 1, TokenId: 2},
		100,
		funding,
		tokenAccounting,
	)
	require.Nil(t, apiErr)
	assert.True(t, session.NeedsRefund())

	session.Refund(context)
	select {
	case <-funding.refunded:
	case <-time.After(time.Second):
		t.Fatal("funding refund was not called")
	}
	select {
	case <-tokenAccounting.refunded:
	case <-time.After(time.Second):
		t.Fatal("token refund was not called")
	}
	assert.False(t, session.NeedsRefund())
}

func TestBillingSessionRefundRetainsFailedReservationForRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance, refundFailures: 1}
	tokenAccounting := &recordingTokenAccounting{}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{UserId: 1, TokenId: 2},
		100,
		funding,
		tokenAccounting,
	)
	require.Nil(t, apiErr)

	session.Refund(context)
	assert.True(t, session.NeedsRefund())
	assert.Equal(t, 1, funding.refundCalls)
	assert.Equal(t, 1, tokenAccounting.refundCalls)

	session.Refund(context)
	assert.False(t, session.NeedsRefund())
	assert.Equal(t, 2, funding.refundCalls)
	// The successful token refund is not repeated because its reservation was
	// cleared independently on the first attempt.
	assert.Equal(t, 1, tokenAccounting.refundCalls)
}

func TestMasterDatabaseRefundsAreAtMostOnceAfterAmbiguousError(t *testing.T) {
	ambiguousErr := errors.New("ambiguous database update result")
	failingDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	attempts := 0
	require.NoError(t, failingDB.Callback().Update().Before("gorm:update").Register("test:ambiguous_refund", func(tx *gorm.DB) {
		attempts++
		tx.AddError(ambiguousErr)
	}))
	originalDB := model.DB
	require.False(t, common.BatchUpdateEnabled)
	require.False(t, common.RedisEnabled)
	model.DB = failingDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	wallet := &WalletFunding{userId: 1, consumed: 150}
	assert.ErrorIs(t, wallet.Reserve(-50), ambiguousErr)
	assert.True(t, wallet.HasReservation())
	assert.Equal(t, 100, wallet.consumed)
	assert.ErrorIs(t, wallet.Refund(), ambiguousErr)
	assert.False(t, wallet.HasReservation())
	require.NoError(t, wallet.Refund())
	assert.Equal(t, 2, attempts)

	accounting := &databaseTokenQuotaAccounting{
		relayInfo: &relaycommon.RelayInfo{TokenId: 2, TokenKey: "ambiguous-token-refund"},
		reserved:  100,
	}
	assert.ErrorIs(t, accounting.Refund(), ambiguousErr)
	assert.False(t, accounting.HasReservation())
	require.NoError(t, accounting.Refund())
	assert.Equal(t, 3, attempts)
}

func TestBillingSessionReservePoisonedWhenFundingRollbackFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{
		source:             BillingSourceEdgeBalance,
		reserveRollbackErr: errors.New("funding rollback failed"),
	}
	tokenAccounting := &recordingTokenAccounting{reserveErr: errors.New("token reserve failed")}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{UserId: 1, TokenId: 2},
		100,
		funding,
		tokenAccounting,
	)
	require.Nil(t, apiErr)

	require.Error(t, session.Reserve(150))
	assert.Equal(t, []int{50, -50}, funding.reserved)
	require.Error(t, session.Reserve(150))
	assert.Equal(t, []int{50, -50}, funding.reserved)
	require.Error(t, session.Settle(100))

	session.Refund(context)
	assert.False(t, session.NeedsRefund())
}

func TestBillingSessionCustomConstructorRejectsUnsafeInputsBeforeFunding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance}

	_, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		-1,
		funding,
		NoopTokenQuotaAccounting{},
	)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeModelPriceError, apiErr.GetErrorCode())
	assert.Empty(t, funding.preConsumed)

	_, apiErr = NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{QuotaClamp: &common.QuotaClamp{Op: "test", Kind: common.QuotaClampOverflow}},
		1,
		funding,
		NoopTokenQuotaAccounting{},
	)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeModelPriceError, apiErr.GetErrorCode())
	assert.Empty(t, funding.preConsumed)

	var typedNilFunding *recordingFunding
	_, apiErr = NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		1,
		typedNilFunding,
		NoopTokenQuotaAccounting{},
	)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())

	var typedNilAccounting *recordingTokenAccounting
	_, apiErr = NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		1,
		funding,
		typedNilAccounting,
	)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
}

func TestPreConsumeBillingEdgeFailsClosedWithoutLeaseFactory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	SetEdgeBillingSessionFactory(nil)
	t.Cleanup(func() {
		SetEdgeBillingSessionFactory(nil)
		require.NoError(t, common.SetRuntimeMode(common.RuntimeModeMaster))
	})

	relayInfo := &relaycommon.RelayInfo{}
	apiErr := PreConsumeBilling(context, 1, relayInfo)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeUpdateDataError, apiErr.GetErrorCode())
	assert.Nil(t, relayInfo.Billing)
}

func TestPreConsumeBillingEdgeFailsClosedWhenFactoryReturnsNoSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	SetEdgeBillingSessionFactory(func(*gin.Context, int, *relaycommon.RelayInfo) (*BillingSession, *types.NewAPIError) {
		return nil, nil
	})
	t.Cleanup(func() {
		SetEdgeBillingSessionFactory(nil)
		require.NoError(t, common.SetRuntimeMode(common.RuntimeModeMaster))
	})

	relayInfo := &relaycommon.RelayInfo{}
	apiErr := PreConsumeBilling(context, 1, relayInfo)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeUpdateDataError, apiErr.GetErrorCode())
	assert.Nil(t, relayInfo.Billing)
}

func TestBillingSessionReservePreservesErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())

	for _, test := range []struct {
		name     string
		source   string
		expected types.ErrorCode
	}{
		{name: "wallet", source: BillingSourceWallet, expected: types.ErrorCodeUpdateDataError},
		{name: "subscription", source: BillingSourceSubscription, expected: types.ErrorCodeInsufficientUserQuota},
	} {
		t.Run(test.name, func(t *testing.T) {
			funding := &recordingFunding{source: test.source, reservePositiveErr: errors.New("reserve failed")}
			session, apiErr := NewBillingSessionWithFunding(context, &relaycommon.RelayInfo{}, 100, funding, NoopTokenQuotaAccounting{})
			require.Nil(t, apiErr)
			err := session.Reserve(150)
			var newAPIErr *types.NewAPIError
			require.ErrorAs(t, err, &newAPIErr)
			assert.Equal(t, test.expected, newAPIErr.GetErrorCode())
		})
	}

	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		100,
		funding,
		&recordingTokenAccounting{reserveErr: errors.New("token reserve failed")},
	)
	require.Nil(t, apiErr)
	err := session.Reserve(150)
	var newAPIErr *types.NewAPIError
	require.ErrorAs(t, err, &newAPIErr)
	assert.Equal(t, types.ErrorCodePreConsumeTokenQuotaFailed, newAPIErr.GetErrorCode())
}

func TestBillingSessionRejectsNegativeActualQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	session, apiErr := NewBillingSessionWithFunding(context, &relaycommon.RelayInfo{}, 100, funding, NoopTokenQuotaAccounting{})
	require.Nil(t, apiErr)
	require.Error(t, session.Settle(-1))
	assert.Empty(t, funding.settled)
}

func TestBillingSessionKeepsTokenSettlementFailureVisible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	relayInfo := &relaycommon.RelayInfo{}
	funding := &recordingFunding{source: BillingSourceSubscription}
	settleErr := errors.New("token settlement failed")
	tokenAccounting := &recordingTokenAccounting{settleErr: settleErr}
	session, apiErr := NewBillingSessionWithFunding(context, relayInfo, 100, funding, tokenAccounting)
	require.Nil(t, apiErr)

	assert.ErrorIs(t, session.Settle(80), settleErr)
	assert.ErrorIs(t, session.Settle(80), settleErr)
	assert.Equal(t, []int{-20}, funding.settled)
	assert.Equal(t, []int{-20}, tokenAccounting.settled)
	assert.Equal(t, int64(-20), relayInfo.SubscriptionPostDelta)
	assert.False(t, session.NeedsRefund())
}

func TestBillingSessionFundingSettlementFailureIsTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	settleErr := errors.New("ambiguous funding settlement result")
	funding := &recordingFunding{source: BillingSourceWallet, settleErr: settleErr}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		100,
		funding,
		NoopTokenQuotaAccounting{},
	)
	require.Nil(t, apiErr)

	assert.ErrorIs(t, session.Settle(80), settleErr)
	assert.ErrorIs(t, session.Settle(80), settleErr)
	assert.Equal(t, []int{-20}, funding.settled)
	assert.False(t, session.NeedsRefund())
	session.Refund(context)
	assert.Zero(t, funding.refundCalls)
}

func TestBillingSessionSerializesSettleAndRefund(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &blockingRefundFunding{
		held:    100,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		100,
		funding,
		NoopTokenQuotaAccounting{},
	)
	require.Nil(t, apiErr)

	refundDone := make(chan struct{})
	go func() {
		session.Refund(context)
		close(refundDone)
	}()
	<-funding.started

	require.Error(t, session.Settle(100))
	require.Error(t, session.Reserve(150))
	// A concurrent duplicate refund must observe the in-progress state and
	// return without invoking the non-idempotent funding refund twice.
	session.Refund(context)
	close(funding.release)
	<-refundDone

	assert.Equal(t, 1, funding.refundCalls())
	assert.Equal(t, 0, funding.settleCalls())
	assert.False(t, session.NeedsRefund())
}

func TestBillingSessionConcurrentSettleCommitsOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceWallet}
	tokenAccounting := &recordingTokenAccounting{}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		100,
		funding,
		tokenAccounting,
	)
	require.Nil(t, apiErr)

	const callers = 4
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- session.Settle(100)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	assert.Equal(t, []int{0}, funding.settled)
	assert.Equal(t, []int{0}, tokenAccounting.settled)
}

func TestBillingSessionRejectsConflictingRepeatedSettlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{},
		100,
		funding,
		NoopTokenQuotaAccounting{},
	)
	require.Nil(t, apiErr)

	require.NoError(t, session.Settle(80))
	require.Error(t, session.Settle(81))
	assert.Equal(t, []int{-20}, funding.settled)
}

func TestPreConsumeBillingEdgeCompensatesFactoryErrorOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	funding := &recordingFunding{source: BillingSourceEdgeBalance}
	SetEdgeBillingSessionFactory(func(c *gin.Context, quota int, relayInfo *relaycommon.RelayInfo) (*BillingSession, *types.NewAPIError) {
		session, apiErr := NewBillingSessionWithFunding(c, relayInfo, quota, funding, NoopTokenQuotaAccounting{})
		require.Nil(t, apiErr)
		return session, types.NewError(errors.New("factory failed after reserving quota"), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	})
	t.Cleanup(func() {
		SetEdgeBillingSessionFactory(nil)
		require.NoError(t, common.SetRuntimeMode(common.RuntimeModeMaster))
	})

	relayInfo := &relaycommon.RelayInfo{}
	apiErr := PreConsumeBilling(context, 100, relayInfo)
	require.NotNil(t, apiErr)
	require.NotNil(t, relayInfo.Billing)
	assert.Equal(t, 1, funding.refundCalls)
	assert.False(t, funding.HasReservation())

	// The controller error defer may request compensation again. The completed
	// refund must make that call a no-op rather than crediting the lease twice.
	relayInfo.Billing.Refund(context)
	assert.Equal(t, 1, funding.refundCalls)
}

func TestBillingSessionReturnsCompensatableSessionWhenPreConsumeRollbackFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	fundingErr := errors.New("funding pre-consume failed")
	rollbackErr := errors.New("token rollback failed")
	funding := &recordingFunding{source: BillingSourceEdgeBalance, preConsumeErr: fundingErr}
	tokenAccounting := &recordingTokenAccounting{refundFailures: 1, refundErr: rollbackErr}

	session, apiErr := NewBillingSessionWithFunding(
		context,
		&relaycommon.RelayInfo{UserId: 1, TokenId: 2},
		100,
		funding,
		tokenAccounting,
	)
	require.NotNil(t, session)
	require.NotNil(t, apiErr)
	assert.ErrorIs(t, apiErr, fundingErr)
	assert.ErrorIs(t, apiErr, rollbackErr)
	assert.True(t, session.NeedsRefund())

	session.Refund(context)
	assert.False(t, session.NeedsRefund())
}

type recordingFunding struct {
	mu                 sync.Mutex
	source             string
	held               int
	preConsumed        []int
	reserved           []int
	settled            []int
	refunded           chan struct{}
	reservePositiveErr error
	reserveRollbackErr error
	refundFailures     int
	refundCalls        int
	preConsumeErr      error
	settleErr          error
}

func (f *recordingFunding) Source() string { return f.source }

func (f *recordingFunding) PreConsume(amount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preConsumed = append(f.preConsumed, amount)
	if f.preConsumeErr != nil {
		return f.preConsumeErr
	}
	f.held += amount
	return nil
}

func (f *recordingFunding) Reserve(delta int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved = append(f.reserved, delta)
	if delta > 0 && f.reservePositiveErr != nil {
		return f.reservePositiveErr
	}
	if delta < 0 && f.reserveRollbackErr != nil {
		return f.reserveRollbackErr
	}
	f.held += delta
	return nil
}

func (f *recordingFunding) Settle(delta int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, delta)
	if f.settleErr != nil {
		return f.settleErr
	}
	f.held = 0
	return nil
}

func (f *recordingFunding) Refund() error {
	f.mu.Lock()
	f.refundCalls++
	if f.refundFailures > 0 {
		f.refundFailures--
		f.mu.Unlock()
		return errors.New("funding refund failed")
	}
	f.held = 0
	refunded := f.refunded
	f.mu.Unlock()
	if refunded != nil {
		refunded <- struct{}{}
	}
	return nil
}

func (f *recordingFunding) HasReservation() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held > 0
}

type recordingTokenAccounting struct {
	mu             sync.Mutex
	reserved       int
	reserveErr     error
	refunded       chan struct{}
	settled        []int
	refundCalls    int
	refundFailures int
	refundErr      error
	settleErr      error
}

type blockingRefundFunding struct {
	mu          sync.Mutex
	held        int
	refundCount int
	settleCount int
	started     chan struct{}
	release     chan struct{}
}

func (f *blockingRefundFunding) Source() string { return BillingSourceWallet }

func (f *blockingRefundFunding) PreConsume(amount int) error {
	f.mu.Lock()
	f.held = amount
	f.mu.Unlock()
	return nil
}

func (f *blockingRefundFunding) Reserve(delta int) error {
	f.mu.Lock()
	f.held += delta
	f.mu.Unlock()
	return nil
}

func (f *blockingRefundFunding) Settle(int) error {
	f.mu.Lock()
	f.settleCount++
	f.held = 0
	f.mu.Unlock()
	return nil
}

func (f *blockingRefundFunding) Refund() error {
	f.mu.Lock()
	f.refundCount++
	f.mu.Unlock()
	close(f.started)
	<-f.release
	f.mu.Lock()
	f.held = 0
	f.mu.Unlock()
	return nil
}

func (f *blockingRefundFunding) HasReservation() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held > 0
}

func (f *blockingRefundFunding) refundCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refundCount
}

func (f *blockingRefundFunding) settleCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settleCount
}

func (a *recordingTokenAccounting) PreConsume(amount int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reserved += amount
	return nil
}

func (a *recordingTokenAccounting) Reserve(delta int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reserveErr != nil {
		return a.reserveErr
	}
	a.reserved += delta
	return nil
}

func (a *recordingTokenAccounting) Settle(delta int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settled = append(a.settled, delta)
	if a.settleErr != nil {
		return a.settleErr
	}
	a.reserved = 0
	return nil
}

func (a *recordingTokenAccounting) Refund() error {
	a.mu.Lock()
	a.refundCalls++
	if a.refundFailures > 0 {
		a.refundFailures--
		err := a.refundErr
		a.mu.Unlock()
		if err == nil {
			return errors.New("token refund failed")
		}
		return err
	}
	a.reserved = 0
	refunded := a.refunded
	a.mu.Unlock()
	if refunded != nil {
		refunded <- struct{}{}
	}
	return nil
}

func (a *recordingTokenAccounting) HasReservation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserved > 0
}

func (a *recordingTokenAccounting) ReservedQuota() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserved
}
