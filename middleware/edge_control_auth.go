package middleware

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	edgeservice "github.com/QuantumNous/new-api/service/edge"

	"github.com/gin-gonic/gin"
)

const (
	edgeControlMaxRequestBodyBytes = int64(1 << 20)
	edgeControlMaxClockSkew        = 2 * time.Minute
)

func EdgeControlAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		serverRequestID := strings.ToLower(c.GetString(common.RequestIdKey))
		if serverRequestID == "" {
			serverRequestID = strings.ToLower(common.NewRequestId())
		}

		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != gin.MIMEJSON {
			abortEdgeControlRequest(c, http.StatusUnsupportedMediaType, dto.EdgeControlErrorCodeInvalidRequestV1, "content type must be application/json", false, serverRequestID)
			return
		}
		contentEncoding := strings.TrimSpace(strings.ToLower(c.GetHeader("Content-Encoding")))
		if contentEncoding != "" && contentEncoding != "identity" {
			abortEdgeControlRequest(c, http.StatusUnsupportedMediaType, dto.EdgeControlErrorCodeInvalidRequestV1, "content encoding must be identity", false, serverRequestID)
			return
		}
		if c.Request.Body == nil {
			abortEdgeControlRequest(c, http.StatusBadRequest, dto.EdgeControlErrorCodeInvalidRequestV1, "request body is required", false, serverRequestID)
			return
		}

		bodyReader := http.MaxBytesReader(c.Writer, c.Request.Body, edgeControlMaxRequestBodyBytes)
		rawBody, err := io.ReadAll(bodyReader)
		_ = c.Request.Body.Close()
		if err != nil {
			if common.IsRequestBodyTooLargeError(err) {
				abortEdgeControlRequest(c, http.StatusRequestEntityTooLarge, dto.EdgeControlErrorCodeInvalidRequestV1, "request body exceeds control-plane limit", false, serverRequestID)
				return
			}
			abortEdgeControlRequest(c, http.StatusBadRequest, dto.EdgeControlErrorCodeInvalidRequestV1, "failed to read request body", false, serverRequestID)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
		c.Request.ContentLength = int64(len(rawBody))

		principal, err := edgeservice.AuthenticateControlRequest(c.Request, rawBody, time.Now(), edgeControlMaxClockSkew)
		if err != nil {
			switch {
			case errors.Is(err, edgeauth.ErrUnsupportedVersion), errors.Is(err, edgeservice.ErrControlProtocol):
				abortEdgeControlRequest(c, http.StatusBadRequest, dto.EdgeControlErrorCodeUnsupportedProtocolV1, "unsupported control protocol", false, serverRequestID)
			case errors.Is(err, edgeauth.ErrInvalidSignature):
				abortEdgeControlRequest(c, http.StatusUnauthorized, dto.EdgeControlErrorCodeInvalidSignatureV1, "invalid request signature", false, serverRequestID)
			case errors.Is(err, edgeauth.ErrClockSkew):
				abortEdgeControlRequest(c, http.StatusUnauthorized, dto.EdgeControlErrorCodeAuthenticationFailedV1, "signed request timestamp is outside the allowed clock skew", true, serverRequestID)
			case errors.Is(err, edgeauth.ErrInvalidInput),
				errors.Is(err, edgeservice.ErrControlAuthentication),
				errors.Is(err, edgeservice.ErrControlNodeRevoked):
				abortEdgeControlRequest(c, http.StatusUnauthorized, dto.EdgeControlErrorCodeAuthenticationFailedV1, "control authentication failed", false, serverRequestID)
			default:
				common.SysError("edge control authentication failed: " + err.Error())
				abortEdgeControlRequest(c, http.StatusInternalServerError, dto.EdgeControlErrorCodeInternalV1, "internal control-plane error", true, serverRequestID)
			}
			return
		}

		common.SetContextKey(c, constant.ContextKeyEdgeControlPrincipal, principal)
		c.Next()
	}
}

func abortEdgeControlRequest(c *gin.Context, statusCode int, code dto.EdgeControlErrorCodeV1, message string, retryable bool, serverRequestID string) {
	response, err := edgeservice.NewControlErrorHTTPResponse(
		statusCode,
		code,
		message,
		retryable,
		"",
		serverRequestID,
		time.Now(),
		nil,
		nil,
	)
	if err != nil {
		common.SysError("failed to build edge control error response: " + err.Error())
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Data(response.StatusCode, "application/json; charset=utf-8", response.Body)
	c.Abort()
}
