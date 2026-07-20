package helper

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeModelPriceHelperRatioDefaultsMatchMasterSemantics(t *testing.T) {
	modelRatio := 0.375
	priceData := edgePriceDataForTest(t, dto.EdgePricingPolicyV1{
		PolicyID: "ratio-defaults", Version: "v1", Model: "gpt-ratio-defaults",
		BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &modelRatio, QuotaPerUnit: 500_000,
	}, &types.TokenCountMeta{})

	assert.False(t, priceData.UsePrice)
	assert.Equal(t, float64(-1), priceData.ModelPrice)
	assert.Equal(t, modelRatio, priceData.ModelRatio)
	assert.Equal(t, float64(1), priceData.CompletionRatio)
	assert.Equal(t, float64(1), priceData.CacheRatio)
	assert.Equal(t, float64(1.25), priceData.CacheCreationRatio)
	assert.Equal(t, float64(1.25), priceData.CacheCreation5mRatio)
	assert.Equal(t, float64(2), priceData.CacheCreation1hRatio)
	assert.Equal(t, float64(1), priceData.ImageRatio)
	assert.Equal(t, float64(1), priceData.AudioRatio)
	assert.Equal(t, float64(1), priceData.AudioCompletionRatio)
	assert.Equal(t, float64(-1), priceData.GroupRatioInfo.GroupSpecialRatio)
	assert.False(t, priceData.GroupRatioInfo.HasSpecialRatio)
}

func TestEdgeAndMasterPriceDataLogSemanticsMatchForRatioBilling(t *testing.T) {
	modelName := "price-parity-ratio-model"
	savedModelPrices := ratio_setting.ModelPrice2JSONString()
	savedModelRatios := ratio_setting.ModelRatio2JSONString()
	savedCompletionRatios := ratio_setting.CompletionRatio2JSONString()
	savedCacheRatios := ratio_setting.CacheRatio2JSONString()
	savedCreateCacheRatios := ratio_setting.CreateCacheRatio2JSONString()
	savedImageRatios := ratio_setting.ImageRatio2JSONString()
	savedAudioRatios := ratio_setting.AudioRatio2JSONString()
	savedAudioCompletionRatios := ratio_setting.AudioCompletionRatio2JSONString()
	previousMode := common.CurrentRuntimeMode()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedModelPrices))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatios))
		require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(savedCompletionRatios))
		require.NoError(t, ratio_setting.UpdateCacheRatioByJSONString(savedCacheRatios))
		require.NoError(t, ratio_setting.UpdateCreateCacheRatioByJSONString(savedCreateCacheRatios))
		require.NoError(t, ratio_setting.UpdateImageRatioByJSONString(savedImageRatios))
		require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(savedAudioRatios))
		require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(savedAudioCompletionRatios))
		require.NoError(t, common.SetRuntimeMode(previousMode))
	})
	marshalRatios := func(values map[string]float64) string {
		payload, err := common.Marshal(values)
		require.NoError(t, err)
		return string(payload)
	}
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(marshalRatios(map[string]float64{modelName: 0.375})))
	require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(marshalRatios(map[string]float64{modelName: 6})))
	require.NoError(t, ratio_setting.UpdateCacheRatioByJSONString(marshalRatios(map[string]float64{modelName: 0.1})))
	require.NoError(t, ratio_setting.UpdateCreateCacheRatioByJSONString(marshalRatios(map[string]float64{modelName: 1.25})))
	require.NoError(t, ratio_setting.UpdateImageRatioByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(`{}`))
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeMaster))

	gin.SetMode(gin.TestMode)
	masterCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	masterInfo := &relaycommon.RelayInfo{OriginModelName: modelName, UserGroup: "default", UsingGroup: "default"}
	masterPrice, err := ModelPriceHelper(masterCtx, masterInfo, 100, &types.TokenCountMeta{})
	require.NoError(t, err)
	modelRatio := 0.375
	completionRatio := 6.0
	cacheReadRatio := 0.1
	cacheCreationRatio := 1.25
	cacheCreation1hRatio := 2.0
	edgePrice := edgePriceDataForTest(t, dto.EdgePricingPolicyV1{
		PolicyID: modelName, Version: "v1", Model: modelName, BillingMode: dto.EdgeBillingModeRatioV1,
		ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheReadRatio: &cacheReadRatio,
		CacheCreationRatio: &cacheCreationRatio, CacheCreation1hRatio: &cacheCreation1hRatio,
		QuotaPerUnit: 500_000,
	}, &types.TokenCountMeta{})

	assert.Equal(t, masterPrice.ModelPrice, edgePrice.ModelPrice)
	assert.Equal(t, masterPrice.UsePrice, edgePrice.UsePrice)
	assert.Equal(t, masterPrice.ModelRatio, edgePrice.ModelRatio)
	assert.Equal(t, masterPrice.CompletionRatio, edgePrice.CompletionRatio)
	assert.Equal(t, masterPrice.CacheRatio, edgePrice.CacheRatio)
	assert.Equal(t, masterPrice.CacheCreationRatio, edgePrice.CacheCreationRatio)
	assert.Equal(t, masterPrice.CacheCreation5mRatio, edgePrice.CacheCreation5mRatio)
	assert.Equal(t, masterPrice.CacheCreation1hRatio, edgePrice.CacheCreation1hRatio)
	assert.Equal(t, masterPrice.ImageRatio, edgePrice.ImageRatio)
	assert.Equal(t, masterPrice.AudioRatio, edgePrice.AudioRatio)
	assert.Equal(t, masterPrice.AudioCompletionRatio, edgePrice.AudioCompletionRatio)
	assert.Equal(t, masterPrice.GroupRatioInfo, edgePrice.GroupRatioInfo)
}

func TestEdgeModelPriceHelperFixedPriceZeroRemainsExplicit(t *testing.T) {
	modelPrice := 0.0
	priceData := edgePriceDataForTest(t, dto.EdgePricingPolicyV1{
		PolicyID: "fixed-zero", Version: "v1", Model: "gpt-fixed-zero",
		BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
	}, &types.TokenCountMeta{})

	assert.True(t, priceData.UsePrice)
	assert.Equal(t, float64(0), priceData.ModelPrice)
	assert.Equal(t, float64(0), priceData.ModelRatio)
	assert.Equal(t, float64(0), priceData.CompletionRatio)
	assert.Equal(t, float64(0), priceData.CacheRatio)
	assert.True(t, priceData.FreeModel)
}

func TestEdgeModelPriceHelperPreservesExplicitZeroOptionalRatios(t *testing.T) {
	modelRatio := 1.0
	zero := 0.0
	priceData := edgePriceDataForTest(t, dto.EdgePricingPolicyV1{
		PolicyID: "ratio-explicit-zero", Version: "v1", Model: "gpt-ratio-explicit-zero",
		BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &modelRatio,
		CompletionRatio: &zero, CacheReadRatio: &zero, CacheCreationRatio: &zero,
		CacheCreation1hRatio: &zero, QuotaPerUnit: 500_000,
	}, &types.TokenCountMeta{})

	assert.Zero(t, priceData.CompletionRatio)
	assert.Zero(t, priceData.CacheRatio)
	assert.Zero(t, priceData.CacheCreationRatio)
	assert.Zero(t, priceData.CacheCreation5mRatio)
	assert.Zero(t, priceData.CacheCreation1hRatio)
}

func TestEdgeModelPriceHelperAppliesRequestSpecificRatios(t *testing.T) {
	modelPrice := 0.02
	priceData, err := edgePriceDataForTestResult(t, dto.EdgePricingPolicyV1{
		PolicyID: "fixed-request-ratio", Version: "v1", Model: "gpt-fixed-request-ratio",
		BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
	}, &types.TokenCountMeta{ImagePriceRatio: 2, BillingRatios: map[string]float64{"n": 2}})
	require.NoError(t, err)
	assert.Equal(t, map[string]float64{"image_price_ratio": 2, "n": 2}, priceData.OtherRatios())
	assert.Equal(t, 40_000, priceData.QuotaToPreConsume)
}

func edgePriceDataForTest(t *testing.T, policy dto.EdgePricingPolicyV1, meta *types.TokenCountMeta) types.PriceData {
	t.Helper()
	priceData, err := edgePriceDataForTestResult(t, policy, meta)
	require.NoError(t, err)
	return priceData
}

func edgePriceDataForTestResult(t *testing.T, policy dto.EdgePricingPolicyV1, meta *types.TokenCountMeta) (types.PriceData, error) {
	t.Helper()
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-price.db"))
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	payload, err := common.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.EdgeLocalPricingProjection{
		PolicyID: policy.PolicyID, Version: policy.Version, Model: policy.Model, Payload: string(payload),
	}).Error)

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(ctx, constant.ContextKeyEdgeGroupRatio, float64(1))
	info := &relaycommon.RelayInfo{OriginModelName: policy.Model}
	return edgeModelPriceHelper(ctx, info, 100, meta)
}
