package common

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

const (
	defaultSystemPromptRandomPrefixLength = 16
	maxSystemPromptRandomPrefixLength     = 64
)

// BuildChannelSystemPrompt returns the effective channel-level system prompt.
// When enabled, a per-request cache-bypass marker is prepended before the fixed prompt.
func BuildChannelSystemPrompt(setting dto.ChannelSettings) string {
	systemPrompt := setting.SystemPrompt
	if !setting.SystemPromptRandomPrefixEnabled {
		return systemPrompt
	}

	prefix := fmt.Sprintf("[cache-bypass:%s]", generateSystemPromptRandomToken(setting.SystemPromptRandomPrefixLength))
	if strings.TrimSpace(systemPrompt) == "" {
		return prefix
	}
	return prefix + "\n" + systemPrompt
}

func ApplyChannelSystemPromptToInstructions(setting dto.ChannelSettings, instructions json.RawMessage) (json.RawMessage, bool, error) {
	systemPrompt := BuildChannelSystemPrompt(setting)
	if systemPrompt == "" {
		return instructions, false, nil
	}

	if len(instructions) == 0 {
		updated, err := appcommon.Marshal(systemPrompt)
		return updated, true, err
	}

	if !setting.SystemPromptOverride {
		return instructions, false, nil
	}

	var existing string
	if err := appcommon.Unmarshal(instructions, &existing); err == nil {
		existing = strings.TrimSpace(existing)
		if existing != "" {
			systemPrompt = systemPrompt + "\n" + existing
		}
	}

	updated, err := appcommon.Marshal(systemPrompt)
	return updated, true, err
}

func generateSystemPromptRandomToken(length int) string {
	length = normalizeSystemPromptRandomPrefixLength(length)
	tokenBytes := make([]byte, (length+1)/2)
	if _, err := cryptorand.Read(tokenBytes); err != nil {
		fallback := fmt.Sprintf("%x", time.Now().UnixNano())
		if len(fallback) >= length {
			return fallback[:length]
		}
		return fallback
	}
	return hex.EncodeToString(tokenBytes)[:length]
}

func normalizeSystemPromptRandomPrefixLength(length int) int {
	if length <= 0 {
		return defaultSystemPromptRandomPrefixLength
	}
	if length > maxSystemPromptRandomPrefixLength {
		return maxSystemPromptRandomPrefixLength
	}
	return length
}
