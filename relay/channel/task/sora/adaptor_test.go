package sora

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRequestBodyPreservesSeedanceOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := map[string]any{
		"model":           "doubao-seedance-2-0-260128",
		"prompt":          "animate every reference",
		"resolution":      "480P",
		"durationSeconds": 15,
		"aspectRatio":     "21:9",
		"audio":           true,
		"timeoutMs":       600000,
		"mode":            "omni_reference",
		"generation_mode": "omni_reference",
		"images":          []string{"data:image/png;base64,AAAA"},
		"imageRoles":      []string{"reference_image"},
		"videos":          []string{"data:video/mp4;base64,AAAA"},
		"videoRoles":      []string{"reference_video"},
		"audioRefs":       []string{"data:audio/mpeg;base64,AAAA"},
		"audioRoles":      []string{"reference_audio"},
	}
	body, err := json.Marshal(original)
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	defer storage.Close()

	converted, err := (&TaskAdaptor{}).BuildRequestBody(c, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "navos/doubao-seedance-2-0-260128",
		},
	})
	require.NoError(t, err)
	convertedBody, err := io.ReadAll(converted)
	require.NoError(t, err)

	original["model"] = "navos/doubao-seedance-2-0-260128"
	expected, err := json.Marshal(original)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), string(convertedBody))
}

func TestEstimateBillingUsesSeedanceDurationSeconds(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("task_request", relaycommon.TaskSubmitReq{
		DurationSeconds: 10,
		Duration:        4,
		Seconds:         "3",
	})

	ratios := (&TaskAdaptor{}).EstimateBilling(c, &relaycommon.RelayInfo{
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	})

	require.Equal(t, 10.0, ratios["seconds"])
}

func TestParseTaskResultKeepsCompletedVideoURL(t *testing.T) {
	adaptor := &TaskAdaptor{}

	result, err := adaptor.ParseTaskResult([]byte(`{
		"id":"video_123",
		"status":"completed",
		"progress":100,
		"video_url":"https://cdn.example.com/video.mp4"
	}`))

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, string(model.TaskStatusSuccess), result.Status)
	assert.Equal(t, "https://cdn.example.com/video.mp4", result.Url)
}
