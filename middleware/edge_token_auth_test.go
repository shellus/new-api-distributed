package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeTokenAuthUsesOpaqueFingerprintForTokenContext(t *testing.T) {
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-token-auth.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	fingerprint, err := edgetoken.FingerprintStoredKey("TokenSecret")
	require.NoError(t, err)
	auth := dto.EdgeTokenAuthRecordV1{
		TokenFingerprint: fingerprint, TokenID: 2, UserID: 1, TokenName: "request-token", Enabled: true,
	}
	user := dto.EdgeUserPolicyV1{UserID: 1, Enabled: true, Username: "edge-user", DefaultGroup: "default", Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first", RecordIpLog: true}}
	group := dto.EdgeGroupPolicyV1{
		UserGroup:   "default",
		UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1, SpecialRatio: true}},
	}
	authPayload, err := common.Marshal(auth)
	require.NoError(t, err)
	userPayload, err := common.Marshal(user)
	require.NoError(t, err)
	groupPayload, err := common.Marshal(group)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.EdgeLocalAuthProjection{
		TokenFingerprint: fingerprint, TokenID: auth.TokenID, UserID: auth.UserID, Enabled: true, Payload: string(authPayload),
	}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalUserProjection{
		UserID: user.UserID, Enabled: true, Payload: string(userPayload),
	}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalGroupProjection{
		UserGroup: group.UserGroup, Payload: string(groupPayload),
	}).Error)

	previousDB := model.DB
	previousMode := common.CurrentRuntimeMode()
	model.DB = db
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, common.SetRuntimeMode(previousMode))
	})

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var tokenKey string
	var tokenName string
	var userSetting dto.UserSetting
	var groupSpecialRatio bool
	engine.POST("/v1/chat/completions", EdgeTokenAuth(), func(c *gin.Context) {
		tokenKey = c.GetString("token_key")
		tokenName = c.GetString("token_name")
		userSetting, _ = common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting)
		groupSpecialRatio = common.GetContextKeyBool(c, constant.ContextKeyEdgeGroupSpecialRatio)
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer sk-TokenSecret")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	assert.Equal(t, fingerprint, tokenKey)
	assert.NotEqual(t, "TokenSecret", tokenKey)
	assert.Equal(t, "request-token", tokenName)
	assert.Equal(t, "subscription_first", userSetting.BillingPreference)
	assert.True(t, userSetting.RecordIpLog)
	assert.True(t, groupSpecialRatio)

	user.Setting.RecordIpLog = false
	userPayload, err = common.Marshal(user)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.EdgeLocalUserProjection{}).Where("user_id = ?", user.UserID).Update("payload", string(userPayload)).Error)
	userSetting = dto.UserSetting{RecordIpLog: true}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer sk-TokenSecret")
	engine.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	assert.False(t, userSetting.RecordIpLog)
}
