package setting

import (
	"strings"
	"sync"
)

// SensitivePolicy is one immutable snapshot of the prompt-sensitive filter.
// Callers must use the accessors below so master option reloads and edge
// snapshot installs cannot race with request processing.
type SensitivePolicy struct {
	CheckEnabled       bool
	CheckPromptEnabled bool
	StopOnSensitive    bool
	Words              []string
}

var sensitivePolicyMu sync.RWMutex
var sensitivePolicy = DefaultSensitivePolicy()

// StreamCacheQueueLength 流模式缓存队列长度，0表示无缓存
var StreamCacheQueueLength = 0

func DefaultSensitivePolicy() SensitivePolicy {
	return SensitivePolicy{
		CheckEnabled:       true,
		CheckPromptEnabled: true,
		StopOnSensitive:    true,
		Words:              []string{"test_sensitive"},
	}
}

func GetSensitivePolicy() SensitivePolicy {
	sensitivePolicyMu.RLock()
	defer sensitivePolicyMu.RUnlock()
	return cloneSensitivePolicy(sensitivePolicy)
}

func ApplySensitivePolicy(policy SensitivePolicy) {
	sensitivePolicyMu.Lock()
	sensitivePolicy = cloneSensitivePolicy(policy)
	sensitivePolicyMu.Unlock()
}

func SetCheckSensitiveEnabled(enabled bool) {
	sensitivePolicyMu.Lock()
	sensitivePolicy.CheckEnabled = enabled
	sensitivePolicyMu.Unlock()
}

func SetCheckSensitiveOnPromptEnabled(enabled bool) {
	sensitivePolicyMu.Lock()
	sensitivePolicy.CheckPromptEnabled = enabled
	sensitivePolicyMu.Unlock()
}

func SetStopOnSensitiveEnabled(enabled bool) {
	sensitivePolicyMu.Lock()
	sensitivePolicy.StopOnSensitive = enabled
	sensitivePolicyMu.Unlock()
}

func GetSensitiveWords() []string {
	sensitivePolicyMu.RLock()
	defer sensitivePolicyMu.RUnlock()
	return append([]string(nil), sensitivePolicy.Words...)
}

func SensitiveWordsToString() string {
	return strings.Join(GetSensitiveWords(), "\n")
}

func SensitiveWordsFromString(s string) {
	words := make([]string, 0)
	for _, word := range strings.Split(s, "\n") {
		word = strings.TrimSpace(word)
		if word != "" {
			words = append(words, word)
		}
	}
	sensitivePolicyMu.Lock()
	sensitivePolicy.Words = words
	sensitivePolicyMu.Unlock()
}

func ShouldCheckPromptSensitive() bool {
	sensitivePolicyMu.RLock()
	defer sensitivePolicyMu.RUnlock()
	return sensitivePolicy.CheckEnabled && sensitivePolicy.CheckPromptEnabled
}

func cloneSensitivePolicy(policy SensitivePolicy) SensitivePolicy {
	policy.Words = append([]string(nil), policy.Words...)
	return policy
}

//func ShouldCheckCompletionSensitive() bool {
//	return CheckSensitiveEnabled && CheckSensitiveOnCompletionEnabled
//}
