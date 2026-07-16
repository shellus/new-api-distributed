package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/gorm"
)

const edgeConsumeLogBillingEventKeyDomainV1 = "NEWAPI-EDGE-CONSUME-LOG-BILLING-EVENT-SHA256-V1"

// EdgeConsumeLogBillingEventKey returns the master-global identity used to
// materialize one edge usage event in LOG_DB. Edge event IDs are scoped to a
// node generation, so the raw event ID alone is not a safe unique key.
func EdgeConsumeLogBillingEventKey(nodeUID string, nodeGeneration int64, eventUID string) (string, error) {
	nodeUID = strings.TrimSpace(nodeUID)
	eventUID = strings.TrimSpace(eventUID)
	if err := validateEdgeStoredIdentifier("node UID", nodeUID); err != nil {
		return "", err
	}
	if nodeGeneration <= 0 {
		return "", errors.New("edge node generation must be positive")
	}
	if err := validateEdgeStoredIdentifier("event UID", eventUID); err != nil {
		return "", err
	}
	canonical, err := common.Marshal(struct {
		NodeUID        string `json:"node_uid"`
		NodeGeneration int64  `json:"node_generation"`
		EventUID       string `json:"event_uid"`
	}{
		NodeUID:        nodeUID,
		NodeGeneration: nodeGeneration,
		EventUID:       eventUID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal edge consume-log billing identity: %w", err)
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(edgeConsumeLogBillingEventKeyDomainV1))
	_, _ = hasher.Write([]byte{'\n'})
	_, _ = hasher.Write(canonical)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// CreateEdgeConsumeLogOnce materializes one authoritative edge usage event in
// LOG_DB. SQL log databases enforce the nullable unique key. ClickHouse also
// receives the key as insert_deduplication_token; its MergeTree table enables
// a non-replicated deduplication window during migration.
func CreateEdgeConsumeLogOnce(ctx context.Context, log *Log, billingEventKey string) (bool, error) {
	if LOG_DB == nil {
		return false, errors.New("log database is nil")
	}
	if log == nil {
		return false, errors.New("edge consume log is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	billingEventKey = strings.TrimSpace(billingEventKey)
	if billingEventKey == "" || len(billingEventKey) > 64 || strings.ContainsAny(billingEventKey, "\x00\r\n") {
		return false, errors.New("invalid edge billing event key")
	}
	if log.Type != LogTypeConsume || log.UserId <= 0 || log.TokenId <= 0 || log.ChannelId <= 0 ||
		log.CreatedAt <= 0 || log.Quota < 0 || log.PromptTokens < 0 || log.CompletionTokens < 0 || log.UseTime < 0 {
		return false, errors.New("invalid edge consume log")
	}
	if log.BillingEventKey != nil && *log.BillingEventKey != billingEventKey {
		return false, errors.New("edge consume log has a different billing event key")
	}

	key := billingEventKey
	log.BillingEventKey = &key
	ensureLogRequestId(log)
	logDB := LOG_DB.WithContext(ctx)
	var existing Log
	err := logDB.Where("billing_event_key = ?", billingEventKey).First(&existing).Error
	if err == nil {
		if !sameEdgeConsumeLog(&existing, log) {
			return false, errors.New("edge billing event key already belongs to a different consume log")
		}
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}

	createDB := logDB
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		insertContext := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
			"insert_deduplication_token": billingEventKey,
		}))
		createDB = LOG_DB.WithContext(insertContext)
	}
	if err := createDB.Create(log).Error; err != nil {
		// A concurrent publisher may have won the nullable unique-key race, or a
		// ClickHouse insert may have completed after its client lost the reply.
		var winner Log
		if lookupErr := logDB.Where("billing_event_key = ?", billingEventKey).First(&winner).Error; lookupErr == nil {
			if !sameEdgeConsumeLog(&winner, log) {
				return false, errors.New("edge billing event key was concurrently used by a different consume log")
			}
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func sameEdgeConsumeLog(existing *Log, expected *Log) bool {
	if existing == nil || expected == nil {
		return false
	}
	return existing.UserId == expected.UserId &&
		existing.CreatedAt == expected.CreatedAt &&
		existing.Type == LogTypeConsume && expected.Type == LogTypeConsume &&
		existing.PromptTokens == expected.PromptTokens &&
		existing.CompletionTokens == expected.CompletionTokens &&
		existing.ModelName == expected.ModelName &&
		existing.Quota == expected.Quota &&
		existing.ChannelId == expected.ChannelId &&
		existing.TokenId == expected.TokenId &&
		existing.UseTime == expected.UseTime &&
		existing.IsStream == expected.IsStream &&
		existing.Group == expected.Group &&
		existing.RequestId == expected.RequestId
}
