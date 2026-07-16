package edgeauth

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPRequestSigningRoundTrip(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	body := []byte("hello")
	req := testHTTPRequest(t)
	bodyTracker := &trackingReadCloser{}
	req.Body = bodyTracker

	require.NoError(t, SignHTTPRequest(req, body, privateKey, metadata))
	assert.Equal(t, metadata.Version, req.Header.Get(HeaderSignatureVersion))
	assert.Equal(t, metadata.NodeID, req.Header.Get(HeaderNodeID))
	assert.Equal(t, "7", req.Header.Get(HeaderNodeGeneration))
	assert.Equal(t, metadata.KeyID, req.Header.Get(HeaderKeyID))
	assert.Equal(t, "1784160000", req.Header.Get(HeaderTimestamp))
	assert.Equal(t, metadata.Nonce, req.Header.Get(HeaderNonce))
	assert.Equal(t, metadata.IdempotencyKey, req.Header.Get(HeaderIdempotencyKey))
	assert.NotEmpty(t, req.Header.Get(HeaderSignature))

	signedRequest, err := ParseHTTPRequest(req, body)
	require.NoError(t, err)
	assert.Equal(t, metadata, signedRequest.Metadata)
	assert.Equal(t, "POST", signedRequest.Request.Method)
	assert.Equal(t, "/control/v1/leases/%E4%B8%AD", signedRequest.Request.EscapedPath)
	assert.Equal(t, "cursor=10&include%5Bclosed%5D=false", signedRequest.Request.RawQuery)
	assert.Equal(t, body, signedRequest.Request.Body)
	assert.NoError(t, signedRequest.Verify(publicKey, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: time.Minute,
	}))
	assert.NoError(t, VerifyHTTPRequest(req, body, publicKey, VerifyOptions{
		Now:          time.Unix(metadata.TimestampUnixSeconds, 0),
		MaxClockSkew: time.Minute,
	}))
	assert.Zero(t, bodyTracker.reads)
	assert.Zero(t, bodyTracker.closes)
}

func TestParseHTTPRequestRejectsMissingAndDuplicateSigningHeaders(t *testing.T) {
	_, privateKey := testKeyPair(t)
	metadata := testMetadata()
	body := []byte("hello")
	headerNames := []string{
		HeaderSignatureVersion,
		HeaderNodeID,
		HeaderNodeGeneration,
		HeaderKeyID,
		HeaderTimestamp,
		HeaderNonce,
		HeaderIdempotencyKey,
		HeaderSignature,
	}

	for _, headerName := range headerNames {
		t.Run("missing "+headerName, func(t *testing.T) {
			req := testHTTPRequest(t)
			require.NoError(t, SignHTTPRequest(req, body, privateKey, metadata))
			req.Header.Del(headerName)

			_, err := ParseHTTPRequest(req, body)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assert.Equal(t, headerName, validationErr.Field)
		})

		t.Run("duplicate "+headerName, func(t *testing.T) {
			req := testHTTPRequest(t)
			require.NoError(t, SignHTTPRequest(req, body, privateKey, metadata))
			req.Header.Add(headerName, req.Header.Get(headerName))

			_, err := ParseHTTPRequest(req, body)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assert.Equal(t, headerName, validationErr.Field)
		})
	}
}

func TestParseHTTPRequestRejectsCaseVariantDuplicateHeader(t *testing.T) {
	_, privateKey := testKeyPair(t)
	req := testHTTPRequest(t)
	require.NoError(t, SignHTTPRequest(req, []byte("hello"), privateKey, testMetadata()))
	req.Header[strings.ToLower(HeaderNodeID)] = []string{req.Header.Get(HeaderNodeID)}

	_, err := ParseHTTPRequest(req, []byte("hello"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestParseHTTPRequestRejectsInvalidInt64Headers(t *testing.T) {
	_, privateKey := testKeyPair(t)
	body := []byte("hello")
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "generation text", header: HeaderNodeGeneration, value: "seven"},
		{name: "generation overflow", header: HeaderNodeGeneration, value: "9223372036854775808"},
		{name: "generation leading zero", header: HeaderNodeGeneration, value: "07"},
		{name: "generation plus sign", header: HeaderNodeGeneration, value: "+7"},
		{name: "generation negative", header: HeaderNodeGeneration, value: "-1"},
		{name: "timestamp text", header: HeaderTimestamp, value: "now"},
		{name: "timestamp overflow", header: HeaderTimestamp, value: "9223372036854775808"},
		{name: "timestamp leading zero", header: HeaderTimestamp, value: "01784160000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := testHTTPRequest(t)
			require.NoError(t, SignHTTPRequest(req, body, privateKey, testMetadata()))
			req.Header.Set(test.header, test.value)

			_, err := ParseHTTPRequest(req, body)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestVerifyHTTPRequestRejectsTargetAndBodyTampering(t *testing.T) {
	publicKey, privateKey := testKeyPair(t)
	metadata := testMetadata()
	body := []byte("hello")
	options := VerifyOptions{Now: time.Unix(metadata.TimestampUnixSeconds, 0), MaxClockSkew: time.Minute}

	tests := []struct {
		name   string
		mutate func(*http.Request, *[]byte)
	}{
		{
			name: "body",
			mutate: func(_ *http.Request, body *[]byte) {
				*body = []byte("Hello")
			},
		},
		{
			name: "escaped path",
			mutate: func(req *http.Request, _ *[]byte) {
				req.URL.Path = "/control/v1/leases/日"
				req.URL.RawPath = "/control/v1/leases/%E6%97%A5"
			},
		},
		{
			name: "raw query",
			mutate: func(req *http.Request, _ *[]byte) {
				req.URL.RawQuery = "include%5Bclosed%5D=false&cursor=10"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := testHTTPRequest(t)
			require.NoError(t, SignHTTPRequest(req, body, privateKey, metadata))
			changedBody := append([]byte(nil), body...)
			test.mutate(req, &changedBody)

			err := VerifyHTTPRequest(req, changedBody, publicKey, options)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidSignature)
		})
	}
}

func TestSignHTTPRequestReplacesCaseVariantSigningHeaders(t *testing.T) {
	_, privateKey := testKeyPair(t)
	req := testHTTPRequest(t)
	req.Header[HeaderNodeID] = []string{"stale-node", "duplicate-node"}
	req.Header[strings.ToLower(HeaderNodeID)] = []string{"case-variant-node"}

	require.NoError(t, SignHTTPRequest(req, []byte("hello"), privateKey, testMetadata()))
	values := caseInsensitiveHeaderValues(req.Header, HeaderNodeID)
	assert.Equal(t, []string{testMetadata().NodeID}, values)
}

func TestHTTPRequestHelpersRejectNilRequestsWithoutReadingBody(t *testing.T) {
	_, privateKey := testKeyPair(t)
	assert.ErrorIs(t, SignHTTPRequest(nil, nil, privateKey, testMetadata()), ErrInvalidInput)
	_, err := ParseHTTPRequest(nil, nil)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.ErrorIs(t, (*SignedHTTPRequest)(nil).Verify(nil, VerifyOptions{}), ErrInvalidInput)

	req := &http.Request{Method: http.MethodPost}
	assert.ErrorIs(t, SignHTTPRequest(req, nil, privateKey, testMetadata()), ErrInvalidInput)
	_, err = ParseHTTPRequest(req, nil)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func testHTTPRequest(t *testing.T) *http.Request {
	t.Helper()
	parsedURL, err := url.Parse("https://master.example/control/v1/leases/%E4%B8%AD?cursor=10&include%5Bclosed%5D=false")
	require.NoError(t, err)
	return &http.Request{
		Method: http.MethodPost,
		URL:    parsedURL,
		Header: make(http.Header),
	}
}

func caseInsensitiveHeaderValues(headers http.Header, name string) []string {
	var values []string
	for existingName, existingValues := range headers {
		if strings.EqualFold(existingName, name) {
			values = append(values, existingValues...)
		}
	}
	return values
}

type trackingReadCloser struct {
	reads  int
	closes int
}

func (tracker *trackingReadCloser) Read(_ []byte) (int, error) {
	tracker.reads++
	return 0, io.EOF
}

func (tracker *trackingReadCloser) Close() error {
	tracker.closes++
	return nil
}
