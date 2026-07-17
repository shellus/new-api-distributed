package edge

import (
	"crypto/ed25519"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeSnapshotCompilerProjectsCanonicalSafePolicy(t *testing.T) {
	state := edgeSnapshotCompilerTestState()
	projection, err := projectEdgeSnapshotDatabaseState(state)
	require.NoError(t, err)

	require.Len(t, projection.Authentication, 2)
	assert.Less(t, projection.Authentication[0].TokenFingerprint, projection.Authentication[1].TokenFingerprint)
	assert.Equal(t, []int64{1, 2}, []int64{projection.Users[0].UserID, projection.Users[1].UserID})
	assert.Equal(t, []int64{10, 20, 30, 40}, []int64{
		projection.Channels[0].ChannelID,
		projection.Channels[1].ChannelID,
		projection.Channels[2].ChannelID,
		projection.Channels[3].ChannelID,
	})
	assert.Equal(t, []string{"gpt-5.3-codex", "gpt-5.4", "gpt-5.5", "unsafe-image-model"}, projection.modelNames)
	assert.Equal(t, []dto.EdgeEndpointV1{
		dto.EdgeEndpointOpenAIChatCompletionsV1,
		dto.EdgeEndpointOpenAIResponsesV1,
	}, projection.Models[0].Endpoints)
	assert.Equal(t, []dto.EdgeEndpointV1{
		dto.EdgeEndpointOpenAIChatCompletionsV1,
		dto.EdgeEndpointOpenAIResponsesV1,
	}, projection.Models[1].Endpoints)
	assert.Equal(t, []dto.EdgeEndpointV1{
		dto.EdgeEndpointOpenAIChatCompletionsV1,
		dto.EdgeEndpointOpenAIResponsesV1,
	}, projection.Models[2].Endpoints)

	for _, authentication := range projection.Authentication {
		if authentication.ModelLimitEnabled {
			assert.Equal(t, []string{"gpt-5.3-codex", "gpt-5.4"}, authentication.AllowedModels)
		}
	}
	assert.Equal(t, "edge text policy", projection.Channels[0].TextPolicy.SystemPrompt)
	assert.True(t, projection.Channels[0].TextPolicy.SystemPromptOverride)
	assert.True(t, projection.Channels[0].TextPolicy.AllowServiceTier)
	assert.Equal(t, map[string]string{"gpt-5.5": "gpt-5.5"}, projection.Channels[3].ModelMapping)
	assert.Equal(t, "zh-cn", projection.Users[1].Setting.Language)
}

func TestEdgeSnapshotCompilerDatabaseLoadDoesNotSelectChannelOrUserSecrets(t *testing.T) {
	db := newEdgeSnapshotCompilerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.User{}, &model.Channel{}, &model.Ability{}, &model.Model{}))
	user := model.User{
		Id: 1, Username: "loader-user", Password: "user-password-secret", Status: common.UserStatusEnabled,
		Email: "private-user@example.invalid", Group: "pro20x4",
	}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{Id: 1, UserId: user.Id, Key: "tokenLoaderSecret", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	baseURL := "https://private-cpa.invalid"
	for index, channelName := range []string{"cpa-pro20x4", "mistral"} {
		channel := model.Channel{
			Id: index + 1, Type: constant.ChannelTypeOpenAI, Key: "channel-key-secret", BaseURL: &baseURL,
			Status: common.ChannelStatusEnabled, Name: channelName, OtherSettings: `{}`,
		}
		if channelName == "mistral" {
			channel.Type = constant.ChannelTypeMistral
		}
		require.NoError(t, db.Create(&channel).Error)
	}

	state, err := loadEdgeSnapshotDatabaseState(1_800_000_000)
	require.NoError(t, err)
	require.Len(t, state.Tokens, 1)
	require.Len(t, state.Users, 1)
	require.Len(t, state.Channels, 2)
	assert.Empty(t, state.Users[0].Password)
	assert.Empty(t, state.Users[0].Email)
	for _, channel := range state.Channels {
		assert.Empty(t, channel.Key)
		assert.Nil(t, channel.BaseURL)
	}
}

func TestEdgeSnapshotCompilerPersistsSignedSevenDatasetGraphWithoutSecrets(t *testing.T) {
	db := newEdgeSnapshotCompilerTestDB(t)
	projection, err := projectEdgeSnapshotDatabaseState(edgeSnapshotCompilerTestState())
	require.NoError(t, err)
	completeEdgeSnapshotCompilerTestProjection(t, projection)

	now := int64(1_800_000_000)
	signingKey := edgeSnapshotCompilerTestSigningKey(t)
	snapshotID, err := persistEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 1,
		ExpiresAt: now + 3600,
	}, now)
	require.NoError(t, err)

	bundle, err := model.GetLatestPublishedEdgeCompiledSnapshotManifest(now)
	require.NoError(t, err)
	assert.Equal(t, snapshotID, bundle.Manifest.SnapshotID)
	assert.NoError(t, bundle.Manifest.Validate())
	require.Len(t, bundle.Manifest.Datasets, 7)
	for i, dataset := range edgeSnapshotDatasetOrder {
		assert.Equal(t, dataset, bundle.Manifest.Datasets[i].Dataset)
	}

	var stored model.EdgeCompiledSnapshot
	require.NoError(t, db.Where("snapshot_uid = ?", snapshotID).First(&stored).Error)
	assert.Equal(t, model.EdgeCompiledSnapshotStatusPublished, stored.Status)
	assert.Equal(t, signingKey.PublicKeyB64, stored.SigningPublicKey)
	privateKeyMaterial, err := edgeauth.EncodePrivateKey(signingKey.PrivateKey)
	require.NoError(t, err)
	assert.NotEqual(t, privateKeyMaterial, stored.SigningPublicKey)

	var pages []model.EdgeCompiledSnapshotPage
	require.NoError(t, db.Order("id ASC").Find(&pages).Error)
	require.NotEmpty(t, pages)
	var persisted strings.Builder
	for _, page := range pages {
		persisted.WriteString(page.Payload)
	}
	for _, secret := range []string{
		"tokenSecretOne",
		"tokenSecretTwo",
		"channel-key-secret",
		"https://private-cpa.invalid",
		"user-password-secret",
		"private-user@example.invalid",
		"https://notify.invalid/secret",
		edgeSnapshotCompilerPrivateSecret,
		privateKeyMaterial,
	} {
		assert.NotContains(t, persisted.String(), secret)
	}
	assert.Contains(t, persisted.String(), "edge text policy")

	secondID, err := persistEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 2,
		ExpiresAt: now + 3601,
	}, now+1)
	require.NoError(t, err)
	assert.NotEqual(t, snapshotID, secondID)
	var snapshots []model.EdgeCompiledSnapshot
	require.NoError(t, db.Order("revision ASC").Find(&snapshots).Error)
	require.Len(t, snapshots, 2)
	assert.Equal(t, int64(1), snapshots[0].Revision)
	assert.Equal(t, model.EdgeCompiledSnapshotStatusRetired, snapshots[0].Status)
	assert.Equal(t, int64(2), snapshots[1].Revision)
	assert.Equal(t, model.EdgeCompiledSnapshotStatusPublished, snapshots[1].Status)
}

func TestEdgeSnapshotCompilerDebouncesUnchangedContentAndCleansObsoleteGraphs(t *testing.T) {
	db := newEdgeSnapshotCompilerTestDB(t)
	projection, err := projectEdgeSnapshotDatabaseState(edgeSnapshotCompilerTestState())
	require.NoError(t, err)
	completeEdgeSnapshotCompilerTestProjection(t, projection)
	signingKey := edgeSnapshotCompilerTestSigningKey(t)
	baseTime := int64(1_800_100_000)

	first, err := publishEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 2, TTLSeconds: 3600, ExpiresAt: baseTime + 3600,
	}, baseTime)
	require.NoError(t, err)
	secondCall, err := publishEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 2, TTLSeconds: 3600, ExpiresAt: baseTime + 3610,
	}, baseTime+10)
	require.NoError(t, err)
	assert.Equal(t, first.Manifest.SnapshotID, secondCall.Manifest.SnapshotID)
	var count int64
	require.NoError(t, db.Model(&model.EdgeCompiledSnapshot{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)

	refreshed, err := publishEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 2, TTLSeconds: 3600, ExpiresAt: baseTime + 5501,
	}, baseTime+1901)
	require.NoError(t, err)
	assert.NotEqual(t, first.Manifest.SnapshotID, refreshed.Manifest.SnapshotID)
	require.NoError(t, db.Model(&model.EdgeCompiledSnapshot{}).Count(&count).Error)
	assert.Equal(t, int64(2), count)

	var refreshedSnapshot model.EdgeCompiledSnapshot
	require.NoError(t, db.Where("snapshot_uid = ?", refreshed.Manifest.SnapshotID).First(&refreshedSnapshot).Error)
	staleDraft := refreshedSnapshot
	staleDraft.ID = 0
	staleDraft.SnapshotUID = "snapshot-stale-draft"
	staleDraft.Revision++
	staleDraft.Status = model.EdgeCompiledSnapshotStatusDraft
	staleDraft.CreatedAt = baseTime - 5000
	staleDraft.UpdatedAt = staleDraft.CreatedAt
	staleDraft.ExpiresAt = baseTime + 10_000
	staleDraft.PublishedAt = 0
	staleDraft.RetiredAt = 0
	require.NoError(t, db.Create(&staleDraft).Error)

	*projection.Pricing[0].ModelRatio = *projection.Pricing[0].ModelRatio + 0.25
	changed, err := publishEdgeSnapshotProjection(projection, signingKey, edgeSnapshotCompilerRuntime{
		PageLimit: 2, TTLSeconds: 3600, ExpiresAt: baseTime + 7201,
	}, baseTime+3601)
	require.NoError(t, err)
	assert.NotEqual(t, refreshed.Manifest.SnapshotID, changed.Manifest.SnapshotID)

	var obsoleteCount int64
	require.NoError(t, db.Model(&model.EdgeCompiledSnapshot{}).
		Where("snapshot_uid IN ?", []string{first.Manifest.SnapshotID, staleDraft.SnapshotUID}).
		Count(&obsoleteCount).Error)
	assert.Zero(t, obsoleteCount)
	require.NoError(t, db.Model(&model.EdgeCompiledSnapshot{}).Count(&count).Error)
	assert.Equal(t, int64(2), count, "the unexpired retired snapshot and current published snapshot must remain")
	var orphanDatasets int64
	require.NoError(t, db.Table("edge_compiled_snapshot_datasets AS datasets").
		Joins("LEFT JOIN edge_compiled_snapshots AS snapshots ON snapshots.id = datasets.snapshot_id").
		Where("snapshots.id IS NULL").
		Count(&orphanDatasets).Error)
	assert.Zero(t, orphanDatasets)
	var orphanPages int64
	require.NoError(t, db.Table("edge_compiled_snapshot_pages AS pages").
		Joins("LEFT JOIN edge_compiled_snapshot_datasets AS datasets ON datasets.id = pages.dataset_id").
		Where("datasets.id IS NULL").
		Count(&orphanPages).Error)
	assert.Zero(t, orphanPages)
}

func TestEdgeSnapshotCompilerRejectsInvalidAuthenticationAndChannelJSON(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*edgeSnapshotDatabaseState)
	}{
		{
			name: "auto token group",
			mutate: func(state *edgeSnapshotDatabaseState) {
				state.Tokens[0].Group = "auto"
			},
		},
		{
			name: "invalid channel setting JSON",
			mutate: func(state *edgeSnapshotDatabaseState) {
				setting := `{`
				state.Channels[0].Setting = &setting
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := edgeSnapshotCompilerTestState()
			test.mutate(state)
			_, err := projectEdgeSnapshotDatabaseState(state)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrEdgeSnapshotUnrepresentable)
		})
	}
}

func TestEdgeSnapshotCompilerIgnoresMasterOnlyChannelConfiguration(t *testing.T) {
	state := edgeSnapshotCompilerTestState()
	setting := `{"proxy":"http://proxy-secret.invalid","system_prompt":"kept"}`
	parameterOverride := `{"temperature":1}`
	headerOverride := `{"Authorization":"header-secret"}`
	organization := "org-secret"
	state.Channels[0].Setting = &setting
	state.Channels[0].ParamOverride = &parameterOverride
	state.Channels[0].HeaderOverride = &headerOverride
	state.Channels[0].OpenAIOrganization = &organization
	state.Channels[0].OtherSettings = `{"azure_responses_version":"preview","allow_service_tier":true}`

	projection, err := projectEdgeSnapshotDatabaseState(state)
	require.NoError(t, err)
	payload, err := common.Marshal(projection.Channels)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "proxy-secret")
	assert.NotContains(t, string(payload), "header-secret")
	assert.NotContains(t, string(payload), "org-secret")
	assert.Contains(t, string(payload), "kept")
}

func TestEdgeSnapshotCompilerRejectsDynamicBillingExpressions(t *testing.T) {
	base := edgeSnapshotPricingInput{
		Mode:         billing_setting.BillingModeTieredExpr,
		QuotaPerUnit: 500_000,
	}
	tests := []string{
		`tier("base", p * 2 + c * 8) * (header("x-price") == "high" ? 2 : 1)`,
		`tier("base", p * 2 + c * 8) * (hour("UTC") < 8 ? 0.5 : 1)`,
		`tier("base", p * 2 + c * 8)|||when(header("x-mode") has "fast") * 2`,
	}
	for _, expression := range tests {
		input := base
		input.Expression = expression
		_, err := projectEdgeSnapshotPricing("gpt-5.4", input)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEdgeSnapshotUnrepresentable)
	}

	input := base
	input.Expression = `tier("base", p * 2 + c * 8 + cr * 0.2)`
	pricing, err := projectEdgeSnapshotPricing("gpt-5.4", input)
	require.NoError(t, err)
	assert.Equal(t, dto.EdgeBillingModeTieredExprV1, pricing.BillingMode)
}

func TestEdgeSnapshotCompilerOnlyProjectsAllowlistedAffinityPassHeaders(t *testing.T) {
	setting := operation_setting.ChannelAffinitySetting{
		Enabled:           true,
		MaxEntries:        100,
		DefaultTTLSeconds: 3600,
		Rules: []operation_setting.ChannelAffinityRule{
			{
				Name:       "codex trace",
				ModelRegex: []string{"^gpt-.*$"},
				PathRegex:  []string{"^/v1/responses$"},
				KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "prompt_cache_key"}},
				ParamOverrideTemplate: map[string]interface{}{
					"operations": []map[string]interface{}{{
						"mode": "pass_headers", "value": []string{"X-Codex-Turn-State", "User-Agent"}, "keep_origin": true,
					}},
				},
			},
		},
	}
	routing, err := projectEdgeSnapshotRouting(setting, []string{"gpt-5.4"})
	require.NoError(t, err)
	require.Len(t, routing.ChannelAffinity.Rules, 1)
	assert.Equal(t, []string{"User-Agent", "X-Codex-Turn-State"}, routing.ChannelAffinity.Rules[0].PassHeaders)
	assert.True(t, routing.ChannelAffinity.Rules[0].KeepOrigin)

	setting.Rules[0].ParamOverrideTemplate = map[string]interface{}{
		"operations": []map[string]interface{}{{"mode": "set", "value": map[string]interface{}{"temperature": 1}}},
	}
	_, err = projectEdgeSnapshotRouting(setting, []string{"gpt-5.4"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeSnapshotUnrepresentable)

	setting.Rules[0].ParamOverrideTemplate = map[string]interface{}{
		"operations": []map[string]interface{}{{"mode": "pass_headers", "value": []string{"Authorization"}}},
	}
	_, err = projectEdgeSnapshotRouting(setting, []string{"gpt-5.4"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeSnapshotUnrepresentable)
}

func TestEdgeSnapshotCompilerUsesOpaqueTokenAffinityAndRejectsPlaintextMatchers(t *testing.T) {
	setting := operation_setting.ChannelAffinitySetting{
		Enabled:           true,
		MaxEntries:        100,
		DefaultTTLSeconds: 3600,
		Rules: []operation_setting.ChannelAffinityRule{{
			Name:       "per token",
			ModelRegex: []string{"^gpt-5\\.4$"},
			KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_string", Key: "token_key"}},
		}},
	}

	routing, err := projectEdgeSnapshotRouting(setting, []string{"gpt-5.4"})
	require.NoError(t, err)
	require.Len(t, routing.ChannelAffinity.Rules, 1)
	require.Len(t, routing.ChannelAffinity.Rules[0].KeySources, 1)
	assert.Equal(t, dto.EdgeChannelAffinityKeySourceContextStringV1, routing.ChannelAffinity.Rules[0].KeySources[0].Type)
	assert.Equal(t, "token_key", routing.ChannelAffinity.Rules[0].KeySources[0].Key)

	setting.Rules[0].ValueRegex = `^tokenSecret`
	_, err = projectEdgeSnapshotRouting(setting, []string{"gpt-5.4"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeSnapshotUnrepresentable)
}

func TestEdgeSnapshotCompilerAllowsZeroRatioPricing(t *testing.T) {
	zero := 0.0
	pricing, err := projectEdgeSnapshotPricing("gpt-free", edgeSnapshotPricingInput{
		Mode: billing_setting.BillingModeRatio, ModelRatio: &zero, QuotaPerUnit: 500_000,
	})
	require.NoError(t, err)
	require.NotNil(t, pricing.ModelRatio)
	assert.Zero(t, *pricing.ModelRatio)
	assert.Equal(t, dto.EdgeBillingModeRatioV1, pricing.BillingMode)
}

const edgeSnapshotCompilerPrivateSecret = "edge-snapshot-private-key-secret-marker"

func edgeSnapshotCompilerTestState() *edgeSnapshotDatabaseState {
	priority := int64(5)
	weight := uint(20)
	baseURL := "https://private-cpa.invalid"
	channelSetting := `{"system_prompt":"edge text policy","system_prompt_override":true}`
	modelMapping := `{"gpt-fix":"gpt-5.5","gpt-5.5":"gpt-5.5"}`
	channels := []model.Channel{
		{Id: 40, Type: constant.ChannelTypeOpenAI, Key: "channel-key-secret", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Name: "cpa-vip", Weight: &weight, Priority: &priority, Setting: &channelSetting, ModelMapping: &modelMapping, OtherSettings: `{"allow_service_tier":true}`},
		{Id: 30, Type: constant.ChannelTypeOpenAI, Key: "channel-key-secret", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Name: "cpa-pro20x4", Weight: &weight, Priority: &priority, Setting: &channelSetting, OtherSettings: `{"allow_service_tier":true}`},
		{Id: 10, Type: constant.ChannelTypeOpenAI, Key: "channel-key-secret", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Name: "cpa-pro20x5", Weight: &weight, Priority: &priority, Setting: &channelSetting, OtherSettings: `{"allow_service_tier":true}`},
		{Id: 20, Type: constant.ChannelTypeOpenAI, Key: "channel-key-secret", BaseURL: &baseURL, Status: common.ChannelStatusEnabled, Name: "cpa-pro20x6", Weight: &weight, Priority: &priority, Setting: &channelSetting, OtherSettings: `{"allow_service_tier":true}`},
	}
	allowIPs := "192.168.1.1\n10.0.0.0/8"
	return &edgeSnapshotDatabaseState{
		Tokens: []model.Token{
			{Id: 2, UserId: 2, Key: "tokenSecretTwo", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, ModelLimitsEnabled: false},
			{Id: 1, UserId: 1, Key: "tokenSecretOne", Status: common.TokenStatusEnabled, ExpiredTime: 1_900_000_000, UnlimitedQuota: true, ModelLimitsEnabled: true, ModelLimits: "gpt-5.4-openai-compact,gpt-5.4,gpt-5.3-codex,gpt-5.4", AllowIps: &allowIPs},
		},
		Users: []model.User{
			{Id: 2, Username: "second", Password: "user-password-secret", Status: common.UserStatusEnabled, Email: "private-user@example.invalid", Group: "vip", Setting: `{"language":"zh-CN","webhook_url":"https://notify.invalid/secret"}`},
			{Id: 1, Username: "first", Password: "user-password-secret", Status: common.UserStatusEnabled, Email: "private-user@example.invalid", Group: "default", Setting: `{"accept_unset_model_ratio_model":true}`},
		},
		Channels: channels,
		Abilities: []model.Ability{
			{Group: "vip", Model: "gpt-5.5", ChannelId: 40, Enabled: true, Priority: &priority, Weight: weight},
			{Group: "vip", Model: "gpt-5.4", ChannelId: 20, Enabled: true, Priority: &priority, Weight: weight},
			{Group: "default", Model: "gpt-5.3-codex", ChannelId: 10, Enabled: true, Priority: &priority, Weight: weight},
			{Group: "default", Model: "gpt-5.4", ChannelId: 30, Enabled: true, Priority: &priority, Weight: weight},
			{Group: "default", Model: "gpt-5.4-openai-compact", ChannelId: 30, Enabled: true, Priority: &priority, Weight: weight},
			{Group: "default", Model: "unsafe-image-model", ChannelId: 30, Enabled: true, Priority: &priority, Weight: weight},
		},
		ModelStatuses: map[string]int{"gpt-5.3-codex": 1, "gpt-5.4": 1, "gpt-5.5": 1},
	}
}

func completeEdgeSnapshotCompilerTestProjection(t *testing.T, projection *edgeSnapshotProjection) {
	t.Helper()
	projection.Groups = []dto.EdgeGroupPolicyV1{
		{UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}, {Group: "vip", Enabled: true, Ratio: 1}}},
		{UserGroup: "vip", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}, {Group: "vip", Enabled: true, Ratio: 1}}},
	}
	require.NoError(t, filterEdgeSnapshotAuthorizationGroups(projection))
	for _, modelName := range projection.modelNames {
		pricing, err := projectEdgeSnapshotPricing(modelName, edgeSnapshotPricingInput{
			Mode:               billing_setting.BillingModeRatio,
			ModelRatio:         floatPointer(1),
			CompletionRatio:    floatPointer(6),
			CacheReadRatio:     floatPointer(0.1),
			CacheCreationRatio: floatPointer(1.25),
			QuotaPerUnit:       500_000,
		})
		require.NoError(t, err)
		projection.Pricing = append(projection.Pricing, pricing)
	}
	projection.Routing = []dto.EdgeRoutingPolicyV1{{ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{
		Enabled: true, SwitchOnSuccess: true, MaxEntries: 1000, DefaultTTLSeconds: 3600,
	}}}
}

func edgeSnapshotCompilerTestSigningKey(t *testing.T) *SnapshotSigningKey {
	t.Helper()
	seed := sha256.Sum256([]byte(edgeSnapshotCompilerPrivateSecret))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	publicKeyB64, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	return &SnapshotSigningKey{
		KeyID: "snapshot-test-key", PrivateKey: privateKey, PublicKey: publicKey, PublicKeyB64: publicKeyB64,
		NotBefore: 1_700_000_000, ExpiresAt: 2_000_000_000,
	}
}

func newEdgeSnapshotCompilerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	db, err := gorm.Open(sqlite.Open("file:edge-snapshot-compiler?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.EdgeCompiledSnapshot{},
		&model.EdgeCompiledSnapshotDataset{},
		&model.EdgeCompiledSnapshotPage{},
	))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		_ = sqlDB.Close()
	})
	return db
}

func TestEdgeSnapshotCompilerRuntimeUsesBoundedEnvironment(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := edgeSnapshotCompilerTestSigningKey(t)
	t.Setenv(edgeSnapshotPageLimitEnv, "17")
	t.Setenv(edgeSnapshotTTLSecondsEnv, "7200")
	runtime, err := loadEdgeSnapshotCompilerRuntime(now, key)
	require.NoError(t, err)
	assert.Equal(t, 17, runtime.PageLimit)
	assert.Equal(t, int64(7200), runtime.TTLSeconds)
	assert.Equal(t, now.Unix()+7200, runtime.ExpiresAt)

	t.Setenv(edgeSnapshotPageLimitEnv, "0")
	_, err = loadEdgeSnapshotCompilerRuntime(now, key)
	require.Error(t, err)

	t.Setenv(edgeSnapshotPageLimitEnv, "17")
	t.Setenv(edgeSnapshotTTLSecondsEnv, "59")
	_, err = loadEdgeSnapshotCompilerRuntime(now, key)
	require.Error(t, err)
}
