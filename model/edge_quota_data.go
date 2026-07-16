package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const edgeQuotaDataBucketKeyDomainV1 = "NEWAPI-EDGE-QUOTA-DATA-BUCKET-SHA256-V1"

// EdgeQuotaDataEvent is the durable idempotency marker for projecting one
// authoritative edge usage event into the existing quota_data dashboard.
// The marker and quota_data mutation live in the main database transaction,
// independently of whether logs use the same or a separate database.
type EdgeQuotaDataEvent struct {
	BillingEventKey string `gorm:"type:char(64);primaryKey;autoIncrement:false"`
	Payload         string `gorm:"type:text;not null"`
	ProjectedAt     int64  `gorm:"not null"`
}

func (EdgeQuotaDataEvent) TableName() string { return "edge_quota_data_events" }

// EdgeQuotaDataBucket serializes creation and updates of one logical
// quota_data bucket. quota_data has no composite unique key, so locking only a
// matching quota_data row cannot protect the first concurrent insert.
type EdgeQuotaDataBucket struct {
	BucketKey  string `gorm:"type:char(64);primaryKey;autoIncrement:false"`
	Dimensions string `gorm:"type:text;not null"`
}

func (EdgeQuotaDataBucket) TableName() string { return "edge_quota_data_buckets" }

type edgeQuotaDataEventPayload struct {
	UserID    int    `json:"user_id"`
	Username  string `json:"username,omitempty"`
	ModelName string `json:"model_name"`
	CreatedAt int64  `json:"created_at"`
	UseGroup  string `json:"use_group"`
	TokenID   int    `json:"token_id"`
	ChannelID int    `json:"channel_id"`
	NodeName  string `json:"node_name"`
	TokenUsed int    `json:"token_used"`
	Quota     int    `json:"quota"`
}

func sameEdgeQuotaDataEventPayload(storedPayload string, expected edgeQuotaDataEventPayload) bool {
	var stored edgeQuotaDataEventPayload
	if err := common.UnmarshalJsonStr(storedPayload, &stored); err != nil {
		return false
	}
	// Username is a mutable display field loaded when the outbox is published.
	// It does not change the authoritative event or its amounts, so an account
	// rename between crash recovery attempts must not become a conflict.
	stored.Username = ""
	expected.Username = ""
	return stored == expected
}

// RecordEdgeQuotaDataOnce increments the dashboard exactly once for a global
// edge billing event. A crash after this transaction but before outbox ack is
// safe: replay observes the marker and does not increment the bucket again.
func RecordEdgeQuotaDataOnce(ctx context.Context, billingEventKey string, params QuotaDataLogParams) (bool, error) {
	if DB == nil {
		return false, errors.New("database is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	billingEventKey = strings.TrimSpace(billingEventKey)
	decodedKey, err := hex.DecodeString(billingEventKey)
	if err != nil || len(decodedKey) != 32 || billingEventKey != strings.ToLower(billingEventKey) {
		return false, errors.New("edge quota-data billing event key must be a lowercase SHA-256 digest")
	}
	if params.UserID <= 0 || params.TokenID <= 0 || params.ChannelID <= 0 || params.CreatedAt <= 0 ||
		params.Quota < 0 || params.Quota > common.MaxQuota || params.TokenUsed < 0 || params.TokenUsed > common.MaxQuota ||
		strings.TrimSpace(params.ModelName) == "" || len(params.ModelName) > quotaDataModelNameMaxLength ||
		strings.TrimSpace(params.NodeName) == "" {
		return false, errors.New("invalid edge quota-data projection")
	}
	createdAt := params.CreatedAt - params.CreatedAt%3600
	quotaData := QuotaData{
		UserID: params.UserID, Username: params.Username, ModelName: params.ModelName,
		CreatedAt: createdAt, UseGroup: params.UseGroup, TokenID: params.TokenID,
		ChannelID: params.ChannelID, NodeName: params.NodeName,
		TokenUsed: params.TokenUsed, Count: 1, Quota: params.Quota,
	}
	bucketDimensions, err := common.Marshal(struct {
		UserID    int    `json:"user_id"`
		Username  string `json:"username"`
		ModelName string `json:"model_name"`
		CreatedAt int64  `json:"created_at"`
		UseGroup  string `json:"use_group"`
		TokenID   int    `json:"token_id"`
		ChannelID int    `json:"channel_id"`
		NodeName  string `json:"node_name"`
	}{
		UserID: quotaData.UserID, Username: quotaData.Username, ModelName: quotaData.ModelName,
		CreatedAt: quotaData.CreatedAt, UseGroup: quotaData.UseGroup, TokenID: quotaData.TokenID,
		ChannelID: quotaData.ChannelID, NodeName: quotaData.NodeName,
	})
	if err != nil {
		return false, err
	}
	bucketHasher := sha256.New()
	_, _ = bucketHasher.Write([]byte(edgeQuotaDataBucketKeyDomainV1))
	_, _ = bucketHasher.Write([]byte{'\n'})
	_, _ = bucketHasher.Write(bucketDimensions)
	bucketKey := hex.EncodeToString(bucketHasher.Sum(nil))

	eventPayload := edgeQuotaDataEventPayload{
		UserID: quotaData.UserID, Username: quotaData.Username, ModelName: quotaData.ModelName,
		CreatedAt: quotaData.CreatedAt, UseGroup: quotaData.UseGroup, TokenID: quotaData.TokenID,
		ChannelID: quotaData.ChannelID, NodeName: quotaData.NodeName,
		TokenUsed: quotaData.TokenUsed, Quota: quotaData.Quota,
	}
	payload, err := common.Marshal(eventPayload)
	if err != nil {
		return false, err
	}

	var existing EdgeQuotaDataEvent
	lookup := DB.WithContext(ctx).Where("billing_event_key = ?", billingEventKey).Limit(1).Find(&existing)
	if lookup.Error != nil {
		return false, lookup.Error
	}
	if lookup.RowsAffected == 1 {
		if !sameEdgeQuotaDataEventPayload(existing.Payload, eventPayload) {
			return false, errors.New("edge quota-data billing event key belongs to a different projection")
		}
		if existing.ProjectedAt > 0 {
			return false, nil
		}
	}

	created := false
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := EdgeQuotaDataEvent{
			BillingEventKey: billingEventKey, Payload: string(payload), ProjectedAt: 0,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "billing_event_key"}},
			DoNothing: true,
		}).Create(&candidate).Error; err != nil {
			return err
		}
		var marker EdgeQuotaDataEvent
		if err := lockForUpdate(tx).First(&marker, "billing_event_key = ?", billingEventKey).Error; err != nil {
			return err
		}
		if !sameEdgeQuotaDataEventPayload(marker.Payload, eventPayload) {
			return errors.New("edge quota-data billing event key belongs to a different projection")
		}
		if marker.ProjectedAt > 0 {
			return nil
		}

		bucket := EdgeQuotaDataBucket{BucketKey: bucketKey, Dimensions: string(bucketDimensions)}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "bucket_key"}},
			DoNothing: true,
		}).Create(&bucket).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&bucket, "bucket_key = ?", bucketKey).Error; err != nil {
			return err
		}
		if bucket.Dimensions != string(bucketDimensions) {
			return errors.New("edge quota-data bucket key belongs to different dimensions")
		}

		var quotaBucket QuotaData
		query := lockForUpdate(tx).Where(
			"user_id = ? AND username = ? AND model_name = ? AND created_at = ? AND use_group = ? AND token_id = ? AND channel_id = ? AND node_name = ?",
			quotaData.UserID, quotaData.Username, quotaData.ModelName, quotaData.CreatedAt,
			quotaData.UseGroup, quotaData.TokenID, quotaData.ChannelID, quotaData.NodeName,
		).Order("id ASC").Limit(1).Find(&quotaBucket)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected == 0 {
			if err := tx.Create(&quotaData).Error; err != nil {
				return err
			}
		} else {
			result := tx.Model(&QuotaData{}).Where("id = ?", quotaBucket.Id).Updates(map[string]any{
				"count":      gorm.Expr("count + ?", quotaData.Count),
				"quota":      gorm.Expr("quota + ?", quotaData.Quota),
				"token_used": gorm.Expr("token_used + ?", quotaData.TokenUsed),
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("edge quota-data bucket update lost its target")
			}
		}
		result := tx.Model(&EdgeQuotaDataEvent{}).
			Where("billing_event_key = ? AND projected_at = ?", billingEventKey, 0).
			Update("projected_at", params.CreatedAt)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("edge quota-data marker lost its projection state")
		}
		created = true
		return nil
	})
	if err != nil {
		// The transaction may have committed after its client lost the reply.
		// Reading the durable completion state keeps that replay idempotent.
		existing = EdgeQuotaDataEvent{}
		lookup = DB.WithContext(ctx).Where("billing_event_key = ?", billingEventKey).Limit(1).Find(&existing)
		if lookup.Error == nil && lookup.RowsAffected == 1 && existing.ProjectedAt > 0 {
			if !sameEdgeQuotaDataEventPayload(existing.Payload, eventPayload) {
				return false, errors.New("edge quota-data billing event key belongs to a different projection")
			}
			return false, nil
		}
	}
	return created, err
}
