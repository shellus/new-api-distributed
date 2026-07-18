package model

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeConsumeLogBillingEventKeyScopesRawEventIDs(t *testing.T) {
	key, err := EdgeConsumeLogBillingEventKey("edge.alpha", 1, "event-1")
	require.NoError(t, err)
	retryKey, err := EdgeConsumeLogBillingEventKey("edge.alpha", 1, "event-1")
	require.NoError(t, err)
	otherNodeKey, err := EdgeConsumeLogBillingEventKey("edge.beta", 1, "event-1")
	require.NoError(t, err)
	otherGenerationKey, err := EdgeConsumeLogBillingEventKey("edge.alpha", 2, "event-1")
	require.NoError(t, err)

	assert.Len(t, key, 64)
	assert.Equal(t, "a85180ed1c68353368ef5543154275f3a26a2acc4bbc764f7664d14687ae0aed", key)
	assert.Equal(t, key, retryKey)
	assert.NotEqual(t, key, otherNodeKey)
	assert.NotEqual(t, key, otherGenerationKey)
	_, err = EdgeConsumeLogBillingEventKey("edge.alpha", 0, "event-1")
	assert.Error(t, err)
}

func TestCreateEdgeConsumeLogOnceRejectsConflictingReplay(t *testing.T) {
	assert.True(t, LOG_DB.Migrator().HasIndex(&Log{}, "ux_logs_billing_event_key"))
	key, err := EdgeConsumeLogBillingEventKey("edge.consume-log", 1, "event-create-once")
	require.NoError(t, err)
	requestID := "request-create-once"
	createdAt := time.Now().Unix()
	t.Cleanup(func() {
		LOG_DB.Where("billing_event_key = ?", key).Delete(&Log{})
	})

	newLog := func() *Log {
		return &Log{
			UserId: 1, CreatedAt: createdAt, Type: LogTypeConsume,
			PromptTokens: 12, CompletionTokens: 3, ModelName: "gpt-test", Quota: 25,
			ChannelId: 2, TokenId: 3, UseTime: 1, Group: "default", RequestId: requestID,
		}
	}
	inserted, err := CreateEdgeConsumeLogOnce(context.Background(), newLog(), key)
	require.NoError(t, err)
	assert.True(t, inserted)

	inserted, err = CreateEdgeConsumeLogOnce(context.Background(), newLog(), key)
	require.NoError(t, err)
	assert.False(t, inserted)

	conflict := newLog()
	conflict.Quota++
	inserted, err = CreateEdgeConsumeLogOnce(context.Background(), conflict, key)
	assert.Error(t, err)
	assert.False(t, inserted)

	var count int64
	require.NoError(t, LOG_DB.Model(&Log{}).Where("billing_event_key = ?", key).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestClaimEdgeConsumeLogOutboxReturnsNilForEmptyQueue(t *testing.T) {
	db, err := OpenEdgeSQLite(filepath.Join(t.TempDir(), "empty-master-outbox.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&EdgeConsumeLogOutbox{}))
	previousDB := DB
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	claim, err := ClaimEdgeConsumeLogOutbox(context.Background(), time.Now(), time.Second)
	require.NoError(t, err)
	assert.Nil(t, claim)
}

func TestClaimEdgeConsumeLogOutboxConcurrentCASAndFencing(t *testing.T) {
	db, err := OpenEdgeSQLite(filepath.Join(t.TempDir(), "master-outbox.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&EdgeConsumeLogOutbox{}))
	previousDB := DB
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Now().Truncate(time.Second)
	key, err := EdgeConsumeLogBillingEventKey("edge.claim", 1, "event-claim")
	require.NoError(t, err)
	require.NoError(t, db.Create(&EdgeConsumeLogOutbox{
		EventID: 1, EventUID: key, Payload: "{}", AvailableAt: now.Unix(),
	}).Error)

	const workers = 24
	claims := make(chan *EdgeConsumeLogOutbox, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claim, claimErr := ClaimEdgeConsumeLogOutbox(context.Background(), now, time.Second)
			if claimErr != nil {
				errs <- claimErr
				return
			}
			if claim != nil {
				claims <- claim
			}
		}()
	}
	wait.Wait()
	close(claims)
	close(errs)

	claimed := make([]*EdgeConsumeLogOutbox, 0, 1)
	for claim := range claims {
		claimed = append(claimed, claim)
	}
	assert.Len(t, claimed, 1)
	for claimErr := range errs {
		require.NoError(t, claimErr)
	}

	first := claimed[0]
	recovered, err := ClaimEdgeConsumeLogOutbox(context.Background(), now.Add(2*time.Second), time.Second)
	require.NoError(t, err)
	assert.Equal(t, first.ID, recovered.ID)
	assert.Equal(t, first.Attempts+1, recovered.Attempts)
	assert.ErrorIs(t,
		MarkEdgeConsumeLogOutboxPublished(context.Background(), first, now.Add(2*time.Second)),
		ErrEdgeConsumeLogOutboxClaimLost,
	)
	require.NoError(t, MarkEdgeConsumeLogOutboxPublished(context.Background(), recovered, now.Add(2*time.Second)))

	var stored EdgeConsumeLogOutbox
	require.NoError(t, db.First(&stored, recovered.ID).Error)
	assert.Equal(t, EdgeConsumeLogOutboxStatusPublished, stored.Status)
	assert.Equal(t, 2, stored.Attempts)
}

func TestClaimEdgeConsumeLogOutboxRejectsInvalidClock(t *testing.T) {
	_, err := ClaimEdgeConsumeLogOutbox(context.Background(), time.Unix(-1, 0), time.Second)
	assert.EqualError(t, err, "edge consume-log outbox claim time must be positive")

	claim := &EdgeConsumeLogOutbox{ID: 1, Status: EdgeConsumeLogOutboxStatusPending, Attempts: 1, AvailableAt: 1}
	assert.Error(t, MarkEdgeConsumeLogOutboxPublished(context.Background(), claim, time.Unix(-1, 0)))
	assert.Error(t, MarkEdgeConsumeLogOutboxFailed(context.Background(), claim, errors.New("failed"), time.Unix(1, 0), time.Unix(-1, 0)))
}
