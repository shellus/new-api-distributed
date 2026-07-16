package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeEdgeCPADisablesAndRestoresRuntimeAtomically(t *testing.T) {
	edgeCPAReadinessRequired.Store(true)
	edgeCPAReady.Store(true)
	t.Cleanup(func() {
		edgeCPAReady.Store(false)
		edgeCPAReadinessRequired.Store(false)
	})
	var healthStatus atomic.Int32
	healthStatus.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead || request.URL.Path != "/healthz" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(int(healthStatus.Load()))
	}))
	defer server.Close()
	db, _ := newEdgeRuntimeTestDB(t, server.URL)

	var channel model.Channel
	require.NoError(t, db.First(&channel, 31).Error)
	assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
	var ability model.Ability
	require.NoError(t, db.Where("channel_id = ? AND model = ?", 31, "gpt-test").First(&ability).Error)
	assert.True(t, ability.Enabled)

	statuses, err := ProbeEdgeCPA(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, dto.EdgeLocalServiceCPAPro20x4V1, statuses[0].LocalService)
	assert.False(t, statuses[0].Healthy)
	assert.Empty(t, statuses[0].AvailableModels)
	assert.False(t, edgeCPAReady.Load())

	require.NoError(t, db.First(&channel, 31).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
	require.NoError(t, db.Where("channel_id = ? AND model = ?", 31, "gpt-test").First(&ability).Error)
	assert.False(t, ability.Enabled)
	var signedProjection model.EdgeLocalChannelProjection
	require.NoError(t, db.First(&signedProjection, 31).Error)
	assert.True(t, signedProjection.Enabled, "health must only change the runtime projection")

	healthStatus.Store(http.StatusNoContent)
	statuses, err = ProbeEdgeCPA(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.True(t, statuses[0].Healthy)
	assert.Equal(t, []string{"gpt-test"}, statuses[0].AvailableModels)
	assert.True(t, edgeCPAReady.Load())

	require.NoError(t, db.First(&channel, 31).Error)
	assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
	require.NoError(t, db.Where("channel_id = ? AND model = ?", 31, "gpt-test").First(&ability).Error)
	assert.True(t, ability.Enabled)
}
