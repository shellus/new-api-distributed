package service

import (
	"errors"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// TokenQuotaAccounting separates request billing from Token table mutation.
// Master requests use the database implementation; edge requests pass the
// no-op implementation because token quota was already reserved by the lease.
type TokenQuotaAccounting interface {
	PreConsume(amount int) error
	Reserve(delta int) error
	Settle(delta int) error
	Refund() error
	HasReservation() bool
	ReservedQuota() int
}

type databaseTokenQuotaAccounting struct {
	relayInfo *relaycommon.RelayInfo
	reserved  int
}

func newDatabaseTokenQuotaAccounting(relayInfo *relaycommon.RelayInfo) TokenQuotaAccounting {
	return &databaseTokenQuotaAccounting{relayInfo: relayInfo}
}

func (a *databaseTokenQuotaAccounting) PreConsume(amount int) error {
	if amount <= 0 || a.relayInfo.IsPlayground {
		return nil
	}
	if err := PreConsumeTokenQuota(a.relayInfo, amount); err != nil {
		return err
	}
	a.reserved += amount
	return nil
}

func (a *databaseTokenQuotaAccounting) Reserve(delta int) error {
	if delta == 0 || a.relayInfo.IsPlayground {
		return nil
	}
	if delta > 0 {
		if err := PreConsumeTokenQuota(a.relayInfo, delta); err != nil {
			return err
		}
		a.reserved += delta
		return nil
	}
	amount := -delta
	if amount > a.reserved {
		return errors.New("token reserve rollback exceeds reserved quota")
	}
	if err := model.IncreaseTokenQuota(a.relayInfo.TokenId, a.relayInfo.TokenKey, amount); err != nil {
		return err
	}
	a.reserved -= amount
	return nil
}

func (a *databaseTokenQuotaAccounting) Settle(delta int) error {
	if a.relayInfo.IsPlayground {
		a.reserved = 0
		return nil
	}
	if delta > 0 {
		if err := model.DecreaseTokenQuota(a.relayInfo.TokenId, a.relayInfo.TokenKey, delta); err != nil {
			return err
		}
	} else if delta < 0 {
		if err := model.IncreaseTokenQuota(a.relayInfo.TokenId, a.relayInfo.TokenKey, -delta); err != nil {
			return err
		}
	}
	a.reserved = 0
	return nil
}

func (a *databaseTokenQuotaAccounting) Refund() error {
	if a.reserved <= 0 || a.relayInfo.IsPlayground {
		a.reserved = 0
		return nil
	}
	// Token quota uses a non-idempotent quota += N update. Clear the in-memory
	// reservation before issuing it so an ambiguous database error cannot make
	// a later compensation attempt grant quota twice.
	amount := a.reserved
	a.reserved = 0
	return model.IncreaseTokenQuota(a.relayInfo.TokenId, a.relayInfo.TokenKey, amount)
}

func (a *databaseTokenQuotaAccounting) HasReservation() bool {
	return a != nil && a.reserved > 0
}

func (a *databaseTokenQuotaAccounting) ReservedQuota() int {
	if a == nil {
		return 0
	}
	return a.reserved
}

type NoopTokenQuotaAccounting struct{}

func (NoopTokenQuotaAccounting) PreConsume(int) error { return nil }
func (NoopTokenQuotaAccounting) Reserve(int) error    { return nil }
func (NoopTokenQuotaAccounting) Settle(int) error     { return nil }
func (NoopTokenQuotaAccounting) Refund() error        { return nil }
func (NoopTokenQuotaAccounting) HasReservation() bool { return false }
func (NoopTokenQuotaAccounting) ReservedQuota() int   { return 0 }
