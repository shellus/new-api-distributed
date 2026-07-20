package middleware

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// EdgeTokenAuth reconstructs the same request context consumed by the shared
// relay from the local signed snapshot. It never falls back to Token/User
// tables or calls the master, and it rejects the admin channel-selection token
// suffix because roles are intentionally absent from the edge projection.
func EdgeTokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.IsEdgeMode() || model.DB == nil {
			abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "edge authentication is not ready")
			return
		}
		fingerprint, err := edgetoken.FingerprintAuthorization(edgeAuthorizationValue(c))
		if err != nil {
			status := http.StatusUnauthorized
			message := common.TranslateMessage(c, i18n.MsgTokenInvalid)
			if errors.Is(err, edgetoken.ErrTokenMissing) {
				message = common.TranslateMessage(c, i18n.MsgTokenNotProvided)
			}
			if errors.Is(err, edgetoken.ErrChannelSuffix) {
				status = http.StatusForbidden
				message = "channel-selection token suffixes are not allowed on edge"
				abortWithOpenAiMessage(c, status, message, types.ErrorCodeAccessDenied)
				return
			}
			abortWithOpenAiMessage(c, status, message)
			return
		}

		auth, err := model.GetEdgeLocalTokenAuth(model.DB, fingerprint)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				abortWithOpenAiMessage(c, http.StatusUnauthorized, common.TranslateMessage(c, i18n.MsgTokenInvalid))
			} else {
				common.SysError("edge token projection lookup failed: " + err.Error())
				abortWithOpenAiMessage(c, http.StatusInternalServerError, common.TranslateMessage(c, i18n.MsgDatabaseError))
			}
			return
		}
		nowUnixMilli := time.Now().UnixMilli()
		if !auth.Enabled || (auth.ExpiresAtUnixMilli != nil && *auth.ExpiresAtUnixMilli <= nowUnixMilli) {
			abortWithOpenAiMessage(c, http.StatusUnauthorized, common.TranslateMessage(c, i18n.MsgTokenInvalid))
			return
		}
		user, err := model.GetEdgeLocalUser(model.DB, auth.UserID)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				common.SysError("edge user projection lookup failed: " + err.Error())
			}
			abortWithOpenAiMessage(c, http.StatusForbidden, common.TranslateMessage(c, i18n.MsgAuthUserBanned), types.ErrorCodeAccessDenied)
			return
		}
		if !user.Enabled {
			abortWithOpenAiMessage(c, http.StatusForbidden, common.TranslateMessage(c, i18n.MsgAuthUserBanned), types.ErrorCodeAccessDenied)
			return
		}
		if len(auth.AllowedCIDRs) > 0 {
			clientIP := net.ParseIP(c.ClientIP())
			if clientIP == nil || !common.IsIpInCIDRList(clientIP, auth.AllowedCIDRs) {
				abortWithOpenAiMessage(c, http.StatusForbidden, "client IP is not allowed by this token", types.ErrorCodeAccessDenied)
				return
			}
		}

		groupPolicy, err := model.GetEdgeLocalGroup(model.DB, user.DefaultGroup)
		if err != nil {
			common.SysError(fmt.Sprintf("edge group projection lookup failed for user group %q: %v", user.DefaultGroup, err))
			abortWithOpenAiMessage(c, http.StatusForbidden, "the token group is unavailable on this edge", types.ErrorCodeAccessDenied)
			return
		}
		usingGroup := auth.Group
		if usingGroup == "" {
			usingGroup = user.DefaultGroup
		}
		groupRatio, specialRatio, allowed := edgeUsingGroupRatio(groupPolicy, usingGroup)
		if !allowed {
			abortWithOpenAiMessage(c, http.StatusForbidden, "the token group is unavailable on this edge", types.ErrorCodeAccessDenied)
			return
		}

		c.Set("id", int(auth.UserID))
		c.Set("token_id", int(auth.TokenID))
		// The plaintext token is intentionally absent from the edge projection.
		// Its stable fingerprint preserves per-token affinity semantics without
		// exposing the original credential to the local runtime context.
		c.Set("token_key", fingerprint)
		c.Set("token_name", auth.TokenName)
		c.Set("token_unlimited_quota", true)
		if auth.ModelLimitEnabled {
			allowedModels := make(map[string]bool, len(auth.AllowedModels))
			for _, modelName := range auth.AllowedModels {
				allowedModels[modelName] = true
			}
			c.Set("token_model_limit_enabled", true)
			c.Set("token_model_limit", allowedModels)
		} else {
			c.Set("token_model_limit_enabled", false)
		}
		common.SetContextKey(c, constant.ContextKeyTokenGroup, auth.Group)
		common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, auth.CrossGroupRetry)
		common.SetContextKey(c, constant.ContextKeyUserGroup, user.DefaultGroup)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, usingGroup)
		common.SetContextKey(c, constant.ContextKeyUserQuota, common.MaxQuota)
		common.SetContextKey(c, constant.ContextKeyUserStatus, common.UserStatusEnabled)
		common.SetContextKey(c, constant.ContextKeyUserEmail, user.Email)
		common.SetContextKey(c, constant.ContextKeyUserName, user.Username)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{
			AcceptUnsetRatioModel: user.Setting.AcceptUnsetRatioModel,
			Language:              user.Setting.Language,
			BillingPreference:     user.Setting.BillingPreference,
			RecordIpLog:           user.Setting.RecordIpLog,
		})
		common.SetContextKey(c, constant.ContextKeyEdgeGroupRatio, groupRatio)
		common.SetContextKey(c, constant.ContextKeyEdgeGroupSpecialRatio, specialRatio)
		c.Next()
	}
}

func edgeAuthorizationValue(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if protocols := c.GetHeader("Sec-WebSocket-Protocol"); protocols != "" {
		for _, protocol := range strings.Split(protocols, ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, "openai-insecure-api-key.") {
				return "Bearer " + strings.TrimPrefix(protocol, "openai-insecure-api-key.")
			}
		}
	}
	path := c.Request.URL.Path
	if strings.Contains(path, "/v1/messages") || strings.Contains(path, "/v1/models") {
		if key := c.GetHeader("x-api-key"); key != "" {
			return "Bearer " + key
		}
	}
	if strings.HasPrefix(path, "/v1beta/models") ||
		strings.HasPrefix(path, "/v1beta/openai/models") ||
		strings.HasPrefix(path, "/v1/models/") {
		if key := c.Query("key"); key != "" {
			return "Bearer " + key
		}
		if key := c.GetHeader("x-goog-api-key"); key != "" {
			return "Bearer " + key
		}
	}
	authorization := c.GetHeader("Authorization")
	if authorization == "" || authorization == "midjourney-proxy" {
		return c.GetHeader("mj-api-secret")
	}
	return authorization
}

func edgeUsingGroupRatio(policy *dto.EdgeGroupPolicyV1, usingGroup string) (float64, bool, bool) {
	if policy == nil {
		return 0, false, false
	}
	for _, candidate := range policy.UsingGroups {
		if candidate.Group == usingGroup && candidate.Enabled {
			return candidate.Ratio, candidate.SpecialRatio, true
		}
	}
	return 0, false, false
}
