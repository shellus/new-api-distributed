package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecoverEdgeTaskBillingReconcilesSettledAndZeroQuotaFailedTasks(t *testing.T) {
	db, now := newTaskBillingEdgeTestDB(t)
	previousDB := model.DB
	model.DB = db
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, common.SetRuntimeMode(common.RuntimeModeMaster))
	})

	settledReservation, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-task-settled", RequestID: "request-task-settled",
		UserID: 7, TokenID: 11, Quota: 40, NegativeFloorQuota: -100, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	settledTask := &model.Task{
		TaskID: "task-settled", UserId: 7, Group: "default", ChannelId: 31, Quota: 40,
		Status: model.TaskStatusSuccess, SubmitTime: now.Unix(), FinishTime: now.Add(time.Second).Unix(),
		Properties: model.Properties{OriginModelName: "gpt-task"},
		PrivateData: model.TaskPrivateData{
			TokenId: 11, EdgeReservationID: settledReservation.ReservationID,
			BillingContext: &model.TaskBillingContext{
				ModelPrice: 0.00008, GroupRatio: 1, OriginModelName: "gpt-task", PerCallBilling: true,
				PricingPolicyID: "pricing-task", PricingPolicyVersion: "v1", BillingMode: string(dto.EdgeBillingModeFixedPriceV1),
			},
		},
	}
	require.NoError(t, settledTask.Insert())
	require.NoError(t, model.BindEdgeLocalReservationOwner(db, settledReservation.ReservationID, "task", settledTask.TaskID, now.Add(time.Second).UnixMilli()))
	require.NoError(t, FinalizeEdgeTaskBilling(context.Background(), settledTask, 40))

	// Simulate a crash after the durable ledger settled but before the task row
	// cleared its reservation pointer.
	settledTask.PrivateData.EdgeReservationID = settledReservation.ReservationID
	require.NoError(t, settledTask.Update())
	require.NoError(t, RecoverEdgeTaskBilling(context.Background()))
	reloadedSettled, exists, err := model.GetByOnlyTaskId(settledTask.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, reloadedSettled.PrivateData.EdgeReservationID)
	assert.Equal(t, 40, reloadedSettled.Quota)

	zeroReservation, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-task-zero", RequestID: "request-task-zero",
		UserID: 7, TokenID: 11, Quota: 0, NegativeFloorQuota: -100, NowUnixMilli: now.Add(2 * time.Second).UnixMilli(),
	})
	require.NoError(t, err)
	failedTask := &model.Task{
		TaskID: "task-zero-failed", UserId: 7, Group: "default", ChannelId: 31,
		Status: model.TaskStatusFailure, Progress: "100%", FailReason: "upstream failed",
		Properties:  model.Properties{OriginModelName: "gpt-task"},
		PrivateData: model.TaskPrivateData{TokenId: 11, EdgeReservationID: zeroReservation.ReservationID},
	}
	require.NoError(t, failedTask.Insert())
	require.NoError(t, model.BindEdgeLocalReservationOwner(db, zeroReservation.ReservationID, "task", failedTask.TaskID, now.Add(3*time.Second).UnixMilli()))
	require.NoError(t, RecoverEdgeTaskBilling(context.Background()))
	reloadedFailed, exists, err := model.GetByOnlyTaskId(failedTask.TaskID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, reloadedFailed.PrivateData.EdgeReservationID)
	zeroReservation, err = model.GetEdgeLocalReservation(db, zeroReservation.ReservationID)
	require.NoError(t, err)
	assert.Equal(t, model.EdgeLocalReservationStatusRefunded, zeroReservation.Status)
}

func newTaskBillingEdgeTestDB(t *testing.T) (*gorm.DB, time.Time) {
	t.Helper()
	channelConfigDir := t.TempDir()
	t.Setenv("EDGE_CHANNEL_CONFIG_DIR", channelConfigDir)
	require.NoError(t, os.WriteFile(filepath.Join(channelConfigDir, "edge-channel.yaml"), []byte(`name: edge-channel
type: openai
base_url: http://edge-channel.invalid
auth: edge-test-key
`), 0o600))
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Now().UTC().Truncate(time.Second)
	datasets := []dto.EdgeSnapshotDatasetV1{
		dto.EdgeSnapshotDatasetAuthenticationV1,
		dto.EdgeSnapshotDatasetUsersV1,
		dto.EdgeSnapshotDatasetGroupsV1,
		dto.EdgeSnapshotDatasetModelsV1,
		dto.EdgeSnapshotDatasetChannelsV1,
		dto.EdgeSnapshotDatasetPricingV1,
		dto.EdgeSnapshotDatasetRoutingV1,
	}
	state := dto.EdgeSnapshotStateV1{
		SnapshotID: "snapshot-task-billing", Revision: 1, AppliedAtUnixMilli: now.UnixMilli(),
	}
	for _, dataset := range datasets {
		state.Datasets = append(state.Datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset, Revision: 1})
	}
	modelPrice := 0.00008
	require.NoError(t, model.ApplyEdgeLocalSnapshot(db, model.EdgeLocalSnapshotProjectionData{
		State: state, Digest: strings.Repeat("a", 64), ExpiresAtUnixMilli: now.Add(24 * time.Hour).UnixMilli(),
		TokenFingerprint: dto.EdgeTokenFingerprintSchemeV1{Algorithm: edgetoken.FingerprintAlgorithm, Version: edgetoken.FingerprintVersion},
		Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("b", 64), TokenID: 11, UserID: 7, Enabled: true, Group: "default",
		}},
		Users: []dto.EdgeUserPolicyV1{{
			UserID: 7, Enabled: true, Username: "edge-user", DefaultGroup: "default",
			Setting: dto.EdgeUserSettingV1{BillingPreference: "wallet_only"},
		}},
		Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}},
		Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-task", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointDataPlaneV1},
			Streaming: true, ChannelIDs: []int64{31},
		}},
		Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: 31, Type: 1, Name: "edge-channel", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-task"}, Priority: 1, Weight: 1,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}},
		Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: "pricing-task", Version: "v1", Model: "gpt-task",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
		}},
		Routing: []dto.EdgeRoutingPolicyV1{{ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{
			Enabled: false, MaxEntries: 1_000, DefaultTTLSeconds: 60,
		}}},
	}))
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.task-billing", NodeGeneration: 1}
	require.NoError(t, model.ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 1_000}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 1_000}},
	}, now.UnixMilli()))
	return db, now
}
