// Package edgesnapshot defines the versioned digest and signature primitives
// used to authenticate immutable edge policy snapshots.
package edgesnapshot

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
)

const (
	// VersionV1 identifies the first snapshot digest and signature format.
	VersionV1 = "v1"
	// SignatureAlgorithm is the detached dataset manifest signature algorithm.
	SignatureAlgorithm = edgeauth.Algorithm
	// HashAlgorithm is used for page and aggregate payload digests.
	HashAlgorithm = "sha256"

	// DatasetManifestDomainV1 separates dataset manifest signatures from node
	// request signatures and every other Ed25519 use in the project.
	DatasetManifestDomainV1 = "NEWAPI-EDGE-SNAPSHOT-DATASET-MANIFEST-ED25519-V1"
	// PageDigestAggregateDomainV1 separates an ordered list of page digests
	// from a digest of any individual page or other SHA-256 content.
	PageDigestAggregateDomainV1 = "NEWAPI-EDGE-SNAPSHOT-PAGE-DIGEST-AGGREGATE-SHA256-V1"
	// DatasetManifestAggregateDomainV1 binds the snapshot identity, revision and
	// fixed-order dataset manifests into the top-level snapshot digest.
	DatasetManifestAggregateDomainV1 = "NEWAPI-EDGE-SNAPSHOT-DATASET-MANIFEST-AGGREGATE-SHA256-V1"

	MaxSnapshotIDLength = 64
	MaxDatasetLength    = 64
)

var (
	ErrInvalidInput     = errors.New("edgesnapshot: invalid input")
	ErrInvalidDigest    = fmt.Errorf("%w: invalid lowercase SHA-256 digest", ErrInvalidInput)
	ErrInvalidSignature = errors.New("edgesnapshot: invalid signature")
)

var strictBase64 = base64.StdEncoding.Strict()

// DatasetManifest identifies the immutable dataset content covered by a
// detached signature. PayloadDigest is the value returned by
// AggregatePageDigests for all pages in zero-based ordinal order.
type DatasetManifest struct {
	SnapshotID    string
	Dataset       string
	Revision      int64
	ItemCount     int64
	PageCount     int
	PayloadDigest string
}

// MarshalPagePayload serializes a typed page payload through common.Marshal
// and returns both the exact canonical JSON bytes and their lowercase SHA-256
// digest. Callers must transmit or persist the returned bytes without JSON
// whitespace or representation changes when byte identity matters.
func MarshalPagePayload(payload any) ([]byte, string, error) {
	canonicalJSON, err := common.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("%w: marshal page payload: %w", ErrInvalidInput, err)
	}
	digest := sha256.Sum256(canonicalJSON)
	return canonicalJSON, hex.EncodeToString(digest[:]), nil
}

// AggregatePageDigests hashes page digests in slice order. The slice index is
// the zero-based page ordinal, and both the page count and each ordinal are
// included in the domain-separated aggregate representation. An empty dataset
// therefore has a stable, non-empty aggregate digest distinct from any page.
func AggregatePageDigests(pageDigests []string) (string, error) {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(PageDigestAggregateDomainV1))
	_, _ = hasher.Write([]byte("\npage-count:"))
	_, _ = hasher.Write([]byte(strconv.Itoa(len(pageDigests))))

	for ordinal, pageDigest := range pageDigests {
		if err := validateDigest(pageDigest); err != nil {
			return "", fmt.Errorf("page ordinal %d: %w", ordinal, err)
		}
		_, _ = hasher.Write([]byte("\npage-ordinal:"))
		_, _ = hasher.Write([]byte(strconv.Itoa(ordinal)))
		_, _ = hasher.Write([]byte("\npage-digest:"))
		_, _ = hasher.Write([]byte(pageDigest))
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// AggregateDatasetManifests hashes complete dataset manifests in the caller's
// protocol-defined order. It binds the top-level snapshot identity/revision
// and rejects duplicate or transplanted dataset manifests.
func AggregateDatasetManifests(snapshotID string, revision int64, datasets []DatasetManifest) (string, error) {
	if err := validateText("snapshot ID", snapshotID, MaxSnapshotIDLength); err != nil {
		return "", err
	}
	if revision <= 0 {
		return "", fmt.Errorf("%w: snapshot revision must be greater than zero", ErrInvalidInput)
	}
	if len(datasets) == 0 {
		return "", fmt.Errorf("%w: dataset manifests must not be empty", ErrInvalidInput)
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(DatasetManifestAggregateDomainV1))
	_, _ = hasher.Write([]byte("\nsnapshot-id:"))
	_, _ = hasher.Write([]byte(snapshotID))
	_, _ = hasher.Write([]byte("\nsnapshot-revision:"))
	_, _ = hasher.Write([]byte(strconv.FormatInt(revision, 10)))
	_, _ = hasher.Write([]byte("\ndataset-count:"))
	_, _ = hasher.Write([]byte(strconv.Itoa(len(datasets))))
	seen := make(map[string]struct{}, len(datasets))
	for ordinal, manifest := range datasets {
		if manifest.SnapshotID != snapshotID {
			return "", fmt.Errorf("%w: dataset ordinal %d belongs to a different snapshot", ErrInvalidInput, ordinal)
		}
		if _, exists := seen[manifest.Dataset]; exists {
			return "", fmt.Errorf("%w: duplicate dataset %q", ErrInvalidInput, manifest.Dataset)
		}
		seen[manifest.Dataset] = struct{}{}
		canonical, err := CanonicalDatasetManifest(manifest)
		if err != nil {
			return "", fmt.Errorf("dataset ordinal %d: %w", ordinal, err)
		}
		manifestDigest := sha256.Sum256(canonical)
		_, _ = hasher.Write([]byte("\ndataset-ordinal:"))
		_, _ = hasher.Write([]byte(strconv.Itoa(ordinal)))
		_, _ = hasher.Write([]byte("\ndataset-manifest-sha256:"))
		_, _ = hasher.Write([]byte(hex.EncodeToString(manifestDigest[:])))
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// CanonicalDatasetManifest validates and renders the exact v1 bytes signed by
// the master and verified by an edge. The version is fixed by the domain, so
// the representation binds only the immutable manifest identity and content.
func CanonicalDatasetManifest(manifest DatasetManifest) ([]byte, error) {
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}

	var canonical strings.Builder
	canonical.Grow(len(DatasetManifestDomainV1) + len(manifest.SnapshotID) + len(manifest.Dataset) + len(manifest.PayloadDigest) + 128)
	canonical.WriteString(DatasetManifestDomainV1)
	canonical.WriteString("\nsnapshot-id:")
	canonical.WriteString(manifest.SnapshotID)
	canonical.WriteString("\ndataset:")
	canonical.WriteString(manifest.Dataset)
	canonical.WriteString("\nrevision:")
	canonical.WriteString(strconv.FormatInt(manifest.Revision, 10))
	canonical.WriteString("\nitem-count:")
	canonical.WriteString(strconv.FormatInt(manifest.ItemCount, 10))
	canonical.WriteString("\npage-count:")
	canonical.WriteString(strconv.Itoa(manifest.PageCount))
	canonical.WriteString("\npayload-digest:")
	canonical.WriteString(manifest.PayloadDigest)
	return []byte(canonical.String()), nil
}

// SignDatasetManifest returns a canonical padded standard-base64 Ed25519
// signature. Key serialization is deliberately shared with pkg/edgeauth;
// callers should use edgeauth.EncodePrivateKey and edgeauth.ParsePrivateKey.
func SignDatasetManifest(privateKey ed25519.PrivateKey, manifest DatasetManifest) (string, error) {
	if _, err := edgeauth.EncodePrivateKey(privateKey); err != nil {
		return "", fmt.Errorf("%w: validate private key: %w", ErrInvalidInput, err)
	}
	canonical, err := CanonicalDatasetManifest(manifest)
	if err != nil {
		return "", err
	}

	signature := ed25519.Sign(privateKey, canonical)
	return base64.StdEncoding.EncodeToString(signature), nil
}

// VerifyDatasetManifest authenticates a detached manifest signature. Key
// serialization is deliberately shared with pkg/edgeauth; callers should use
// edgeauth.EncodePublicKey and edgeauth.ParsePublicKey.
func VerifyDatasetManifest(publicKey ed25519.PublicKey, manifest DatasetManifest, signature string) error {
	if _, err := edgeauth.EncodePublicKey(publicKey); err != nil {
		return fmt.Errorf("%w: validate public key: %w", ErrInvalidInput, err)
	}
	canonical, err := CanonicalDatasetManifest(manifest)
	if err != nil {
		return err
	}
	decodedSignature, err := decodeSignature(signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, canonical, decodedSignature) {
		return ErrInvalidSignature
	}
	return nil
}

func validateManifest(manifest DatasetManifest) error {
	if err := validateText("snapshot ID", manifest.SnapshotID, MaxSnapshotIDLength); err != nil {
		return err
	}
	if err := validateText("dataset", manifest.Dataset, MaxDatasetLength); err != nil {
		return err
	}
	if manifest.Revision <= 0 {
		return fmt.Errorf("%w: revision must be greater than zero", ErrInvalidInput)
	}
	if manifest.ItemCount < 0 {
		return fmt.Errorf("%w: item count must not be negative", ErrInvalidInput)
	}
	if manifest.PageCount < 0 {
		return fmt.Errorf("%w: page count must not be negative", ErrInvalidInput)
	}
	if err := validateDigest(manifest.PayloadDigest); err != nil {
		return fmt.Errorf("payload digest: %w", err)
	}
	return nil
}

func validateText(field string, value string, maxLength int) error {
	if len(value) == 0 || len(value) > maxLength {
		return fmt.Errorf("%w: %s length must be between 1 and %d bytes", ErrInvalidInput, field, maxLength)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalidInput, field)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s must not contain control characters", ErrInvalidInput, field)
		}
	}
	return nil
}

func validateDigest(digest string) error {
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("%w: must contain exactly 64 hexadecimal characters", ErrInvalidDigest)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decoded) != digest {
		return fmt.Errorf("%w: must use lowercase hexadecimal encoding", ErrInvalidDigest)
	}
	return nil
}

func decodeSignature(signature string) ([]byte, error) {
	if len(signature) != base64.StdEncoding.EncodedLen(ed25519.SignatureSize) {
		return nil, fmt.Errorf("%w: invalid standard-base64 length", ErrInvalidSignature)
	}
	decoded, err := strictBase64.DecodeString(signature)
	if err != nil || len(decoded) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(decoded) != signature {
		return nil, fmt.Errorf("%w: must use canonical padded standard base64", ErrInvalidSignature)
	}
	return decoded, nil
}
