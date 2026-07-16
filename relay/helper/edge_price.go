package helper

import (
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func edgeModelPriceHelper(c *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *types.TokenCountMeta) (types.PriceData, error) {
	if info == nil || model.DB == nil {
		return types.PriceData{}, errors.New("edge pricing is not ready")
	}
	groupRatio, ok := common.GetContextKeyType[float64](c, constant.ContextKeyEdgeGroupRatio)
	if !ok || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) || groupRatio < 0 {
		return types.PriceData{}, errors.New("edge request has no valid signed group ratio")
	}
	policies, err := model.GetEdgeLocalPricing(model.DB, info.OriginModelName)
	if err != nil {
		return types.PriceData{}, fmt.Errorf("load edge pricing policy: %w", err)
	}
	if len(policies) != 1 {
		return types.PriceData{}, fmt.Errorf("model %s must have exactly one edge pricing policy", info.OriginModelName)
	}
	policy := policies[0]
	if err := policy.Validate(); err != nil {
		return types.PriceData{}, fmt.Errorf("invalid edge pricing policy: %w", err)
	}
	if policy.Model != info.OriginModelName {
		return types.PriceData{}, errors.New("edge pricing policy model mismatch")
	}
	if policy.BillingMode == dto.EdgeBillingModeTieredExprV1 {
		return types.PriceData{}, errors.New("tiered billing is not supported by edge settlement v1")
	}
	if meta == nil {
		meta = &types.TokenCountMeta{}
	}

	groupInfo := types.GroupRatioInfo{
		GroupRatio: groupRatio, GroupSpecialRatio: groupRatio, HasSpecialRatio: true,
	}
	priceData := types.PriceData{GroupRatioInfo: groupInfo}
	switch policy.BillingMode {
	case dto.EdgeBillingModeFixedPriceV1:
		if policy.ModelPrice == nil {
			return types.PriceData{}, errors.New("edge fixed-price policy has no model price")
		}
		priceData.UsePrice = true
		priceData.ModelPrice = *policy.ModelPrice
		quota, err := common.QuotaFromFloatStrict(*policy.ModelPrice * policy.QuotaPerUnit * groupRatio)
		if err != nil {
			return types.PriceData{}, err
		}
		priceData.QuotaToPreConsume = quota
		priceData.FreeModel = groupRatio == 0 || *policy.ModelPrice == 0
	case dto.EdgeBillingModeRatioV1:
		if policy.ModelRatio == nil {
			return types.PriceData{}, errors.New("edge ratio policy has no model ratio")
		}
		priceData.ModelRatio = *policy.ModelRatio
		priceData.CompletionRatio = edgeOptionalRatio(policy.CompletionRatio, 1)
		priceData.CacheRatio = edgeOptionalRatio(policy.CacheReadRatio, 1)
		priceData.CacheCreationRatio = edgeOptionalRatio(policy.CacheCreationRatio, 1)
		priceData.CacheCreation5mRatio = priceData.CacheCreationRatio
		priceData.CacheCreation1hRatio = edgeOptionalRatio(policy.CacheCreation1hRatio, priceData.CacheCreationRatio)
		preConsumedTokens := common.Max(promptTokens, common.PreConsumedQuota)
		if meta.MaxTokens > 0 {
			if preConsumedTokens > common.MaxQuota-meta.MaxTokens {
				return types.PriceData{}, errors.New("edge pre-consume token estimate exceeds the supported quota range")
			}
			preConsumedTokens += meta.MaxTokens
		}
		quota, err := common.QuotaFromFloatStrict(float64(preConsumedTokens) * *policy.ModelRatio * groupRatio)
		if err != nil {
			return types.PriceData{}, err
		}
		priceData.QuotaToPreConsume = quota
		priceData.FreeModel = groupRatio == 0 || *policy.ModelRatio == 0
	default:
		return types.PriceData{}, errors.New("unsupported edge billing mode")
	}
	info.PriceData = priceData
	policyCopy := policy
	info.EdgePricingPolicy = &policyCopy
	return priceData, nil
}

func edgeOptionalRatio(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}
