package edge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesnapshot"
	coreservice "github.com/QuantumNous/new-api/service"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	edgeBootstrapRetryDelay      = 2 * time.Second
	edgeSnapshotPageAttempts     = 3
	edgeSnapshotPageRetryDelay   = 25 * time.Millisecond
	maxEdgeSnapshotDownloadItems = dto.EdgeControlMaxSnapshotItemsV1
)

var (
	edgeControlReady        atomic.Bool
	activeEdgeControlClient atomic.Pointer[EdgeControlClient]
	activeEdgeControlConfig atomic.Pointer[dto.EdgeNodeControlConfigV1]
	edgeSettlementUploadMu  sync.Mutex
)

// EdgeControlLocalStore is the durable edge-local boundary used by the
// control loop. Implementations must apply a complete snapshot atomically and
// preserve the exact pending settlement request across retries and restarts.
type EdgeControlLocalStore interface {
	SnapshotState(context.Context) (*dto.EdgeSnapshotStateV1, error)
	SnapshotExpiry(context.Context) (int64, error)
	SettlementState(context.Context) (*dto.EdgeSettlementStateV1, error)
	BalanceState(context.Context) (*model.EdgeLocalBalanceState, error)
	ApplySnapshot(context.Context, model.EdgeLocalSnapshotProjectionData) error
	ApplyControl(context.Context, dto.EdgeNodeControlConfigV1, int64) error
	ApplyBalance(context.Context, dto.EdgeNodeControlConfigV1, dto.EdgeBalanceDeltaV2, int64) error
	RefreshChannelRuntime(context.Context) error
	InstallRoutingPolicy(context.Context) error
	PendingSettlementBlock(context.Context) (*dto.EdgeSettlementBlockRequestV1, error)
	BuildSettlementBlock(context.Context, dto.EdgeControlRequestMetaV1, string, int, int64, int64) (*dto.EdgeSettlementBlockRequestV1, error)
	RefreshSettlementRequest(context.Context, string, dto.EdgeControlRequestMetaV1, int64) (*dto.EdgeSettlementBlockRequestV1, error)
	AcknowledgeSettlement(context.Context, dto.EdgeSettlementAckV1) error
}

type EdgeControlLoopDependencies struct {
	Client        *EdgeControlClient
	Store         EdgeControlLocalStore
	RuntimeStatus func() dto.EdgeRuntimeStatusV1
	Ready         func(bool)
	Now           func() time.Time
}

type edgeControlLoop struct {
	client        *EdgeControlClient
	store         EdgeControlLocalStore
	runtimeStatus func() dto.EdgeRuntimeStatusV1
	setReady      func(bool)
	now           func() time.Time
}

type edgeControlGormStore struct {
	db *gorm.DB
}

// EdgeControlReady reports whether bootstrap completed and a fully verified
// snapshot is available in the edge-local database for request admission.
func EdgeControlReady() bool {
	return edgeControlReady.Load()
}

// ActiveEdgeControlClient returns the single validated client owned by the
// running control loop. Edge funding paths reuse it instead of reparsing key
// material or constructing a second transport.
func ActiveEdgeControlClient() (*EdgeControlClient, bool) {
	client := activeEdgeControlClient.Load()
	return client, client != nil
}

func ActiveEdgeControlConfig() (*dto.EdgeNodeControlConfigV1, bool) {
	config := activeEdgeControlConfig.Load()
	if config == nil {
		return nil, false
	}
	copy := *config
	copy.SnapshotVerificationKeys = append([]dto.EdgeSnapshotVerificationKeyV1(nil), config.SnapshotVerificationKeys...)
	return &copy, true
}

func storeActiveEdgeControlConfig(config dto.EdgeNodeControlConfigV1) {
	copy := config
	copy.SnapshotVerificationKeys = append([]dto.EdgeSnapshotVerificationKeyV1(nil), config.SnapshotVerificationKeys...)
	activeEdgeControlConfig.Store(&copy)
}

// RunEdgeControlLoops loads the edge control-plane configuration from the
// environment and runs bootstrap, heartbeat, snapshot polling and settlement
// upload until ctx is cancelled.
func RunEdgeControlLoops(ctx context.Context) error {
	startedAt := time.Unix(common.StartTime, 0)
	config, err := LoadEdgeControlClientConfigFromEnv(startedAt)
	if err != nil {
		return err
	}
	client, err := NewEdgeControlClient(config)
	if err != nil {
		return err
	}
	if model.DB == nil {
		return errors.New("edge local database is not initialized")
	}
	return RunEdgeControlLoopsWithDependencies(ctx, EdgeControlLoopDependencies{
		Client: client,
		Store:  &edgeControlGormStore{db: model.DB},
		RuntimeStatus: func() dto.EdgeRuntimeStatusV1 {
			uptime := time.Now().Unix() - common.StartTime
			if uptime < 0 {
				uptime = 0
			}
			return dto.EdgeRuntimeStatusV1{UptimeSeconds: uptime}
		},
	})
}

// RunEdgeControlLoopsWithDependencies is the deterministic orchestration
// entry used by tests and alternate edge runtime integrations.
func RunEdgeControlLoopsWithDependencies(ctx context.Context, dependencies EdgeControlLoopDependencies) error {
	if ctx == nil {
		return errors.New("edge control context is nil")
	}
	if dependencies.Client == nil {
		return errors.New("edge control client is nil")
	}
	if dependencies.Store == nil {
		return errors.New("edge control local store is nil")
	}
	if !activeEdgeControlClient.CompareAndSwap(nil, dependencies.Client) {
		return errors.New("edge control loop is already running")
	}
	defer activeEdgeControlClient.CompareAndSwap(dependencies.Client, nil)
	defer activeEdgeControlConfig.Store(nil)
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	runtimeStatus := dependencies.RuntimeStatus
	if runtimeStatus == nil {
		runtimeStatus = func() dto.EdgeRuntimeStatusV1 { return dto.EdgeRuntimeStatusV1{} }
	}
	setReady := func(ready bool) {
		if !ready {
			edgeControlReady.Store(false)
		} else {
			edgeControlReady.Store(true)
		}
		if dependencies.Ready != nil {
			dependencies.Ready(ready)
		}
	}
	runner := edgeControlLoop{
		client: dependencies.Client, store: dependencies.Store,
		runtimeStatus: runtimeStatus, setReady: setReady, now: now,
	}
	return runner.run(ctx)
}

func (r *edgeControlLoop) run(ctx context.Context) error {
	r.setReady(false)
	edgeBalanceReady.Store(false)
	defer r.setReady(false)

	if err := r.restoreLocalReadiness(ctx); err != nil {
		return fmt.Errorf("restore edge local readiness: %w", err)
	}
	var control dto.EdgeNodeControlConfigV1
	for {
		var err error
		control, err = r.bootstrap(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		if edgeControlFatalError(err) {
			return fmt.Errorf("edge control bootstrap: %w", err)
		}
		common.SysError("edge control bootstrap failed: " + err.Error())
		if !EdgeControlReady() {
			restoreErr := r.restoreLocalReadiness(ctx)
			if restoreErr != nil {
				return fmt.Errorf("restore edge local readiness while bootstrap is unavailable: %w", restoreErr)
			}
		}
		timer := time.NewTimer(edgeBootstrapRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	if !EdgeControlReady() {
		return errors.New("edge bootstrap completed without an active verified data plane")
	}
	storeActiveEdgeControlConfig(control)

	heartbeatTicker := time.NewTicker(time.Duration(control.HeartbeatIntervalSeconds) * time.Second)
	snapshotTicker := time.NewTicker(time.Duration(control.SnapshotPollIntervalSeconds) * time.Second)
	settlementTicker := time.NewTicker(time.Duration(control.SettlementMaxDelaySeconds) * time.Second)
	defer heartbeatTicker.Stop()
	defer snapshotTicker.Stop()
	defer settlementTicker.Stop()

	if err := r.flushSettlement(ctx, control); err != nil {
		if edgeControlFatalError(err) {
			return err
		}
		common.SysError("edge settlement upload failed: " + err.Error())
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeatTicker.C:
			updated, heartbeatErr := r.heartbeat(ctx, control)
			if heartbeatErr != nil {
				if edgeControlFatalError(heartbeatErr) {
					return heartbeatErr
				}
				common.SysError("edge heartbeat failed: " + heartbeatErr.Error())
				continue
			}
			if updated.HeartbeatIntervalSeconds != control.HeartbeatIntervalSeconds {
				heartbeatTicker.Reset(time.Duration(updated.HeartbeatIntervalSeconds) * time.Second)
			}
			if updated.SnapshotPollIntervalSeconds != control.SnapshotPollIntervalSeconds {
				snapshotTicker.Reset(time.Duration(updated.SnapshotPollIntervalSeconds) * time.Second)
			}
			if updated.SettlementMaxDelaySeconds != control.SettlementMaxDelaySeconds {
				settlementTicker.Reset(time.Duration(updated.SettlementMaxDelaySeconds) * time.Second)
			}
			control = updated
			storeActiveEdgeControlConfig(updated)
		case <-snapshotTicker.C:
			if pollErr := r.pollSnapshot(ctx, control); pollErr != nil {
				if edgeControlFatalError(pollErr) {
					return pollErr
				}
				common.SysError("edge snapshot poll failed: " + pollErr.Error())
			}
		case <-settlementTicker.C:
			if settlementErr := r.flushSettlement(ctx, control); settlementErr != nil {
				if edgeControlFatalError(settlementErr) {
					return settlementErr
				}
				common.SysError("edge settlement upload failed: " + settlementErr.Error())
			}
		}
	}
}

func (r *edgeControlLoop) restoreLocalReadiness(ctx context.Context) error {
	state, err := r.store.SnapshotState(ctx)
	if err != nil {
		return err
	}
	if state.SnapshotID == "" || state.Revision <= 0 || state.AppliedAtUnixMilli <= 0 || len(state.Datasets) != len(edgeSnapshotDatasetOrder) {
		return nil
	}
	for i, dataset := range state.Datasets {
		if dataset.Dataset != edgeSnapshotDatasetOrder[i] || dataset.Revision <= 0 {
			return fmt.Errorf("%w: persisted snapshot dataset state is incomplete", ErrEdgeControlProtocolViolation)
		}
	}
	balance, err := r.store.BalanceState(ctx)
	if err != nil {
		return err
	}
	edgeBalanceReady.Store(balance.Initialized && balance.Revision > 0)
	edgeSettlementCircuitOpen.Store(balance.SettlementCircuitOpen)
	if err := withEdgeDataPlanePolicyMutation(func() error {
		r.transitionDataPlaneNotReady()
		if err := r.store.RefreshChannelRuntime(ctx); err != nil {
			return err
		}
		return r.store.InstallRoutingPolicy(ctx)
	}); err != nil {
		return err
	}
	return r.activateSnapshot(0)
}

func (r *edgeControlLoop) bootstrap(ctx context.Context) (dto.EdgeNodeControlConfigV1, error) {
	snapshot, err := r.store.SnapshotState(ctx)
	if err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	settlement, err := r.store.SettlementState(ctx)
	if err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	response, err := r.client.Bootstrap(ctx, dto.EdgeBootstrapRequestV1{
		SupportedProtocolVersions: []string{dto.EdgeControlProtocolVersionV2, dto.EdgeControlProtocolVersionV1},
		Declaration:               r.client.Declaration(),
		Snapshot:                  *snapshot,
		Settlement:                *settlement,
	})
	if err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	if response.SettlementAck != nil {
		if err := r.store.AcknowledgeSettlement(ctx, *response.SettlementAck); err != nil {
			return dto.EdgeNodeControlConfigV1{}, err
		}
	}
	if _, err := r.syncSnapshot(ctx, response.Snapshot, response.Control); err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	updated, err := r.heartbeat(ctx, response.Control)
	if err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	balance, err := r.store.BalanceState(ctx)
	if err != nil {
		return dto.EdgeNodeControlConfigV1{}, err
	}
	if !balance.Initialized || balance.Revision <= 0 {
		return dto.EdgeNodeControlConfigV1{}, fmt.Errorf("%w: bootstrap heartbeat did not initialize balances", ErrEdgeControlProtocolViolation)
	}
	return updated, nil
}

func (r *edgeControlLoop) heartbeat(ctx context.Context, current dto.EdgeNodeControlConfigV1) (dto.EdgeNodeControlConfigV1, error) {
	snapshot, err := r.store.SnapshotState(ctx)
	if err != nil {
		return current, err
	}
	settlement, err := r.store.SettlementState(ctx)
	if err != nil {
		return current, err
	}
	balance, err := r.store.BalanceState(ctx)
	if err != nil {
		return current, err
	}
	response, err := r.client.Heartbeat(ctx, dto.EdgeHeartbeatRequestV1{
		Declaration: r.client.Declaration(), Snapshot: *snapshot, Settlement: *settlement,
		BalanceRevision: balance.Revision, Runtime: r.runtimeStatus(), CPA: nil,
	})
	if err != nil {
		return current, err
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	if err := r.store.ApplyControl(ctx, response.Control, now().UTC().UnixMilli()); err != nil {
		return current, err
	}
	edgeSettlementCircuitOpen.Store(response.Control.SettlementCircuitOpen)
	if response.SettlementAck != nil {
		if err := r.store.AcknowledgeSettlement(ctx, *response.SettlementAck); err != nil {
			return current, err
		}
	}
	if response.BalanceDelta != nil {
		if err := r.store.ApplyBalance(ctx, response.Control, *response.BalanceDelta, now().UTC().UnixMilli()); err != nil {
			edgeBalanceReady.Store(false)
			return current, err
		}
		edgeBalanceReady.Store(true)
	}
	if response.Snapshot != nil {
		if _, err := r.syncSnapshot(ctx, *response.Snapshot, response.Control); err != nil {
			return current, err
		}
	}
	return response.Control, nil
}

func (r *edgeControlLoop) transitionDataPlaneNotReady() {
	if edgeControlReady.Load() {
		r.setDataPlaneReady(false)
	}
}

func (r *edgeControlLoop) setDataPlaneReady(ready bool) {
	if r.setReady != nil {
		r.setReady(ready)
		return
	}
	if !ready {
		edgeControlReady.Store(false)
		return
	}
	edgeControlReady.Store(true)
}

func (r *edgeControlLoop) activateSnapshot(_ int64) error {
	return withEdgeDataPlanePolicyMutation(func() error {
		r.setDataPlaneReady(true)
		return nil
	})
}

func (r *edgeControlLoop) pollSnapshot(ctx context.Context, control dto.EdgeNodeControlConfigV1) error {
	current, err := r.store.SnapshotState(ctx)
	if err != nil {
		return err
	}
	response, err := r.client.SnapshotManifest(ctx, dto.EdgeSnapshotManifestRequestV1{Current: *current})
	if err != nil {
		return err
	}
	if !response.Changed {
		if !EdgeControlReady() {
			return r.restoreLocalReadiness(ctx)
		}
		return nil
	}
	_, err = r.syncSnapshot(ctx, *response.Snapshot, control)
	return err
}

func (r *edgeControlLoop) flushSettlement(ctx context.Context, control dto.EdgeNodeControlConfigV1) error {
	if control.SettlementCircuitOpen {
		return nil
	}
	edgeSettlementUploadMu.Lock()
	defer edgeSettlementUploadMu.Unlock()
	block, err := r.store.PendingSettlementBlock(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		meta, metaErr := r.client.NewRequestMeta("settlement")
		if metaErr != nil {
			return metaErr
		}
		block, err = r.store.BuildSettlementBlock(
			ctx, meta, "block-"+uuid.NewString(), control.SettlementMaxEvents, r.now().UTC().UnixMilli(), control.SettlementCircuitEpoch,
		)
	}
	if errors.Is(err, model.ErrEdgeLocalNoPendingUsageEvents) {
		return nil
	}
	if err != nil {
		return err
	}
	meta, err := r.client.NewRequestMeta("settlement")
	if err != nil {
		return err
	}
	block, err = r.store.RefreshSettlementRequest(ctx, block.BlockID, meta, control.SettlementCircuitEpoch)
	if err != nil {
		return err
	}
	response, err := r.client.SubmitSettlement(ctx, *block)
	if err != nil {
		return err
	}
	return r.store.AcknowledgeSettlement(ctx, response.Ack)
}

func (r *edgeControlLoop) syncSnapshot(ctx context.Context, manifest dto.EdgeSnapshotManifestV1, control dto.EdgeNodeControlConfigV1) (bool, error) {
	if err := verifyEdgeSnapshotManifest(manifest, control, r.now()); err != nil {
		return false, err
	}
	current, err := r.store.SnapshotState(ctx)
	if err != nil {
		return false, err
	}
	localExpiry, err := r.store.SnapshotExpiry(ctx)
	if err != nil {
		return false, err
	}
	if edgeSnapshotStateMatchesManifest(*current, manifest) {
		if localExpiry != manifest.ExpiresAtUnixMilli {
			return false, fmt.Errorf("%w: persisted snapshot expiry differs from the immutable manifest", ErrEdgeControlProtocolViolation)
		}
		if err := withEdgeDataPlanePolicyMutation(func() error {
			r.transitionDataPlaneNotReady()
			if err := r.store.RefreshChannelRuntime(ctx); err != nil {
				return err
			}
			if err := r.store.InstallRoutingPolicy(ctx); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return false, err
		}
		if err := r.activateSnapshot(manifest.ExpiresAtUnixMilli); err != nil {
			return false, err
		}
		return false, nil
	}
	projection, err := r.downloadEdgeSnapshot(ctx, manifest, control.SnapshotPageLimit)
	if err != nil {
		return false, err
	}
	if err := withEdgeDataPlanePolicyMutation(func() error {
		r.transitionDataPlaneNotReady()
		if err := r.store.ApplySnapshot(ctx, projection); err != nil {
			return err
		}
		if err := r.store.InstallRoutingPolicy(ctx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := r.activateSnapshot(manifest.ExpiresAtUnixMilli); err != nil {
		return false, err
	}
	return true, nil
}

func verifyEdgeSnapshotManifest(manifest dto.EdgeSnapshotManifestV1, control dto.EdgeNodeControlConfigV1, now time.Time) error {
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("%w: invalid snapshot manifest: %v", ErrEdgeControlProtocolViolation, err)
	}
	if err := control.Validate(); err != nil {
		return fmt.Errorf("%w: invalid control configuration: %v", ErrEdgeControlProtocolViolation, err)
	}
	if len(manifest.Datasets) != len(edgeSnapshotDatasetOrder) {
		return fmt.Errorf("%w: snapshot must contain all seven datasets", ErrEdgeControlProtocolViolation)
	}
	skewMillis := control.ClockSkewToleranceSeconds * int64(time.Second/time.Millisecond)
	nowMillis := now.UTC().UnixMilli()
	if manifest.CreatedAtUnixMilli > nowMillis+skewMillis {
		return fmt.Errorf("%w: snapshot was created beyond the allowed clock skew", ErrEdgeControlProtocolViolation)
	}
	if manifest.ExpiresAtUnixMilli <= nowMillis {
		return fmt.Errorf("%w: snapshot has expired", ErrEdgeControlProtocolViolation)
	}
	keys := make(map[string]dto.EdgeSnapshotVerificationKeyV1, len(control.SnapshotVerificationKeys))
	for _, key := range control.SnapshotVerificationKeys {
		keys[key.KeyID] = key
	}
	aggregate := make([]edgesnapshot.DatasetManifest, 0, len(manifest.Datasets))
	var totalItems int64
	for index, dataset := range manifest.Datasets {
		if dataset.Dataset != edgeSnapshotDatasetOrder[index] {
			return fmt.Errorf("%w: snapshot datasets are not in the required complete order", ErrEdgeControlProtocolViolation)
		}
		totalItems += dataset.ItemCount
		if totalItems > maxEdgeSnapshotDownloadItems {
			return fmt.Errorf("%w: snapshot exceeds the edge download item limit", ErrEdgeControlProtocolViolation)
		}
		key, exists := keys[dataset.DetachedSignature.KeyID]
		if !exists {
			return fmt.Errorf("%w: no verification key for dataset %s", ErrEdgeControlProtocolViolation, dataset.Dataset)
		}
		if key.NotBeforeUnixMilli > manifest.CreatedAtUnixMilli || key.ExpiresAtUnixMilli < manifest.ExpiresAtUnixMilli {
			return fmt.Errorf("%w: verification key %s does not cover the snapshot lifetime", ErrEdgeControlProtocolViolation, key.KeyID)
		}
		publicKey, err := edgeauth.ParsePublicKey(key.PublicKey)
		if err != nil {
			return fmt.Errorf("%w: parse snapshot verification key: %v", ErrEdgeControlProtocolViolation, err)
		}
		primitive := edgeSnapshotPrimitiveManifest(manifest.SnapshotID, dataset)
		if err := edgesnapshot.VerifyDatasetManifest(publicKey, primitive, dataset.DetachedSignature.Value); err != nil {
			return fmt.Errorf("%w: verify dataset %s: %v", ErrEdgeControlProtocolViolation, dataset.Dataset, err)
		}
		aggregate = append(aggregate, primitive)
	}
	digest, err := edgesnapshot.AggregateDatasetManifests(manifest.SnapshotID, manifest.Revision, aggregate)
	if err != nil {
		return fmt.Errorf("%w: aggregate snapshot manifest: %v", ErrEdgeControlProtocolViolation, err)
	}
	if digest != manifest.Digest {
		return fmt.Errorf("%w: snapshot digest does not match its dataset manifests", ErrEdgeControlProtocolViolation)
	}
	return nil
}

func edgeSnapshotPrimitiveManifest(snapshotID string, manifest dto.EdgeSnapshotDatasetManifestV1) edgesnapshot.DatasetManifest {
	return edgesnapshot.DatasetManifest{
		SnapshotID: snapshotID, Dataset: string(manifest.Dataset), Revision: manifest.Revision,
		ItemCount: manifest.ItemCount, PageCount: manifest.PageCount, PayloadDigest: manifest.Digest,
	}
}

func edgeSnapshotStateMatchesManifest(state dto.EdgeSnapshotStateV1, manifest dto.EdgeSnapshotManifestV1) bool {
	if state.SnapshotID != manifest.SnapshotID || state.Revision != manifest.Revision || state.AppliedAtUnixMilli <= 0 || len(state.Datasets) != len(manifest.Datasets) {
		return false
	}
	for i := range manifest.Datasets {
		if state.Datasets[i].Dataset != manifest.Datasets[i].Dataset || state.Datasets[i].Revision != manifest.Datasets[i].Revision {
			return false
		}
	}
	return true
}

func (r *edgeControlLoop) downloadEdgeSnapshot(ctx context.Context, manifest dto.EdgeSnapshotManifestV1, pageLimit int) (model.EdgeLocalSnapshotProjectionData, error) {
	projection := model.EdgeLocalSnapshotProjectionData{
		State: dto.EdgeSnapshotStateV1{
			SnapshotID: manifest.SnapshotID, Revision: manifest.Revision,
			AppliedAtUnixMilli: r.now().UTC().UnixMilli(),
			Datasets:           make([]dto.EdgeSnapshotDatasetStateV1, 0, len(manifest.Datasets)),
		},
		Digest: manifest.Digest, ExpiresAtUnixMilli: manifest.ExpiresAtUnixMilli, TokenFingerprint: manifest.TokenFingerprint,
	}
	ordering := edgeSnapshotPageOrdering{}
	for _, dataset := range manifest.Datasets {
		projection.State.Datasets = append(projection.State.Datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset.Dataset, Revision: dataset.Revision})
		pageDigests := make([]string, 0, dataset.PageCount)
		var downloadedItems int64
		for ordinal := 0; ordinal < dataset.PageCount; ordinal++ {
			cursor := ""
			if ordinal > 0 {
				cursor = fmt.Sprintf("p%d", ordinal)
			}
			meta, err := r.client.NewRequestMeta("snapshot-page")
			if err != nil {
				return model.EdgeLocalSnapshotProjectionData{}, err
			}
			request := dto.EdgeSnapshotPageRequestV1{
				Meta: meta, SnapshotID: manifest.SnapshotID, Dataset: dataset.Dataset,
				Cursor: cursor, Limit: pageLimit,
			}
			page, err := r.fetchEdgeSnapshotPage(ctx, request)
			if err != nil {
				return model.EdgeLocalSnapshotProjectionData{}, err
			}
			if page.SnapshotID != manifest.SnapshotID || page.Dataset != dataset.Dataset || page.Revision != dataset.Revision || page.Cursor != cursor {
				return model.EdgeLocalSnapshotProjectionData{}, fmt.Errorf("%w: snapshot page identity does not match its request", ErrEdgeControlProtocolViolation)
			}
			expectedNext := ""
			if ordinal+1 < dataset.PageCount {
				expectedNext = fmt.Sprintf("p%d", ordinal+1)
			}
			if page.NextCursor != expectedNext {
				return model.EdgeLocalSnapshotProjectionData{}, fmt.Errorf("%w: snapshot page cursor sequence is invalid", ErrEdgeControlProtocolViolation)
			}
			_, digest, err := edgesnapshot.MarshalPagePayload(page.Payload)
			if err != nil {
				return model.EdgeLocalSnapshotProjectionData{}, err
			}
			if digest != page.Digest {
				return model.EdgeLocalSnapshotProjectionData{}, fmt.Errorf("%w: snapshot page payload digest mismatch", ErrEdgeControlProtocolViolation)
			}
			if err := appendEdgeSnapshotPage(&projection, &ordering, dataset.Dataset, page.Payload); err != nil {
				return model.EdgeLocalSnapshotProjectionData{}, err
			}
			pageDigests = append(pageDigests, digest)
			downloadedItems += int64(page.ItemCount)
		}
		digest, err := edgesnapshot.AggregatePageDigests(pageDigests)
		if err != nil {
			return model.EdgeLocalSnapshotProjectionData{}, err
		}
		if downloadedItems != dataset.ItemCount || digest != dataset.Digest {
			return model.EdgeLocalSnapshotProjectionData{}, fmt.Errorf("%w: downloaded dataset %s does not match its manifest", ErrEdgeControlProtocolViolation, dataset.Dataset)
		}
	}
	return projection, nil
}

func (r *edgeControlLoop) fetchEdgeSnapshotPage(ctx context.Context, request dto.EdgeSnapshotPageRequestV1) (*dto.EdgeSnapshotPageResponseV1, error) {
	var lastErr error
	for attempt := 0; attempt < edgeSnapshotPageAttempts; attempt++ {
		response, err := r.client.SnapshotPage(ctx, request)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if !edgeSnapshotPageRetryable(err) || attempt+1 == edgeSnapshotPageAttempts {
			break
		}
		delay := edgeSnapshotPageRetryDelay * time.Duration(1<<attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func edgeSnapshotPageRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrEdgeControlProtocolViolation) {
		return false
	}
	var remote *EdgeControlRemoteError
	if errors.As(err, &remote) {
		return remote.Retryable()
	}
	return true
}

type edgeSnapshotPageOrdering struct {
	authentication string
	userID         int64
	group          string
	model          string
	channelID      int64
	pricing        string
	routingCount   int
}

func appendEdgeSnapshotPage(projection *model.EdgeLocalSnapshotProjectionData, ordering *edgeSnapshotPageOrdering, dataset dto.EdgeSnapshotDatasetV1, payload dto.EdgeSnapshotPagePayloadV1) error {
	switch dataset {
	case dto.EdgeSnapshotDatasetAuthenticationV1:
		if ordering.authentication != "" && payload.Authentication[0].TokenFingerprint <= ordering.authentication {
			return fmt.Errorf("%w: authentication pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.authentication = payload.Authentication[len(payload.Authentication)-1].TokenFingerprint
		projection.Authentication = append(projection.Authentication, payload.Authentication...)
	case dto.EdgeSnapshotDatasetUsersV1:
		if ordering.userID != 0 && payload.Users[0].UserID <= ordering.userID {
			return fmt.Errorf("%w: user pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.userID = payload.Users[len(payload.Users)-1].UserID
		projection.Users = append(projection.Users, payload.Users...)
	case dto.EdgeSnapshotDatasetGroupsV1:
		if ordering.group != "" && payload.Groups[0].UserGroup <= ordering.group {
			return fmt.Errorf("%w: group pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.group = payload.Groups[len(payload.Groups)-1].UserGroup
		projection.Groups = append(projection.Groups, payload.Groups...)
	case dto.EdgeSnapshotDatasetModelsV1:
		if ordering.model != "" && payload.Models[0].Model <= ordering.model {
			return fmt.Errorf("%w: model pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.model = payload.Models[len(payload.Models)-1].Model
		projection.Models = append(projection.Models, payload.Models...)
	case dto.EdgeSnapshotDatasetChannelsV1:
		if ordering.channelID != 0 && payload.Channels[0].ChannelID <= ordering.channelID {
			return fmt.Errorf("%w: channel pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.channelID = payload.Channels[len(payload.Channels)-1].ChannelID
		projection.Channels = append(projection.Channels, payload.Channels...)
	case dto.EdgeSnapshotDatasetPricingV1:
		if ordering.pricing != "" && payload.Pricing[0].PolicyID <= ordering.pricing {
			return fmt.Errorf("%w: pricing pages are not globally ordered", ErrEdgeControlProtocolViolation)
		}
		ordering.pricing = payload.Pricing[len(payload.Pricing)-1].PolicyID
		projection.Pricing = append(projection.Pricing, payload.Pricing...)
	case dto.EdgeSnapshotDatasetRoutingV1:
		ordering.routingCount += len(payload.Routing)
		if ordering.routingCount != 1 {
			return fmt.Errorf("%w: routing dataset must contain exactly one policy", ErrEdgeControlProtocolViolation)
		}
		projection.Routing = append(projection.Routing, payload.Routing...)
	default:
		return fmt.Errorf("%w: unsupported snapshot dataset %q", ErrEdgeControlProtocolViolation, dataset)
	}
	return nil
}

func edgeControlFatalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrEdgeControlNodeDisabled) || errors.Is(err, ErrEdgeControlProtocolViolation) {
		return true
	}
	var remote *EdgeControlRemoteError
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.Response.Error.Code {
	case dto.EdgeControlErrorCodeUnsupportedProtocolV1,
		dto.EdgeControlErrorCodeAuthenticationFailedV1,
		dto.EdgeControlErrorCodeInvalidSignatureV1,
		dto.EdgeControlErrorCodeNodeDisabledV1,
		dto.EdgeControlErrorCodeIdempotencyConflictV1:
		return true
	default:
		return false
	}
}

func (s *edgeControlGormStore) SnapshotState(ctx context.Context) (*dto.EdgeSnapshotStateV1, error) {
	return model.GetEdgeLocalSnapshotState(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) SnapshotExpiry(ctx context.Context) (int64, error) {
	return model.GetEdgeLocalSnapshotExpiry(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) SettlementState(ctx context.Context) (*dto.EdgeSettlementStateV1, error) {
	return model.GetEdgeLocalSettlementState(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) BalanceState(ctx context.Context) (*model.EdgeLocalBalanceState, error) {
	return model.GetEdgeLocalBalanceState(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) ApplySnapshot(ctx context.Context, snapshot model.EdgeLocalSnapshotProjectionData) error {
	return model.ApplyEdgeLocalSnapshot(s.db.WithContext(ctx), snapshot)
}

func (s *edgeControlGormStore) ApplyControl(ctx context.Context, control dto.EdgeNodeControlConfigV1, nowUnixMilli int64) error {
	return model.ApplyEdgeLocalControlConfig(s.db.WithContext(ctx), control, nowUnixMilli)
}

func (s *edgeControlGormStore) ApplyBalance(ctx context.Context, control dto.EdgeNodeControlConfigV1, delta dto.EdgeBalanceDeltaV2, nowUnixMilli int64) error {
	return model.ApplyEdgeLocalBalanceDelta(s.db.WithContext(ctx), control, delta, nowUnixMilli)
}

func (s *edgeControlGormStore) RefreshChannelRuntime(ctx context.Context) error {
	return model.RefreshEdgeLocalChannelRuntime(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) InstallRoutingPolicy(ctx context.Context) error {
	routing, err := model.GetEdgeLocalRouting(s.db.WithContext(ctx))
	if err != nil {
		return err
	}
	for _, rule := range routing.ChannelAffinity.Rules {
		if err := validateEdgeChannelAffinityRuleSemantics(rule); err != nil {
			return fmt.Errorf("%w: affinity rule %q: %v", ErrEdgeControlProtocolViolation, rule.Name, err)
		}
	}
	return coreservice.SetEdgeChannelAffinityPolicy(routing.ChannelAffinity)
}

func (s *edgeControlGormStore) PendingSettlementBlock(ctx context.Context) (*dto.EdgeSettlementBlockRequestV1, error) {
	return model.GetEdgeLocalPendingSettlementBlock(s.db.WithContext(ctx))
}

func (s *edgeControlGormStore) BuildSettlementBlock(ctx context.Context, meta dto.EdgeControlRequestMetaV1, blockID string, maxEvents int, createdAtUnixMilli int64, settlementCircuitEpoch int64) (*dto.EdgeSettlementBlockRequestV1, error) {
	return model.BuildEdgeLocalSettlementBlock(s.db.WithContext(ctx), meta, blockID, maxEvents, createdAtUnixMilli, settlementCircuitEpoch)
}

func (s *edgeControlGormStore) RefreshSettlementRequest(ctx context.Context, blockID string, meta dto.EdgeControlRequestMetaV1, settlementCircuitEpoch int64) (*dto.EdgeSettlementBlockRequestV1, error) {
	return model.RefreshEdgeLocalSettlementRequest(s.db.WithContext(ctx), blockID, meta, settlementCircuitEpoch)
}

func (s *edgeControlGormStore) AcknowledgeSettlement(ctx context.Context, ack dto.EdgeSettlementAckV1) error {
	return model.AcknowledgeEdgeLocalSettlementBlock(s.db.WithContext(ctx), ack)
}
