// Package edgeauth signs control-plane requests exchanged between trusted
// New API master and edge nodes.
package edgeauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// Algorithm is the credential algorithm identifier stored by the control plane.
	Algorithm = "ed25519"
	// CanonicalDomain separates node request signatures from other Ed25519 uses.
	CanonicalDomain = "NEWAPI-EDGE-ED25519"
	// IdempotencyDomain separates the stable logical request digest from the
	// signed request representation, which intentionally includes retry-varying
	// timestamp and nonce fields.
	IdempotencyDomain = "NEWAPI-EDGE-IDEMPOTENCY-V1"
	// VersionV1 is the first canonical request format.
	VersionV1 = "v1"

	MaxNodeIDLength           = 64
	MaxKeyIDLength            = 64
	MinNonceLength            = 16
	MaxNonceLength            = 128
	MaxIdempotencyKeyLength   = 64
	MaxMethodLength           = 32
	MaxEscapedPathLength      = 8192
	MaxRawQueryLength         = 8192
	maxSupportedUnixTimestamp = int64(253402300799) // 9999-12-31T23:59:59Z
	generatedNonceBytes       = 16
)

var (
	strictBase64       = base64.StdEncoding.Strict()
	strictRawURLBase64 = base64.RawURLEncoding.Strict()
)

// Metadata identifies the node credential and the request instance. NodeID,
// KeyID, and IdempotencyKey use canonical lowercase ASCII so database collation
// differences cannot change their identity. Nonce and IdempotencyKey are signed
// here; their durable replay checks belong to the caller because this package
// is intentionally stateless.
type Metadata struct {
	Version              string
	NodeID               string
	Generation           int64
	KeyID                string
	TimestampUnixSeconds int64
	Nonce                string
	IdempotencyKey       string
}

// Request contains the exact request target and body bytes covered by a
// signature. EscapedPath and RawQuery must be taken before URL normalization.
type Request struct {
	Method      string
	EscapedPath string
	RawQuery    string
	Body        []byte
}

// VerifyOptions defines the receiving node's clock policy.
type VerifyOptions struct {
	Now          time.Time
	MaxClockSkew time.Duration
}

// BodySHA256 returns the lowercase hexadecimal SHA-256 digest used in the
// canonical request.
func BodySHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// IdempotencySHA256 returns a stable digest for one logical HTTP request. It
// excludes signing metadata so a retry with a fresh timestamp, nonce or
// rotated credential resolves to the same persistent receipt.
func IdempotencySHA256(request Request) (string, error) {
	if err := validateRequest(request); err != nil {
		return "", err
	}

	var canonical strings.Builder
	canonical.Grow(len(IdempotencyDomain) + len(request.Method) + len(request.EscapedPath) + len(request.RawQuery) + 128)
	canonical.WriteString(IdempotencyDomain)
	canonical.WriteString("\nmethod:")
	canonical.WriteString(request.Method)
	canonical.WriteString("\nescaped-path:")
	canonical.WriteString(request.EscapedPath)
	canonical.WriteString("\nraw-query:")
	canonical.WriteString(request.RawQuery)
	canonical.WriteString("\nbody-sha256:")
	canonical.WriteString(BodySHA256(request.Body))

	digest := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(digest[:]), nil
}

// CanonicalRequest validates and renders the versioned request representation
// signed by an edge node and verified by the master.
func CanonicalRequest(metadata Metadata, request Request) ([]byte, error) {
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}

	bodyDigest := BodySHA256(request.Body)
	var canonical strings.Builder
	canonical.Grow(len(CanonicalDomain) + len(metadata.NodeID) + len(metadata.KeyID) + len(request.Method) +
		len(request.EscapedPath) + len(request.RawQuery) + len(metadata.Nonce) + len(metadata.IdempotencyKey) + 256)
	canonical.WriteString(CanonicalDomain)
	canonical.WriteString("\nversion:")
	canonical.WriteString(metadata.Version)
	canonical.WriteString("\nnode-id:")
	canonical.WriteString(metadata.NodeID)
	canonical.WriteString("\ngeneration:")
	canonical.WriteString(strconv.FormatInt(metadata.Generation, 10))
	canonical.WriteString("\nkey-id:")
	canonical.WriteString(metadata.KeyID)
	canonical.WriteString("\nmethod:")
	canonical.WriteString(request.Method)
	canonical.WriteString("\nescaped-path:")
	canonical.WriteString(request.EscapedPath)
	canonical.WriteString("\nraw-query:")
	canonical.WriteString(request.RawQuery)
	canonical.WriteString("\ntimestamp:")
	canonical.WriteString(strconv.FormatInt(metadata.TimestampUnixSeconds, 10))
	canonical.WriteString("\nnonce:")
	canonical.WriteString(metadata.Nonce)
	canonical.WriteString("\nidempotency-key:")
	canonical.WriteString(metadata.IdempotencyKey)
	canonical.WriteString("\nbody-sha256:")
	canonical.WriteString(bodyDigest)
	return []byte(canonical.String()), nil
}

// Sign returns a canonical standard-base64 Ed25519 signature.
func Sign(privateKey ed25519.PrivateKey, metadata Metadata, request Request) (string, error) {
	if err := validatePrivateKey(privateKey); err != nil {
		return "", err
	}
	canonical, err := CanonicalRequest(metadata, request)
	if err != nil {
		return "", err
	}

	signature := ed25519.Sign(privateKey, canonical)
	return base64.StdEncoding.EncodeToString(signature), nil
}

// Verify authenticates a signed request and then enforces the configured clock
// skew. Replay protection for Nonce and IdempotencyKey is deliberately left to
// the persistent control-plane service.
func Verify(publicKey ed25519.PublicKey, metadata Metadata, request Request, signature string, options VerifyOptions) error {
	if err := validatePublicKey(publicKey); err != nil {
		return err
	}
	if options.Now.IsZero() {
		return newValidationError("now", "must be set", ErrInvalidInput)
	}
	if options.MaxClockSkew < 0 {
		return newValidationError("max clock skew", "must not be negative", ErrInvalidInput)
	}

	canonical, err := CanonicalRequest(metadata, request)
	if err != nil {
		return err
	}
	providedSignature, err := decodeSignature(signature)
	if err != nil {
		return err
	}

	if !ed25519.Verify(publicKey, canonical, providedSignature) {
		return &SignatureError{}
	}

	signedAt := time.Unix(metadata.TimestampUnixSeconds, 0)
	delta := options.Now.Sub(signedAt)
	if delta > options.MaxClockSkew || delta < -options.MaxClockSkew {
		return &ClockSkewError{
			TimestampUnixSeconds: metadata.TimestampUnixSeconds,
			Now:                  options.Now,
			MaxClockSkew:         options.MaxClockSkew,
		}
	}
	return nil
}

// ValidateNodeID enforces the canonical identifier shared by protocol and
// persistence layers.
func ValidateNodeID(nodeID string) error {
	return validateIdentifier("node ID", nodeID, MaxNodeIDLength)
}

// ValidateKeyID enforces the canonical node credential identifier.
func ValidateKeyID(keyID string) error {
	return validateIdentifier("key ID", keyID, MaxKeyIDLength)
}

// ValidateIdempotencyKey enforces the canonical transport idempotency key.
func ValidateIdempotencyKey(idempotencyKey string) error {
	return validateIdentifier("idempotency key", idempotencyKey, MaxIdempotencyKeyLength)
}

// ValidateNonce requires canonical unpadded base64url text. Persistent replay
// detection remains the caller's responsibility.
func ValidateNonce(nonce string) error {
	if len(nonce) < MinNonceLength || len(nonce) > MaxNonceLength {
		return newValidationError("nonce", "length must be between 16 and 128 characters", ErrInvalidInput)
	}
	for i := 0; i < len(nonce); i++ {
		if !isAlphaNumeric(nonce[i]) && nonce[i] != '-' && nonce[i] != '_' {
			return newValidationError("nonce", "must use base64url characters without padding", ErrInvalidInput)
		}
	}
	if _, err := strictRawURLBase64.DecodeString(nonce); err != nil {
		return newValidationError("nonce", "must use canonical unpadded base64url encoding", ErrInvalidInput)
	}
	return nil
}

// NewNonce returns 128 bits of operating-system randomness in canonical
// unpadded base64url form.
func NewNonce() (string, error) {
	random := make([]byte, generatedNonceBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("edgeauth: generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// EncodePublicKey validates and encodes a master-side node public key.
func EncodePublicKey(publicKey ed25519.PublicKey) (string, error) {
	if err := validatePublicKey(publicKey); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

// ParsePublicKey decodes a public key produced by EncodePublicKey.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := decodeValue(encoded, ed25519.PublicKeySize, "public key", ErrInvalidPublicKey)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(decoded), nil
}

// EncodePrivateKey validates and encodes an edge-side node private key.
func EncodePrivateKey(privateKey ed25519.PrivateKey) (string, error) {
	if err := validatePrivateKey(privateKey); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(privateKey), nil
}

// ParsePrivateKey decodes a private key produced by EncodePrivateKey and checks
// that its embedded public key matches its seed.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	decoded, err := decodeValue(encoded, ed25519.PrivateKeySize, "private key", ErrInvalidPrivateKey)
	if err != nil {
		return nil, err
	}
	privateKey := ed25519.PrivateKey(decoded)
	if err := validatePrivateKey(privateKey); err != nil {
		return nil, err
	}
	return privateKey, nil
}

func validatePublicKey(publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return newValidationError("public key", "must contain exactly 32 bytes", ErrInvalidPublicKey)
	}
	return nil
}

func validatePrivateKey(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return newValidationError("private key", "must contain exactly 64 bytes", ErrInvalidPrivateKey)
	}
	derived := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(derived, privateKey) != 1 {
		return newValidationError("private key", "embedded public key does not match its seed", ErrInvalidPrivateKey)
	}
	return nil
}

func validateMetadata(metadata Metadata) error {
	if metadata.Version != VersionV1 {
		return newValidationError("version", "is unsupported", ErrUnsupportedVersion)
	}
	if err := ValidateNodeID(metadata.NodeID); err != nil {
		return err
	}
	if metadata.Generation <= 0 {
		return newValidationError("generation", "must be greater than zero", ErrInvalidInput)
	}
	if err := ValidateKeyID(metadata.KeyID); err != nil {
		return err
	}
	if metadata.TimestampUnixSeconds <= 0 || metadata.TimestampUnixSeconds > maxSupportedUnixTimestamp {
		return newValidationError("timestamp", "must be a supported positive Unix timestamp", ErrInvalidInput)
	}
	if err := ValidateNonce(metadata.Nonce); err != nil {
		return err
	}
	if err := ValidateIdempotencyKey(metadata.IdempotencyKey); err != nil {
		return err
	}
	return nil
}

func validateRequest(request Request) error {
	if len(request.Method) == 0 || len(request.Method) > MaxMethodLength {
		return newValidationError("method", "length must be between 1 and 32 bytes", ErrInvalidInput)
	}
	for i := 0; i < len(request.Method); i++ {
		if !isHTTPTokenByte(request.Method[i]) {
			return newValidationError("method", "must be an uppercase HTTP token", ErrInvalidInput)
		}
		if request.Method[i] >= 'a' && request.Method[i] <= 'z' {
			return newValidationError("method", "must be uppercase", ErrInvalidInput)
		}
	}

	if len(request.EscapedPath) == 0 || len(request.EscapedPath) > MaxEscapedPathLength {
		return newValidationError("escaped path", "length must be between 1 and 8192 bytes", ErrInvalidInput)
	}
	if request.EscapedPath[0] != '/' {
		return newValidationError("escaped path", "must start with a slash", ErrInvalidInput)
	}
	for i := 0; i < len(request.EscapedPath); i++ {
		value := request.EscapedPath[i]
		if value == '%' {
			if i+2 >= len(request.EscapedPath) || !isHex(request.EscapedPath[i+1]) || !isHex(request.EscapedPath[i+2]) {
				return newValidationError("escaped path", "contains invalid percent encoding", ErrInvalidInput)
			}
			i += 2
			continue
		}
		if !isEscapedPathByte(value) {
			return newValidationError("escaped path", "contains a character that must be percent-encoded", ErrInvalidInput)
		}
	}

	if len(request.RawQuery) > MaxRawQueryLength {
		return newValidationError("raw query", "must not exceed 8192 bytes", ErrInvalidInput)
	}
	if strings.HasPrefix(request.RawQuery, "?") {
		return newValidationError("raw query", "must not include the leading question mark", ErrInvalidInput)
	}
	for i := 0; i < len(request.RawQuery); i++ {
		value := request.RawQuery[i]
		if value == '%' {
			if i+2 >= len(request.RawQuery) || !isHex(request.RawQuery[i+1]) || !isHex(request.RawQuery[i+2]) {
				return newValidationError("raw query", "contains invalid percent encoding", ErrInvalidInput)
			}
			i += 2
			continue
		}
		if !isEscapedPathByte(value) && value != '?' {
			return newValidationError("raw query", "contains a character that must be percent-encoded", ErrInvalidInput)
		}
	}
	return nil
}

func validateIdentifier(field string, value string, maxLength int) error {
	if len(value) == 0 || len(value) > maxLength {
		return newValidationError(field, "has an invalid length", ErrInvalidInput)
	}
	for i := 0; i < len(value); i++ {
		if !isLowerAlphaNumeric(value[i]) {
			switch value[i] {
			case '-', '_', '.', ':':
				continue
			default:
				return newValidationError(field, "contains an invalid character", ErrInvalidInput)
			}
		}
	}
	return nil
}

func decodeSignature(signature string) ([]byte, error) {
	return decodeValue(signature, ed25519.SignatureSize, "signature", ErrInvalidSignature)
}

func decodeValue(encoded string, decodedSize int, field string, kind error) ([]byte, error) {
	if len(encoded) != base64.StdEncoding.EncodedLen(decodedSize) {
		return nil, newValidationError(field, "has an invalid base64 length", kind)
	}
	decoded, err := strictBase64.DecodeString(encoded)
	if err != nil || len(decoded) != decodedSize {
		return nil, newValidationError(field, "must use canonical standard base64 encoding", kind)
	}
	return decoded, nil
}

func isAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isLowerAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func isHTTPTokenByte(value byte) bool {
	if isAlphaNumeric(value) {
		return true
	}
	switch value {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func isEscapedPathByte(value byte) bool {
	if isAlphaNumeric(value) {
		return true
	}
	switch value {
	case '-', '.', '_', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@', '/':
		return true
	default:
		return false
	}
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
