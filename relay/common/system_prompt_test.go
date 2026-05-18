package common

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	appcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func TestBuildChannelSystemPromptRandomPrefixChangesEachCall(t *testing.T) {
	setting := dto.ChannelSettings{
		SystemPrompt:                    "keep existing channel prompt",
		SystemPromptRandomPrefixEnabled: true,
		SystemPromptRandomPrefixLength:  16,
	}

	first := BuildChannelSystemPrompt(setting)
	second := BuildChannelSystemPrompt(setting)

	pattern := regexp.MustCompile(`^\[cache-bypass:[0-9a-f]{16}\]\nkeep existing channel prompt$`)
	if !pattern.MatchString(first) {
		t.Fatalf("expected first prompt to include random cache-bypass prefix, got %q", first)
	}
	if !pattern.MatchString(second) {
		t.Fatalf("expected second prompt to include random cache-bypass prefix, got %q", second)
	}
	if first == second {
		t.Fatalf("expected random prefix to change between calls, got %q", first)
	}
}

func TestBuildChannelSystemPromptRandomPrefixCanStandAlone(t *testing.T) {
	setting := dto.ChannelSettings{
		SystemPromptRandomPrefixEnabled: true,
		SystemPromptRandomPrefixLength:  12,
	}

	prompt := BuildChannelSystemPrompt(setting)

	pattern := regexp.MustCompile(`^\[cache-bypass:[0-9a-f]{12}\]$`)
	if !pattern.MatchString(prompt) {
		t.Fatalf("expected standalone random cache-bypass prefix, got %q", prompt)
	}
}

func TestBuildChannelSystemPromptDisabledKeepsExistingBehavior(t *testing.T) {
	setting := dto.ChannelSettings{
		SystemPrompt: "fixed prompt",
	}

	prompt := BuildChannelSystemPrompt(setting)

	if prompt != "fixed prompt" {
		t.Fatalf("expected fixed prompt to remain unchanged, got %q", prompt)
	}
	if strings.Contains(prompt, "cache-bypass") {
		t.Fatalf("expected no random prefix when disabled, got %q", prompt)
	}
}

func TestApplyChannelSystemPromptToInstructionsPrependsRandomPrefix(t *testing.T) {
	setting := dto.ChannelSettings{
		SystemPromptOverride:            true,
		SystemPromptRandomPrefixEnabled: true,
		SystemPromptRandomPrefixLength:  10,
	}

	updated, changed, err := ApplyChannelSystemPromptToInstructions(setting, json.RawMessage(`"existing instructions"`))
	if err != nil {
		t.Fatalf("ApplyChannelSystemPromptToInstructions returned error: %v", err)
	}
	if !changed {
		t.Fatalf("expected instructions to be changed")
	}

	var prompt string
	if err := appcommon.Unmarshal(updated, &prompt); err != nil {
		t.Fatalf("failed to unmarshal updated instructions: %v", err)
	}
	pattern := regexp.MustCompile(`^\[cache-bypass:[0-9a-f]{10}\]\nexisting instructions$`)
	if !pattern.MatchString(prompt) {
		t.Fatalf("expected random prefix before existing instructions, got %q", prompt)
	}
}
