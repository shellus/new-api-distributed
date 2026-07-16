package edgeauth

import (
	"crypto/ed25519"
	"net/http"
	"strconv"
	"strings"
)

const (
	HeaderSignatureVersion = "X-Newapi-Edge-Signature-Version"
	HeaderNodeID           = "X-Newapi-Edge-Node-Id"
	HeaderNodeGeneration   = "X-Newapi-Edge-Node-Generation"
	HeaderKeyID            = "X-Newapi-Edge-Key-Id"
	HeaderTimestamp        = "X-Newapi-Edge-Timestamp"
	HeaderNonce            = "X-Newapi-Edge-Nonce"
	HeaderIdempotencyKey   = "X-Newapi-Edge-Idempotency-Key"
	HeaderSignature        = "X-Newapi-Edge-Signature"
)

// SignedHTTPRequest is the parsed, still-unverified form of a control-plane
// request. Callers can use its key ID to load the public key before Verify.
type SignedHTTPRequest struct {
	Metadata  Metadata
	Request   Request
	Signature string
}

// SignHTTPRequest signs the supplied body bytes and replaces every signing
// header with exactly one canonical value. It never reads or closes req.Body.
func SignHTTPRequest(req *http.Request, body []byte, privateKey ed25519.PrivateKey, metadata Metadata) error {
	request, err := requestFromHTTP(req, body)
	if err != nil {
		return err
	}
	signature, err := Sign(privateKey, metadata, request)
	if err != nil {
		return err
	}

	if req.Header == nil {
		req.Header = make(http.Header)
	}
	setSingleHeader(req.Header, HeaderSignatureVersion, metadata.Version)
	setSingleHeader(req.Header, HeaderNodeID, metadata.NodeID)
	setSingleHeader(req.Header, HeaderNodeGeneration, strconv.FormatInt(metadata.Generation, 10))
	setSingleHeader(req.Header, HeaderKeyID, metadata.KeyID)
	setSingleHeader(req.Header, HeaderTimestamp, strconv.FormatInt(metadata.TimestampUnixSeconds, 10))
	setSingleHeader(req.Header, HeaderNonce, metadata.Nonce)
	setSingleHeader(req.Header, HeaderIdempotencyKey, metadata.IdempotencyKey)
	setSingleHeader(req.Header, HeaderSignature, signature)
	return nil
}

// ParseHTTPRequest reads only signed headers and the URL metadata. The body
// must already have been read by the caller and is never read from req.Body.
func ParseHTTPRequest(req *http.Request, body []byte) (*SignedHTTPRequest, error) {
	request, err := requestFromHTTP(req, body)
	if err != nil {
		return nil, err
	}

	version, err := singleHeader(req.Header, HeaderSignatureVersion)
	if err != nil {
		return nil, err
	}
	nodeID, err := singleHeader(req.Header, HeaderNodeID)
	if err != nil {
		return nil, err
	}
	generationText, err := singleHeader(req.Header, HeaderNodeGeneration)
	if err != nil {
		return nil, err
	}
	generation, err := parseCanonicalInt64Header(HeaderNodeGeneration, generationText)
	if err != nil {
		return nil, err
	}
	keyID, err := singleHeader(req.Header, HeaderKeyID)
	if err != nil {
		return nil, err
	}
	timestampText, err := singleHeader(req.Header, HeaderTimestamp)
	if err != nil {
		return nil, err
	}
	timestamp, err := parseCanonicalInt64Header(HeaderTimestamp, timestampText)
	if err != nil {
		return nil, err
	}
	nonce, err := singleHeader(req.Header, HeaderNonce)
	if err != nil {
		return nil, err
	}
	idempotencyKey, err := singleHeader(req.Header, HeaderIdempotencyKey)
	if err != nil {
		return nil, err
	}
	signature, err := singleHeader(req.Header, HeaderSignature)
	if err != nil {
		return nil, err
	}

	metadata := Metadata{
		Version:              version,
		NodeID:               nodeID,
		Generation:           generation,
		KeyID:                keyID,
		TimestampUnixSeconds: timestamp,
		Nonce:                nonce,
		IdempotencyKey:       idempotencyKey,
	}
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	if _, err := decodeSignature(signature); err != nil {
		return nil, err
	}

	return &SignedHTTPRequest{
		Metadata:  metadata,
		Request:   request,
		Signature: signature,
	}, nil
}

// Verify verifies a parsed request after the caller resolves its public key.
func (request *SignedHTTPRequest) Verify(publicKey ed25519.PublicKey, options VerifyOptions) error {
	if request == nil {
		return newValidationError("signed HTTP request", "must not be nil", ErrInvalidInput)
	}
	return Verify(publicKey, request.Metadata, request.Request, request.Signature, options)
}

// VerifyHTTPRequest parses and verifies a request when its public key is
// already known.
func VerifyHTTPRequest(req *http.Request, body []byte, publicKey ed25519.PublicKey, options VerifyOptions) error {
	signedRequest, err := ParseHTTPRequest(req, body)
	if err != nil {
		return err
	}
	return signedRequest.Verify(publicKey, options)
}

func requestFromHTTP(req *http.Request, body []byte) (Request, error) {
	if req == nil {
		return Request{}, newValidationError("HTTP request", "must not be nil", ErrInvalidInput)
	}
	if req.URL == nil {
		return Request{}, newValidationError("request URL", "must not be nil", ErrInvalidInput)
	}
	return Request{
		Method:      req.Method,
		EscapedPath: req.URL.EscapedPath(),
		RawQuery:    req.URL.RawQuery,
		Body:        body,
	}, nil
}

func setSingleHeader(headers http.Header, name string, value string) {
	for existingName := range headers {
		if strings.EqualFold(existingName, name) {
			delete(headers, existingName)
		}
	}
	headers[name] = []string{value}
}

func singleHeader(headers http.Header, name string) (string, error) {
	var values []string
	for existingName, existingValues := range headers {
		if strings.EqualFold(existingName, name) {
			values = append(values, existingValues...)
		}
	}
	if len(values) != 1 {
		return "", newValidationError(name, "must appear exactly once", ErrInvalidInput)
	}
	return values[0], nil
}

func parseCanonicalInt64Header(name string, value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, newValidationError(name, "must be a canonical base-10 int64", ErrInvalidInput)
	}
	return parsed, nil
}
