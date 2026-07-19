package model

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecordConsumeLogUsesProvidedRequestSnapshot(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:consume-log-request-snapshot?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))
	previousDB, previousLogDB := DB, LOG_DB
	previousLogEnabled := common.LogConsumeEnabled
	DB, LOG_DB = db, db
	common.LogConsumeEnabled = true
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.LogConsumeEnabled = previousLogEnabled
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx.Request.RemoteAddr = "198.51.100.20:1234"
	ctx.Set("username", "mutable-context-user")
	ctx.Set(common.RequestIdKey, "mutable-context-request")
	ctx.Set(common.UpstreamRequestIdKey, "mutable-context-upstream")
	useTime := int64(4)
	snapshot := &dto.EdgeConsumeLogSnapshotV1{
		Username: "request-user", TokenName: "request-token", ModelName: "request-model",
		Content: "request-content", UseTimeSeconds: &useTime, IP: "203.0.113.10",
		RequestID: "request-visible", UpstreamRequestID: "upstream-visible",
		Other: map[string]interface{}{"future_auto_carry": true},
	}

	RecordConsumeLog(ctx, 7, RecordConsumeLogParams{
		ChannelId: 31, PromptTokens: 10, CompletionTokens: 2, Quota: 12,
		TokenId: 11, IsStream: true, Group: "default", RequestSnapshot: snapshot,
	})

	var stored Log
	require.NoError(t, db.First(&stored).Error)
	assert.Equal(t, snapshot.Username, stored.Username)
	assert.Equal(t, snapshot.TokenName, stored.TokenName)
	assert.Equal(t, snapshot.ModelName, stored.ModelName)
	assert.Equal(t, snapshot.Content, stored.Content)
	assert.Equal(t, int(*snapshot.UseTimeSeconds), stored.UseTime)
	assert.Equal(t, snapshot.IP, stored.Ip)
	assert.Equal(t, snapshot.RequestID, stored.RequestId)
	assert.Equal(t, snapshot.UpstreamRequestID, stored.UpstreamRequestId)
	var other map[string]interface{}
	require.NoError(t, common.UnmarshalJsonStr(stored.Other, &other))
	assert.Equal(t, true, other["future_auto_carry"])
}
