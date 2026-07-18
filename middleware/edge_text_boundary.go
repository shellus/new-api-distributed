package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

var errUnsupportedEdgeTextFeature = errors.New("request uses a feature outside the edge text boundary")

// EdgeTextBoundary keeps the first edge data plane limited to Chat and
// Responses text requests whose accounting is represented by the v1 snapshot.
// It runs before channel selection, balance reservation and any CPA request.
func EdgeTextBoundary() gin.HandlerFunc {
	return func(c *gin.Context) {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "invalid request body", types.ErrorCodeBadRequestBody)
			return
		}
		body, err := storage.Bytes()
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "invalid request body", types.ErrorCodeBadRequestBody)
			return
		}

		switch c.Request.URL.Path {
		case "/v1/chat/completions":
			err = validateEdgeChatTextRequest(body)
		case "/v1/responses":
			err = validateEdgeResponsesTextRequest(body)
		default:
			err = errUnsupportedEdgeTextFeature
		}
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, errUnsupportedEdgeTextFeature.Error(), types.ErrorCodeInvalidRequest)
			return
		}
		c.Next()
	}
}

type edgeChatBoundaryRequest struct {
	Model            string          `json:"model"`
	WebSearchOptions json.RawMessage `json:"web_search_options"`
	Audio            json.RawMessage `json:"audio"`
	Modalities       []string        `json:"modalities"`
	Tools            []struct {
		Type string `json:"type"`
	} `json:"tools"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func validateEdgeChatTextRequest(body []byte) error {
	var request edgeChatBoundaryRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return err
	}
	if edgeModelHasUnsupportedBuiltInBilling(request.Model) ||
		(len(request.WebSearchOptions) != 0 && common.GetJsonType(request.WebSearchOptions) != "unknown" && string(request.WebSearchOptions) != "null") {
		return errUnsupportedEdgeTextFeature
	}
	if len(request.Audio) != 0 && string(request.Audio) != "null" {
		return errUnsupportedEdgeTextFeature
	}
	if !edgeModalitiesAreTextOnly(request.Modalities) {
		return errUnsupportedEdgeTextFeature
	}
	for _, tool := range request.Tools {
		switch strings.ToLower(strings.TrimSpace(tool.Type)) {
		case "", "function", "custom":
		default:
			return errUnsupportedEdgeTextFeature
		}
	}
	for _, message := range request.Messages {
		if common.GetJsonType(message.Content) != "array" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if err := common.Unmarshal(message.Content, &parts); err != nil {
			return err
		}
		for _, part := range parts {
			switch strings.ToLower(strings.TrimSpace(part.Type)) {
			case "", "text", "input_text":
			default:
				return errUnsupportedEdgeTextFeature
			}
		}
	}
	return nil
}

type edgeResponsesBoundaryRequest struct {
	Background bool            `json:"background"`
	Model      string          `json:"model"`
	Modalities []string        `json:"modalities"`
	Input      json.RawMessage `json:"input"`
	Tools      []struct {
		Type string `json:"type"`
	} `json:"tools"`
}

func validateEdgeResponsesTextRequest(body []byte) error {
	var request edgeResponsesBoundaryRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return err
	}
	if request.Background || edgeModelHasUnsupportedBuiltInBilling(request.Model) || !edgeModalitiesAreTextOnly(request.Modalities) {
		return errUnsupportedEdgeTextFeature
	}
	for _, tool := range request.Tools {
		switch strings.ToLower(strings.TrimSpace(tool.Type)) {
		case "", "function", "custom":
		default:
			return errUnsupportedEdgeTextFeature
		}
	}
	if len(request.Input) == 0 || common.GetJsonType(request.Input) == "string" {
		return nil
	}
	var input any
	if err := common.Unmarshal(request.Input, &input); err != nil {
		return err
	}
	if edgeContainsBinaryInput(input) {
		return errUnsupportedEdgeTextFeature
	}
	return nil
}

func edgeModalitiesAreTextOnly(modalities []string) bool {
	for _, modality := range modalities {
		if !strings.EqualFold(strings.TrimSpace(modality), "text") {
			return false
		}
	}
	return true
}

func edgeModelHasUnsupportedBuiltInBilling(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "search-preview")
}

func edgeContainsBinaryInput(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if edgeContainsBinaryInput(item) {
				return true
			}
		}
	case map[string]any:
		if rawType, ok := typed["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(rawType)) {
			case "input_image", "input_file", "input_audio", "input_video", "image", "image_url", "audio", "video", "file", "computer_screenshot", "screenshot":
				return true
			}
		}
		for _, item := range typed {
			if edgeContainsBinaryInput(item) {
				return true
			}
		}
	}
	return false
}
