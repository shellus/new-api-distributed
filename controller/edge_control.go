package controller

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	edgeservice "github.com/QuantumNous/new-api/service/edge"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func EdgeControlBootstrap(c *gin.Context) {
	handleEdgeControlRequest(c, edgeservice.ControlRequestKindBootstrap,
		func(request dto.EdgeBootstrapRequestV1) string { return request.Meta.RequestID },
		edgeservice.ProcessBootstrap,
	)
}

func EdgeControlHeartbeat(c *gin.Context) {
	handleEdgeControlRequest(c, edgeservice.ControlRequestKindHeartbeat,
		func(request dto.EdgeHeartbeatRequestV1) string { return request.Meta.RequestID },
		edgeservice.ProcessHeartbeat,
	)
}

func EdgeControlSnapshotManifest(c *gin.Context) {
	handleEdgeControlRequest(c, edgeservice.ControlRequestKindSnapshotManifest,
		func(request dto.EdgeSnapshotManifestRequestV1) string { return request.Meta.RequestID },
		edgeservice.ProcessSnapshotManifest,
	)
}

func EdgeControlSnapshotPage(c *gin.Context) {
	handleEdgeControlRequest(c, edgeservice.ControlRequestKindSnapshotPage,
		func(request dto.EdgeSnapshotPageRequestV1) string { return request.Meta.RequestID },
		edgeservice.ProcessSnapshotPage,
	)
}

func EdgeControlSettlementBlock(c *gin.Context) {
	handleEdgeControlRequest(c, edgeservice.ControlRequestKindSettlementBlock,
		func(request dto.EdgeSettlementBlockRequestV1) string { return request.Meta.RequestID },
		edgeservice.ProcessSettlementBlock,
	)
}

func handleEdgeControlRequest[T any](
	c *gin.Context,
	requestKind string,
	requestID func(T) string,
	process func(*edgeservice.ControlPrincipal, T, string, time.Time) (*edgeservice.ControlHTTPResponse, error),
) {
	principal, ok := common.GetContextKeyType[*edgeservice.ControlPrincipal](c, constant.ContextKeyEdgeControlPrincipal)
	serverRequestID := strings.ToLower(c.GetString(common.RequestIdKey))
	if serverRequestID == "" {
		serverRequestID = strings.ToLower(common.NewRequestId())
	}
	if !ok || principal == nil {
		writeEdgeControlFailure(c, errors.New("edge control principal is missing"), "", serverRequestID)
		return
	}

	var request T
	if err := common.DecodeJsonStrict(bytes.NewReader(principal.RawBody), &request); err != nil {
		response, persistErr := edgeservice.PersistInvalidControlRequest(principal, requestKind, "", serverRequestID, time.Now(), err)
		if persistErr != nil {
			writeEdgeControlFailure(c, persistErr, "", serverRequestID)
			return
		}
		writeEdgeControlResponse(c, response)
		return
	}

	clientRequestID := requestID(request)
	response, err := process(principal, request, serverRequestID, time.Now())
	if err != nil {
		writeEdgeControlFailure(c, err, clientRequestID, serverRequestID)
		return
	}
	writeEdgeControlResponse(c, response)
}

func writeEdgeControlResponse(c *gin.Context, response *edgeservice.ControlHTTPResponse) {
	if response == nil {
		writeEdgeControlFailure(c, errors.New("edge control response is missing"), "", strings.ToLower(c.GetString(common.RequestIdKey)))
		return
	}
	c.Data(response.StatusCode, "application/json; charset=utf-8", response.Body)
}

func writeEdgeControlFailure(c *gin.Context, err error, clientRequestID string, serverRequestID string) {
	statusCode := http.StatusInternalServerError
	code := dto.EdgeControlErrorCodeInternalV1
	message := "internal control-plane error"
	retryable := true
	var retryAfterSeconds *int64

	switch {
	case errors.Is(err, edgeservice.ErrControlReceiptProcessing):
		statusCode = http.StatusConflict
		code = dto.EdgeControlErrorCodeReplayDetectedV1
		message = "control request is still processing"
		retryAfter := int64(1)
		retryAfterSeconds = &retryAfter
	case errors.Is(err, model.ErrEdgeRequestReceiptIdempotencyConflict):
		statusCode = http.StatusConflict
		code = dto.EdgeControlErrorCodeIdempotencyConflictV1
		message = "idempotency key conflicts with an earlier request"
		retryable = false
	case errors.Is(err, model.ErrEdgeRequestReceiptNonceConflict):
		statusCode = http.StatusConflict
		code = dto.EdgeControlErrorCodeReplayDetectedV1
		message = "signed nonce was already used"
		retryable = false
	case errors.Is(err, edgeservice.ErrControlAuthentication), errors.Is(err, edgeservice.ErrControlNodeRevoked):
		statusCode = http.StatusUnauthorized
		code = dto.EdgeControlErrorCodeAuthenticationFailedV1
		message = "control authentication failed"
		retryable = false
	case errors.Is(err, edgeservice.ErrControlProtocol):
		statusCode = http.StatusBadRequest
		code = dto.EdgeControlErrorCodeUnsupportedProtocolV1
		message = "unsupported control protocol"
		retryable = false
	case errors.Is(err, model.ErrEdgeSettlementSubscriptionUnavailable):
		statusCode = http.StatusServiceUnavailable
		code = dto.EdgeControlErrorCodeTemporarilyUnavailableV1
		message = "authoritative settlement subscription is unavailable"
		retryAfter := int64(5)
		retryAfterSeconds = &retryAfter
		common.SysError("edge settlement subscription unavailable: " + err.Error())
	case errors.Is(err, gorm.ErrRecordNotFound):
		statusCode = http.StatusServiceUnavailable
		code = dto.EdgeControlErrorCodeTemporarilyUnavailableV1
		message = "no published edge snapshot is available"
		retryAfter := int64(5)
		retryAfterSeconds = &retryAfter
	default:
		common.SysError("edge control request failed: " + err.Error())
	}

	if edgeauth.ValidateIdempotencyKey(clientRequestID) != nil {
		clientRequestID = ""
	}
	response, responseErr := edgeservice.NewControlErrorHTTPResponse(
		statusCode,
		code,
		message,
		retryable,
		clientRequestID,
		serverRequestID,
		time.Now(),
		retryAfterSeconds,
		nil,
	)
	if responseErr != nil {
		common.SysError("failed to build edge control failure response: " + responseErr.Error())
		c.Status(http.StatusInternalServerError)
		return
	}
	writeEdgeControlResponse(c, response)
}
