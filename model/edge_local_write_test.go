package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeLocalWriteCoordinatorSerializesAndCancelsWaitingWrites(t *testing.T) {
	coordinator := newEdgeLocalWriteCoordinator()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinator.run(context.Background(), "first write", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	waitingContext, cancelWaiting := context.WithCancel(context.Background())
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	var secondExecuted atomic.Bool
	go func() {
		close(secondStarted)
		secondDone <- coordinator.run(waitingContext, "second write", func() error {
			secondExecuted.Store(true)
			return nil
		})
	}()
	<-secondStarted
	cancelWaiting()
	require.ErrorIs(t, <-secondDone, context.Canceled)
	assert.False(t, secondExecuted.Load())

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, coordinator.run(context.Background(), "third write", func() error {
		return nil
	}))
}

func TestRetryEdgeSQLiteWriteRetriesOnlyTransientLockErrors(t *testing.T) {
	t.Run("driver code", func(t *testing.T) {
		attempts := 0
		err := retryEdgeSQLiteWrite(context.Background(), "coded busy", []time.Duration{0, 0}, func() error {
			attempts++
			if attempts < 3 {
				return edgeSQLiteCodedTestError{code: 5}
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, attempts)
	})

	t.Run("wrapped message", func(t *testing.T) {
		attempts := 0
		err := retryEdgeSQLiteWrite(context.Background(), "message busy", []time.Duration{0}, func() error {
			attempts++
			if attempts == 1 {
				return fmt.Errorf("stage usage: %w", errors.New("database is locked (5) (SQLITE_BUSY)"))
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("domain error", func(t *testing.T) {
		attempts := 0
		domainErr := errors.New("reservation conflict")
		err := retryEdgeSQLiteWrite(context.Background(), "domain failure", []time.Duration{0, 0}, func() error {
			attempts++
			return domainErr
		})
		require.ErrorIs(t, err, domainErr)
		assert.Equal(t, 1, attempts)
	})
}

type edgeSQLiteCodedTestError struct {
	code int
}

func (err edgeSQLiteCodedTestError) Error() string {
	return fmt.Sprintf("sqlite error code %d", err.code)
}

func (err edgeSQLiteCodedTestError) Code() int {
	return err.code
}
