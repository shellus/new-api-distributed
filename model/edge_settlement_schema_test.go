package model

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeUsageEventConsumeLogPayloadColumnsRemainNullable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "edge-settlement-schema.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&EdgeUsageEvent{}))

	type sqliteColumn struct {
		Name    string `gorm:"column:name"`
		NotNull int    `gorm:"column:notnull"`
	}
	var columns []sqliteColumn
	require.NoError(t, db.Raw("PRAGMA table_info(edge_usage_events)").Scan(&columns).Error)
	notNull := make(map[string]int, 2)
	for _, column := range columns {
		switch column.Name {
		case "consume_log_snapshot_payload", "consume_log_settlement_payload":
			notNull[column.Name] = column.NotNull
		}
	}
	assert.Equal(t, map[string]int{"consume_log_snapshot_payload": 0, "consume_log_settlement_payload": 0}, notNull)
}
