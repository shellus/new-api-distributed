package setting

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSensitivePolicyAccessorsUseIndependentSnapshots(t *testing.T) {
	previous := GetSensitivePolicy()
	t.Cleanup(func() { ApplySensitivePolicy(previous) })

	input := SensitivePolicy{
		CheckEnabled:       false,
		CheckPromptEnabled: true,
		StopOnSensitive:    false,
		Words:              []string{"alpha", "beta"},
	}
	ApplySensitivePolicy(input)
	input.Words[0] = "mutated-input"

	snapshot := GetSensitivePolicy()
	assert.Equal(t, []string{"alpha", "beta"}, snapshot.Words)
	snapshot.Words[0] = "mutated-output"
	assert.Equal(t, []string{"alpha", "beta"}, GetSensitiveWords())
	assert.False(t, ShouldCheckPromptSensitive())
}

func TestSensitiveWordsFromStringNormalizesLines(t *testing.T) {
	previous := GetSensitivePolicy()
	t.Cleanup(func() { ApplySensitivePolicy(previous) })

	SensitiveWordsFromString(" alpha \n\n beta\r\n")
	assert.Equal(t, []string{"alpha", "beta"}, GetSensitiveWords())
	assert.Equal(t, "alpha\nbeta", SensitiveWordsToString())
}

func TestSensitivePolicyConcurrentReadersAndWriters(t *testing.T) {
	previous := GetSensitivePolicy()
	t.Cleanup(func() { ApplySensitivePolicy(previous) })

	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				ApplySensitivePolicy(SensitivePolicy{
					CheckEnabled:       iteration%2 == 0,
					CheckPromptEnabled: true,
					StopOnSensitive:    index%2 == 0,
					Words:              []string{"alpha", "beta"},
				})
				_ = GetSensitivePolicy()
				_ = ShouldCheckPromptSensitive()
			}
		}(worker)
	}
	workers.Wait()
}
