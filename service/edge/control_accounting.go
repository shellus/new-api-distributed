package edge

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

func ProcessSettlementBlock(principal *ControlPrincipal, request dto.EdgeSettlementBlockRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSettlementBlock, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSettlementBlock, request.Meta.RequestID, serverRequestID, now, err)
	}
	response, err := ExecuteControlMutation(principal, controlRequestKindSettlementBlock, controlReceiptTTL(), func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		ack, err := SettleMasterUsageBlockTx(tx, identity, MasterSettlementCommand{
			Request:        request,
			IdempotencyKey: principal.SignedRequest.Metadata.IdempotencyKey,
			RequestHash:    principal.RequestHash,
			Now:            now,
		})
		if err != nil {
			var sequenceError *SettlementSequenceError
			var circuitError *SettlementCircuitError
			switch {
			case errors.As(err, &circuitError):
				common.SysError(fmt.Sprintf(
					"edge settlement circuit opened node=%s generation=%d block=%s epoch=%d: %s",
					principal.NodeUID, principal.Generation, request.BlockID, circuitError.Epoch, circuitError.Reason,
				))
				result, resultErr := controlDomainErrorResult(
					http.StatusTooManyRequests, dto.EdgeControlErrorCodeRateLimitedV1,
					"edge settlement circuit is open", false, request.Meta.RequestID, serverRequestID, now, nil, nil,
				)
				if result != nil {
					result.commitSettlementRejection = true
				}
				return result, resultErr
			case errors.As(err, &sequenceError):
				expected := sequenceError.Expected
				return controlDomainErrorResult(
					http.StatusConflict,
					dto.EdgeControlErrorCodeSettlementOutOfOrderV1,
					"settlement block is out of order",
					false,
					request.Meta.RequestID,
					serverRequestID,
					now,
					nil,
					&dto.EdgeControlExpectedStateV1{NextSettlementSequence: &expected},
				)
			case errors.Is(err, ErrMasterSettlementConflict), errors.Is(err, ErrMasterDynamicPricingUnsupported):
				common.SysError(fmt.Sprintf(
					"edge settlement rejected node=%s generation=%d block=%s: %v",
					principal.NodeUID, principal.Generation, request.BlockID, err,
				))
				return controlDomainErrorResult(http.StatusConflict, dto.EdgeControlErrorCodeSettlementConflictV1, "settlement block conflicts with authoritative state", false, request.Meta.RequestID, serverRequestID, now, nil, nil)
			default:
				return nil, err
			}
		}
		result := dto.EdgeSettlementBlockResponseV1{Meta: NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now), Ack: *ack}
		if err := result.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: ack.BlockID, Response: result}, nil
	})
	return response, err
}

func controlDomainErrorResult(
	statusCode int,
	code dto.EdgeControlErrorCodeV1,
	message string,
	retryable bool,
	clientRequestID string,
	serverRequestID string,
	now time.Time,
	retryAfterSeconds *int64,
	expected *dto.EdgeControlExpectedStateV1,
) (*ControlMutationResult, error) {
	response, err := NewControlErrorResponse(statusCode, code, message, retryable, clientRequestID, serverRequestID, now, retryAfterSeconds, expected)
	if err != nil {
		return nil, err
	}
	return &ControlMutationResult{StatusCode: statusCode, Response: response}, nil
}
