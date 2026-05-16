package middleware

import (
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	auditservice "github.com/QuantumNous/new-api/service/audit"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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
			Model:        modelInfo,
			Billing:      auditservice.BillingInfoFromContext(c),
			Request:      requestBody,
			Response:     responseBody,
			DurationMS:   time.Since(startedAt).Milliseconds(),
			Conversation: auditConversation(c, requestBody, responseBody),
			Metadata:     auditMetadata(c),
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

var auditSessionPattern = regexp.MustCompile(`_session_([A-Za-z0-9-]+)$`)

func auditConversation(c *gin.Context, requestBody auditservice.Body, responseBody auditservice.Body) auditservice.ConversationInfo {
	candidates := make(map[string]string)
	put := func(key string, value string) {
		if value != "" {
			candidates[key] = value
		}
	}

	put("header.session_id", c.GetHeader("Session_id"))
	put("header.x_amp_thread_id", c.GetHeader("X-Amp-Thread-Id"))
	put("header.x_session_id", c.GetHeader("X-Session-ID"))
	put("header.x_client_request_id", c.GetHeader("X-Client-Request-Id"))

	requestContent := requestBody.Content
	put("body.conversation_id", gjson.Get(requestContent, "conversation_id").String())
	put("body.session_id", gjson.Get(requestContent, "session_id").String())
	put("body.previous_response_id", gjson.Get(requestContent, "previous_response_id").String())
	put("body.prompt_cache_key", gjson.Get(requestContent, "prompt_cache_key").String())
	put("body.metadata.conversation_id", gjson.Get(requestContent, "metadata.conversation_id").String())
	put("body.metadata.session_id", gjson.Get(requestContent, "metadata.session_id").String())
	userID := gjson.Get(requestContent, "metadata.user_id").String()
	put("body.metadata.user_id", userID)
	if matches := auditSessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		put("body.metadata.user_id_session", matches[1])
	} else if len(userID) > 0 && userID[0] == '{' {
		put("body.metadata.user_id_session", gjson.Get(userID, "session_id").String())
	}

	responseID, messageID := auditResponseIDs(responseBody.Content)
	put("response.id", responseID)
	put("response.message_id", messageID)

	if len(candidates) == 0 {
		return auditservice.ConversationInfo{}
	}
	return auditservice.ConversationInfo{Candidates: candidates}
}

func auditResponseIDs(content string) (string, string) {
	if id := gjson.Get(content, "id").String(); id != "" {
		return id, ""
	}
	if id := gjson.Get(content, "response.id").String(); id != "" {
		return id, ""
	}
	if id := gjson.Get(content, "message.id").String(); id != "" {
		return "", id
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if id := gjson.Get(data, "response.id").String(); id != "" {
			return id, ""
		}
		if id := gjson.Get(data, "id").String(); id != "" {
			return id, ""
		}
		if id := gjson.Get(data, "message.id").String(); id != "" {
			return "", id
		}
	}

	return "", ""
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
