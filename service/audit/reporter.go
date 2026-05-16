package audit

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	envEndpointURL  = "AUDIT_ENDPOINT_URL"
	envAPIKey       = "AUDIT_API_KEY"
	envTimeout      = "AUDIT_TIMEOUT_SECONDS"
	envMaxBodyBytes = "AUDIT_MAX_BODY_BYTES"
	defaultTimeout  = 3 * time.Second
	defaultMaxBody  = int64(0)
)

type Reporter struct {
	config Config
	client *http.Client
}

var defaultReporter atomic.Value

func LoadConfigFromEnv() Config {
	timeout := defaultTimeout
	if raw := strings.TrimSpace(os.Getenv(envTimeout)); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}

	maxBodyBytes := defaultMaxBody
	if raw := strings.TrimSpace(os.Getenv(envMaxBodyBytes)); raw != "" {
		if bytesLimit, err := strconv.ParseInt(raw, 10, 64); err == nil && bytesLimit >= 0 {
			maxBodyBytes = bytesLimit
		}
	}

	return Config{
		EndpointURL:  strings.TrimSpace(os.Getenv(envEndpointURL)),
		APIKey:       strings.TrimSpace(os.Getenv(envAPIKey)),
		Timeout:      timeout,
		MaxBodyBytes: maxBodyBytes,
		NodeName:     strings.TrimSpace(os.Getenv("NODE_NAME")),
	}
}

func InitFromEnv() {
	config := LoadConfigFromEnv()
	if !config.Enabled() {
		defaultReporter.Store((*Reporter)(nil))
		return
	}
	defaultReporter.Store(NewReporter(config, nil))
	common.SysLog("audit reporter enabled")
}

func DefaultReporter() *Reporter {
	if reporter, ok := defaultReporter.Load().(*Reporter); ok {
		return reporter
	}
	return nil
}

func Enabled() bool {
	reporter := DefaultReporter()
	return reporter != nil && reporter.config.Enabled()
}

func MaxBodyBytes() int64 {
	reporter := DefaultReporter()
	if reporter == nil {
		return defaultMaxBody
	}
	return reporter.config.MaxBodyBytes
}

func NodeName() string {
	reporter := DefaultReporter()
	if reporter == nil {
		return ""
	}
	return reporter.config.NodeName
}

func NewReporter(config Config, client *http.Client) *Reporter {
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.MaxBodyBytes < 0 {
		config.MaxBodyBytes = 0
	}
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	return &Reporter{
		config: config,
		client: client,
	}
}

func (r *Reporter) Report(event Event) error {
	if r == nil || !r.config.Enabled() {
		return nil
	}
	if event.Version == "" {
		event.Version = ProtocolVersion
	}
	if event.Event == "" {
		event.Event = EventRequestResponse
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Node == "" {
		event.Node = r.config.NodeName
	}

	payload, err := common.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.config.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.config.EndpointURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create audit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.config.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("post audit event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("audit endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

func ReportAsync(event Event) {
	reporter := DefaultReporter()
	if reporter == nil || !reporter.config.Enabled() {
		return
	}
	go func() {
		if err := reporter.Report(event); err != nil {
			common.SysError("audit report failed: " + err.Error())
		}
	}()
}
