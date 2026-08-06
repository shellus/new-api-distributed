package common

import (
	"bytes"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogStartupSuccessUsesEdgeIdentityWithoutSessionWarning(t *testing.T) {
	originalMode := CurrentRuntimeMode()
	originalSystemName := SystemName
	originalSessionCookieSecure := SessionCookieSecure
	originalWriter := gin.DefaultWriter
	t.Cleanup(func() {
		require.NoError(t, SetRuntimeMode(originalMode))
		SystemName = originalSystemName
		SessionCookieSecure = originalSessionCookieSecure
		LogWriterMu.Lock()
		gin.DefaultWriter = originalWriter
		LogWriterMu.Unlock()
	})

	require.NoError(t, SetRuntimeMode(RuntimeModeEdge))
	SystemName = "Test API"
	SessionCookieSecure = false
	var output bytes.Buffer
	LogWriterMu.Lock()
	gin.DefaultWriter = &output
	LogWriterMu.Unlock()

	LogStartupSuccess(time.Now(), "3000")

	assert.Contains(t, output.String(), "Test API Edge")
	assert.NotContains(t, output.String(), "Refresh cookie is not secure")

	require.NoError(t, SetRuntimeMode(RuntimeModeMaster))
	output.Reset()
	LogStartupSuccess(time.Now(), "3000")
	assert.Contains(t, output.String(), "Test API")
	assert.NotContains(t, output.String(), "Test API Edge")
	assert.Contains(t, output.String(), "Refresh cookie is not secure")
}
