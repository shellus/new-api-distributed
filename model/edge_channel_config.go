package model

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"gopkg.in/yaml.v3"
)

const (
	edgeChannelConfigDirEnv     = "EDGE_CHANNEL_CONFIG_DIR"
	defaultEdgeChannelConfigDir = "/config/channels"
)

type edgeLocalChannelConfigFile struct {
	ID                 int               `yaml:"id"`
	Name               string            `yaml:"name"`
	Type               string            `yaml:"type"`
	Enabled            *bool             `yaml:"enabled"`
	Status             *int              `yaml:"status"`
	BaseURL            *string           `yaml:"base_url"`
	Auth               string            `yaml:"auth"`
	AuthData           any               `yaml:"auth_data"`
	AuthFiles          []string          `yaml:"auth_files"`
	OpenAIOrganization *string           `yaml:"openai_organization"`
	Other              any               `yaml:"other"`
	Models             []string          `yaml:"models"`
	Groups             []string          `yaml:"groups"`
	ModelMapping       map[string]string `yaml:"model_mapping"`
	ChannelSetting     map[string]any    `yaml:"channel_setting"`
	Settings           map[string]any    `yaml:"settings"`
	Priority           *int64            `yaml:"priority"`
	Weight             *uint             `yaml:"weight"`
	MultiKeyMode       string            `yaml:"multi_key_mode"`
	ParamOverride      map[string]any    `yaml:"param_override"`
	HeaderOverride     map[string]any    `yaml:"header_override"`
	Tag                string            `yaml:"tag"`
	TestModel          string            `yaml:"test_model"`
	Remark             string            `yaml:"remark"`
}

type edgeLocalChannelConfig struct {
	Name               string
	Type               int
	Enabled            bool
	BaseURL            string
	Auth               string
	OpenAIOrganization *string
	Other              string
	Models             map[string]struct{}
	Groups             map[string]struct{}
	ModelMapping       map[string]string
	ChannelSetting     map[string]any
	Settings           map[string]any
	Priority           *int64
	Weight             *uint
	MultiKeyMode       constant.MultiKeyMode
	ParamOverride      map[string]any
	HeaderOverride     map[string]any
}

func loadEdgeLocalChannelConfigs() (map[string]edgeLocalChannelConfig, error) {
	directory := strings.TrimSpace(os.Getenv(edgeChannelConfigDirEnv))
	if directory == "" {
		directory = defaultEdgeChannelConfigDir
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]edgeLocalChannelConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read edge channel config directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	configs := make(map[string]edgeLocalChannelConfig)
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		config, err := loadEdgeLocalChannelConfig(path, directory)
		if err != nil {
			return nil, err
		}
		if _, exists := configs[config.Name]; exists {
			return nil, fmt.Errorf("edge channel config contains duplicate name %q", config.Name)
		}
		configs[config.Name] = config
	}
	return configs, nil
}

// ValidateEdgeLocalChannelConfigs parses every channel YAML under the edge
// channel config directory and returns the canonical channel names sorted
// ascending. It is the CLI validation entry used before restarting an edge,
// so a broken file is reported here instead of costing data-plane readiness.
func ValidateEdgeLocalChannelConfigs() ([]string, error) {
	configs, err := loadEdgeLocalChannelConfigs()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func loadEdgeLocalChannelConfig(path string, directory string) (edgeLocalChannelConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return edgeLocalChannelConfig{}, err
	}
	var file edgeLocalChannelConfigFile
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return edgeLocalChannelConfig{}, fmt.Errorf("parse edge channel config %s: %w", filepath.Base(path), err)
	}
	file.Name = strings.TrimSpace(file.Name)
	if file.Name == "" {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s has no name", filepath.Base(path))
	}
	localService := strings.ToLower(file.Name)
	if file.Name != localService {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s name must use lowercase canonical form", filepath.Base(path))
	}
	channelType, err := parseEdgeLocalChannelType(file.Type)
	if err != nil {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s: %w", filepath.Base(path), err)
	}
	auth, err := resolveEdgeLocalChannelAuth(file, directory)
	if err != nil {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s: %w", filepath.Base(path), err)
	}
	enabled := true
	if file.Status != nil && *file.Status != common.ChannelStatusEnabled {
		enabled = false
	}
	if file.Enabled != nil {
		enabled = *file.Enabled
	}
	baseURL := ""
	if file.BaseURL != nil {
		baseURL = strings.TrimSpace(*file.BaseURL)
	}
	if baseURL == "" && channelType > constant.ChannelTypeUnknown && channelType < len(constant.ChannelBaseURLs) {
		baseURL = constant.ChannelBaseURLs[channelType]
	}
	if baseURL != "" {
		baseURL, err = normalizeEdgeLocalChannelBaseURL(baseURL)
		if err != nil {
			return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s: %w", filepath.Base(path), err)
		}
	}
	other, err := marshalEdgeLocalChannelValue(file.Other)
	if err != nil {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s other: %w", filepath.Base(path), err)
	}
	multiKeyMode, err := parseEdgeLocalMultiKeyMode(file.MultiKeyMode, auth)
	if err != nil {
		return edgeLocalChannelConfig{}, fmt.Errorf("edge channel config %s: %w", filepath.Base(path), err)
	}
	return edgeLocalChannelConfig{
		Name: file.Name, Type: channelType, Enabled: enabled, BaseURL: baseURL, Auth: auth,
		OpenAIOrganization: file.OpenAIOrganization, Other: other,
		Models: stringSet(file.Models), Groups: stringSet(file.Groups), ModelMapping: cloneStringMap(file.ModelMapping),
		ChannelSetting: cloneAnyMap(file.ChannelSetting), Settings: cloneAnyMap(file.Settings),
		Priority: file.Priority, Weight: file.Weight, MultiKeyMode: multiKeyMode,
		ParamOverride: cloneAnyMap(file.ParamOverride), HeaderOverride: cloneAnyMap(file.HeaderOverride),
	}, nil
}

func resolveEdgeLocalChannelAuth(file edgeLocalChannelConfigFile, directory string) (string, error) {
	variants := 0
	if strings.TrimSpace(file.Auth) != "" {
		variants++
	}
	if file.AuthData != nil {
		variants++
	}
	if len(file.AuthFiles) > 0 {
		variants++
	}
	if variants > 1 {
		return "", errors.New("auth, auth_data and auth_files are mutually exclusive")
	}
	if strings.TrimSpace(file.Auth) != "" {
		return strings.Trim(file.Auth, "\n"), nil
	}
	if file.AuthData != nil {
		return marshalEdgeLocalChannelValue(file.AuthData)
	}
	if len(file.AuthFiles) == 0 {
		return "", nil
	}
	credentials := make([]any, 0, len(file.AuthFiles))
	for _, relative := range file.AuthFiles {
		path, err := resolveEdgeLocalChannelReferencedFile(directory, relative)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		var credential any
		if err := yaml.Unmarshal(content, &credential); err != nil {
			return "", fmt.Errorf("parse credential file %s: %w", relative, err)
		}
		credentials = append(credentials, credential)
	}
	return marshalEdgeLocalChannelValue(credentials)
}

func resolveEdgeLocalChannelReferencedFile(directory string, relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("credential file path %q must be relative", relative)
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("credential file path %q escapes the channel directory", relative)
	}
	return filepath.Join(directory, clean), nil
}

func parseEdgeLocalChannelType(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return constant.ChannelTypeUnknown, nil
	}
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(value))
	switch normalized {
	case "openai":
		return constant.ChannelTypeOpenAI, nil
	case "anthropic", "claude":
		return constant.ChannelTypeAnthropic, nil
	case "vertexai", "vertex":
		return constant.ChannelTypeVertexAi, nil
	case "mistral":
		return constant.ChannelTypeMistral, nil
	case "codex":
		return constant.ChannelTypeCodex, nil
	case "custom":
		return constant.ChannelTypeCustom, nil
	case "deepseek":
		return constant.ChannelTypeDeepSeek, nil
	}
	for channelType, name := range constant.ChannelTypeNames {
		candidate := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(name))
		if normalized == candidate {
			return channelType, nil
		}
	}
	return 0, fmt.Errorf("unsupported channel type %q", value)
}

func parseEdgeLocalMultiKeyMode(value string, auth string) (constant.MultiKeyMode, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	keyCount := len(strings.Split(strings.Trim(auth, "\n"), "\n"))
	if strings.TrimSpace(auth) == "" {
		keyCount = 0
	}
	switch value {
	case "", "single":
		if keyCount > 1 {
			return constant.MultiKeyModeRandom, nil
		}
		return "", nil
	case string(constant.MultiKeyModeRandom):
		return constant.MultiKeyModeRandom, nil
	case string(constant.MultiKeyModePolling):
		return constant.MultiKeyModePolling, nil
	default:
		return "", fmt.Errorf("invalid multi_key_mode %q", value)
	}
}

func normalizeEdgeLocalChannelBaseURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("base_url must be an absolute HTTP URL without credentials")
	}
	if (parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() == "0" {
		return "", errors.New("base_url must use a root path without query, fragment, or port zero")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String(), nil
}

func marshalEdgeLocalChannelValue(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	payload, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneAnyMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
