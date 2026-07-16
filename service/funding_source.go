package service

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"
)

// ---------------------------------------------------------------------------
// FundingSource — 资金来源接口（钱包 or 订阅）
// ---------------------------------------------------------------------------

// FundingSource 抽象了预扣费的资金来源。
type FundingSource interface {
	// Source 返回资金来源标识："wallet" 或 "subscription"
	Source() string
	// PreConsume 从该资金来源预扣 amount 额度
	PreConsume(amount int) error
	// Reserve 调整发送请求前的额外预留。正数增加预留，负数仅用于回滚本次增加。
	Reserve(delta int) error
	// Settle 根据差额调整资金来源（正数补扣，负数退还）
	Settle(delta int) error
	// Refund 退还所有预扣费
	Refund() error
	// HasReservation 表示该资金来源仍持有可退款的预留。
	HasReservation() bool
}

// ---------------------------------------------------------------------------
// WalletFunding — 钱包资金来源实现
// ---------------------------------------------------------------------------

type WalletFunding struct {
	userId   int
	consumed int // 实际预扣的用户额度
}

func (w *WalletFunding) Source() string { return BillingSourceWallet }

func (w *WalletFunding) PreConsume(amount int) error {
	if amount <= 0 {
		return nil
	}
	if err := model.DecreaseUserQuota(w.userId, amount, false); err != nil {
		return err
	}
	w.consumed = amount
	return nil
}

func (w *WalletFunding) Reserve(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		if err := model.DecreaseUserQuota(w.userId, delta, false); err != nil {
			return err
		}
		w.consumed += delta
		return nil
	}
	amount := -delta
	if amount > w.consumed {
		return errors.New("wallet reserve rollback exceeds consumed quota")
	}
	// This is the same non-idempotent credit operation as Refund. Record the
	// rollback attempt before issuing it so a later full refund cannot credit
	// this delta twice when the database result is ambiguous.
	w.consumed -= amount
	return model.IncreaseUserQuota(w.userId, amount, false)
}

func (w *WalletFunding) Settle(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		return model.DecreaseUserQuota(w.userId, delta, false)
	}
	return model.IncreaseUserQuota(w.userId, -delta, false)
}

func (w *WalletFunding) Refund() error {
	if w.consumed <= 0 {
		return nil
	}
	// IncreaseUserQuota 是 quota += N 的非幂等操作，不能重试，否则会多退额度。
	// 数据库返回错误时提交结果可能不明确，所以必须在调用前清除本地待退款
	// 状态，保持 at-most-once；订阅退款有 requestId 幂等保护，不受此限制。
	amount := w.consumed
	w.consumed = 0
	return model.IncreaseUserQuota(w.userId, amount, false)
}

func (w *WalletFunding) HasReservation() bool { return w != nil && w.consumed > 0 }

// ---------------------------------------------------------------------------
// SubscriptionFunding — 订阅资金来源实现
// ---------------------------------------------------------------------------

type SubscriptionFunding struct {
	requestId      string
	userId         int
	modelName      string
	amount         int64 // 预扣的订阅额度（subConsume）
	subscriptionId int
	preConsumed    int64
	extraReserved  int64
	// 以下字段在 PreConsume 成功后填充，供 RelayInfo 同步使用
	AmountTotal     int64
	AmountUsedAfter int64
	PlanId          int
	PlanTitle       string
}

func (s *SubscriptionFunding) Source() string { return BillingSourceSubscription }

func (s *SubscriptionFunding) PreConsume(_ int) error {
	// amount 参数被忽略，使用内部 s.amount（已在构造时根据 preConsumedQuota 计算）
	res, err := model.PreConsumeUserSubscription(s.requestId, s.userId, s.modelName, 0, s.amount)
	if err != nil {
		return err
	}
	s.subscriptionId = res.UserSubscriptionId
	s.preConsumed = res.PreConsumed
	s.AmountTotal = res.AmountTotal
	s.AmountUsedAfter = res.AmountUsedAfter
	// 获取订阅计划信息
	if planInfo, err := model.GetSubscriptionPlanInfoByUserSubscriptionId(res.UserSubscriptionId); err == nil && planInfo != nil {
		s.PlanId = planInfo.PlanId
		s.PlanTitle = planInfo.PlanTitle
	}
	return nil
}

func (s *SubscriptionFunding) Reserve(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta < 0 && int64(-delta) > s.extraReserved {
		return errors.New("subscription reserve rollback exceeds extra reserved quota")
	}
	if err := model.AdjustSubscriptionPreConsume(s.requestId, int64(delta)); err != nil {
		return err
	}
	s.extraReserved += int64(delta)
	return nil
}

func (s *SubscriptionFunding) Settle(delta int) error {
	if delta == 0 {
		return nil
	}
	return model.PostConsumeUserSubscriptionDelta(s.subscriptionId, int64(delta))
}

func (s *SubscriptionFunding) Refund() error {
	if s.preConsumed > 0 || s.extraReserved > 0 {
		if err := refundWithRetry(func() error {
			return model.RefundSubscriptionPreConsume(s.requestId)
		}); err != nil {
			return err
		}
		s.preConsumed = 0
		s.extraReserved = 0
	}
	return nil
}

func (s *SubscriptionFunding) HasReservation() bool {
	return s != nil && (s.preConsumed > 0 || s.extraReserved > 0)
}

// refundWithRetry 尝试多次执行退款操作以提高成功率，只能用于基于事务的退款函数！！！！！！
// try to refund with retries, only for refund functions based on transactions!!!
func refundWithRetry(fn func() error) error {
	if fn == nil {
		return nil
	}
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i < maxAttempts-1 {
			time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		}
	}
	return lastErr
}
