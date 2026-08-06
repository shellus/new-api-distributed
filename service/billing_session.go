package service

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
type BillingSession struct {
	relayInfo        *relaycommon.RelayInfo
	funding          FundingSource
	tokenAccounting  TokenQuotaAccounting
	preConsumedQuota int  // 实际预扣额度（信任用户可能为 0）
	tokenConsumed    int  // 令牌额度实际扣减量
	extraReserved    int  // 发送前补充预扣的额度（订阅退款时需要单独回滚）
	trusted          bool // 是否命中信任额度旁路
	allowTrust       bool // 仅 master 自建钱包/订阅会话允许信任额度旁路
	fundingSettled   bool // funding.Settle 已成功，资金来源已提交
	settled          bool // Settle 全部完成（资金 + 令牌）
	settlementErr    error
	settlementActual int
	refunding        bool // Refund 正在同步执行
	refunded         bool // Refund 已调用
	poisoned         error
	mu               sync.Mutex
}

// Settle 根据实际消耗额度进行结算。
// 资金来源和令牌额度分两步提交：若资金来源已提交但令牌调整失败，
// 会标记 fundingSettled 防止 Refund 对已提交的资金来源执行退款。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if actualQuota < 0 {
		return errors.New("actual billing quota cannot be negative")
	}
	if s.settled {
		if actualQuota != s.settlementActual {
			return fmt.Errorf("billing session already settled for actual quota %d", s.settlementActual)
		}
		return nil
	}
	if s.settlementErr != nil {
		if actualQuota != s.settlementActual {
			return fmt.Errorf("billing settlement already failed for actual quota %d: %w", s.settlementActual, s.settlementErr)
		}
		return s.settlementErr
	}
	if s.refunded {
		return errors.New("billing session was already refunded")
	}
	if s.refunding {
		return errors.New("billing session refund is in progress")
	}
	if s.poisoned != nil {
		return fmt.Errorf("billing session cannot settle after a failed reserve rollback: %w", s.poisoned)
	}
	delta := actualQuota - s.preConsumedQuota
	// 1) 调整资金来源（仅在尚未提交时执行，防止重复调用）
	if !s.fundingSettled {
		if err := s.funding.Settle(delta); err != nil {
			// The upstream request has already produced billable usage. A funding
			// error may have an ambiguous commit result, so repeating the mutation
			// or refunding the original hold could over-credit the account.
			s.settlementErr = err
			s.settlementActual = actualQuota
			return err
		}
		s.fundingSettled = true
		// The funding mutation is already committed at this point. Keep the
		// subscription audit fields in sync even when the later token mutation
		// fails; otherwise logs would claim that no post-consume adjustment was
		// applied while the subscription balance has actually changed.
		if s.funding.Source() == BillingSourceSubscription {
			s.relayInfo.SubscriptionPostDelta += int64(delta)
		}
	}
	// 2) 调整令牌额度。edge 使用 no-op accounting，避免重复扣本地投影。
	tokenErr := s.tokenAccounting.Settle(delta)
	if tokenErr != nil {
		// 资金来源已提交，令牌调整失败不能再 Refund，也不能把后续调用
		// 伪装成成功；保留同一个终态错误供调用方审计。
		common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
			s.relayInfo.UserId, s.relayInfo.TokenId, delta, tokenErr.Error()))
		s.settlementErr = tokenErr
		s.settlementActual = actualQuota
		return tokenErr
	}
	s.settlementActual = actualQuota
	s.settled = true
	return nil
}

// Refund 同步退还所有预扣费。幂等资金源可在失败后保留 reservation
// 供后续补偿；非幂等资金源必须清除状态以维持 at-most-once。
func (s *BillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	if s.settled || s.settlementErr != nil || s.refunded || s.refunding {
		s.mu.Unlock()
		return
	}
	refundFunding := s.funding.HasReservation()
	refundToken := s.tokenAccounting.HasReservation()
	if !refundFunding && !refundToken {
		s.refunded = true
		s.mu.Unlock()
		return
	}
	s.refunding = true
	s.mu.Unlock()

	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费（token_quota=%s, funding=%s）",
		s.relayInfo.UserId,
		logger.FormatQuota(s.tokenConsumed),
		s.funding.Source(),
	))

	// 两项都必须尝试，避免一项失败阻断另一项补偿。实现通过
	// HasReservation 声明失败后是否仍可安全重试。
	var fundingErr error
	if refundFunding {
		fundingErr = s.funding.Refund()
	}
	var tokenErr error
	if refundToken {
		tokenErr = s.tokenAccounting.Refund()
	}
	if fundingErr != nil {
		common.SysLog("error refunding billing source: " + fundingErr.Error())
	}
	if tokenErr != nil {
		common.SysLog("error refunding token quota: " + tokenErr.Error())
	}

	s.mu.Lock()
	s.refunding = false
	if !s.hasReservationLocked() {
		s.refunded = true
	}
	s.mu.Unlock()
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.settlementErr != nil || s.refunded || s.fundingSettled {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	if s.refunding {
		return true
	}
	return s.hasReservationLocked()
}

func (s *BillingSession) hasReservationLocked() bool {
	return s.tokenAccounting.HasReservation() || s.funding.HasReservation()
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preConsumedQuota
}

func (s *BillingSession) Reserve(targetQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.settled || s.refunded || s.trusted || targetQuota <= s.preConsumedQuota {
		return nil
	}
	if s.refunding {
		return errors.New("billing session refund is in progress")
	}
	if s.poisoned != nil {
		return fmt.Errorf("billing session cannot reserve after a failed rollback: %w", s.poisoned)
	}

	delta := targetQuota - s.preConsumedQuota
	if delta <= 0 {
		return nil
	}

	if err := s.funding.Reserve(delta); err != nil {
		return wrapFundingReserveError(s.funding.Source(), err)
	}
	if err := s.tokenAccounting.Reserve(delta); err != nil {
		if rollbackErr := s.funding.Reserve(-delta); rollbackErr != nil {
			common.SysLog("error rolling back funding reserve: " + rollbackErr.Error())
			s.poisoned = errors.Join(err, rollbackErr)
			return fmt.Errorf("token reserve failed and funding rollback failed: %w", s.poisoned)
		}
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}

	s.preConsumedQuota += delta
	s.tokenConsumed = s.tokenAccounting.ReservedQuota()
	s.extraReserved += delta
	s.syncRelayInfo()
	return nil
}

func wrapFundingReserveError(source string, err error) error {
	if err == nil {
		return nil
	}
	if source == BillingSourceSubscription {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("订阅额度不足或未配置订阅: %w", err),
			types.ErrorCodeInsufficientUserQuota,
			http.StatusForbidden,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithNoRecordErrorLog(),
		)
	}
	return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口（含信任额度旁路）
// ---------------------------------------------------------------------------

// preConsume 执行预扣费：信任检查 -> 令牌预扣 -> 资金来源预扣。
// 后一步失败时同步补偿前一步；若补偿本身失败，会话保留 reservation
// 并随错误返回，让入口立即再次补偿且保留可审计的组合错误。
func (s *BillingSession) preConsume(c *gin.Context, quota int) *types.NewAPIError {
	effectiveQuota := quota

	// ---- 信任额度旁路 ----
	if s.shouldTrust(c) {
		s.trusted = true
		effectiveQuota = 0
		logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足, 信任且不需要预扣费 (funding=%s)", s.relayInfo.UserId, s.funding.Source()))
	} else if effectiveQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s (funding=%s)", s.relayInfo.UserId, logger.FormatQuota(effectiveQuota), s.funding.Source()))
	}

	// ---- 1) 预扣令牌额度 ----
	if effectiveQuota > 0 {
		if err := s.tokenAccounting.PreConsume(effectiveQuota); err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		s.tokenConsumed = s.tokenAccounting.ReservedQuota()
	}

	// ---- 2) 预扣资金来源 ----
	if err := s.funding.PreConsume(effectiveQuota); err != nil {
		// 预扣费失败，回滚令牌额度
		var rollbackErr error
		if s.tokenAccounting.HasReservation() {
			if rollbackErr = s.tokenAccounting.Refund(); rollbackErr != nil {
				common.SysLog(fmt.Sprintf("error rolling back token quota (userId=%d, tokenId=%d, amount=%d, fundingErr=%s): %s",
					s.relayInfo.UserId, s.relayInfo.TokenId, s.tokenConsumed, err.Error(), rollbackErr.Error()))
			}
		}
		s.tokenConsumed = s.tokenAccounting.ReservedQuota()
		if rollbackErr != nil {
			s.poisoned = errors.Join(err, rollbackErr)
			return types.NewError(s.poisoned, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		// TODO: model 层应定义哨兵错误（如 ErrNoActiveSubscription），用 errors.Is 替代字符串匹配
		errMsg := err.Error()
		if strings.Contains(errMsg, "no active subscription") || strings.Contains(errMsg, "subscription quota insufficient") {
			return types.NewErrorWithStatusCode(fmt.Errorf("订阅额度不足或未配置订阅: %s", errMsg), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}

	s.preConsumedQuota = effectiveQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return nil
}

// shouldTrust 统一信任额度检查，适用于钱包和订阅。
func (s *BillingSession) shouldTrust(c *gin.Context) bool {
	if !s.allowTrust {
		return false
	}
	// 异步任务（ForcePreConsume=true）必须预扣全额，不允许信任旁路
	if s.relayInfo.ForcePreConsume {
		return false
	}

	trustQuota := common.GetTrustQuota()
	if trustQuota <= 0 {
		return false
	}

	// 检查令牌是否充足
	tokenTrusted := s.relayInfo.TokenUnlimited
	if !tokenTrusted {
		tokenQuota := c.GetInt("token_quota")
		tokenTrusted = tokenQuota > trustQuota
	}
	if !tokenTrusted {
		return false
	}

	switch s.funding.Source() {
	case BillingSourceWallet:
		return s.relayInfo.UserQuota > trustQuota
	case BillingSourceSubscription:
		// 订阅不能启用信任旁路。原因：
		// 1. PreConsumeUserSubscription 要求 amount>0 来创建预扣记录并锁定订阅
		// 2. SubscriptionFunding.PreConsume 忽略参数，始终用 s.amount 预扣
		// 3. 若信任旁路将 effectiveQuota 设为 0，会导致 preConsumedQuota 与实际订阅预扣不一致
		return false
	default:
		return false
	}
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的兼容字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()
	if synchronizer, ok := s.funding.(interface {
		SyncBillingRelayInfo(*relaycommon.RelayInfo)
	}); ok {
		synchronizer.SyncBillingRelayInfo(info)
		return
	}

	if sub, ok := s.funding.(*SubscriptionFunding); ok {
		info.SubscriptionId = sub.subscriptionId
		info.SubscriptionPreConsumed = sub.preConsumed + int64(s.extraReserved)
		info.SubscriptionPostDelta = 0
		info.SubscriptionAmountTotal = sub.AmountTotal
		info.SubscriptionAmountUsedAfterPreConsume = sub.AmountUsedAfter + int64(s.extraReserved)
		info.SubscriptionPlanId = sub.PlanId
		info.SubscriptionPlanTitle = sub.PlanTitle
	} else {
		info.SubscriptionId = 0
		info.SubscriptionPreConsumed = 0
	}
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 根据计费偏好创建会话并处理回退
// ---------------------------------------------------------------------------

func validateBillingSessionInput(relayInfo *relaycommon.RelayInfo, preConsumedQuota int) *types.NewAPIError {
	if relayInfo == nil {
		return types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	if relayInfo.QuotaClamp != nil {
		return types.NewErrorWithStatusCode(
			relayInfo.QuotaClamp,
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if preConsumedQuota < 0 {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("pre-consume quota cannot be negative: %d", preConsumedQuota),
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	return nil
}

// NewBillingSessionWithFunding creates a session with caller-owned funding and
// token accounting. Edge uses this entry point with a durable local funding
// adapter that atomically owns both funding and token balance reservations.
func NewBillingSessionWithFunding(
	c *gin.Context,
	relayInfo *relaycommon.RelayInfo,
	preConsumedQuota int,
	funding FundingSource,
	tokenAccounting TokenQuotaAccounting,
) (*BillingSession, *types.NewAPIError) {
	if apiErr := validateBillingSessionInput(relayInfo, preConsumedQuota); apiErr != nil {
		return nil, apiErr
	}
	fundingValue := reflect.ValueOf(funding)
	tokenAccountingValue := reflect.ValueOf(tokenAccounting)
	if funding == nil || tokenAccounting == nil ||
		((fundingValue.Kind() == reflect.Chan || fundingValue.Kind() == reflect.Func || fundingValue.Kind() == reflect.Interface || fundingValue.Kind() == reflect.Map || fundingValue.Kind() == reflect.Pointer || fundingValue.Kind() == reflect.Slice) && fundingValue.IsNil()) ||
		((tokenAccountingValue.Kind() == reflect.Chan || tokenAccountingValue.Kind() == reflect.Func || tokenAccountingValue.Kind() == reflect.Interface || tokenAccountingValue.Kind() == reflect.Map || tokenAccountingValue.Kind() == reflect.Pointer || tokenAccountingValue.Kind() == reflect.Slice) && tokenAccountingValue.IsNil()) {
		return nil, types.NewError(fmt.Errorf("billing session dependencies are incomplete"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	session := &BillingSession{
		relayInfo:       relayInfo,
		funding:         funding,
		tokenAccounting: tokenAccounting,
	}
	if apiErr := session.preConsume(c, preConsumedQuota); apiErr != nil {
		if session.NeedsRefund() {
			return session, apiErr
		}
		return nil, apiErr
	}
	return session, nil
}

// NewBillingSession 根据用户计费偏好创建 BillingSession，处理 subscription_first / wallet_first 的回退。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	if apiErr := validateBillingSessionInput(relayInfo, preConsumedQuota); apiErr != nil {
		return nil, apiErr
	}

	pref := common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference)

	// 钱包路径需要先检查用户额度
	tryWallet := func() (*BillingSession, *types.NewAPIError) {
		userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if userQuota <= 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if userQuota-preConsumedQuota < 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		relayInfo.UserQuota = userQuota

		session := &BillingSession{
			relayInfo:       relayInfo,
			funding:         &WalletFunding{userId: relayInfo.UserId},
			tokenAccounting: newDatabaseTokenQuotaAccounting(relayInfo),
			allowTrust:      true,
		}
		if apiErr := session.preConsume(c, preConsumedQuota); apiErr != nil {
			if session.NeedsRefund() {
				return session, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}

	trySubscription := func() (*BillingSession, *types.NewAPIError) {
		subConsume := int64(preConsumedQuota)
		if subConsume <= 0 {
			subConsume = 1
		}
		session := &BillingSession{
			relayInfo:       relayInfo,
			tokenAccounting: newDatabaseTokenQuotaAccounting(relayInfo),
			allowTrust:      true,
			funding: &SubscriptionFunding{
				requestId: relayInfo.RequestId,
				userId:    relayInfo.UserId,
				modelName: relayInfo.OriginModelName,
				amount:    subConsume,
			},
		}
		// 必须传 subConsume 而非 preConsumedQuota，保证 SubscriptionFunding.amount、
		// preConsume 参数和 FinalPreConsumedQuota 三者一致，避免订阅多扣费。
		if apiErr := session.preConsume(c, int(subConsume)); apiErr != nil {
			if session.NeedsRefund() {
				return session, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}

	switch pref {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		session, err := tryWallet()
		if err != nil {
			if session != nil {
				return session, err
			}
			if err.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				return trySubscription()
			}
			return nil, err
		}
		return session, nil
	case "subscription_first":
		fallthrough
	default:
		hasSub, subCheckErr := model.HasActiveUserSubscription(relayInfo.UserId)
		if subCheckErr != nil {
			return nil, types.NewError(subCheckErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !hasSub {
			return tryWallet()
		}
		session, apiErr := trySubscription()
		if apiErr != nil {
			if session != nil {
				return session, apiErr
			}
			if apiErr.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				// 仅当用户的活跃订阅允许钱包回退时才回退到钱包，否则返回订阅额度不足错误
				allowOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
				if overflowErr != nil {
					return nil, types.NewError(overflowErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
				}
				if allowOverflow {
					return tryWallet()
				}
				return nil, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}
}
