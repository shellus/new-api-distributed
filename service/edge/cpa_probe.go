package edge

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

// ProbeEdgeCPA checks the three signed local channel projections and applies
// the result back to Channel/Ability in one SQLite transaction. Failed or
// missing probes therefore become unavailable to the shared distributor
// before any user request can reach them.
func ProbeEdgeCPA(ctx context.Context) ([]dto.EdgeCPAStatusV1, error) {
	var stored []model.EdgeLocalChannelProjection
	if err := model.DB.WithContext(ctx).Order("channel_id asc").Find(&stored).Error; err != nil {
		return nil, err
	}
	timeoutSeconds := common.GetEnvOrDefault("EDGE_CPA_HEALTH_TIMEOUT_SECONDS", 3)
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	if timeoutSeconds > 30 {
		timeoutSeconds = 30
	}
	client := &http.Client{
		Timeout: time.Duration(timeoutSeconds) * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	type probeTarget struct {
		projection dto.EdgeChannelProjectionV1
		baseURL    string
		usable     bool
	}
	targets := make([]probeTarget, len(stored))
	for i := range stored {
		if err := common.UnmarshalJsonStr(stored[i].Payload, &targets[i].projection); err != nil {
			return nil, err
		}
		var channel model.Channel
		if err := model.DB.WithContext(ctx).First(&channel, int(targets[i].projection.ChannelID)).Error; err == nil &&
			channel.BaseURL != nil && channel.Key != "" && targets[i].projection.Enabled {
			targets[i].baseURL = *channel.BaseURL
			targets[i].usable = true
		}
	}
	statuses := make([]dto.EdgeCPAStatusV1, len(stored))
	var wait sync.WaitGroup
	for i := range stored {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			checkedAt := time.Now().UTC()
			projection := targets[index].projection
			status := dto.EdgeCPAStatusV1{LocalService: projection.LocalService, CheckedAtUnixMilli: checkedAt.UnixMilli()}
			if !targets[index].usable {
				statuses[index] = status
				return
			}
			target, err := url.Parse(targets[index].baseURL)
			if err != nil {
				statuses[index] = status
				return
			}
			target.Path = "/healthz"
			target.RawPath = ""
			target.RawQuery = ""
			target.Fragment = ""
			request, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
			if err != nil {
				statuses[index] = status
				return
			}
			started := time.Now()
			response, err := client.Do(request)
			latency := time.Since(started).Milliseconds()
			if latency < 0 {
				latency = 0
			}
			status.LatencyMilliseconds = latency
			if err == nil {
				response.Body.Close()
				status.Healthy = response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
			}
			if status.Healthy {
				status.AvailableModels = append([]string(nil), projection.Models...)
				sort.Strings(status.AvailableModels)
			}
			status.CheckedAtUnixMilli = time.Now().UTC().UnixMilli()
			statuses[index] = status
		}(i)
	}
	wait.Wait()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].LocalService < statuses[j].LocalService })
	healthy := make(map[dto.EdgeLocalServiceV1]bool, len(statuses))
	for i := range statuses {
		if statuses[i].LocalService.Valid() {
			healthy[statuses[i].LocalService] = statuses[i].Healthy
		}
	}
	if err := withEdgeDataPlanePolicyMutation(func() error {
		if edgeCPAReadinessRequired.Load() {
			edgeCPAReady.Store(false)
		}
		if len(stored) > 0 {
			if err := model.RefreshEdgeLocalChannelRuntimeWithHealth(model.DB.WithContext(ctx), healthy); err != nil {
				return err
			}
		}
		if edgeCPAReadinessRequired.Load() {
			edgeCPAReady.Store(edgeCPAStatusesReady(statuses))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return statuses, nil
}
