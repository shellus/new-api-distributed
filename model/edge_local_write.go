package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const edgeLocalSlowWriteThreshold = time.Second

var (
	edgeLocalWriteRetryDelays = []time.Duration{50 * time.Millisecond, 250 * time.Millisecond}
	edgeLocalWrites           = newEdgeLocalWriteCoordinator()
)

type edgeLocalWriteCoordinator struct {
	permit chan struct{}
}

func newEdgeLocalWriteCoordinator() *edgeLocalWriteCoordinator {
	coordinator := &edgeLocalWriteCoordinator{permit: make(chan struct{}, 1)}
	coordinator.permit <- struct{}{}
	return coordinator
}

func withEdgeLocalWrite(db *gorm.DB, operation string, write func() error) error {
	return edgeLocalWrites.run(edgeLocalWriteContext(db), operation, write)
}

func withEdgeLocalTransaction(db *gorm.DB, operation string, transaction func(*gorm.DB) error) error {
	return withEdgeLocalWrite(db, operation, func() error {
		return db.Transaction(transaction)
	})
}

func edgeLocalWriteContext(db *gorm.DB) context.Context {
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		return db.Statement.Context
	}
	return context.Background()
}

func (coordinator *edgeLocalWriteCoordinator) run(ctx context.Context, operation string, write func() error) (err error) {
	if coordinator == nil || write == nil {
		return errors.New("edge local write coordinator is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	waitStarted := time.Now()
	slowWaitTimer := time.NewTimer(edgeLocalSlowWriteThreshold)
	slowWaitTimerC := slowWaitTimer.C
	for {
		select {
		case <-ctx.Done():
			if !slowWaitTimer.Stop() {
				select {
				case <-slowWaitTimer.C:
				default:
				}
			}
			return ctx.Err()
		case <-slowWaitTimerC:
			common.SysLog(fmt.Sprintf("edge SQLite write waiting: operation=%s wait=%s", operation, time.Since(waitStarted).Round(time.Millisecond)))
			slowWaitTimerC = nil
		case <-coordinator.permit:
			if !slowWaitTimer.Stop() && slowWaitTimerC != nil {
				select {
				case <-slowWaitTimer.C:
				default:
				}
			}
			goto acquired
		}
	}

acquired:
	waitDuration := time.Since(waitStarted)
	runStarted := time.Now()
	slowRunTimer := time.AfterFunc(edgeLocalSlowWriteThreshold, func() {
		common.SysLog(fmt.Sprintf("edge SQLite write still running: operation=%s run=%s", operation, time.Since(runStarted).Round(time.Millisecond)))
	})
	defer func() {
		runDuration := time.Since(runStarted)
		slowRunTimer.Stop()
		coordinator.permit <- struct{}{}
		if waitDuration >= edgeLocalSlowWriteThreshold || runDuration >= edgeLocalSlowWriteThreshold {
			common.SysLog(fmt.Sprintf("edge SQLite write completed: operation=%s wait=%s run=%s error=%v",
				operation, waitDuration.Round(time.Millisecond), runDuration.Round(time.Millisecond), err))
		}
	}()

	return retryEdgeSQLiteWrite(ctx, operation, edgeLocalWriteRetryDelays, write)
}

func retryEdgeSQLiteWrite(ctx context.Context, operation string, retryDelays []time.Duration, write func() error) error {
	for attempt := 0; ; attempt++ {
		err := write()
		if err == nil || !isTransientEdgeSQLiteWriteError(err) || attempt >= len(retryDelays) {
			return err
		}
		delay := retryDelays[attempt]
		common.SysLog(fmt.Sprintf("edge SQLite transient write conflict: operation=%s attempt=%d retry_in=%s error=%v",
			operation, attempt+1, delay, err))
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type sqliteErrorCoder interface {
	Code() int
}

func isTransientEdgeSQLiteWriteError(err error) bool {
	if err == nil {
		return false
	}
	var coded sqliteErrorCoder
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY and SQLITE_LOCKED, including extended result codes.
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked")
}
