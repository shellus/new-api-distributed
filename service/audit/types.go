package audit

import "time"

const (
	ProtocolVersion      = "new-api.audit.v1"
	EventRequestResponse = "request_response"
)

type Config struct {
	EndpointURL  string
	APIKey       string
	Timeout      time.Duration
	MaxBodyBytes int64
	NodeName     string
}

func (c Config) Enabled() bool {
	return c.EndpointURL != "" && c.APIKey != ""
}

type Event struct {
	Version    string            `json:"version"`
	Event      string            `json:"event"`
	RequestID  string            `json:"request_id,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
	Node       string            `json:"node,omitempty"`
	Route      string            `json:"route,omitempty"`
	User       UserInfo          `json:"user"`
	Key        KeyInfo           `json:"key"`
	Client     ClientInfo        `json:"client"`
	Model      ModelInfo         `json:"model,omitempty"`
	Billing    BillingInfo       `json:"billing,omitempty"`
	Request    Body              `json:"request"`
	Response   Body              `json:"response"`
	DurationMS int64             `json:"duration_ms"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type UserInfo struct {
	ID       int    `json:"id,omitempty"`
	Username string `json:"username,omitempty"`
}

type KeyInfo struct {
	ID   int    `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type ClientInfo struct {
	IP        string `json:"ip,omitempty"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

type Body struct {
	ContentType string `json:"content_type,omitempty"`
	Content     string `json:"content"`
	SizeBytes   int64  `json:"size_bytes"`
	Truncated   bool   `json:"truncated"`
	StatusCode  int    `json:"status_code,omitempty"`
}

type ModelInfo struct {
	Name         string `json:"name,omitempty"`
	OriginName   string `json:"origin_name,omitempty"`
	UpstreamName string `json:"upstream_name,omitempty"`
}

type BillingInfo struct {
	Quota                 int                `json:"quota,omitempty"`
	PromptTokens          int                `json:"prompt_tokens,omitempty"`
	CompletionTokens      int                `json:"completion_tokens,omitempty"`
	TotalTokens           int                `json:"total_tokens,omitempty"`
	CacheTokens           int                `json:"cache_tokens,omitempty"`
	CacheCreationTokens   int                `json:"cache_creation_tokens,omitempty"`
	CacheCreationTokens5m int                `json:"cache_creation_tokens_5m,omitempty"`
	CacheCreationTokens1h int                `json:"cache_creation_tokens_1h,omitempty"`
	ImageTokens           int                `json:"image_tokens,omitempty"`
	AudioTokens           int                `json:"audio_tokens,omitempty"`
	UseTimeSeconds        int64              `json:"use_time_seconds,omitempty"`
	ModelRatio            float64            `json:"model_ratio,omitempty"`
	GroupRatio            float64            `json:"group_ratio,omitempty"`
	CompletionRatio       float64            `json:"completion_ratio,omitempty"`
	CacheRatio            float64            `json:"cache_ratio,omitempty"`
	CacheCreationRatio    float64            `json:"cache_creation_ratio,omitempty"`
	CacheCreationRatio5m  float64            `json:"cache_creation_ratio_5m,omitempty"`
	CacheCreationRatio1h  float64            `json:"cache_creation_ratio_1h,omitempty"`
	ImageRatio            float64            `json:"image_ratio,omitempty"`
	ModelPrice            float64            `json:"model_price,omitempty"`
	UsePrice              bool               `json:"use_price,omitempty"`
	FreeModel             bool               `json:"free_model,omitempty"`
	QuotaToPreConsume     int                `json:"quota_to_pre_consume,omitempty"`
	FinalPreConsumedQuota int                `json:"final_pre_consumed_quota,omitempty"`
	QuotaPerUnit          float64            `json:"quota_per_unit,omitempty"`
	UsageSemantic         string             `json:"usage_semantic,omitempty"`
	OtherRatios           map[string]float64 `json:"other_ratios,omitempty"`
	Details               map[string]any     `json:"details,omitempty"`
}
