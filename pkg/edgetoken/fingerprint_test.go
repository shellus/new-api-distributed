package edgetoken

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAuthorizationMatchesNewAPITokenSyntax(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		key       string
		hasSuffix bool
		suffix    string
	}{
		{name: "bearer sk prefix", value: "Bearer sk-Abc123", key: "Abc123"},
		{name: "case insensitive bearer", value: "bEaReR Abc123", key: "Abc123"},
		{name: "direct key", value: "Abc123", key: "Abc123"},
		{name: "channel suffix", value: "Bearer sk-Abc123-42", key: "Abc123", hasSuffix: true, suffix: "42"},
		{name: "multi part suffix", value: "sk-Abc123-channel-blue", key: "Abc123", hasSuffix: true, suffix: "channel-blue"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseAuthorization(test.value)
			require.NoError(t, err)
			assert.Equal(t, test.key, parsed.Key)
			assert.Equal(t, test.hasSuffix, parsed.ChannelSuffixPresent)
			assert.Equal(t, test.suffix, parsed.ChannelSuffix)
		})
	}
}

func TestFingerprintAuthorizationAndStoredKeyMatch(t *testing.T) {
	fromRequest, err := FingerprintAuthorization("Bearer sk-Abc123")
	require.NoError(t, err)
	fromMaster, err := FingerprintStoredKey("Abc123")
	require.NoError(t, err)
	assert.Equal(t, "7f91e8a4b648b0125b15dc5a3b6466f9f4906d92c72bea9bd6be92c853bebda2", fromMaster)
	assert.Equal(t, fromMaster, fromRequest)
	assert.NoError(t, ValidateFingerprint(fromRequest))
}

func TestFingerprintAuthorizationRejectsChannelSelectionSuffix(t *testing.T) {
	_, err := FingerprintAuthorization("Bearer sk-Abc123-42")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChannelSuffix)
}

func TestTokenNormalizationRejectsMalformedInputs(t *testing.T) {
	tests := []struct {
		name  string
		value string
		err   error
	}{
		{name: "empty", value: "", err: ErrTokenMissing},
		{name: "empty bearer", value: "Bearer   ", err: ErrTokenMissing},
		{name: "unsupported scheme", value: "Basic Abc123", err: ErrTokenMalformed},
		{name: "empty sk prefix", value: "sk-", err: ErrTokenMissing},
		{name: "whitespace inside key", value: "sk-Abc 123", err: ErrTokenMalformed},
		{name: "non ascii", value: "sk-令牌", err: ErrTokenMalformed},
		{name: "too long", value: strings.Repeat("a", MaxTokenKeyLength+1), err: ErrTokenMalformed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseAuthorization(test.value)
			require.Error(t, err)
			assert.True(t, errors.Is(err, test.err))
		})
	}
}

func TestValidateFingerprintRejectsNonCanonicalValues(t *testing.T) {
	valid, err := FingerprintStoredKey("Abc123")
	require.NoError(t, err)

	for _, fingerprint := range []string{
		valid[:len(valid)-1],
		strings.ToUpper(valid),
		strings.Repeat("z", len(valid)),
	} {
		assert.ErrorIs(t, ValidateFingerprint(fingerprint), ErrInvalidFingerprint)
	}
}
