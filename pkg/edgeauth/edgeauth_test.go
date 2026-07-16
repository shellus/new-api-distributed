package edgeauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalRequestAndSignatureAreDeterministic(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	request := testRequest()

	canonical, err := CanonicalRequest(metadata, request)
	require.NoError(t, err)
	assert.Equal(t, `NEWAPI-EDGE-ED25519
version:v1
node-id:edge.ap-southeast-1
generation:7
key-id:key-2026-07
method:POST
escaped-path:/control/v1/leases/%E4%B8%AD
raw-query:cursor=10&include%5Bclosed%5D=false
timestamp:1784160000
nonce:MDEyMzQ1Njc4OWFiY2RlZg
idempotency-key:lease:00000042
body-sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824`, string(canonical))

	signature, err := Sign(privateKey, metadata, request)
	require.NoError(t, err)
	assert.Equal(t, "1lz7RKcHp2qbLv6qeX1Dirhuni+9bgR8J6zd2dQC2bILb9pwFHAqHvAdBMbHc5NICZf/iUjbEFxYdK4mqwBFAw==", signature)
	assert.NoError(t, Verify(publicKey, metadata, request, signature, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: 5 * time.Minute,
	}))
}

func TestVerifyRejectsEverySignedFieldTampering(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	request := testRequest()
	signature, err := Sign(privateKey, metadata, request)
	require.NoError(t, err)
	options := VerifyOptions{Now: time.Unix(metadata.TimestampUnixSeconds, 0), MaxClockSkew: 5 * time.Minute}

	tests := []struct {
		name    string
		mutate  func(*Metadata, *Request, *string)
		wantErr error
	}{
		{
			name: "version",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.Version = "v2"
			},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name: "node ID",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.NodeID = "edge.ap-southeast-2"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "generation",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.Generation++
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "key ID",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.KeyID = "key-2026-08"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "method",
			mutate: func(_ *Metadata, request *Request, _ *string) {
				request.Method = "PUT"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "escaped path",
			mutate: func(_ *Metadata, request *Request, _ *string) {
				request.EscapedPath = "/control/v1/leases/%E6%97%A5"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "raw query",
			mutate: func(_ *Metadata, request *Request, _ *string) {
				request.RawQuery = "include%5Bclosed%5D=false&cursor=10"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "timestamp",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.TimestampUnixSeconds++
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "nonce",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.Nonce = "MDEyMzQ1Njc4OWFiY2RlZA"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "idempotency key",
			mutate: func(metadata *Metadata, _ *Request, _ *string) {
				metadata.IdempotencyKey = "lease:00000043"
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "body",
			mutate: func(_ *Metadata, request *Request, _ *string) {
				request.Body = []byte("Hello")
			},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "signature",
			mutate: func(_ *Metadata, _ *Request, signature *string) {
				replacement := byte('A')
				if (*signature)[0] == replacement {
					replacement = 'B'
				}
				*signature = string(replacement) + (*signature)[1:]
			},
			wantErr: ErrInvalidSignature,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedMetadata := metadata
			changedRequest := request
			changedRequest.Body = append([]byte(nil), request.Body...)
			changedSignature := signature
			test.mutate(&changedMetadata, &changedRequest, &changedSignature)

			err := Verify(publicKey, changedMetadata, changedRequest, changedSignature, options)
			require.Error(t, err)
			assert.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestVerifyRejectsWrongPublicKey(t *testing.T) {
	_, privateKey := testKeyPair(t)
	wrongPrivateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	wrongPublicKey := wrongPrivateKey.Public().(ed25519.PublicKey)
	metadata := testMetadata()
	request := testRequest()
	signature, err := Sign(privateKey, metadata, request)
	require.NoError(t, err)

	err = Verify(wrongPublicKey, metadata, request, signature, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: time.Minute,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSignature)
}

func TestVerifyEnforcesClockSkewAfterAuthentication(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	request := testRequest()
	signature, err := Sign(privateKey, metadata, request)
	require.NoError(t, err)
	signedAt := time.Unix(metadata.TimestampUnixSeconds, 0)

	for _, now := range []time.Time{signedAt.Add(-5 * time.Minute), signedAt.Add(5 * time.Minute)} {
		assert.NoError(t, Verify(publicKey, metadata, request, signature, VerifyOptions{
			Now:          now,
			MaxClockSkew: 5 * time.Minute,
		}))
	}

	for _, test := range []struct {
		name string
		now  time.Time
	}{
		{name: "verifier clock ahead", now: signedAt.Add(5*time.Minute + time.Nanosecond)},
		{name: "verifier clock behind", now: signedAt.Add(-5*time.Minute - time.Nanosecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Verify(publicKey, metadata, request, signature, VerifyOptions{
				Now:          test.now,
				MaxClockSkew: 5 * time.Minute,
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrClockSkew)
			var clockErr *ClockSkewError
			require.ErrorAs(t, err, &clockErr)
			assert.Equal(t, metadata.TimestampUnixSeconds, clockErr.TimestampUnixSeconds)
		})
	}

	tamperedMetadata := metadata
	tamperedMetadata.TimestampUnixSeconds += int64((24 * time.Hour).Seconds())
	err = Verify(publicKey, tamperedMetadata, request, signature, VerifyOptions{
		Now:          signedAt,
		MaxClockSkew: 5 * time.Minute,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSignature)
	assert.NotErrorIs(t, err, ErrClockSkew)
}

func TestKeyEncodingRoundTripAndSeparation(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)

	encodedPublicKey, err := EncodePublicKey(publicKey)
	require.NoError(t, err)
	assert.Equal(t, "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", encodedPublicKey)
	parsedPublicKey, err := ParsePublicKey(encodedPublicKey)
	require.NoError(t, err)
	assert.Equal(t, publicKey, parsedPublicKey)

	encodedPrivateKey, err := EncodePrivateKey(privateKey)
	require.NoError(t, err)
	assert.Equal(t, "nWGxne/9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2DXWpgBgrEKt9VL/tPJZAc6DuFy89qmIyWvAhpo9wdRGg==", encodedPrivateKey)
	parsedPrivateKey, err := ParsePrivateKey(encodedPrivateKey)
	require.NoError(t, err)
	assert.Equal(t, privateKey, parsedPrivateKey)

	_, err = ParsePrivateKey(encodedPublicKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPrivateKey)
	_, err = ParsePublicKey(encodedPrivateKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPublicKey)
}

func TestKeyParsingRejectsNonCanonicalAndInconsistentKeys(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	encodedPublicKey, err := EncodePublicKey(publicKey)
	require.NoError(t, err)
	encodedPrivateKey, err := EncodePrivateKey(privateKey)
	require.NoError(t, err)

	publicKeyTests := []string{
		strings.TrimSuffix(encodedPublicKey, "="),
		encodedPublicKey + " ",
		strings.Repeat("A", base64.StdEncoding.EncodedLen(ed25519.PublicKeySize)),
		strings.Repeat("_", base64.StdEncoding.EncodedLen(ed25519.PublicKeySize)),
	}
	for _, encoded := range publicKeyTests {
		_, err := ParsePublicKey(encoded)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidPublicKey)
	}

	privateKeyTests := []string{
		strings.TrimSuffix(encodedPrivateKey, "="),
		encodedPrivateKey + "\n",
		strings.Repeat("_", base64.StdEncoding.EncodedLen(ed25519.PrivateKeySize)),
	}
	for _, encoded := range privateKeyTests {
		_, err := ParsePrivateKey(encoded)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidPrivateKey)
	}

	inconsistentPrivateKey := append(ed25519.PrivateKey(nil), privateKey...)
	inconsistentPrivateKey[len(inconsistentPrivateKey)-1] ^= 1
	_, err = EncodePrivateKey(inconsistentPrivateKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPrivateKey)

	inconsistentEncoding := base64.StdEncoding.EncodeToString(inconsistentPrivateKey)
	_, err = ParsePrivateKey(inconsistentEncoding)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPrivateKey)
}

func TestCanonicalRequestRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Metadata, *Request)
		wantErr error
	}{
		{name: "unsupported version", mutate: func(metadata *Metadata, _ *Request) { metadata.Version = "" }, wantErr: ErrUnsupportedVersion},
		{name: "empty node ID", mutate: func(metadata *Metadata, _ *Request) { metadata.NodeID = "" }, wantErr: ErrInvalidInput},
		{name: "uppercase node ID", mutate: func(metadata *Metadata, _ *Request) { metadata.NodeID = "edge.AP-southeast-1" }, wantErr: ErrInvalidInput},
		{name: "node ID newline", mutate: func(metadata *Metadata, _ *Request) { metadata.NodeID = "edge\nother" }, wantErr: ErrInvalidInput},
		{name: "long node ID", mutate: func(metadata *Metadata, _ *Request) { metadata.NodeID = strings.Repeat("a", MaxNodeIDLength+1) }, wantErr: ErrInvalidInput},
		{name: "zero generation", mutate: func(metadata *Metadata, _ *Request) { metadata.Generation = 0 }, wantErr: ErrInvalidInput},
		{name: "negative generation", mutate: func(metadata *Metadata, _ *Request) { metadata.Generation = -1 }, wantErr: ErrInvalidInput},
		{name: "uppercase key ID", mutate: func(metadata *Metadata, _ *Request) { metadata.KeyID = "key-2026-A" }, wantErr: ErrInvalidInput},
		{name: "invalid key ID", mutate: func(metadata *Metadata, _ *Request) { metadata.KeyID = "key/id" }, wantErr: ErrInvalidInput},
		{name: "long key ID", mutate: func(metadata *Metadata, _ *Request) { metadata.KeyID = strings.Repeat("a", MaxKeyIDLength+1) }, wantErr: ErrInvalidInput},
		{name: "zero timestamp", mutate: func(metadata *Metadata, _ *Request) { metadata.TimestampUnixSeconds = 0 }, wantErr: ErrInvalidInput},
		{name: "timestamp beyond year 9999", mutate: func(metadata *Metadata, _ *Request) { metadata.TimestampUnixSeconds = 253402300800 }, wantErr: ErrInvalidInput},
		{name: "short nonce", mutate: func(metadata *Metadata, _ *Request) { metadata.Nonce = "short" }, wantErr: ErrInvalidInput},
		{name: "noncanonical nonce", mutate: func(metadata *Metadata, _ *Request) { metadata.Nonce = "abcdefghijklmnopq" }, wantErr: ErrInvalidInput},
		{name: "padded nonce", mutate: func(metadata *Metadata, _ *Request) { metadata.Nonce = "MDEyMzQ1Njc4OWFi=" }, wantErr: ErrInvalidInput},
		{name: "empty idempotency key", mutate: func(metadata *Metadata, _ *Request) { metadata.IdempotencyKey = "" }, wantErr: ErrInvalidInput},
		{name: "uppercase idempotency key", mutate: func(metadata *Metadata, _ *Request) { metadata.IdempotencyKey = "lease:0000004A" }, wantErr: ErrInvalidInput},
		{name: "idempotency key newline", mutate: func(metadata *Metadata, _ *Request) { metadata.IdempotencyKey = "lease\n42" }, wantErr: ErrInvalidInput},
		{name: "long idempotency key", mutate: func(metadata *Metadata, _ *Request) {
			metadata.IdempotencyKey = strings.Repeat("a", MaxIdempotencyKeyLength+1)
		}, wantErr: ErrInvalidInput},
		{name: "lowercase method", mutate: func(_ *Metadata, request *Request) { request.Method = "Post" }, wantErr: ErrInvalidInput},
		{name: "method whitespace", mutate: func(_ *Metadata, request *Request) { request.Method = "PO ST" }, wantErr: ErrInvalidInput},
		{name: "method too long", mutate: func(_ *Metadata, request *Request) { request.Method = strings.Repeat("A", MaxMethodLength+1) }, wantErr: ErrInvalidInput},
		{name: "path missing slash", mutate: func(_ *Metadata, request *Request) { request.EscapedPath = "control/v1" }, wantErr: ErrInvalidInput},
		{name: "path invalid escape", mutate: func(_ *Metadata, request *Request) { request.EscapedPath = "/control/%GG" }, wantErr: ErrInvalidInput},
		{name: "path contains query", mutate: func(_ *Metadata, request *Request) { request.EscapedPath = "/control?x=1" }, wantErr: ErrInvalidInput},
		{name: "path contains raw unicode", mutate: func(_ *Metadata, request *Request) { request.EscapedPath = "/控制" }, wantErr: ErrInvalidInput},
		{name: "query leading question mark", mutate: func(_ *Metadata, request *Request) { request.RawQuery = "?x=1" }, wantErr: ErrInvalidInput},
		{name: "query invalid escape", mutate: func(_ *Metadata, request *Request) { request.RawQuery = "x=%0" }, wantErr: ErrInvalidInput},
		{name: "query whitespace", mutate: func(_ *Metadata, request *Request) { request.RawQuery = "x=hello world" }, wantErr: ErrInvalidInput},
		{name: "query raw bracket", mutate: func(_ *Metadata, request *Request) { request.RawQuery = "include[closed]=false" }, wantErr: ErrInvalidInput},
		{name: "query fragment", mutate: func(_ *Metadata, request *Request) { request.RawQuery = "x=1#fragment" }, wantErr: ErrInvalidInput},
		{name: "query too long", mutate: func(_ *Metadata, request *Request) { request.RawQuery = strings.Repeat("x", MaxRawQueryLength+1) }, wantErr: ErrInvalidInput},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := testMetadata()
			request := testRequest()
			test.mutate(&metadata, &request)

			_, err := CanonicalRequest(metadata, request)
			require.Error(t, err)
			assert.ErrorIs(t, err, test.wantErr)
			var validationErr *ValidationError
			assert.ErrorAs(t, err, &validationErr)
		})
	}
}

func TestSignVerifyRejectMalformedKeysSignatureAndOptions(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	request := testRequest()
	signature, err := Sign(privateKey, metadata, request)
	require.NoError(t, err)

	_, err = Sign(ed25519.PrivateKey(privateKey[:ed25519.PrivateKeySize-1]), metadata, request)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPrivateKey)

	err = Verify(ed25519.PublicKey(publicKey[:ed25519.PublicKeySize-1]), metadata, request, signature, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: time.Minute,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPublicKey)

	for _, malformedSignature := range []string{
		strings.TrimSuffix(signature, "="),
		signature + " ",
		strings.Repeat("_", base64.StdEncoding.EncodedLen(ed25519.SignatureSize)),
	} {
		err = Verify(publicKey, metadata, request, malformedSignature, VerifyOptions{
			Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
			MaxClockSkew: time.Minute,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidSignature)
	}

	err = Verify(publicKey, metadata, request, signature, VerifyOptions{MaxClockSkew: time.Minute})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	err = Verify(publicKey, metadata, request, signature, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: -time.Second,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestBodySHA256(t *testing.T) {
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", BodySHA256(nil))
	assert.Equal(t, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", BodySHA256([]byte("hello")))
}

func TestIdempotencySHA256IsStableAcrossSigningRetries(t *testing.T) {
	request := testRequest()
	digest, err := IdempotencySHA256(request)
	require.NoError(t, err)
	assert.Equal(t, "5e361c1103bcb7ea595f4a1510ecfe2722954d742b337c9e0beed353806da8c9", digest)

	metadata := testMetadata()
	canonicalBefore, err := CanonicalRequest(metadata, request)
	require.NoError(t, err)
	metadata.TimestampUnixSeconds++
	metadata.Nonce = "ZmVkY2JhOTg3NjU0MzIxMA"
	canonicalAfter, err := CanonicalRequest(metadata, request)
	require.NoError(t, err)
	assert.NotEqual(t, canonicalBefore, canonicalAfter)

	retryDigest, err := IdempotencySHA256(request)
	require.NoError(t, err)
	assert.Equal(t, digest, retryDigest)
}

func TestIdempotencySHA256CoversLogicalRequestFields(t *testing.T) {
	base := testRequest()
	baseDigest, err := IdempotencySHA256(base)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "method", mutate: func(request *Request) { request.Method = "PUT" }},
		{name: "path", mutate: func(request *Request) { request.EscapedPath += "/close" }},
		{name: "query", mutate: func(request *Request) { request.RawQuery += "&page=2" }},
		{name: "body", mutate: func(request *Request) { request.Body = []byte("Hello") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			changed.Body = append([]byte(nil), base.Body...)
			test.mutate(&changed)
			digest, err := IdempotencySHA256(changed)
			require.NoError(t, err)
			assert.NotEqual(t, baseDigest, digest)
		})
	}
}

func TestNewNonceUsesCanonicalBase64URL(t *testing.T) {
	nonce, err := NewNonce()
	require.NoError(t, err)
	assert.NoError(t, ValidateNonce(nonce))
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(nonce)
	require.NoError(t, err)
	assert.Len(t, decoded, 16)
}

func TestErrorsSupportSentinelAndTypedChecks(t *testing.T) {
	_, err := CanonicalRequest(Metadata{}, Request{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
	assert.True(t, errors.Is(err, ErrUnsupportedVersion))
	var validationErr *ValidationError
	require.True(t, errors.As(err, &validationErr))
	assert.Equal(t, "version", validationErr.Field)
}

func testKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed, err := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	require.NoError(t, err)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return publicKey, privateKey
}

func testMetadata() Metadata {
	return Metadata{
		Version:              VersionV1,
		NodeID:               "edge.ap-southeast-1",
		Generation:           7,
		KeyID:                "key-2026-07",
		TimestampUnixSeconds: 1784160000,
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "lease:00000042",
	}
}

func testRequest() Request {
	return Request{
		Method:      "POST",
		EscapedPath: "/control/v1/leases/%E4%B8%AD",
		RawQuery:    "cursor=10&include%5Bclosed%5D=false",
		Body:        []byte("hello"),
	}
}
