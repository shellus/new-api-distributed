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
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func edgeModelPriceHelper(c *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *relaytypes.TokenCountMeta) (hosttypes.PriceData, error) {
	if info == nil || model.DB == nil {
		return hosttypes.PriceData{}, errors.New("edge pricing is not ready")
	}
	groupRatio, ok := common.GetContextKeyType[float64](c, constant.ContextKeyEdgeGroupRatio)
	if !ok || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) || groupRatio < 0 {
		return hosttypes.PriceData{}, errors.New("edge request has no valid signed group ratio")
	}
	policies, err := model.GetEdgeLocalPricing(model.DB, info.OriginModelName)
	if err != nil {
		return hosttypes.PriceData{}, fmt.Errorf("load edge pricing policy: %w", err)
	}
	if len(policies) != 1 {
		return hosttypes.PriceData{}, fmt.Errorf("model %s must have exactly one edge pricing policy", info.OriginModelName)
	}
	policy := policies[0]
	if err := policy.Validate(); err != nil {
		return hosttypes.PriceData{}, fmt.Errorf("invalid edge pricing policy: %w", err)
	}
	if policy.Model != info.OriginModelName {
		return hosttypes.PriceData{}, errors.New("edge pricing policy model mismatch")
	}
	if meta == nil {
		meta = &relaytypes.TokenCountMeta{}
	}

	groupInfo := hosttypes.GroupRatioInfo{GroupRatio: groupRatio, GroupSpecialRatio: -1}
	if common.GetContextKeyBool(c, constant.ContextKeyEdgeGroupSpecialRatio) {
		groupInfo.GroupSpecialRatio = groupRatio
		groupInfo.HasSpecialRatio = true
	}
	if policy.BillingMode == dto.EdgeBillingModeTieredExprV1 {
		priceData, err := modelPriceHelperTieredExpression(
			c, info, promptTokens, meta, groupInfo, policy.BillingExpression, policy.QuotaPerUnit,
		)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		policyCopy := policy
		info.EdgePricingPolicy = &policyCopy
		return priceData, nil
	}

	priceData := hosttypes.PriceData{GroupRatioInfo: groupInfo}
	switch policy.BillingMode {
	case dto.EdgeBillingModeFixedPriceV1:
		if policy.ModelPrice == nil {
			return hosttypes.PriceData{}, errors.New("edge fixed-price policy has no model price")
		}
		priceData.UsePrice = true
		priceData.ModelPrice = *policy.ModelPrice
		if meta.ImagePriceRatio != 0 {
			priceData.AddOtherRatio("image_price_ratio", meta.ImagePriceRatio)
		}
		for name, ratio := range meta.BillingRatios {
			priceData.AddOtherRatio(name, ratio)
		}
		quota, err := common.QuotaFromFloatStrict(priceData.ApplyOtherRatiosToFloat(*policy.ModelPrice * policy.QuotaPerUnit * groupRatio))
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		priceData.QuotaToPreConsume = quota
		priceData.FreeModel = groupRatio == 0 || *policy.ModelPrice == 0
	case dto.EdgeBillingModeRatioV1:
		if policy.ModelRatio == nil {
			return hosttypes.PriceData{}, errors.New("edge ratio policy has no model ratio")
		}
		priceData.ModelPrice = -1
		priceData.ModelRatio = *policy.ModelRatio
		priceData.CompletionRatio = edgeOptionalRatio(policy.CompletionRatio, 1)
		priceData.CacheRatio = edgeOptionalRatio(policy.CacheReadRatio, 1)
		priceData.CacheCreationRatio = edgeOptionalRatio(policy.CacheCreationRatio, 1.25)
		priceData.CacheCreation5mRatio = priceData.CacheCreationRatio
		priceData.CacheCreation1hRatio = edgeOptionalRatio(policy.CacheCreation1hRatio, priceData.CacheCreationRatio*claudeCacheCreation1hMultiplier)
		priceData.ImageRatio = edgeOptionalRatio(policy.ImageRatio, 1)
		priceData.AudioRatio = edgeOptionalRatio(policy.AudioRatio, 1)
		priceData.AudioCompletionRatio = edgeOptionalRatio(policy.AudioCompletionRatio, 1)
		preConsumedQuota := common.PreConsumedQuota
		if policy.PreConsumedQuota != nil {
			preConsumedQuota = *policy.PreConsumedQuota
		}
		preConsumedTokens := common.Max(promptTokens, preConsumedQuota)
		if meta.MaxTokens > 0 {
			if preConsumedTokens > common.MaxQuota-meta.MaxTokens {
				return hosttypes.PriceData{}, errors.New("edge pre-consume token estimate exceeds the supported quota range")
			}
			preConsumedTokens += meta.MaxTokens
		}
		quota, err := common.QuotaFromFloatStrict(float64(preConsumedTokens) * *policy.ModelRatio * groupRatio)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		priceData.QuotaToPreConsume = quota
		priceData.FreeModel = groupRatio == 0 || *policy.ModelRatio == 0
	default:
		return hosttypes.PriceData{}, errors.New("unsupported edge billing mode")
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

func edgeModelPriceHelperPerCall(c *gin.Context, info *relaycommon.RelayInfo) (hosttypes.PriceData, error) {
	if info == nil || model.DB == nil {
		return hosttypes.PriceData{}, errors.New("edge pricing is not ready")
	}
	groupRatio, ok := common.GetContextKeyType[float64](c, constant.ContextKeyEdgeGroupRatio)
	if !ok || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) || groupRatio < 0 {
		return hosttypes.PriceData{}, errors.New("edge request has no valid signed group ratio")
	}
	policies, err := model.GetEdgeLocalPricing(model.DB, info.OriginModelName)
	if err != nil {
		return hosttypes.PriceData{}, fmt.Errorf("load edge pricing policy: %w", err)
	}
	if len(policies) != 1 {
		return hosttypes.PriceData{}, fmt.Errorf("model %s must have exactly one edge pricing policy", info.OriginModelName)
	}
	policy := policies[0]
	if err := policy.Validate(); err != nil {
		return hosttypes.PriceData{}, fmt.Errorf("invalid edge pricing policy: %w", err)
	}
	groupInfo := hosttypes.GroupRatioInfo{GroupRatio: groupRatio, GroupSpecialRatio: -1}
	if common.GetContextKeyBool(c, constant.ContextKeyEdgeGroupSpecialRatio) {
		groupInfo.GroupSpecialRatio = groupRatio
		groupInfo.HasSpecialRatio = true
	}
	priceData := hosttypes.PriceData{GroupRatioInfo: groupInfo}
	switch policy.BillingMode {
	case dto.EdgeBillingModeFixedPriceV1:
		if policy.ModelPrice == nil {
			return hosttypes.PriceData{}, errors.New("edge fixed-price policy has no model price")
		}
		priceData.UsePrice = true
		priceData.ModelPrice = *policy.ModelPrice
		priceData.Quota, err = common.QuotaFromFloatStrict(*policy.ModelPrice * policy.QuotaPerUnit * groupRatio)
	case dto.EdgeBillingModeRatioV1:
		if policy.ModelRatio == nil {
			return hosttypes.PriceData{}, errors.New("edge ratio policy has no model ratio")
		}
		priceData.ModelPrice = -1
		priceData.ModelRatio = *policy.ModelRatio
		priceData.Quota, err = common.QuotaFromFloatStrict(*policy.ModelRatio / 2 * policy.QuotaPerUnit * groupRatio)
	case dto.EdgeBillingModeTieredExprV1:
		return hosttypes.PriceData{}, errors.New("tiered expression is not a per-call task billing mode")
	default:
		return hosttypes.PriceData{}, errors.New("unsupported edge billing mode")
	}
	if err != nil {
		return hosttypes.PriceData{}, err
	}
	priceData.FreeModel = groupRatio == 0 || priceData.Quota == 0
	info.PriceData = priceData
	policyCopy := policy
	info.EdgePricingPolicy = &policyCopy
	return priceData, nil
}
