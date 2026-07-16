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

func ProcessLeaseAcquire(principal *ControlPrincipal, request dto.EdgeLeaseAcquireRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindLeaseAcquire, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindLeaseAcquire, request.Meta.RequestID, serverRequestID, now, err)
	}
	response, err := ExecuteControlMutation(principal, controlRequestKindLeaseAcquire, controlReceiptTTL(), func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		if identity.Node.Status != model.EdgeNodeStatusActive {
			return controlDomainErrorResult(http.StatusForbidden, dto.EdgeControlErrorCodeNodeDisabledV1, "edge node is disabled", false, request.Meta.RequestID, serverRequestID, now, nil, nil)
		}
		lease, err := AcquireMasterQuotaLeaseTx(tx, identity, MasterLeaseAcquireCommand{
			Request:        request,
			IdempotencyKey: principal.SignedRequest.Metadata.IdempotencyKey,
			RequestHash:    principal.RequestHash,
			Now:            now,
			Policy: MasterLeasePolicy{
				TTL:           masterLeaseTTL(),
				MaxLeaseQuota: masterLeaseMaxQuota(),
				RenewDivisor:  int64(common.GetEnvOrDefault("EDGE_LEASE_RENEW_DIVISOR", 4)),
			},
		})
		if err != nil {
			switch {
			case errors.Is(err, ErrMasterLeaseUnavailable):
				retryAfter := int64(2)
				return controlDomainErrorResult(http.StatusConflict, dto.EdgeControlErrorCodeLeaseUnavailableV1, "quota lease is unavailable", true, request.Meta.RequestID, serverRequestID, now, &retryAfter, nil)
			case errors.Is(err, ErrMasterLeaseConflict):
				return controlDomainErrorResult(http.StatusConflict, dto.EdgeControlErrorCodeLeaseConflictV1, "quota lease conflicts with authoritative state", false, request.Meta.RequestID, serverRequestID, now, nil, nil)
			default:
				return nil, err
			}
		}
		leaseDTO := lease.ToDTO(identity.Node.NodeUID)
		result := dto.EdgeLeaseAcquireResponseV1{
			Meta:  NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now),
			Lease: leaseDTO,
		}
		if err := result.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: lease.LeaseUID, Response: result}, nil
	})
	if err == nil && response.StatusCode == http.StatusOK {
		InvalidateMasterLeaseSubjectCaches(int(request.Subject.UserID))
	}
	return response, err
}

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
			switch {
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
			case errors.Is(err, ErrMasterSettlementConflict), errors.Is(err, ErrMasterLeaseQuotaExceeded), errors.Is(err, ErrMasterDynamicPricingUnsupported):
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
	if err == nil && response.StatusCode == http.StatusOK {
		seenUsers := make(map[int]struct{}, len(request.Events))
		for _, event := range request.Events {
			userID := int(event.UserID)
			if _, exists := seenUsers[userID]; exists {
				continue
			}
			seenUsers[userID] = struct{}{}
			InvalidateMasterLeaseSubjectCaches(userID)
		}
	}
	return response, err
}

func ProcessLeaseClose(principal *ControlPrincipal, request dto.EdgeLeaseCloseRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindLeaseClose, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindLeaseClose, request.Meta.RequestID, serverRequestID, now, err)
	}
	userID := 0
	response, err := ExecuteControlMutation(principal, controlRequestKindLeaseClose, controlReceiptTTL(), func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		closed, err := CloseMasterQuotaLeaseTx(tx, identity, MasterLeaseCloseCommand{Request: request, Now: now})
		if err != nil {
			if errors.Is(err, ErrMasterLeaseConflict) {
				return controlDomainErrorResult(http.StatusConflict, dto.EdgeControlErrorCodeLeaseConflictV1, "quota lease close conflicts with authoritative state", false, request.Meta.RequestID, serverRequestID, now, nil, nil)
			}
			return nil, err
		}
		var lease model.EdgeQuotaLease
		if err := tx.Where("lease_uid = ?", request.LeaseID).First(&lease).Error; err != nil {
			return nil, err
		}
		userID = lease.UserID
		closed.Meta = NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now)
		if err := closed.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: request.LeaseID, Response: closed}, nil
	})
	if err == nil && response.StatusCode == http.StatusOK && userID > 0 {
		InvalidateMasterLeaseSubjectCaches(userID)
	}
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

func masterLeaseTTL() time.Duration {
	seconds := common.GetEnvOrDefault("EDGE_LEASE_TTL_SECONDS", 900)
	if seconds < 60 {
		seconds = 60
	}
	if seconds > 86400 {
		seconds = 86400
	}
	return time.Duration(seconds) * time.Second
}

func masterLeaseMaxQuota() int64 {
	quota := common.GetEnvOrDefault("EDGE_LEASE_MAX_QUOTA", 500_000)
	if quota <= 0 || quota > common.MaxQuota {
		quota = 500_000
	}
	return int64(quota)
}
