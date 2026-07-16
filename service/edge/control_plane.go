package edge

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
)

const (
	controlRequestKindBootstrap        = "bootstrap"
	controlRequestKindHeartbeat        = "heartbeat"
	controlRequestKindSnapshotManifest = "snapshot_manifest"
	controlRequestKindSnapshotPage     = "snapshot_page"
	controlRequestKindLeaseAcquire     = "lease_acquire"
	controlRequestKindLeaseClose       = "lease_close"
	controlRequestKindSettlementBlock  = "settlement_block"
	defaultControlReceiptTTLSeconds    = 86400
	maximumControlReceiptTTLSeconds    = 604800
)

const (
	ControlRequestKindBootstrap        = controlRequestKindBootstrap
	ControlRequestKindHeartbeat        = controlRequestKindHeartbeat
	ControlRequestKindSnapshotManifest = controlRequestKindSnapshotManifest
	ControlRequestKindSnapshotPage     = controlRequestKindSnapshotPage
	ControlRequestKindLeaseAcquire     = controlRequestKindLeaseAcquire
	ControlRequestKindLeaseClose       = controlRequestKindLeaseClose
	ControlRequestKindSettlementBlock  = controlRequestKindSettlementBlock
)

func ProcessBootstrap(principal *ControlPrincipal, request dto.EdgeBootstrapRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindBootstrap, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindBootstrap, request.Meta.RequestID, serverRequestID, now, err)
	}
	return ExecuteControlMutation(principal, controlRequestKindBootstrap, controlReceiptTTL(), func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		updated, err := model.UpdateEdgeNodeDeclarationTx(tx, identity.Node.NodeUID, identity.Node.Generation, declarationUpdate(request.Declaration, request.Snapshot.Revision, now))
		if err != nil {
			return nil, err
		}
		if !updated {
			return nil, ErrControlAuthentication
		}
		bundle, err := model.GetLatestPublishedEdgeCompiledSnapshotManifestTx(tx, now.Unix())
		if err != nil {
			return nil, err
		}
		control, err := buildNodeControlConfig(identity.Node, bundle)
		if err != nil {
			return nil, err
		}
		response := dto.EdgeBootstrapResponseV1{
			Meta:     NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now),
			Control:  control,
			Snapshot: bundle.Manifest,
		}
		if err := response.Meta.Validate(); err != nil {
			return nil, err
		}
		if err := response.Control.Validate(); err != nil {
			return nil, err
		}
		if err := response.Snapshot.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: bundle.Manifest.SnapshotID, Response: response}, nil
	})
}

func ProcessHeartbeat(principal *ControlPrincipal, request dto.EdgeHeartbeatRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindHeartbeat, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindHeartbeat, request.Meta.RequestID, serverRequestID, now, err)
	}
	return ExecuteControlMutation(principal, controlRequestKindHeartbeat, controlReceiptTTL(), func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		updated, err := model.UpdateEdgeNodeDeclarationTx(tx, identity.Node.NodeUID, identity.Node.Generation, declarationUpdate(request.Declaration, request.Snapshot.Revision, now))
		if err != nil {
			return nil, err
		}
		if !updated {
			return nil, ErrControlAuthentication
		}
		if err := model.UpsertEdgeNodeHeartbeatTx(tx, identity.Node.ID, identity.Node.Generation, model.EdgeNodeHeartbeatObservation{
			Snapshot:   request.Snapshot,
			Settlement: request.Settlement,
			Leases:     request.Leases,
			Runtime:    request.Runtime,
			CPA:        request.CPA,
			ObservedAt: now.Unix(),
		}); err != nil {
			return nil, err
		}
		bundle, err := model.GetLatestPublishedEdgeCompiledSnapshotManifestTx(tx, now.Unix())
		if err != nil {
			return nil, err
		}
		control, err := buildNodeControlConfig(identity.Node, bundle)
		if err != nil {
			return nil, err
		}
		response := dto.EdgeHeartbeatResponseV1{
			Meta:    NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now),
			Control: control,
		}
		if snapshotStateChanged(request.Snapshot, bundle.Manifest) {
			manifest := bundle.Manifest
			response.Snapshot = &manifest
		}
		if err := response.Meta.Validate(); err != nil {
			return nil, err
		}
		if err := response.Control.Validate(); err != nil {
			return nil, err
		}
		if response.Snapshot != nil {
			if err := response.Snapshot.Validate(); err != nil {
				return nil, err
			}
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: bundle.Manifest.SnapshotID, Response: response}, nil
	})
}

func ProcessSnapshotManifest(principal *ControlPrincipal, request dto.EdgeSnapshotManifestRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSnapshotManifest, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSnapshotManifest, request.Meta.RequestID, serverRequestID, now, err)
	}
	return ExecuteControlMutation(principal, controlRequestKindSnapshotManifest, controlReceiptTTL(), func(tx *gorm.DB, _ *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		bundle, err := model.GetLatestPublishedEdgeCompiledSnapshotManifestTx(tx, now.Unix())
		if err != nil {
			return nil, err
		}
		response := dto.EdgeSnapshotManifestResponseV1{
			Meta:    NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now),
			Changed: snapshotStateChanged(request.Current, bundle.Manifest),
		}
		if response.Changed {
			manifest := bundle.Manifest
			response.Snapshot = &manifest
		}
		if err := response.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: bundle.Manifest.SnapshotID, Response: response}, nil
	})
}

func ProcessSnapshotPage(principal *ControlPrincipal, request dto.EdgeSnapshotPageRequestV1, serverRequestID string, now time.Time) (*ControlHTTPResponse, error) {
	if err := request.Validate(); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSnapshotPage, request.Meta.RequestID, serverRequestID, now, err)
	}
	if err := validateControlRequestCorrelation(principal, request.Meta); err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSnapshotPage, request.Meta.RequestID, serverRequestID, now, err)
	}
	ordinal, err := parseSnapshotCursor(request.Cursor)
	if err != nil {
		return PersistInvalidControlRequest(principal, controlRequestKindSnapshotPage, request.Meta.RequestID, serverRequestID, now, err)
	}
	return ExecuteControlMutation(principal, controlRequestKindSnapshotPage, controlReceiptTTL(), func(tx *gorm.DB, _ *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		bundle, err := model.GetEdgeCompiledSnapshotManifestTx(tx, request.SnapshotID, now.Unix())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return snapshotNotFoundResult(request.Meta.RequestID, serverRequestID, now, tx)
		}
		if err != nil {
			return nil, err
		}
		var datasetManifest *dto.EdgeSnapshotDatasetManifestV1
		for i := range bundle.Manifest.Datasets {
			if bundle.Manifest.Datasets[i].Dataset == request.Dataset {
				datasetManifest = &bundle.Manifest.Datasets[i]
				break
			}
		}
		if datasetManifest == nil || ordinal >= datasetManifest.PageCount {
			return snapshotNotFoundResult(request.Meta.RequestID, serverRequestID, now, tx)
		}
		page, err := model.GetPublishedEdgeCompiledSnapshotPageTx(tx, request.SnapshotID, request.Dataset, ordinal, now.Unix())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return snapshotNotFoundResult(request.Meta.RequestID, serverRequestID, now, tx)
		}
		if err != nil {
			return nil, err
		}
		if page.ItemCount > int64(request.Limit) {
			return invalidControlResult(request.Meta.RequestID, serverRequestID, now, "snapshot page limit is smaller than the immutable page")
		}
		var payload dto.EdgeSnapshotPagePayloadV1
		if err := common.DecodeJsonStrict(bytes.NewReader([]byte(page.Payload)), &payload); err != nil {
			return nil, err
		}
		response := dto.EdgeSnapshotPageResponseV1{
			Meta:       NewControlResponseMeta(request.Meta.RequestID, serverRequestID, now),
			SnapshotID: request.SnapshotID,
			Dataset:    request.Dataset,
			Revision:   datasetManifest.Revision,
			Cursor:     request.Cursor,
			ItemCount:  int(page.ItemCount),
			Digest:     page.Digest,
			Payload:    payload,
		}
		if ordinal+1 < datasetManifest.PageCount {
			response.NextCursor = fmt.Sprintf("p%d", ordinal+1)
		}
		if err := response.Validate(); err != nil {
			return nil, err
		}
		return &ControlMutationResult{StatusCode: http.StatusOK, ResultRef: request.SnapshotID, Response: response}, nil
	})
}

func PersistInvalidControlRequest(principal *ControlPrincipal, requestKind string, clientRequestID string, serverRequestID string, now time.Time, validationErr error) (*ControlHTTPResponse, error) {
	if validationErr == nil {
		validationErr = errors.New("invalid control request")
	}
	return ExecuteControlMutation(principal, requestKind, controlReceiptTTL(), func(_ *gorm.DB, _ *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		return invalidControlResult(safeControlRequestID(clientRequestID), serverRequestID, now, validationErr.Error())
	})
}

func invalidControlResult(clientRequestID string, serverRequestID string, now time.Time, message string) (*ControlMutationResult, error) {
	response, err := NewControlErrorResponse(
		http.StatusBadRequest,
		dto.EdgeControlErrorCodeInvalidRequestV1,
		message,
		false,
		safeControlRequestID(clientRequestID),
		serverRequestID,
		now,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return &ControlMutationResult{StatusCode: http.StatusBadRequest, Response: response}, nil
}

func snapshotNotFoundResult(clientRequestID string, serverRequestID string, now time.Time, tx *gorm.DB) (*ControlMutationResult, error) {
	var expected *dto.EdgeControlExpectedStateV1
	latest, err := model.GetLatestPublishedEdgeCompiledSnapshotManifestTx(tx, now.Unix())
	if err == nil {
		revision := latest.Manifest.Revision
		expected = &dto.EdgeControlExpectedStateV1{
			SnapshotID:       latest.Manifest.SnapshotID,
			SnapshotRevision: &revision,
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	response, err := NewControlErrorResponse(
		http.StatusNotFound,
		dto.EdgeControlErrorCodeSnapshotNotFoundV1,
		"snapshot page is not available",
		false,
		clientRequestID,
		serverRequestID,
		now,
		nil,
		expected,
	)
	if err != nil {
		return nil, err
	}
	return &ControlMutationResult{StatusCode: http.StatusNotFound, Response: response}, nil
}

func buildNodeControlConfig(node *model.EdgeNode, bundle *model.EdgeCompiledSnapshotManifest) (dto.EdgeNodeControlConfigV1, error) {
	if node == nil || bundle == nil {
		return dto.EdgeNodeControlConfigV1{}, errors.New("edge control configuration dependencies are missing")
	}
	config := dto.EdgeNodeControlConfigV1{
		NodeID:                      node.NodeUID,
		NodeGeneration:              node.Generation,
		Enabled:                     node.Status == model.EdgeNodeStatusActive,
		HeartbeatIntervalSeconds:    int64(common.GetEnvOrDefault("EDGE_HEARTBEAT_INTERVAL_SECONDS", 30)),
		SnapshotPollIntervalSeconds: int64(common.GetEnvOrDefault("EDGE_SNAPSHOT_POLL_INTERVAL_SECONDS", 60)),
		SnapshotPageLimit:           common.GetEnvOrDefault("EDGE_SNAPSHOT_PAGE_LIMIT", 500),
		SettlementMaxEvents:         common.GetEnvOrDefault("EDGE_SETTLEMENT_MAX_EVENTS", 500),
		SettlementMaxDelaySeconds:   int64(common.GetEnvOrDefault("EDGE_SETTLEMENT_MAX_DELAY_SECONDS", 10)),
		ClockSkewToleranceSeconds:   int64(common.GetEnvOrDefault("EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS", 120)),
		SnapshotVerificationKeys:    []dto.EdgeSnapshotVerificationKeyV1{bundle.VerificationKey},
	}
	if err := config.Validate(); err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	return config, nil
}

func declarationUpdate(declaration dto.EdgeNodeDeclarationV1, lastPolicyVersion int64, now time.Time) model.EdgeNodeDeclarationUpdate {
	return model.EdgeNodeDeclarationUpdate{
		Name:              declaration.Name,
		Region:            declaration.Region,
		PublicURL:         declaration.PublicURL,
		SoftwareVersion:   declaration.SoftwareVersion,
		StartedAt:         declaration.StartedAtUnixMilli / 1000,
		Capabilities:      declaration.Capabilities,
		LastPolicyVersion: lastPolicyVersion,
		LastSeenAt:        now.Unix(),
	}
}

func snapshotStateChanged(current dto.EdgeSnapshotStateV1, manifest dto.EdgeSnapshotManifestV1) bool {
	if current.SnapshotID != manifest.SnapshotID || current.Revision != manifest.Revision || len(current.Datasets) != len(manifest.Datasets) {
		return true
	}
	revisions := make(map[dto.EdgeSnapshotDatasetV1]int64, len(current.Datasets))
	for _, dataset := range current.Datasets {
		revisions[dataset.Dataset] = dataset.Revision
	}
	for _, dataset := range manifest.Datasets {
		if revisions[dataset.Dataset] != dataset.Revision {
			return true
		}
	}
	return false
}

func parseSnapshotCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	if !strings.HasPrefix(cursor, "p") || len(cursor) < 2 {
		return 0, errors.New("snapshot cursor is invalid")
	}
	ordinal, err := strconv.Atoi(cursor[1:])
	if err != nil || ordinal <= 0 || ordinal >= dto.EdgeControlMaxSnapshotPagesV1 || fmt.Sprintf("p%d", ordinal) != cursor {
		return 0, errors.New("snapshot cursor is invalid")
	}
	return ordinal, nil
}

func validateControlRequestCorrelation(principal *ControlPrincipal, meta dto.EdgeControlRequestMetaV1) error {
	if principal == nil || principal.SignedRequest == nil {
		return errors.New("edge control principal is missing")
	}
	if meta.RequestID != principal.SignedRequest.Metadata.IdempotencyKey {
		return errors.New("body request_id must equal the signed idempotency key")
	}
	return nil
}

func safeControlRequestID(requestID string) string {
	if edgeauth.ValidateIdempotencyKey(requestID) != nil {
		return ""
	}
	return requestID
}

func controlReceiptTTL() time.Duration {
	seconds := common.GetEnvOrDefault("EDGE_CONTROL_RECEIPT_TTL_SECONDS", defaultControlReceiptTTLSeconds)
	if seconds <= 0 || seconds > maximumControlReceiptTTLSeconds {
		seconds = defaultControlReceiptTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}
