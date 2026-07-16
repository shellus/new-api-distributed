package middleware

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// EdgeModelPolicy enforces the endpoint and streaming capabilities from the
// signed model projection before the shared distributor can select a channel.
func EdgeModelPolicy() gin.HandlerFunc {
	return func(c *gin.Context) {
		var envelope struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := common.UnmarshalBodyReusable(c, &envelope); err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "invalid request body", types.ErrorCodeBadRequestBody)
			return
		}
		if envelope.Model == "" {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "model is required", types.ErrorCodeInvalidRequest)
			return
		}
		policy, err := model.GetEdgeLocalModel(model.DB, envelope.Model)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				common.SysError("edge model projection lookup failed: " + err.Error())
			}
			abortWithOpenAiMessage(c, http.StatusNotFound, "model is not available on this edge", types.ErrorCodeModelNotFound)
			return
		}
		endpoint := dto.EdgeEndpointOpenAIChatCompletionsV1
		if c.Request.URL.Path == "/v1/responses" {
			endpoint = dto.EdgeEndpointOpenAIResponsesV1
		}
		if !policy.Enabled || !edgeModelSupportsEndpoint(policy, endpoint) || (envelope.Stream && !policy.Streaming) {
			abortWithOpenAiMessage(c, http.StatusNotFound, "model is not available for this edge endpoint", types.ErrorCodeModelNotFound)
			return
		}
		c.Next()
	}
}

func edgeModelSupportsEndpoint(policy *dto.EdgeModelPolicyV1, endpoint dto.EdgeEndpointV1) bool {
	if policy == nil {
		return false
	}
	for _, candidate := range policy.Endpoints {
		if candidate == endpoint {
			return true
		}
	}
	return false
}
