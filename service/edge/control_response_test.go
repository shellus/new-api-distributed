package edge

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewControlErrorHTTPResponseUsesRealStatusAndServerCorrelation(t *testing.T) {
	now := time.UnixMilli(1_784_160_000_123)
	response, err := NewControlErrorHTTPResponse(
		400,
		dto.EdgeControlErrorCodeInvalidRequestV1,
		"invalid declaration",
		false,
		"request-1",
		"server-request-1",
		now,
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 400, response.StatusCode)
	assert.Equal(t, `{"meta":{"protocol_version":"edge-control.v1","request_id":"request-1","server_request_id":"server-request-1","server_time_unix_milli":1784160000123},"error":{"code":"invalid_request","message":"invalid declaration","retryable":false}}`, string(response.Body))
}

func TestNewControlErrorHTTPResponseDoesNotEchoUntrustedClientRequestID(t *testing.T) {
	response, err := NewControlErrorHTTPResponse(
		401,
		dto.EdgeControlErrorCodeAuthenticationFailedV1,
		"authentication failed",
		false,
		"",
		"server-request-2",
		time.UnixMilli(1_784_160_000_000),
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, string(response.Body), `"request_id"`)
	assert.Contains(t, string(response.Body), `"server_request_id":"server-request-2"`)
}
