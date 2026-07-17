package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/constant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadEdgeLocalChannelConfigsSupportsSparseOverridesAndMultiKey(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(edgeChannelConfigDirEnv, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "mistral.yaml"), []byte(`name: mistral
type: mistral
base_url: https://api.mistral.ai
auth: |
  key-one
  key-two
models:
  - mistral-small-latest
groups:
  - vip
channel_setting:
  proxy: socks5h://proxy.internal:1080
settings:
  allow_speed: true
model_mapping:
  mistral-small-latest: mistral-small-upstream
param_override:
  temperature: 0
header_override:
  X-Edge: jp
`), 0o600))

	configs, err := loadEdgeLocalChannelConfigs()
	require.NoError(t, err)
	require.Contains(t, configs, "mistral")
	config := configs["mistral"]
	assert.Equal(t, constant.ChannelTypeMistral, config.Type)
	assert.Equal(t, "https://api.mistral.ai", config.BaseURL)
	assert.Equal(t, "key-one\nkey-two", config.Auth)
	assert.Equal(t, constant.MultiKeyModeRandom, config.MultiKeyMode)
	assert.Contains(t, config.Models, "mistral-small-latest")
	assert.Contains(t, config.Groups, "vip")
	assert.Equal(t, "socks5h://proxy.internal:1080", config.ChannelSetting["proxy"])
	assert.Equal(t, true, config.Settings["allow_speed"])
	assert.Equal(t, "mistral-small-upstream", config.ModelMapping["mistral-small-latest"])
}

func TestLoadEdgeLocalChannelConfigsSupportsCredentialJSON(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(edgeChannelConfigDirEnv, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "codex.yaml"), []byte(`name: codex-local
type: codex
auth_data:
  access_token: token-value
  account_id: account-value
`), 0o600))

	configs, err := loadEdgeLocalChannelConfigs()
	require.NoError(t, err)
	assert.JSONEq(t, `{"access_token":"token-value","account_id":"account-value"}`, configs["codex-local"].Auth)
}

func TestLoadEdgeLocalChannelConfigsRejectsAmbiguousCredentials(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(edgeChannelConfigDirEnv, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "invalid.yaml"), []byte(`name: invalid
type: openai
auth: key
auth_data:
  token: other
`), 0o600))

	_, err := loadEdgeLocalChannelConfigs()
	assert.ErrorContains(t, err, "mutually exclusive")
}

func TestValidateEdgeLocalChannelConfigsReportsNamesAndErrors(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(edgeChannelConfigDirEnv, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "mistral.yaml"), []byte(`name: mistral
type: mistral
base_url: https://api.mistral.ai
auth: key-one
`), 0o600))

	names, err := ValidateEdgeLocalChannelConfigs()
	require.NoError(t, err)
	assert.Equal(t, []string{"mistral"}, names)

	require.NoError(t, os.WriteFile(filepath.Join(directory, "broken.yaml"), []byte("name: Broken\n"), 0o600))
	_, err = ValidateEdgeLocalChannelConfigs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lowercase canonical form")
}
