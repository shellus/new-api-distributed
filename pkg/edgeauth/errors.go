package edgeauth

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidInput       = errors.New("edgeauth: invalid input")
	ErrInvalidPublicKey   = errors.New("edgeauth: invalid public key")
	ErrInvalidPrivateKey  = errors.New("edgeauth: invalid private key")
	ErrUnsupportedVersion = errors.New("edgeauth: unsupported canonical request version")
	ErrInvalidSignature   = errors.New("edgeauth: invalid signature")
	ErrClockSkew          = errors.New("edgeauth: timestamp outside allowed clock skew")
)

// ValidationError identifies the malformed field without echoing credential or
// request contents into logs.
type ValidationError struct {
	Field  string
	Reason string
	kind   error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("edgeauth: invalid %s: %s", e.Field, e.Reason)
}

func (e *ValidationError) Unwrap() error {
	return e.kind
}

func (e *ValidationError) Is(target error) bool {
	return target == ErrInvalidInput || target == e.kind
}

func newValidationError(field string, reason string, kind error) *ValidationError {
	return &ValidationError{Field: field, Reason: reason, kind: kind}
}

// SignatureError reports an authentication mismatch without disclosing either
// the expected or provided signature.
type SignatureError struct{}

func (e *SignatureError) Error() string {
	return ErrInvalidSignature.Error()
}

func (e *SignatureError) Unwrap() error {
	return ErrInvalidSignature
}

// ClockSkewError contains the values needed to diagnose clocks on trusted
// nodes. It is returned only after the request signature is valid.
type ClockSkewError struct {
	TimestampUnixSeconds int64
	Now                  time.Time
	MaxClockSkew         time.Duration
}

func (e *ClockSkewError) Error() string {
	return fmt.Sprintf(
		"edgeauth: signed timestamp %s is outside %s of verifier time %s",
		time.Unix(e.TimestampUnixSeconds, 0).UTC().Format(time.RFC3339),
		e.MaxClockSkew,
		e.Now.UTC().Format(time.RFC3339Nano),
	)
}

func (e *ClockSkewError) Unwrap() error {
	return ErrClockSkew
}
