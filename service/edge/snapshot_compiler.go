package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesnapshot"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
	coreservice "github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	edgeSnapshotPageLimitEnv        = "EDGE_SNAPSHOT_PAGE_LIMIT"
	edgeSnapshotTTLSecondsEnv       = "EDGE_SNAPSHOT_TTL_SECONDS"
	defaultEdgeSnapshotPageLimit    = 500
	defaultEdgeSnapshotTTLSeconds   = int64(3600)
	minimumEdgeSnapshotTTLSeconds   = int64(60)
	maximumEdgeSnapshotTTLSeconds   = int64(86400)
	claudeCacheCreation1hMultiplier = 6.0 / 3.75
)

var (
	edgeSnapshotCompileMutex sync.Mutex

	ErrEdgeSnapshotUnrepresentable = errors.New("edge snapshot contains configuration that cannot be represented safely")
)

var edgeSnapshotDatasetOrder = [...]dto.EdgeSnapshotDatasetV1{
	dto.EdgeSnapshotDatasetAuthenticationV1,
	dto.EdgeSnapshotDatasetUsersV1,
	dto.EdgeSnapshotDatasetGroupsV1,
	dto.EdgeSnapshotDatasetModelsV1,
	dto.EdgeSnapshotDatasetChannelsV1,
	dto.EdgeSnapshotDatasetPricingV1,
	dto.EdgeSnapshotDatasetRoutingV1,
}

var edgeSnapshotPassHeaderAllowlist = map[string]string{
	"originator":                             "Originator",
	"session_id":                             "Session_id",
	"thread_id":                              "Thread_id",
	"session-id":                             "Session-Id",
	"thread-id":                              "Thread-Id",
	"user-agent":                             "User-Agent",
	"x-client-request-id":                    "X-Client-Request-Id",
	"x-codex-beta-features":                  "X-Codex-Beta-Features",
	"x-codex-turn-state":                     "X-Codex-Turn-State",
	"x-codex-turn-metadata":                  "X-Codex-Turn-Metadata",
	"x-codex-window-id":                      "X-Codex-Window-Id",
	"x-codex-parent-thread-id":               "X-Codex-Parent-Thread-Id",
	"x-openai-subagent":                      "X-OpenAI-Subagent",
	"x-openai-memgen-request":                "X-OpenAI-Memgen-Request",
	"x-responsesapi-include-timing-metrics":  "X-ResponsesAPI-Include-Timing-Metrics",
	"x-openai-internal-codex-responses-lite": "X-OpenAI-Internal-Codex-Responses-Lite",
}

type edgeSnapshotCompilerRuntime struct {
	PageLimit  int
	TTLSeconds int64
	ExpiresAt  int64
}

type edgeSnapshotDatabaseState struct {
	Tokens        []model.Token
	Users         []model.User
	Channels      []model.Channel
	Abilities     []model.Ability
	ModelStatuses map[string]int
}

type edgeSnapshotProjection struct {
	Authentication []dto.EdgeTokenAuthRecordV1
	Users          []dto.EdgeUserPolicyV1
	Groups         []dto.EdgeGroupPolicyV1
	Models         []dto.EdgeModelPolicyV1
	Channels       []dto.EdgeChannelProjectionV1
	Pricing        []dto.EdgePricingPolicyV1
	Routing        []dto.EdgeRoutingPolicyV1

	userGroups  []string
	modelNames  []string
	usingGroups []string
}

type edgeSnapshotPricingInput struct {
	Mode                 string
	Expression           string
	ModelPrice           *float64
	ModelRatio           *float64
	CompletionRatio      *float64
	CacheReadRatio       *float64
	CacheCreationRatio   *float64
	ImageRatio           *float64
	AudioRatio           *float64
	AudioCompletionRatio *float64
	AudioInputPrice      *float64
	ToolPrices           map[string]float64
	QuotaPerUnit         float64
	PreConsumedQuota     *int
}

type edgeSnapshotPageBuild struct {
	Ordinal   int
	ItemCount int64
	Digest    string
	Payload   string
}

type edgeSnapshotDatasetBuild struct {
	Dataset   dto.EdgeSnapshotDatasetV1
	ItemCount int64
	Digest    string
	Signature string
	Pages     []edgeSnapshotPageBuild
}

// CompileAndPublishEdgeSnapshot compiles the complete v1 master policy graph
// and atomically publishes it. The process-wide mutex prevents overlapping
// compilers from observing different revisions or retiring each other's work.
func CompileAndPublishEdgeSnapshot() (*model.EdgeCompiledSnapshotManifest, error) {
	edgeSnapshotCompileMutex.Lock()
	defer edgeSnapshotCompileMutex.Unlock()

	return compileAndPublishEdgeSnapshotAt(time.Now())
}

func compileAndPublishEdgeSnapshotAt(now time.Time) (*model.EdgeCompiledSnapshotManifest, error) {
	now = now.UTC().Truncate(time.Second)
	signingKey, err := LoadSnapshotSigningKeyFromEnv(now)
	if err != nil {
		return nil, err
	}
	runtime, err := loadEdgeSnapshotCompilerRuntime(now, signingKey)
	if err != nil {
		return nil, err
	}
	databaseState, err := loadEdgeSnapshotDatabaseState(now.Unix())
	if err != nil {
		return nil, err
	}
	projection, err := projectEdgeSnapshotDatabaseState(databaseState)
	if err != nil {
		return nil, err
	}
	if err := captureEdgeSnapshotSettings(projection); err != nil {
		return nil, err
	}
	return publishEdgeSnapshotProjection(projection, signingKey, runtime, now.Unix())
}

func publishEdgeSnapshotProjection(projection *edgeSnapshotProjection, signingKey *SnapshotSigningKey, runtime edgeSnapshotCompilerRuntime, createdAt int64) (*model.EdgeCompiledSnapshotManifest, error) {
	if runtime.TTLSeconds < minimumEdgeSnapshotTTLSeconds || runtime.TTLSeconds > maximumEdgeSnapshotTTLSeconds {
		return nil, errors.New("invalid edge snapshot TTL")
	}
	contents, err := buildEdgeSnapshotDatasetContents(projection, runtime.PageLimit)
	if err != nil {
		return nil, err
	}
	latest, err := model.GetLatestPublishedEdgeCompiledSnapshotManifest(createdAt)
	if err == nil && edgeSnapshotContentMatchesManifest(contents, latest.Manifest) &&
		latest.Manifest.ExpiresAtUnixMilli-createdAt*1000 > runtime.TTLSeconds*1000/2 {
		if _, cleanupErr := model.CleanupObsoleteEdgeCompiledSnapshots(createdAt, createdAt-runtime.TTLSeconds); cleanupErr != nil {
			return nil, cleanupErr
		}
		return latest, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	snapshotID, err := persistEdgeSnapshotProjectionWithContents(contents, signingKey, runtime, createdAt)
	if err != nil {
		return nil, err
	}
	if _, err := model.CleanupObsoleteEdgeCompiledSnapshots(createdAt, createdAt-runtime.TTLSeconds); err != nil {
		return nil, err
	}
	manifest, err := model.GetLatestPublishedEdgeCompiledSnapshotManifest(createdAt)
	if err != nil {
		return nil, err
	}
	if manifest.Manifest.SnapshotID != snapshotID {
		return nil, errors.New("published edge snapshot is not the latest manifest")
	}
	return manifest, nil
}

func loadEdgeSnapshotCompilerRuntime(now time.Time, signingKey *SnapshotSigningKey) (edgeSnapshotCompilerRuntime, error) {
	pageLimit, err := parseBoundedEdgeSnapshotEnv(
		edgeSnapshotPageLimitEnv,
		defaultEdgeSnapshotPageLimit,
		1,
		dto.EdgeControlMaxSnapshotPageLimitV1,
	)
	if err != nil {
		return edgeSnapshotCompilerRuntime{}, err
	}
	ttlSeconds, err := parseBoundedEdgeSnapshotEnv64(
		edgeSnapshotTTLSecondsEnv,
		defaultEdgeSnapshotTTLSeconds,
		minimumEdgeSnapshotTTLSeconds,
		maximumEdgeSnapshotTTLSeconds,
	)
	if err != nil {
		return edgeSnapshotCompilerRuntime{}, err
	}
	expiresAt := now.Unix() + ttlSeconds
	if expiresAt > signingKey.ExpiresAt {
		expiresAt = signingKey.ExpiresAt
	}
	if expiresAt-now.Unix() < minimumEdgeSnapshotTTLSeconds {
		return edgeSnapshotCompilerRuntime{}, errors.New("snapshot signing key has less than the minimum snapshot lifetime remaining")
	}
	return edgeSnapshotCompilerRuntime{PageLimit: pageLimit, TTLSeconds: ttlSeconds, ExpiresAt: expiresAt}, nil
}

func parseBoundedEdgeSnapshotEnv(name string, fallback int, minimum int, maximum int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return parsed, nil
}

func parseBoundedEdgeSnapshotEnv64(name string, fallback int64, minimum int64, maximum int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return parsed, nil
}

func loadEdgeSnapshotDatabaseState(now int64) (*edgeSnapshotDatabaseState, error) {
	state := &edgeSnapshotDatabaseState{ModelStatuses: make(map[string]int)}
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("status = ?", common.TokenStatusEnabled).
			Where("expired_time = ? OR expired_time >= ?", -1, now).
			Where("unlimited_quota = ? OR remain_quota > ?", true, 0).
			Order("id ASC").
			Find(&state.Tokens).Error; err != nil {
			return err
		}

		userIDs := make([]int, 0, len(state.Tokens))
		seenUsers := make(map[int]struct{}, len(state.Tokens))
		for i := range state.Tokens {
			if _, exists := seenUsers[state.Tokens[i].UserId]; exists {
				continue
			}
			seenUsers[state.Tokens[i].UserId] = struct{}{}
			userIDs = append(userIDs, state.Tokens[i].UserId)
		}
		if len(userIDs) > 0 {
			if err := tx.Select([]string{"id", "username", "status", "group", "setting"}).
				Where("id IN ?", userIDs).
				Order("id ASC").
				Find(&state.Users).Error; err != nil {
				return err
			}
		}

		channelFields := []string{
			"Id", "Type", "OpenAIOrganization", "Status", "Name", "Weight", "Models", "Group",
			"ModelMapping", "StatusCodeMapping", "Priority", "Setting", "ParamOverride", "HeaderOverride", "OtherSettings",
		}
		if err := tx.Select(channelFields).
			Order("id ASC").
			Find(&state.Channels).Error; err != nil {
			return err
		}

		channelIDs := make([]int, 0, len(state.Channels))
		for i := range state.Channels {
			channelIDs = append(channelIDs, state.Channels[i].Id)
		}
		if len(channelIDs) > 0 {
			if err := tx.Where("channel_id IN ?", channelIDs).
				Order("channel_id ASC, model ASC").
				Find(&state.Abilities).Error; err != nil {
				return err
			}
		}

		modelSet := make(map[string]struct{}, len(state.Abilities))
		for i := range state.Abilities {
			if state.Abilities[i].Enabled {
				modelSet[state.Abilities[i].Model] = struct{}{}
			}
		}
		if len(modelSet) > 0 {
			modelNames := sortedEdgeSnapshotKeys(modelSet)
			var metadata []model.Model
			if err := tx.Select([]string{"model_name", "status"}).
				Where("model_name IN ?", modelNames).
				Find(&metadata).Error; err != nil {
				return err
			}
			for i := range metadata {
				state.ModelStatuses[metadata[i].ModelName] = metadata[i].Status
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func projectEdgeSnapshotDatabaseState(state *edgeSnapshotDatabaseState) (*edgeSnapshotProjection, error) {
	if state == nil {
		return nil, errors.New("edge snapshot database state is nil")
	}
	projection := &edgeSnapshotProjection{}
	consumeLogSnapshotFieldsEnabled := edgeConsumeLogSnapshotFieldsEnabled()

	usersByID := make(map[int]model.User, len(state.Users))
	for i := range state.Users {
		usersByID[state.Users[i].Id] = state.Users[i]
	}
	referencedUsers := make(map[int]model.User)
	fingerprints := make(map[string]int)
	for i := range state.Tokens {
		token := state.Tokens[i]
		user, exists := usersByID[token.UserId]
		if !exists || user.Status != common.UserStatusEnabled {
			continue
		}
		if token.Group == "auto" {
			return nil, fmt.Errorf("%w: token %d uses auto group", ErrEdgeSnapshotUnrepresentable, token.Id)
		}
		fingerprint, err := edgetoken.FingerprintStoredKey(token.Key)
		if err != nil {
			return nil, fmt.Errorf("token %d fingerprint: %w", token.Id, err)
		}
		if previousTokenID, exists := fingerprints[fingerprint]; exists {
			return nil, fmt.Errorf("token fingerprint collision between token %d and %d", previousTokenID, token.Id)
		}
		fingerprints[fingerprint] = token.Id

		allowedModels := make([]string, 0)
		if token.ModelLimitsEnabled {
			seenModels := make(map[string]struct{})
			for _, modelName := range token.GetModelLimits() {
				modelName = strings.TrimSpace(modelName)
				if modelName == "" {
					continue
				}
				if _, exists := seenModels[modelName]; exists {
					continue
				}
				seenModels[modelName] = struct{}{}
				allowedModels = append(allowedModels, modelName)
			}
			sort.Strings(allowedModels)
		}
		allowedCIDRs, err := projectEdgeSnapshotCIDRs(token.GetIpLimits())
		if err != nil {
			return nil, fmt.Errorf("token %d: %w", token.Id, err)
		}

		record := dto.EdgeTokenAuthRecordV1{
			TokenFingerprint:  fingerprint,
			TokenID:           int64(token.Id),
			UserID:            int64(token.UserId),
			Enabled:           true,
			Group:             token.Group,
			ModelLimitEnabled: token.ModelLimitsEnabled,
			AllowedModels:     allowedModels,
			AllowedCIDRs:      allowedCIDRs,
			CrossGroupRetry:   token.CrossGroupRetry,
		}
		if consumeLogSnapshotFieldsEnabled {
			record.TokenName = token.Name
		}
		if token.ExpiredTime != -1 {
			if token.ExpiredTime > math.MaxInt64/1000-1 {
				return nil, fmt.Errorf("token %d expiry overflows Unix milliseconds", token.Id)
			}
			expiresAt := (token.ExpiredTime + 1) * 1000
			record.ExpiresAtUnixMilli = &expiresAt
		}
		projection.Authentication = append(projection.Authentication, record)
		referencedUsers[user.Id] = user
	}
	sort.Slice(projection.Authentication, func(i, j int) bool {
		return projection.Authentication[i].TokenFingerprint < projection.Authentication[j].TokenFingerprint
	})

	for _, user := range referencedUsers {
		if user.Group == "" || user.Group == "auto" {
			return nil, fmt.Errorf("%w: user %d has unsupported default group %q", ErrEdgeSnapshotUnrepresentable, user.Id, user.Group)
		}
		userSetting := dto.UserSetting{}
		if strings.TrimSpace(user.Setting) != "" {
			if err := common.Unmarshal([]byte(user.Setting), &userSetting); err != nil {
				return nil, fmt.Errorf("user %d setting: %w", user.Id, err)
			}
		}
		setting := dto.EdgeUserSettingV1{
			AcceptUnsetRatioModel: userSetting.AcceptUnsetRatioModel,
			Language:              strings.ToLower(strings.TrimSpace(userSetting.Language)),
			BillingPreference:     common.NormalizeBillingPreference(userSetting.BillingPreference),
		}
		if consumeLogSnapshotFieldsEnabled {
			setting.RecordIpLog = userSetting.RecordIpLog
		}
		projection.Users = append(projection.Users, dto.EdgeUserPolicyV1{
			UserID:       int64(user.Id),
			Enabled:      true,
			Username:     user.Username,
			DefaultGroup: user.Group,
			Setting:      setting,
		})
	}
	sort.Slice(projection.Users, func(i, j int) bool { return projection.Users[i].UserID < projection.Users[j].UserID })
	groupSet := make(map[string]struct{})
	for i := range projection.Users {
		groupSet[projection.Users[i].DefaultGroup] = struct{}{}
	}
	projection.userGroups = sortedEdgeSnapshotKeys(groupSet)

	channelByID := make(map[int]model.Channel, len(state.Channels))
	serviceByChannelID := make(map[int]dto.EdgeLocalServiceV1, len(state.Channels))
	for i := range state.Channels {
		channel := state.Channels[i]
		switch channel.Status {
		case common.ChannelStatusEnabled, common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled:
		default:
			return nil, fmt.Errorf("%w: channel %q has invalid status", ErrEdgeSnapshotUnrepresentable, channel.Name)
		}
		localService := dto.EdgeLocalServiceV1(channel.Name)
		if !localService.Valid() {
			return nil, fmt.Errorf("%w: channel %q cannot be used as an edge local service name", ErrEdgeSnapshotUnrepresentable, channel.Name)
		}
		channelByID[channel.Id] = channel
		serviceByChannelID[channel.Id] = localService
	}

	channelModels := make(map[int]map[string]struct{}, len(channelByID))
	channelGroups := make(map[int]map[string]struct{}, len(channelByID))
	modelChannels := make(map[string]map[int64]struct{})
	for i := range state.Abilities {
		ability := state.Abilities[i]
		channel, exists := channelByID[ability.ChannelId]
		if !exists || !ability.Enabled || channel.Status != common.ChannelStatusEnabled {
			continue
		}
		if status, exists := state.ModelStatuses[ability.Model]; exists && status != 1 {
			continue
		}
		if ability.Group == "" || ability.Group == "auto" {
			return nil, fmt.Errorf("%w: ability for %s uses unsupported group %q", ErrEdgeSnapshotUnrepresentable, ability.Model, ability.Group)
		}
		channelPriority := int64(0)
		if channel.Priority != nil {
			channelPriority = *channel.Priority
		}
		abilityPriority := int64(0)
		if ability.Priority != nil {
			abilityPriority = *ability.Priority
		}
		channelWeight := uint(0)
		if channel.Weight != nil {
			channelWeight = *channel.Weight
		}
		if abilityPriority != channelPriority || ability.Weight != channelWeight {
			return nil, fmt.Errorf("%w: channel %q has per-ability priority or weight", ErrEdgeSnapshotUnrepresentable, channel.Name)
		}
		if channelModels[channel.Id] == nil {
			channelModels[channel.Id] = make(map[string]struct{})
			channelGroups[channel.Id] = make(map[string]struct{})
		}
		channelModels[channel.Id][ability.Model] = struct{}{}
		channelGroups[channel.Id][ability.Group] = struct{}{}
		if modelChannels[ability.Model] == nil {
			modelChannels[ability.Model] = make(map[int64]struct{})
		}
		modelChannels[ability.Model][int64(channel.Id)] = struct{}{}
	}

	for channelID, channel := range channelByID {
		if len(channelModels[channelID]) == 0 {
			continue
		}
		projectionChannel, err := projectEdgeSnapshotChannel(channel, serviceByChannelID[channelID])
		if err != nil {
			return nil, err
		}
		projectionChannel.Groups = sortedEdgeSnapshotKeys(channelGroups[channelID])
		projectionChannel.Models = sortedEdgeSnapshotKeys(channelModels[channelID])
		if len(projectionChannel.ModelMapping) > 0 {
			filteredMapping := make(map[string]string)
			for _, modelName := range projectionChannel.Models {
				if upstreamModel, exists := projectionChannel.ModelMapping[modelName]; exists {
					filteredMapping[modelName] = upstreamModel
				}
			}
			projectionChannel.ModelMapping = filteredMapping
		}
		projection.Channels = append(projection.Channels, projectionChannel)
	}
	sort.Slice(projection.Channels, func(i, j int) bool { return projection.Channels[i].ChannelID < projection.Channels[j].ChannelID })

	for modelName, channels := range modelChannels {
		channelIDs := make([]int64, 0, len(channels))
		for channelID := range channels {
			channelIDs = append(channelIDs, channelID)
		}
		sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
		projection.Models = append(projection.Models, dto.EdgeModelPolicyV1{
			Model:      modelName,
			Enabled:    true,
			Endpoints:  []dto.EdgeEndpointV1{dto.EdgeEndpointDataPlaneV1},
			Streaming:  true,
			ChannelIDs: channelIDs,
		})
	}
	sort.Slice(projection.Models, func(i, j int) bool { return projection.Models[i].Model < projection.Models[j].Model })
	if len(projection.Models) == 0 {
		return nil, errors.New("no channel models are available for edge")
	}
	projection.modelNames = make([]string, 0, len(projection.Models))
	compiledModels := make(map[string]struct{}, len(projection.Models))
	for i := range projection.Models {
		projection.modelNames = append(projection.modelNames, projection.Models[i].Model)
		compiledModels[projection.Models[i].Model] = struct{}{}
	}
	for i := range projection.Authentication {
		if !projection.Authentication[i].ModelLimitEnabled {
			continue
		}
		filtered := projection.Authentication[i].AllowedModels[:0]
		for _, modelName := range projection.Authentication[i].AllowedModels {
			if _, exists := compiledModels[modelName]; exists {
				filtered = append(filtered, modelName)
			}
		}
		projection.Authentication[i].AllowedModels = filtered
	}
	edgeUsingGroups := make(map[string]struct{})
	for _, groups := range channelGroups {
		for group := range groups {
			edgeUsingGroups[group] = struct{}{}
		}
	}
	projection.usingGroups = sortedEdgeSnapshotKeys(edgeUsingGroups)
	return projection, nil
}

func projectEdgeSnapshotCIDRs(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if net.ParseIP(value) == nil {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return nil, fmt.Errorf("allowed IP %q is not an IP address or CIDR", value)
			}
		}
		seen[value] = struct{}{}
	}
	return sortedEdgeSnapshotKeys(seen), nil
}

func projectEdgeSnapshotChannel(channel model.Channel, localService dto.EdgeLocalServiceV1) (dto.EdgeChannelProjectionV1, error) {
	settings := dto.ChannelSettings{}
	if err := unmarshalEdgeSnapshotKnownFields("channel setting", pointerString(channel.Setting), &settings); err != nil {
		return dto.EdgeChannelProjectionV1{}, fmt.Errorf("%w: channel %q: %v", ErrEdgeSnapshotUnrepresentable, channel.Name, err)
	}
	other := dto.ChannelOtherSettings{}
	if err := unmarshalEdgeSnapshotKnownFields("channel other settings", channel.OtherSettings, &other); err != nil {
		return dto.EdgeChannelProjectionV1{}, fmt.Errorf("%w: channel %q: %v", ErrEdgeSnapshotUnrepresentable, channel.Name, err)
	}

	weight := 0
	if channel.Weight != nil {
		if uint64(*channel.Weight) > uint64(math.MaxInt) {
			return dto.EdgeChannelProjectionV1{}, fmt.Errorf("channel %q weight overflows int", channel.Name)
		}
		weight = int(*channel.Weight)
	}
	priority := int64(0)
	if channel.Priority != nil {
		priority = *channel.Priority
	}
	sanitizedOther, err := sanitizeEdgeChannelOtherSettings(other)
	if err != nil {
		return dto.EdgeChannelProjectionV1{}, fmt.Errorf("%w: channel %q: sanitize typed settings: %v", ErrEdgeSnapshotUnrepresentable, channel.Name, err)
	}
	projection := dto.EdgeChannelProjectionV1{
		ChannelID:       int64(channel.Id),
		Type:            channel.Type,
		Name:            channel.Name,
		Enabled:         channel.Status == common.ChannelStatusEnabled,
		Priority:        priority,
		Weight:          weight,
		LocalService:    localService,
		SettingsVersion: 1,
		ChannelSettings: dto.EdgeChannelSettingsV1{
			ForceFormat: settings.ForceFormat, ThinkingToContent: settings.ThinkingToContent,
			PassThroughBodyEnabled: settings.PassThroughBodyEnabled,
			SystemPrompt:           settings.SystemPrompt, SystemPromptOverride: settings.SystemPromptOverride,
		},
		ChannelOther: sanitizedOther,
		TextPolicy: dto.EdgeTextRequestPolicyV1{
			ForceFormat:             settings.ForceFormat,
			ThinkingToContent:       settings.ThinkingToContent,
			PassThroughBodyEnabled:  settings.PassThroughBodyEnabled,
			SystemPrompt:            settings.SystemPrompt,
			SystemPromptOverride:    settings.SystemPromptOverride,
			AllowServiceTier:        other.AllowServiceTier,
			AllowInferenceGeo:       other.AllowInferenceGeo,
			AllowSpeed:              other.AllowSpeed,
			DisableStore:            other.DisableStore,
			AllowSafetyIdentifier:   other.AllowSafetyIdentifier,
			AllowIncludeObfuscation: other.AllowIncludeObfuscation,
		},
	}
	if channel.ModelMapping != nil && strings.TrimSpace(*channel.ModelMapping) != "" && strings.TrimSpace(*channel.ModelMapping) != "null" {
		if err := common.Unmarshal([]byte(*channel.ModelMapping), &projection.ModelMapping); err != nil {
			return dto.EdgeChannelProjectionV1{}, fmt.Errorf("%w: channel %q: model_mapping is invalid JSON: %v", ErrEdgeSnapshotUnrepresentable, channel.Name, err)
		}
	}
	if channel.StatusCodeMapping != nil && strings.TrimSpace(*channel.StatusCodeMapping) != "" && strings.TrimSpace(*channel.StatusCodeMapping) != "null" {
		if err := common.Unmarshal([]byte(*channel.StatusCodeMapping), &projection.StatusCodeMapping); err != nil {
			return dto.EdgeChannelProjectionV1{}, fmt.Errorf("%w: channel %q: status_code_mapping is invalid JSON: %v", ErrEdgeSnapshotUnrepresentable, channel.Name, err)
		}
	}
	if err := projection.Validate(); err != nil {
		return dto.EdgeChannelProjectionV1{}, err
	}
	return projection, nil
}

func sanitizeEdgeChannelOtherSettings(source dto.ChannelOtherSettings) (dto.EdgeChannelOtherSettingsV1, error) {
	result := dto.EdgeChannelOtherSettingsV1{
		AzureResponsesVersion: source.AzureResponsesVersion, VertexKeyType: source.VertexKeyType,
		OpenRouterEnterprise: source.OpenRouterEnterprise, ClaudeBetaQuery: source.ClaudeBetaQuery,
		AllowServiceTier: source.AllowServiceTier, AllowInferenceGeo: source.AllowInferenceGeo,
		AllowSpeed: source.AllowSpeed, AllowSafetyIdentifier: source.AllowSafetyIdentifier,
		DisableStore: source.DisableStore, AllowIncludeObfuscation: source.AllowIncludeObfuscation,
		DisableTaskPollingSleep: source.DisableTaskPollingSleep, AwsKeyType: source.AwsKeyType,
	}
	if source.AdvancedCustom != nil {
		result.AdvancedCustom = &dto.EdgeAdvancedCustomConfigV1{
			Routes: make([]dto.EdgeAdvancedCustomRouteV1, 0, len(source.AdvancedCustom.Routes)),
		}
		for _, route := range source.AdvancedCustom.Routes {
			result.AdvancedCustom.Routes = append(result.AdvancedCustom.Routes, dto.EdgeAdvancedCustomRouteV1{
				IncomingPath: route.IncomingPath, UpstreamPath: route.UpstreamPath,
				Converter: route.Converter, Models: append([]string(nil), route.Models...),
			})
		}
	}
	return result, nil
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func unmarshalEdgeSnapshotKnownFields(field string, value string, target any) error {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" {
		return nil
	}
	if err := common.Unmarshal([]byte(value), target); err != nil {
		return fmt.Errorf("%s is invalid: %w", field, err)
	}
	return nil
}

func captureEdgeSnapshotSettings(projection *edgeSnapshotProjection) error {
	if projection == nil {
		return errors.New("edge snapshot projection is nil")
	}
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()

	edgeUsingGroups := make(map[string]struct{}, len(projection.usingGroups))
	for _, usingGroup := range projection.usingGroups {
		edgeUsingGroups[usingGroup] = struct{}{}
	}
	for _, userGroup := range projection.userGroups {
		usableGroups := coreservice.GetUserUsableGroups(userGroup)
		usingGroupNames := make([]string, 0, len(usableGroups))
		for usingGroup := range usableGroups {
			if usingGroup == "auto" {
				continue
			}
			if _, availableOnEdge := edgeUsingGroups[usingGroup]; !availableOnEdge {
				continue
			}
			usingGroupNames = append(usingGroupNames, usingGroup)
		}
		sort.Strings(usingGroupNames)
		policy := dto.EdgeGroupPolicyV1{UserGroup: userGroup}
		for _, usingGroup := range usingGroupNames {
			ratio, hasSpecialRatio := ratio_setting.GetGroupGroupRatio(userGroup, usingGroup)
			if !hasSpecialRatio {
				if !ratio_setting.ContainsGroupRatio(usingGroup) {
					return fmt.Errorf("%w: group %q has no ratio", ErrEdgeSnapshotUnrepresentable, usingGroup)
				}
				ratio = coreservice.GetUserGroupRatio(userGroup, usingGroup)
			}
			policy.UsingGroups = append(policy.UsingGroups, dto.EdgeUsingGroupPolicyV1{
				Group:        usingGroup,
				Enabled:      true,
				Ratio:        ratio,
				SpecialRatio: hasSpecialRatio && edgeConsumeLogSnapshotFieldsEnabled(),
			})
		}
		if err := (dto.EdgeSnapshotPagePayloadV1{Groups: []dto.EdgeGroupPolicyV1{policy}}).
			Validate(dto.EdgeSnapshotDatasetGroupsV1, 1); err != nil {
			return fmt.Errorf("group %q: %w", userGroup, err)
		}
		projection.Groups = append(projection.Groups, policy)
	}
	sort.Slice(projection.Groups, func(i, j int) bool { return projection.Groups[i].UserGroup < projection.Groups[j].UserGroup })
	if err := filterEdgeSnapshotAuthorizationGroups(projection); err != nil {
		return err
	}

	billableModels := make(map[string]struct{}, len(projection.modelNames))
	for _, modelName := range projection.modelNames {
		input := edgeSnapshotPricingInput{
			Mode:         billing_setting.GetBillingMode(modelName),
			QuotaPerUnit: common.QuotaPerUnit,
		}
		if edgePreConsumedQuotaSnapshotEnabled() {
			preConsumedQuota := common.PreConsumedQuota
			input.PreConsumedQuota = &preConsumedQuota
		}
		if expression, ok := billing_setting.GetBillingExpr(modelName); ok {
			input.Expression = expression
		}
		if value, ok := ratio_setting.GetModelPrice(modelName, false); ok {
			input.ModelPrice = floatPointer(value)
		}
		if value, ok, _ := ratio_setting.GetModelRatio(modelName); ok {
			input.ModelRatio = floatPointer(value)
		}
		input.CompletionRatio = floatPointer(ratio_setting.GetCompletionRatio(modelName))
		cacheRead, _ := ratio_setting.GetCacheRatio(modelName)
		input.CacheReadRatio = floatPointer(cacheRead)
		cacheCreation, _ := ratio_setting.GetCreateCacheRatio(modelName)
		input.CacheCreationRatio = floatPointer(cacheCreation)
		imageRatio, _ := ratio_setting.GetImageRatio(modelName)
		input.ImageRatio = floatPointer(imageRatio)
		input.AudioRatio = floatPointer(ratio_setting.GetAudioRatio(modelName))
		input.AudioCompletionRatio = floatPointer(ratio_setting.GetAudioCompletionRatio(modelName))
		if audioInputPrice := operation_setting.GetGeminiInputAudioPricePerMillionTokens(modelName); audioInputPrice > 0 {
			input.AudioInputPrice = floatPointer(audioInputPrice)
		}
		input.ToolPrices = map[string]float64{
			"web_search_preview": operation_setting.GetToolPriceForModel("web_search_preview", modelName),
			"web_search":         operation_setting.GetToolPriceForModel("web_search", modelName),
			"file_search":        operation_setting.GetToolPriceForModel("file_search", modelName),
		}

		policy, err := projectEdgeSnapshotPricing(modelName, input)
		if err != nil {
			continue
		}
		projection.Pricing = append(projection.Pricing, policy)
		billableModels[modelName] = struct{}{}
	}
	filterEdgeSnapshotModelsByPricing(projection, billableModels)
	sort.Slice(projection.Pricing, func(i, j int) bool { return projection.Pricing[i].PolicyID < projection.Pricing[j].PolicyID })

	affinityBytes, err := common.Marshal(operation_setting.GetChannelAffinitySetting())
	if err != nil {
		return err
	}
	affinity := operation_setting.ChannelAffinitySetting{}
	if err := common.Unmarshal(affinityBytes, &affinity); err != nil {
		return err
	}
	routing, err := projectEdgeSnapshotRouting(affinity, projection.modelNames)
	if err != nil {
		return err
	}
	projection.Routing = []dto.EdgeRoutingPolicyV1{routing}
	return nil
}

func edgeConsumeLogSnapshotFieldsEnabled() bool {
	return common.GetEnvOrDefaultBool("EDGE_CONSUME_LOG_SNAPSHOT_FIELDS_ENABLED", false)
}

func edgePreConsumedQuotaSnapshotEnabled() bool {
	return common.GetEnvOrDefaultBool("EDGE_PRE_CONSUMED_QUOTA_SNAPSHOT_ENABLED", false)
}

func filterEdgeSnapshotModelsByPricing(projection *edgeSnapshotProjection, billableModels map[string]struct{}) {
	filteredModels := projection.Models[:0]
	projection.modelNames = projection.modelNames[:0]
	for i := range projection.Models {
		if _, exists := billableModels[projection.Models[i].Model]; !exists {
			continue
		}
		filteredModels = append(filteredModels, projection.Models[i])
		projection.modelNames = append(projection.modelNames, projection.Models[i].Model)
	}
	projection.Models = filteredModels
	for i := range projection.Channels {
		filteredChannelModels := projection.Channels[i].Models[:0]
		filteredMapping := make(map[string]string)
		for _, modelName := range projection.Channels[i].Models {
			if _, exists := billableModels[modelName]; !exists {
				continue
			}
			filteredChannelModels = append(filteredChannelModels, modelName)
			if upstreamModel, exists := projection.Channels[i].ModelMapping[modelName]; exists {
				filteredMapping[modelName] = upstreamModel
			}
		}
		projection.Channels[i].Models = filteredChannelModels
		projection.Channels[i].ModelMapping = filteredMapping
	}
	for i := range projection.Authentication {
		if !projection.Authentication[i].ModelLimitEnabled {
			continue
		}
		filteredAllowedModels := projection.Authentication[i].AllowedModels[:0]
		for _, modelName := range projection.Authentication[i].AllowedModels {
			if _, exists := billableModels[modelName]; exists {
				filteredAllowedModels = append(filteredAllowedModels, modelName)
			}
		}
		projection.Authentication[i].AllowedModels = filteredAllowedModels
	}
}

func filterEdgeSnapshotAuthorizationGroups(projection *edgeSnapshotProjection) error {
	userGroups := make(map[int64]string, len(projection.Users))
	for i := range projection.Users {
		userGroups[projection.Users[i].UserID] = projection.Users[i].DefaultGroup
	}
	groupPolicies := make(map[string]map[string]struct{}, len(projection.Groups))
	for i := range projection.Groups {
		usingGroups := make(map[string]struct{}, len(projection.Groups[i].UsingGroups))
		for _, usingGroup := range projection.Groups[i].UsingGroups {
			if usingGroup.Enabled {
				usingGroups[usingGroup.Group] = struct{}{}
			}
		}
		groupPolicies[projection.Groups[i].UserGroup] = usingGroups
	}
	filteredAuthentication := projection.Authentication[:0]
	referencedUsers := make(map[int64]struct{})
	for i := range projection.Authentication {
		userGroup, exists := userGroups[projection.Authentication[i].UserID]
		if !exists {
			return fmt.Errorf("token %d references a missing edge user", projection.Authentication[i].TokenID)
		}
		usingGroup := projection.Authentication[i].Group
		if usingGroup == "" {
			usingGroup = userGroup
		}
		usable, exists := groupPolicies[userGroup]
		if !exists {
			continue
		}
		if _, exists := usable[usingGroup]; !exists {
			continue
		}
		filteredAuthentication = append(filteredAuthentication, projection.Authentication[i])
		referencedUsers[projection.Authentication[i].UserID] = struct{}{}
	}
	projection.Authentication = filteredAuthentication
	filteredUsers := projection.Users[:0]
	referencedGroups := make(map[string]struct{})
	for i := range projection.Users {
		if _, exists := referencedUsers[projection.Users[i].UserID]; !exists {
			continue
		}
		filteredUsers = append(filteredUsers, projection.Users[i])
		referencedGroups[projection.Users[i].DefaultGroup] = struct{}{}
	}
	projection.Users = filteredUsers
	filteredGroups := projection.Groups[:0]
	for i := range projection.Groups {
		if _, exists := referencedGroups[projection.Groups[i].UserGroup]; exists {
			filteredGroups = append(filteredGroups, projection.Groups[i])
		}
	}
	projection.Groups = filteredGroups
	return nil
}

func projectEdgeSnapshotPricing(modelName string, input edgeSnapshotPricingInput) (dto.EdgePricingPolicyV1, error) {
	policy := dto.EdgePricingPolicyV1{
		PolicyID:             modelName,
		Version:              "v1",
		Model:                modelName,
		QuotaPerUnit:         input.QuotaPerUnit,
		ImageRatio:           cloneFloatPointer(input.ImageRatio),
		AudioRatio:           cloneFloatPointer(input.AudioRatio),
		AudioCompletionRatio: cloneFloatPointer(input.AudioCompletionRatio),
		AudioInputPrice:      cloneFloatPointer(input.AudioInputPrice),
		ToolPrices:           cloneEdgeSnapshotFloatMap(input.ToolPrices),
	}
	if input.PreConsumedQuota != nil {
		preConsumedQuota := *input.PreConsumedQuota
		policy.PreConsumedQuota = &preConsumedQuota
	}
	if input.Mode != billing_setting.BillingModeRatio && input.Mode != billing_setting.BillingModeTieredExpr {
		return dto.EdgePricingPolicyV1{}, fmt.Errorf("%w: model %q uses unknown billing mode %q", ErrEdgeSnapshotUnrepresentable, modelName, input.Mode)
	}
	if input.Mode == billing_setting.BillingModeTieredExpr {
		expression := input.Expression
		if strings.TrimSpace(expression) == "" {
			return dto.EdgePricingPolicyV1{}, fmt.Errorf("%w: model %q has empty tiered billing expression", ErrEdgeSnapshotUnrepresentable, modelName)
		}
		if err := billing_setting.SmokeTestExpr(expression); err != nil {
			return dto.EdgePricingPolicyV1{}, fmt.Errorf("model %q billing expression: %w", modelName, err)
		}
		policy.BillingMode = dto.EdgeBillingModeTieredExprV1
		policy.BillingExpression = expression
		policy.BillingExpressionHash = billingexpr.ExprHashString(expression)
		policy.BillingExpressionVersion = billingexpr.ExprVersion(expression)
	} else if input.ModelPrice != nil {
		policy.BillingMode = dto.EdgeBillingModeFixedPriceV1
		policy.ModelPrice = floatPointer(*input.ModelPrice)
	} else {
		if input.ModelRatio == nil {
			return dto.EdgePricingPolicyV1{}, fmt.Errorf("model %q has no ratio or fixed price", modelName)
		}
		policy.BillingMode = dto.EdgeBillingModeRatioV1
		policy.ModelRatio = floatPointer(*input.ModelRatio)
		policy.CompletionRatio = cloneFloatPointer(input.CompletionRatio)
		policy.CacheReadRatio = cloneFloatPointer(input.CacheReadRatio)
		policy.CacheCreationRatio = cloneFloatPointer(input.CacheCreationRatio)
		if input.CacheCreationRatio != nil {
			policy.CacheCreation1hRatio = floatPointer(*input.CacheCreationRatio * claudeCacheCreation1hMultiplier)
		}
	}
	if err := policy.Validate(); err != nil {
		return dto.EdgePricingPolicyV1{}, fmt.Errorf("model %q pricing: %w", modelName, err)
	}
	return policy, nil
}

func cloneEdgeSnapshotFloatMap(values map[string]float64) map[string]float64 {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]float64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func floatPointer(value float64) *float64 {
	return &value
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return floatPointer(*value)
}

func projectEdgeSnapshotRouting(setting operation_setting.ChannelAffinitySetting, models []string) (dto.EdgeRoutingPolicyV1, error) {
	policy := dto.EdgeChannelAffinityPolicyV1{
		Enabled:               setting.Enabled,
		SwitchOnSuccess:       setting.SwitchOnSuccess,
		KeepOnChannelDisabled: setting.KeepOnChannelDisabled,
		MaxEntries:            setting.MaxEntries,
		DefaultTTLSeconds:     int64(setting.DefaultTTLSeconds),
	}
	for _, rule := range setting.Rules {
		relevant, err := edgeSnapshotAffinityRuleRelevant(rule, models)
		if err != nil {
			return dto.EdgeRoutingPolicyV1{}, err
		}
		if !relevant {
			continue
		}
		passHeaders, keepOrigin, err := projectEdgeSnapshotPassHeaders(rule.ParamOverrideTemplate)
		if err != nil {
			return dto.EdgeRoutingPolicyV1{}, fmt.Errorf("%w: affinity rule %q: %v", ErrEdgeSnapshotUnrepresentable, rule.Name, err)
		}
		projected := dto.EdgeChannelAffinityRuleV1{
			Name:              rule.Name,
			ModelRegex:        append([]string(nil), rule.ModelRegex...),
			PathRegex:         append([]string(nil), rule.PathRegex...),
			UserAgentInclude:  append([]string(nil), rule.UserAgentInclude...),
			ValueRegex:        rule.ValueRegex,
			TTLSeconds:        int64(rule.TTLSeconds),
			PassHeaders:       passHeaders,
			KeepOrigin:        keepOrigin,
			SkipRetry:         rule.SkipRetryOnFailure,
			IncludeUsingGroup: rule.IncludeUsingGroup,
			IncludeModelName:  rule.IncludeModelName,
			IncludeRuleName:   rule.IncludeRuleName,
		}
		for _, source := range rule.KeySources {
			projected.KeySources = append(projected.KeySources, dto.EdgeChannelAffinityKeySourceV1{
				Type: dto.EdgeChannelAffinityKeySourceTypeV1(source.Type),
				Key:  source.Key,
				Path: source.Path,
			})
		}
		if err := validateEdgeChannelAffinityRuleSemantics(projected); err != nil {
			return dto.EdgeRoutingPolicyV1{}, fmt.Errorf("%w: affinity rule %q: %v", ErrEdgeSnapshotUnrepresentable, rule.Name, err)
		}
		if err := projected.Validate(); err != nil {
			return dto.EdgeRoutingPolicyV1{}, fmt.Errorf("affinity rule %q: %w", rule.Name, err)
		}
		policy.Rules = append(policy.Rules, projected)
	}
	routing := dto.EdgeRoutingPolicyV1{ChannelAffinity: policy}
	if err := routing.Validate(); err != nil {
		return dto.EdgeRoutingPolicyV1{}, err
	}
	return routing, nil
}

// validateEdgeChannelAffinityRuleSemantics covers edge-only meaning that the
// wire DTO cannot express by shape alone. token_key contains an opaque digest
// on edge, so a master regex written for the plaintext credential would match
// different data and must fail closed during both compilation and install.
func validateEdgeChannelAffinityRuleSemantics(rule dto.EdgeChannelAffinityRuleV1) error {
	if rule.ValueRegex == "" {
		return nil
	}
	for _, source := range rule.KeySources {
		if source.Type == dto.EdgeChannelAffinityKeySourceContextStringV1 &&
			source.Key == string(constant.ContextKeyTokenKey) {
			return errors.New("plaintext token_key value matching is unavailable on edge")
		}
	}
	return nil
}

func edgeSnapshotAffinityRuleRelevant(rule operation_setting.ChannelAffinityRule, models []string) (bool, error) {
	modelRegexes := make([]*regexp.Regexp, 0, len(rule.ModelRegex))
	for _, pattern := range rule.ModelRegex {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return false, fmt.Errorf("affinity rule %q model regex: %w", rule.Name, err)
		}
		modelRegexes = append(modelRegexes, compiled)
	}
	modelMatched := false
	for _, modelName := range models {
		for _, compiled := range modelRegexes {
			if compiled.MatchString(modelName) {
				modelMatched = true
				break
			}
		}
		if modelMatched {
			break
		}
	}
	if !modelMatched {
		return false, nil
	}
	if len(rule.PathRegex) == 0 {
		return true, nil
	}
	for _, pattern := range rule.PathRegex {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return false, fmt.Errorf("affinity rule %q path regex: %w", rule.Name, err)
		}
		if compiled.MatchString("/v1/chat/completions") || compiled.MatchString("/v1/responses") {
			return true, nil
		}
	}
	return false, nil
}

func projectEdgeSnapshotPassHeaders(template map[string]interface{}) ([]string, bool, error) {
	if len(template) == 0 {
		return nil, false, nil
	}
	encoded, err := common.Marshal(template)
	if err != nil {
		return nil, false, err
	}
	var root map[string]json.RawMessage
	if err := common.Unmarshal(encoded, &root); err != nil {
		return nil, false, err
	}
	if len(root) != 1 || root["operations"] == nil {
		return nil, false, errors.New("only operations is allowed in an affinity override template")
	}
	var operations []map[string]json.RawMessage
	if err := common.Unmarshal(root["operations"], &operations); err != nil {
		return nil, false, errors.New("affinity override operations must be an array")
	}
	if len(operations) != 1 {
		return nil, false, errors.New("affinity override must contain exactly one pass_headers operation")
	}
	operation := operations[0]
	for key := range operation {
		switch key {
		case "mode", "value", "keep_origin":
		default:
			return nil, false, fmt.Errorf("affinity override operation field %q is not allowed", key)
		}
	}
	var mode string
	if err := common.Unmarshal(operation["mode"], &mode); err != nil || mode != "pass_headers" {
		return nil, false, errors.New("affinity override operation must use pass_headers mode")
	}
	var headers []string
	if err := common.Unmarshal(operation["value"], &headers); err != nil || len(headers) == 0 {
		return nil, false, errors.New("pass_headers value must be a non-empty string array")
	}
	keepOrigin := false
	if raw := operation["keep_origin"]; raw != nil {
		if err := common.Unmarshal(raw, &keepOrigin); err != nil {
			return nil, false, errors.New("pass_headers keep_origin must be a boolean")
		}
	}
	seen := make(map[string]struct{}, len(headers))
	projected := make([]string, 0, len(headers))
	for _, header := range headers {
		lower := strings.ToLower(strings.TrimSpace(header))
		canonical, allowed := edgeSnapshotPassHeaderAllowlist[lower]
		if !allowed || http.CanonicalHeaderKey(header) == "" {
			return nil, false, fmt.Errorf("pass_headers value %q is not allowlisted", header)
		}
		if _, exists := seen[lower]; exists {
			continue
		}
		seen[lower] = struct{}{}
		projected = append(projected, canonical)
	}
	sort.Slice(projected, func(i, j int) bool { return strings.ToLower(projected[i]) < strings.ToLower(projected[j]) })
	return projected, keepOrigin, nil
}

func persistEdgeSnapshotProjection(projection *edgeSnapshotProjection, signingKey *SnapshotSigningKey, runtime edgeSnapshotCompilerRuntime, createdAt int64) (string, error) {
	if projection == nil || signingKey == nil {
		return "", errors.New("edge snapshot projection or signing key is nil")
	}
	contents, err := buildEdgeSnapshotDatasetContents(projection, runtime.PageLimit)
	if err != nil {
		return "", err
	}
	return persistEdgeSnapshotProjectionWithContents(contents, signingKey, runtime, createdAt)
}

func persistEdgeSnapshotProjectionWithContents(contents []edgeSnapshotDatasetBuild, signingKey *SnapshotSigningKey, runtime edgeSnapshotCompilerRuntime, createdAt int64) (string, error) {
	if signingKey == nil {
		return "", errors.New("edge snapshot signing key is nil")
	}
	if len(contents) != len(edgeSnapshotDatasetOrder) {
		return "", errors.New("edge snapshot content must contain all protocol datasets")
	}
	if runtime.PageLimit <= 0 || runtime.PageLimit > dto.EdgeControlMaxSnapshotPageLimitV1 {
		return "", errors.New("invalid edge snapshot page limit")
	}
	if runtime.ExpiresAt <= createdAt {
		return "", errors.New("invalid edge snapshot expiry")
	}

	snapshotUID := "snapshot-" + uuid.NewString()
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		revision := int64(1)
		var latest model.EdgeCompiledSnapshot
		err := tx.Order("revision DESC").Limit(1).Take(&latest).Error
		if err == nil {
			if latest.Revision == math.MaxInt64 {
				return errors.New("edge snapshot revision overflow")
			}
			revision = latest.Revision + 1
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		datasets, topDigest, err := signEdgeSnapshotDatasets(snapshotUID, revision, contents, signingKey)
		if err != nil {
			return err
		}
		snapshot := model.EdgeCompiledSnapshot{
			SnapshotUID:               snapshotUID,
			Revision:                  revision,
			ProtocolVersion:           dto.EdgeControlProtocolVersionV2,
			Status:                    model.EdgeCompiledSnapshotStatusDraft,
			HashAlgorithm:             edgesnapshot.HashAlgorithm,
			Digest:                    topDigest,
			TokenFingerprintAlgorithm: edgetoken.FingerprintAlgorithm,
			TokenFingerprintKeyID:     "",
			TokenFingerprintVersion:   edgetoken.FingerprintVersion,
			SigningAlgorithm:          edgeauth.Algorithm,
			SigningKeyID:              signingKey.KeyID,
			SigningPublicKey:          signingKey.PublicKeyB64,
			SigningKeyNotBefore:       signingKey.NotBefore,
			SigningKeyExpiresAt:       signingKey.ExpiresAt,
			CreatedAt:                 createdAt,
			ExpiresAt:                 runtime.ExpiresAt,
			UpdatedAt:                 createdAt,
		}
		if err := tx.Create(&snapshot).Error; err != nil {
			return err
		}
		for i := range datasets {
			dataset := model.EdgeCompiledSnapshotDataset{
				SnapshotID:   snapshot.ID,
				Dataset:      datasets[i].Dataset,
				Revision:     revision,
				ItemCount:    datasets[i].ItemCount,
				PageCount:    len(datasets[i].Pages),
				Digest:       datasets[i].Digest,
				Signature:    datasets[i].Signature,
				SigningKeyID: signingKey.KeyID,
			}
			if err := tx.Create(&dataset).Error; err != nil {
				return err
			}
			for pageIndex := range datasets[i].Pages {
				page := model.EdgeCompiledSnapshotPage{
					DatasetID: dataset.ID,
					Ordinal:   datasets[i].Pages[pageIndex].Ordinal,
					ItemCount: datasets[i].Pages[pageIndex].ItemCount,
					Digest:    datasets[i].Pages[pageIndex].Digest,
					Payload:   datasets[i].Pages[pageIndex].Payload,
				}
				if err := tx.Create(&page).Error; err != nil {
					return err
				}
			}
		}
		published, err := model.PublishEdgeCompiledSnapshotTx(tx, snapshot.ID, createdAt)
		if err != nil {
			return err
		}
		if !published {
			return errors.New("compiled edge snapshot was not published")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return snapshotUID, nil
}

func buildEdgeSnapshotDatasetContents(projection *edgeSnapshotProjection, pageLimit int) ([]edgeSnapshotDatasetBuild, error) {
	if projection == nil {
		return nil, errors.New("edge snapshot projection is nil")
	}
	if pageLimit <= 0 || pageLimit > dto.EdgeControlMaxSnapshotPageLimitV1 {
		return nil, errors.New("invalid edge snapshot page limit")
	}
	datasets := make([]edgeSnapshotDatasetBuild, 0, len(edgeSnapshotDatasetOrder))
	for _, dataset := range edgeSnapshotDatasetOrder {
		pages, itemCount, err := buildEdgeSnapshotPages(dataset, projection, pageLimit)
		if err != nil {
			return nil, err
		}
		pageDigests := make([]string, 0, len(pages))
		for i := range pages {
			pageDigests = append(pageDigests, pages[i].Digest)
		}
		digest, err := edgesnapshot.AggregatePageDigests(pageDigests)
		if err != nil {
			return nil, err
		}
		datasets = append(datasets, edgeSnapshotDatasetBuild{
			Dataset: dataset, ItemCount: itemCount, Digest: digest, Pages: pages,
		})
	}
	return datasets, nil
}

func signEdgeSnapshotDatasets(snapshotUID string, revision int64, contents []edgeSnapshotDatasetBuild, signingKey *SnapshotSigningKey) ([]edgeSnapshotDatasetBuild, string, error) {
	datasets := make([]edgeSnapshotDatasetBuild, len(contents))
	copy(datasets, contents)
	for i := range datasets {
		manifest := edgesnapshot.DatasetManifest{
			SnapshotID:    snapshotUID,
			Dataset:       string(datasets[i].Dataset),
			Revision:      revision,
			ItemCount:     datasets[i].ItemCount,
			PageCount:     len(datasets[i].Pages),
			PayloadDigest: datasets[i].Digest,
		}
		signature, err := edgesnapshot.SignDatasetManifest(signingKey.PrivateKey, manifest)
		if err != nil {
			return nil, "", err
		}
		datasets[i].Signature = signature
	}
	manifests := make([]edgesnapshot.DatasetManifest, 0, len(datasets))
	for i := range datasets {
		manifests = append(manifests, edgesnapshot.DatasetManifest{
			SnapshotID:    snapshotUID,
			Dataset:       string(datasets[i].Dataset),
			Revision:      revision,
			ItemCount:     datasets[i].ItemCount,
			PageCount:     len(datasets[i].Pages),
			PayloadDigest: datasets[i].Digest,
		})
	}
	topDigest, err := edgesnapshot.AggregateDatasetManifests(snapshotUID, revision, manifests)
	if err != nil {
		return nil, "", err
	}
	return datasets, topDigest, nil
}

func edgeSnapshotContentMatchesManifest(contents []edgeSnapshotDatasetBuild, manifest dto.EdgeSnapshotManifestV1) bool {
	if len(contents) != len(edgeSnapshotDatasetOrder) || len(manifest.Datasets) != len(edgeSnapshotDatasetOrder) {
		return false
	}
	for i, dataset := range edgeSnapshotDatasetOrder {
		if contents[i].Dataset != dataset || manifest.Datasets[i].Dataset != dataset {
			return false
		}
		if contents[i].ItemCount != manifest.Datasets[i].ItemCount ||
			len(contents[i].Pages) != manifest.Datasets[i].PageCount ||
			contents[i].Digest != manifest.Datasets[i].Digest {
			return false
		}
	}
	return true
}

func buildEdgeSnapshotPages(dataset dto.EdgeSnapshotDatasetV1, projection *edgeSnapshotProjection, pageLimit int) ([]edgeSnapshotPageBuild, int64, error) {
	itemCount := edgeSnapshotProjectionItemCount(dataset, projection)
	if itemCount == 0 {
		return nil, 0, nil
	}
	pages := make([]edgeSnapshotPageBuild, 0, (itemCount+pageLimit-1)/pageLimit)
	for start := 0; start < itemCount; start += pageLimit {
		end := start + pageLimit
		if end > itemCount {
			end = itemCount
		}
		payload := edgeSnapshotProjectionPage(dataset, projection, start, end)
		if err := payload.Validate(dataset, end-start); err != nil {
			return nil, 0, err
		}
		canonical, digest, err := edgesnapshot.MarshalPagePayload(payload)
		if err != nil {
			return nil, 0, err
		}
		pages = append(pages, edgeSnapshotPageBuild{
			Ordinal: len(pages), ItemCount: int64(end - start), Digest: digest, Payload: string(canonical),
		})
	}
	return pages, int64(itemCount), nil
}

func edgeSnapshotProjectionItemCount(dataset dto.EdgeSnapshotDatasetV1, projection *edgeSnapshotProjection) int {
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		return len(projection.Authentication)
	case dto.EdgeSnapshotDatasetUsersV1:
		return len(projection.Users)
	case dto.EdgeSnapshotDatasetGroupsV1:
		return len(projection.Groups)
	case dto.EdgeSnapshotDatasetModelsV1:
		return len(projection.Models)
	case dto.EdgeSnapshotDatasetChannelsV1:
		return len(projection.Channels)
	case dto.EdgeSnapshotDatasetPricingV1:
		return len(projection.Pricing)
	case dto.EdgeSnapshotDatasetRoutingV1:
		return len(projection.Routing)
	default:
		return 0
	}
}

func edgeSnapshotProjectionPage(dataset dto.EdgeSnapshotDatasetV1, projection *edgeSnapshotProjection, start int, end int) dto.EdgeSnapshotPagePayloadV1 {
	payload := dto.EdgeSnapshotPagePayloadV1{}
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		payload.Authentication = projection.Authentication[start:end]
	case dto.EdgeSnapshotDatasetUsersV1:
		payload.Users = projection.Users[start:end]
	case dto.EdgeSnapshotDatasetGroupsV1:
		payload.Groups = projection.Groups[start:end]
	case dto.EdgeSnapshotDatasetModelsV1:
		payload.Models = projection.Models[start:end]
	case dto.EdgeSnapshotDatasetChannelsV1:
		payload.Channels = projection.Channels[start:end]
	case dto.EdgeSnapshotDatasetPricingV1:
		payload.Pricing = projection.Pricing[start:end]
	case dto.EdgeSnapshotDatasetRoutingV1:
		payload.Routing = projection.Routing[start:end]
	}
	return payload
}

func sortedEdgeSnapshotKeys[T ~string](values map[T]struct{}) []T {
	keys := make([]T, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
