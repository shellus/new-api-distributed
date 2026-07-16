package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeMode(t *testing.T) {
	originalMode := CurrentRuntimeMode()
	t.Cleanup(func() {
		require.NoError(t, SetRuntimeMode(originalMode))
	})

	require.NoError(t, SetRuntimeMode(RuntimeModeEdge))
	assert.Equal(t, RuntimeModeEdge, CurrentRuntimeMode())
	assert.True(t, IsEdgeMode())

	require.NoError(t, SetRuntimeMode(RuntimeModeMaster))
	assert.Equal(t, RuntimeModeMaster, CurrentRuntimeMode())
	assert.False(t, IsEdgeMode())
}

func TestSetRuntimeModeRejectsUnknownMode(t *testing.T) {
	originalMode := CurrentRuntimeMode()
	err := SetRuntimeMode(RuntimeMode("unknown"))
	require.Error(t, err)
	assert.Equal(t, originalMode, CurrentRuntimeMode())
}
