package edge

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func NewControlResponseMeta(clientRequestID string, serverRequestID string, now time.Time) dto.EdgeControlResponseMetaV1 {
	return dto.EdgeControlResponseMetaV1{
		ProtocolVersion:     dto.EdgeControlProtocolVersionV1,
		RequestID:           clientRequestID,
		ServerRequestID:     serverRequestID,
		ServerTimeUnixMilli: now.UnixMilli(),
	}
}

func NewControlErrorHTTPResponse(
	statusCode int,
	code dto.EdgeControlErrorCodeV1,
	message string,
	retryable bool,
	clientRequestID string,
	serverRequestID string,
	now time.Time,
	retryAfterSeconds *int64,
	expected *dto.EdgeControlExpectedStateV1,
) (*ControlHTTPResponse, error) {
	response, err := NewControlErrorResponse(
		statusCode,
		code,
		message,
		retryable,
		clientRequestID,
		serverRequestID,
		now,
		retryAfterSeconds,
		expected,
	)
	if err != nil {
		return nil, err
	}
	body, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &ControlHTTPResponse{StatusCode: statusCode, Body: body}, nil
}

func NewControlErrorResponse(
	statusCode int,
	code dto.EdgeControlErrorCodeV1,
	message string,
	retryable bool,
	clientRequestID string,
	serverRequestID string,
	now time.Time,
	retryAfterSeconds *int64,
	expected *dto.EdgeControlExpectedStateV1,
) (dto.EdgeControlErrorResponseV1, error) {
	if statusCode < 400 || statusCode > 599 {
		return dto.EdgeControlErrorResponseV1{}, errors.New("edge control error requires a 4xx or 5xx status")
	}
	response := dto.EdgeControlErrorResponseV1{
		Meta: NewControlResponseMeta(clientRequestID, serverRequestID, now),
		Error: dto.EdgeControlErrorV1{
			Code:              code,
			Message:           message,
			Retryable:         retryable,
			RetryAfterSeconds: retryAfterSeconds,
			Expected:          expected,
		},
	}
	if err := response.Meta.Validate(); err != nil {
		return dto.EdgeControlErrorResponseV1{}, err
	}
	return response, nil
}
