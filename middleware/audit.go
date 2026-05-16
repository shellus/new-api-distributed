package middleware

import (
	"bytes"
	"io"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	auditservice "github.com/QuantumNous/new-api/service/audit"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// auditResponseWriter wraps gin.ResponseWriter for admin operation auditing.
type auditResponseWriter struct {
	gin.ResponseWriter
	body    *bytes.Buffer
	maxSize int
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.body.Len() < w.maxSize {
		remain := w.maxSize - w.body.Len()
		if remain >= len(b) {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remain])
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// auditRouteActions maps "METHOD + route template" to language-neutral action ids.
var auditRouteActions = map[string]string{
	// 用户管理
	"POST /api/user/topup/complete":                    "user.topup_complete",
	"DELETE /api/user/:id/reset_passkey":               "user.reset_passkey",
	"DELETE /api/user/:id/oauth/bindings/:provider_id": "user.oauth_unbind",

	// 系统设置（root）
	"POST /api/option/payment_compliance":       "option.payment_compliance",
	"POST /api/option/rest_model_ratio":         "option.reset_ratio",
	"DELETE /api/option/channel_affinity_cache": "option.clear_affinity_cache",

	// 自定义 OAuth（root）
	"POST /api/custom-oauth-provider/":      "custom_oauth.create",
	"PUT /api/custom-oauth-provider/:id":    "custom_oauth.update",
	"DELETE /api/custom-oauth-provider/:id": "custom_oauth.delete",

	// 性能/缓存（root）
	"DELETE /api/performance/disk_cache": "performance.clear_disk_cache",
	"POST /api/performance/gc":           "performance.gc",
	"DELETE /api/performance/logs":       "performance.clear_logs",

	// 兑换码
	"PUT /api/redemption/":           "redemption.update",
	"DELETE /api/redemption/:id":     "redemption.delete",
	"DELETE /api/redemption/invalid": "redemption.delete_invalid",

	// 预填组
	"POST /api/prefill_group/":      "prefill_group.create",
	"PUT /api/prefill_group/":       "prefill_group.update",
	"DELETE /api/prefill_group/:id": "prefill_group.delete",

	// 供应商
	"POST /api/vendors/":      "vendor.create",
	"PUT /api/vendors/":       "vendor.update",
	"DELETE /api/vendors/:id": "vendor.delete",

	// 模型元数据
	"POST /api/models/":              "model.create",
	"PUT /api/models/":               "model.update",
	"DELETE /api/models/:id":         "model.delete",
	"POST /api/models/sync_upstream": "model.sync_upstream",

	// 部署
	"POST /api/deployments/":      "deployment.create",
	"PUT /api/deployments/:id":    "deployment.update",
	"DELETE /api/deployments/:id": "deployment.delete",

	// 订阅（管理员）
	"POST /api/subscription/admin/plans":    "subscription.plan_create",
	"PUT /api/subscription/admin/plans/:id": "subscription.plan_update",
	"POST /api/subscription/admin/bind":     "subscription.bind",

	// 日志
	"DELETE /api/log/": "log.clear",
}

// beginAdminAudit wraps admin/root write requests after auth has passed.
func beginAdminAudit(c *gin.Context) *auditResponseWriter {
	method := c.Request.Method
	if method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" {
		return nil
	}
	writer := &auditResponseWriter{
		ResponseWriter: c.Writer,
		body:           bytes.NewBuffer(nil),
		maxSize:        64 * 1024,
	}
	c.Writer = writer
	return writer
}

// finishAdminAudit records fallback admin/root write-operation audit logs.
func finishAdminAudit(c *gin.Context, writer *auditResponseWriter) {
	if writer == nil {
		return
	}
	method := c.Request.Method

	if common.GetContextKeyBool(c, constant.ContextKeyAuditLogged) {
		return
	}

	operatorId := c.GetInt("id")
	operatorName := c.GetString("username")
	operatorRole := c.GetInt("role")
	ip := c.ClientIP()
	status := writer.Status()
	success := auditResponseSuccess(status, writer.body.Bytes())

	route := c.FullPath()
	action := auditRouteActions[method+" "+route]
	if action == "" {
		action = "generic"
	}

	routeParams := map[string]string{}
	for _, p := range c.Params {
		routeParams[p.Key] = p.Value
	}

	opParams := map[string]interface{}{}
	if action == "generic" {
		opParams["method"] = method
		opParams["route"] = route
	}

	content := method + " " + route

	adminInfo := map[string]interface{}{
		"admin_id":       operatorId,
		"admin_username": operatorName,
		"admin_role":     operatorRole,
		"auth_method":    auditAuthMethod(c),
	}
	auditInfo := map[string]interface{}{
		"method":  method,
		"route":   route,
		"path":    c.Request.URL.Path,
		"status":  status,
		"success": success,
	}
	if len(routeParams) > 0 {
		auditInfo["params"] = routeParams
	}

	gopool.Go(func() {
		model.RecordOperationAuditLog(operatorId, content, ip, action, opParams, adminInfo, auditInfo)
	})
}

func auditAuthMethod(c *gin.Context) string {
	if c.GetBool("use_access_token") {
		return "access_token"
	}
	return "session"
}

// auditResponseSuccess infers operation success from HTTP status and JSON success field.
func auditResponseSuccess(status int, body []byte) bool {
	if status >= 400 {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var resp struct {
			Success *bool `json:"success"`
		}
		if err := common.Unmarshal(trimmed, &resp); err == nil && resp.Success != nil {
			return *resp.Success
		}
	}
	return status < 400
}

type requestResponseAuditWriter struct {
	gin.ResponseWriter
	capture *auditservice.CaptureBuffer
}

func (w *requestResponseAuditWriter) Write(data []byte) (int, error) {
	w.capture.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *requestResponseAuditWriter) WriteString(data string) (int, error) {
	w.capture.Write(common.StringToByteSlice(data))
	return w.ResponseWriter.WriteString(data)
}

func Audit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auditservice.Enabled() {
			c.Next()
			return
		}

		startedAt := time.Now()
		responseCapture := auditservice.NewCaptureBuffer(auditservice.MaxBodyBytes())
		originalWriter := c.Writer
		c.Writer = &requestResponseAuditWriter{
			ResponseWriter: originalWriter,
			capture:        responseCapture,
		}

		c.Next()

		requestBody := captureRequestBody(c, auditservice.MaxBodyBytes())
		responseBody := responseCapture.Body(c.Writer.Header().Get("Content-Type"))
		responseBody.StatusCode = c.Writer.Status()
		modelInfo := auditModelInfo(c)

		auditservice.ReportAsync(auditservice.Event{
			Version:   auditservice.ProtocolVersion,
			Event:     auditservice.EventRequestResponse,
			RequestID: c.GetString(common.RequestIdKey),
			Timestamp: time.Now(),
			Node:      auditservice.NodeName(),
			Route:     c.FullPath(),
			User: auditservice.UserInfo{
				ID: c.GetInt("id"),
				Username: firstNonEmpty(
					c.GetString("username"),
					common.GetContextKeyString(c, constant.ContextKeyUserName),
				),
			},
			Key: auditservice.KeyInfo{
				ID:   c.GetInt("token_id"),
				Name: c.GetString("token_name"),
			},
			Client: auditservice.ClientInfo{
				IP:        c.ClientIP(),
				Method:    c.Request.Method,
				Path:      c.Request.URL.RequestURI(),
				UserAgent: c.Request.UserAgent(),
			},
			Model:      modelInfo,
			Billing:    auditservice.BillingInfoFromContext(c),
			Request:    requestBody,
			Response:   responseBody,
			DurationMS: time.Since(startedAt).Milliseconds(),
			Metadata:   auditMetadata(c),
		})

		c.Writer = originalWriter
	}
}

func captureRequestBody(c *gin.Context, maxBodyBytes int64) auditservice.Body {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		common.SysError("audit request body capture failed: " + err.Error())
		return auditservice.Body{ContentType: c.Request.Header.Get("Content-Type")}
	}

	data, err := storage.Bytes()
	if err != nil {
		common.SysError("audit request body read failed: " + err.Error())
		return auditservice.Body{ContentType: c.Request.Header.Get("Content-Type")}
	}
	if _, err := storage.Seek(0, io.SeekStart); err != nil {
		common.SysError("audit request body rewind failed: " + err.Error())
	}
	c.Request.Body = io.NopCloser(storage)

	return auditservice.BodyFromBytes(data, c.Request.Header.Get("Content-Type"), maxBodyBytes)
}

func auditModelInfo(c *gin.Context) auditservice.ModelInfo {
	info := auditservice.ModelInfoFromContext(c)
	if info.Name == "" {
		info.Name = common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	}
	if info.OriginName == "" {
		info.OriginName = common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	}
	return info
}

func auditMetadata(c *gin.Context) map[string]string {
	metadata := make(map[string]string)
	putString := func(key string, value string) {
		if value != "" {
			metadata[key] = value
		}
	}
	putInt := func(key string, value int) {
		if value != 0 {
			metadata[key] = strconv.Itoa(value)
		}
	}

	putString("model", common.GetContextKeyString(c, constant.ContextKeyOriginalModel))
	putString("group", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
	putString("token_group", common.GetContextKeyString(c, constant.ContextKeyTokenGroup))
	putString("channel_name", common.GetContextKeyString(c, constant.ContextKeyChannelName))
	if _, exists := common.GetContextKey(c, constant.ContextKeyIsStream); exists {
		putString("is_stream", strconv.FormatBool(common.GetContextKeyBool(c, constant.ContextKeyIsStream)))
	}
	putInt("channel_id", common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	putInt("channel_type", common.GetContextKeyInt(c, constant.ContextKeyChannelType))

	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
