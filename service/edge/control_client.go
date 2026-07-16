package edge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/google/uuid"
)

const (
	edgeMasterURLEnv                = "EDGE_MASTER_URL"
	edgeNodeIDEnv                   = "EDGE_NODE_ID"
	edgeNodeGenerationEnv           = "EDGE_NODE_GENERATION"
	edgeCredentialKeyIDEnv          = "EDGE_CREDENTIAL_KEY_ID"
	edgeCredentialPrivateKeyEnv     = "EDGE_CREDENTIAL_PRIVATE_KEY"
	edgePublicURLEnv                = "EDGE_PUBLIC_URL"
	edgeNodeNameEnv                 = "EDGE_NODE_NAME"
	edgeRegionEnv                   = "EDGE_REGION"
	edgeControlRequestTimeoutEnv    = "EDGE_CONTROL_REQUEST_TIMEOUT_SECONDS"
	edgeControlMaxResponseBytesEnv  = "EDGE_CONTROL_MAX_RESPONSE_BYTES"
	defaultEdgeControlTimeout       = 15 * time.Second
	defaultEdgeControlResponseBytes = int64(16 << 20)
	maximumEdgeControlResponseBytes = int64(64 << 20)
)

var (
	ErrEdgeControlNodeDisabled      = errors.New("edge control node is disabled")
	ErrEdgeControlProtocolViolation = errors.New("edge control protocol violation")
)

type EdgeControlClientConfig struct {
	MasterURL        string
	NodeID           string
	NodeGeneration   int64
	CredentialKeyID  string
	CredentialKey    ed25519.PrivateKey
	Declaration      dto.EdgeNodeDeclarationV1
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
}

type EdgeControlClient struct {
	masterURL        *url.URL
	nodeID           string
	nodeGeneration   int64
	credentialKeyID  string
	credentialKey    ed25519.PrivateKey
	declaration      dto.EdgeNodeDeclarationV1
	httpClient       *http.Client
	requestTimeout   time.Duration
	maxResponseBytes int64
	now              func() time.Time
}

type EdgeControlRemoteError struct {
	StatusCode int
	Response   dto.EdgeControlErrorResponseV1
}

func (e *EdgeControlRemoteError) Error() string {
	if e == nil {
		return "edge control remote error"
	}
	return fmt.Sprintf("edge control request failed with HTTP %d: %s: %s", e.StatusCode, e.Response.Error.Code, e.Response.Error.Message)
}

func (e *EdgeControlRemoteError) Retryable() bool {
	return e != nil && e.Response.Error.Retryable
}

func LoadEdgeControlClientConfigFromEnv(startedAt time.Time) (EdgeControlClientConfig, error) {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	generationText := strings.TrimSpace(os.Getenv(edgeNodeGenerationEnv))
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != generationText {
		return EdgeControlClientConfig{}, fmt.Errorf("%s must be a positive canonical integer", edgeNodeGenerationEnv)
	}
	privateKey, err := edgeauth.ParsePrivateKey(strings.TrimSpace(os.Getenv(edgeCredentialPrivateKeyEnv)))
	if err != nil {
		return EdgeControlClientConfig{}, fmt.Errorf("%s is invalid: %w", edgeCredentialPrivateKeyEnv, err)
	}
	requestTimeoutSeconds := int64(defaultEdgeControlTimeout / time.Second)
	if value := strings.TrimSpace(os.Getenv(edgeControlRequestTimeoutEnv)); value != "" {
		requestTimeoutSeconds, err = strconv.ParseInt(value, 10, 64)
		if err != nil || strconv.FormatInt(requestTimeoutSeconds, 10) != value {
			return EdgeControlClientConfig{}, fmt.Errorf("%s must be a canonical integer", edgeControlRequestTimeoutEnv)
		}
	}
	if requestTimeoutSeconds <= 0 || requestTimeoutSeconds > 300 {
		return EdgeControlClientConfig{}, fmt.Errorf("%s must be between 1 and 300", edgeControlRequestTimeoutEnv)
	}
	maxResponseBytes, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(edgeControlMaxResponseBytesEnv)), 10, 64)
	if strings.TrimSpace(os.Getenv(edgeControlMaxResponseBytesEnv)) == "" {
		maxResponseBytes = defaultEdgeControlResponseBytes
		err = nil
	}
	if err != nil || maxResponseBytes <= 0 || maxResponseBytes > maximumEdgeControlResponseBytes {
		return EdgeControlClientConfig{}, fmt.Errorf("%s must be between 1 and %d", edgeControlMaxResponseBytesEnv, maximumEdgeControlResponseBytes)
	}
	declaration := dto.EdgeNodeDeclarationV1{
		Name:               strings.TrimSpace(os.Getenv(edgeNodeNameEnv)),
		Region:             strings.TrimSpace(os.Getenv(edgeRegionEnv)),
		PublicURL:          strings.TrimSpace(os.Getenv(edgePublicURLEnv)),
		SoftwareVersion:    common.Version,
		StartedAtUnixMilli: startedAt.UTC().UnixMilli(),
		Capabilities: []dto.EdgeEndpointCapabilityV1{
			{Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Streaming: true},
			{Endpoint: dto.EdgeEndpointOpenAIResponsesV1, Streaming: true},
		},
	}
	return EdgeControlClientConfig{
		MasterURL:        strings.TrimSpace(os.Getenv(edgeMasterURLEnv)),
		NodeID:           strings.TrimSpace(os.Getenv(edgeNodeIDEnv)),
		NodeGeneration:   generation,
		CredentialKeyID:  strings.TrimSpace(os.Getenv(edgeCredentialKeyIDEnv)),
		CredentialKey:    privateKey,
		Declaration:      declaration,
		RequestTimeout:   time.Duration(requestTimeoutSeconds) * time.Second,
		MaxResponseBytes: maxResponseBytes,
	}, nil
}

func NewEdgeControlClient(config EdgeControlClientConfig) (*EdgeControlClient, error) {
	masterURL, err := parseStrictEdgeControlURL(config.MasterURL, true)
	if err != nil {
		return nil, fmt.Errorf("master URL: %w", err)
	}
	publicURL, err := parseStrictEdgeControlURL(config.Declaration.PublicURL, false)
	if err != nil {
		return nil, fmt.Errorf("public URL: %w", err)
	}
	config.Declaration.PublicURL = publicURL.String()
	if err := edgeauth.ValidateNodeID(config.NodeID); err != nil {
		return nil, err
	}
	if config.NodeGeneration <= 0 {
		return nil, errors.New("edge node generation must be positive")
	}
	if err := edgeauth.ValidateKeyID(config.CredentialKeyID); err != nil {
		return nil, err
	}
	encodedPrivateKey, err := edgeauth.EncodePrivateKey(config.CredentialKey)
	if err != nil {
		return nil, err
	}
	privateKey, err := edgeauth.ParsePrivateKey(encodedPrivateKey)
	if err != nil {
		return nil, err
	}
	if err := config.Declaration.Validate(); err != nil {
		return nil, err
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultEdgeControlTimeout
	}
	if requestTimeout < time.Second || requestTimeout > 5*time.Minute {
		return nil, errors.New("edge control request timeout must be between one second and five minutes")
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultEdgeControlResponseBytes
	}
	if maxResponseBytes <= 0 || maxResponseBytes > maximumEdgeControlResponseBytes {
		return nil, errors.New("edge control response limit is invalid")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &EdgeControlClient{
		masterURL: masterURL, nodeID: config.NodeID, nodeGeneration: config.NodeGeneration,
		credentialKeyID: config.CredentialKeyID, credentialKey: privateKey,
		declaration: config.Declaration, httpClient: &clientCopy,
		requestTimeout: requestTimeout, maxResponseBytes: maxResponseBytes, now: now,
	}, nil
}

func parseStrictEdgeControlURL(value string, requireRootPath bool) (*url.URL, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("URL is empty or contains surrounding/control characters")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errors.New("URL must be an absolute hierarchical URL without userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, errors.New("URL must not contain a query or fragment")
	}
	if requireRootPath && parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
		return nil, errors.New("master URL path must be empty or root")
	}
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !edgeControlLoopbackHost(parsed.Hostname()) {
			return nil, errors.New("URL must use HTTPS; HTTP is allowed only for loopback testing")
		}
	}
	if (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Port() == "0" {
		return nil, errors.New("URL port must not be zero")
	}
	if requireRootPath {
		parsed.Path = ""
		parsed.RawPath = ""
	}
	return parsed, nil
}

func edgeControlLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *EdgeControlClient) Declaration() dto.EdgeNodeDeclarationV1 {
	declaration := c.declaration
	declaration.Capabilities = append([]dto.EdgeEndpointCapabilityV1(nil), c.declaration.Capabilities...)
	return declaration
}

func (c *EdgeControlClient) NewRequestMeta(kind string) (dto.EdgeControlRequestMetaV1, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, "_", "-")
	requestID := kind + "-" + uuid.NewString()
	if err := edgeauth.ValidateIdempotencyKey(requestID); err != nil {
		return dto.EdgeControlRequestMetaV1{}, err
	}
	return dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: requestID}, nil
}

func (c *EdgeControlClient) prepareMeta(meta dto.EdgeControlRequestMetaV1, kind string) (dto.EdgeControlRequestMetaV1, error) {
	if meta.ProtocolVersion == "" {
		meta.ProtocolVersion = dto.EdgeControlProtocolVersionV1
	}
	if meta.RequestID == "" {
		generated, err := c.NewRequestMeta(kind)
		if err != nil {
			return dto.EdgeControlRequestMetaV1{}, err
		}
		meta.RequestID = generated.RequestID
	}
	if err := meta.Validate(); err != nil {
		return dto.EdgeControlRequestMetaV1{}, err
	}
	return meta, nil
}

func (c *EdgeControlClient) Bootstrap(ctx context.Context, request dto.EdgeBootstrapRequestV1) (*dto.EdgeBootstrapResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "bootstrap")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeBootstrapResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/bootstrap", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if err := c.validateControl(response.Control); err != nil {
		return nil, err
	}
	if err := response.Snapshot.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("bootstrap snapshot", err)
	}
	if response.SettlementAck != nil {
		if err := c.validateSettlementAck(*response.SettlementAck); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (c *EdgeControlClient) Heartbeat(ctx context.Context, request dto.EdgeHeartbeatRequestV1) (*dto.EdgeHeartbeatResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "heartbeat")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeHeartbeatResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/heartbeat", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if err := c.validateControl(response.Control); err != nil {
		return nil, err
	}
	if response.Snapshot != nil {
		if err := response.Snapshot.Validate(); err != nil {
			return nil, edgeControlInvalidResponse("heartbeat snapshot", err)
		}
	}
	if response.SettlementAck != nil {
		if err := c.validateSettlementAck(*response.SettlementAck); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (c *EdgeControlClient) SnapshotManifest(ctx context.Context, request dto.EdgeSnapshotManifestRequestV1) (*dto.EdgeSnapshotManifestResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "snapshot-manifest")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeSnapshotManifestResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/snapshot/manifest", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("snapshot manifest response", err)
	}
	return response, nil
}

func (c *EdgeControlClient) SnapshotPage(ctx context.Context, request dto.EdgeSnapshotPageRequestV1) (*dto.EdgeSnapshotPageResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "snapshot-page")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeSnapshotPageResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/snapshot/page", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("snapshot page response", err)
	}
	return response, nil
}

func (c *EdgeControlClient) AcquireLease(ctx context.Context, request dto.EdgeLeaseAcquireRequestV1) (*dto.EdgeLeaseAcquireResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "lease-acquire")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeLeaseAcquireResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/lease/acquire", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("lease acquire response", err)
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if response.Lease.NodeID != c.nodeID || response.Lease.NodeGeneration != c.nodeGeneration {
		return nil, fmt.Errorf("%w: lease belongs to another node generation", ErrEdgeControlProtocolViolation)
	}
	if response.Lease.Subject != request.Subject || response.Lease.SnapshotID != request.SnapshotID || response.Lease.SnapshotRevision != request.SnapshotRevision {
		return nil, fmt.Errorf("%w: lease does not match the requested subject and snapshot", ErrEdgeControlProtocolViolation)
	}
	return response, nil
}

func (c *EdgeControlClient) CloseLease(ctx context.Context, request dto.EdgeLeaseCloseRequestV1) (*dto.EdgeLeaseCloseResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "lease-close")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeLeaseCloseResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/lease/close", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("lease close response", err)
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if response.LeaseID != request.LeaseID || response.LeaseVersion < request.LeaseVersion {
		return nil, fmt.Errorf("%w: lease close response does not match the request", ErrEdgeControlProtocolViolation)
	}
	return response, nil
}

func (c *EdgeControlClient) SubmitSettlement(ctx context.Context, request dto.EdgeSettlementBlockRequestV1) (*dto.EdgeSettlementBlockResponseV1, error) {
	meta, err := c.prepareMeta(request.Meta, "settlement")
	if err != nil {
		return nil, err
	}
	request.Meta = meta
	if err := request.Validate(); err != nil {
		return nil, err
	}
	response := &dto.EdgeSettlementBlockResponseV1{}
	if err := c.doJSON(ctx, "/control/v1/settlement/block", request.Meta.RequestID, request, response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, edgeControlInvalidResponse("settlement response", err)
	}
	if err := c.validateResponseMeta(response.Meta, request.Meta.RequestID); err != nil {
		return nil, err
	}
	if err := c.validateSettlementAck(response.Ack); err != nil {
		return nil, err
	}
	if response.Ack.BlockID != request.BlockID || response.Ack.AckedThroughSequence < request.LastSequence {
		return nil, fmt.Errorf("%w: settlement acknowledgement does not cover the submitted block", ErrEdgeControlProtocolViolation)
	}
	return response, nil
}

func (c *EdgeControlClient) validateControl(control dto.EdgeNodeControlConfigV1) error {
	if err := control.Validate(); err != nil {
		return edgeControlInvalidResponse("control configuration", err)
	}
	if control.NodeID != c.nodeID || control.NodeGeneration != c.nodeGeneration {
		return fmt.Errorf("%w: control response belongs to another node generation", ErrEdgeControlProtocolViolation)
	}
	if !control.Enabled {
		return ErrEdgeControlNodeDisabled
	}
	return nil
}

func (c *EdgeControlClient) validateSettlementAck(ack dto.EdgeSettlementAckV1) error {
	if err := ack.Validate(); err != nil {
		return edgeControlInvalidResponse("settlement acknowledgement", err)
	}
	if ack.NodeID != c.nodeID || ack.NodeGeneration != c.nodeGeneration {
		return fmt.Errorf("%w: settlement acknowledgement belongs to another node generation", ErrEdgeControlProtocolViolation)
	}
	return nil
}

func (c *EdgeControlClient) validateResponseMeta(meta dto.EdgeControlResponseMetaV1, requestID string) error {
	if err := meta.Validate(); err != nil {
		return edgeControlInvalidResponse("response metadata", err)
	}
	if meta.RequestID != requestID {
		return fmt.Errorf("%w: response request_id does not match request", ErrEdgeControlProtocolViolation)
	}
	return nil
}

func edgeControlInvalidResponse(component string, err error) error {
	return fmt.Errorf("%w: invalid %s: %v", ErrEdgeControlProtocolViolation, component, err)
}

func (c *EdgeControlClient) doJSON(ctx context.Context, path string, requestID string, request any, response any) error {
	body, err := common.Marshal(request)
	if err != nil {
		return err
	}
	target := *c.masterURL
	target.Path = path
	target.RawPath = ""
	target.RawQuery = ""
	requestContext, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Accept-Encoding", "identity")
	nonce, err := edgeauth.NewNonce()
	if err != nil {
		return err
	}
	metadata := edgeauth.Metadata{
		Version: edgeauth.VersionV1, NodeID: c.nodeID, Generation: c.nodeGeneration,
		KeyID: c.credentialKeyID, TimestampUnixSeconds: c.now().UTC().Unix(), Nonce: nonce,
		IdempotencyKey: requestID,
	}
	if err := edgeauth.SignHTTPRequest(httpRequest, body, c.credentialKey, metadata); err != nil {
		return err
	}
	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()
	if encoding := strings.TrimSpace(httpResponse.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("%w: unsupported response content encoding %q", ErrEdgeControlProtocolViolation, encoding)
	}
	limited := io.LimitReader(httpResponse.Body, c.maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(responseBody)) > c.maxResponseBytes {
		return fmt.Errorf("%w: response exceeds %d bytes", ErrEdgeControlProtocolViolation, c.maxResponseBytes)
	}
	contentType := strings.TrimSpace(httpResponse.Header.Get("Content-Type"))
	mediaType, _, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil || !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("%w: response content type must be application/json", ErrEdgeControlProtocolViolation)
	}
	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		var remote dto.EdgeControlErrorResponseV1
		if err := common.DecodeJsonStrict(bytes.NewReader(responseBody), &remote); err != nil {
			return fmt.Errorf("%w: decode structured error: %v", ErrEdgeControlProtocolViolation, err)
		}
		if err := c.validateResponseMeta(remote.Meta, requestID); err != nil {
			return err
		}
		if remote.Error.Code == "" || strings.TrimSpace(remote.Error.Message) == "" {
			return fmt.Errorf("%w: structured error is incomplete", ErrEdgeControlProtocolViolation)
		}
		return &EdgeControlRemoteError{StatusCode: httpResponse.StatusCode, Response: remote}
	}
	if err := common.DecodeJsonStrict(bytes.NewReader(responseBody), response); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrEdgeControlProtocolViolation, err)
	}
	return nil
}
