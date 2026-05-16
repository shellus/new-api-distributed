package middleware

import (
	"io"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	auditservice "github.com/QuantumNous/new-api/service/audit"
	"github.com/gin-gonic/gin"
)

type auditResponseWriter struct {
	gin.ResponseWriter
	capture *auditservice.CaptureBuffer
}

func (w *auditResponseWriter) Write(data []byte) (int, error) {
	w.capture.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *auditResponseWriter) WriteString(data string) (int, error) {
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
		c.Writer = &auditResponseWriter{
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
