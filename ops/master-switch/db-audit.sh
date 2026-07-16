#!/usr/bin/env bash
set -Eeuo pipefail

: "${DB_CONTAINER:?DB_CONTAINER is required}"
: "${DB_USER:?DB_USER is required}"
: "${DB_NAME:?DB_NAME is required}"

psql_target() {
  docker exec -i "$DB_CONTAINER" psql -X -v ON_ERROR_STOP=1 -U "$DB_USER" -d "$DB_NAME" "$@"
}

table_exists() {
  [[ $(psql_target -qAtc "SELECT to_regclass('public.$1') IS NOT NULL") == t ]]
}

metrics() {
  psql_target -qAt <<'SQL'
SELECT jsonb_build_object(
  'users', (SELECT jsonb_build_object('count', count(*), 'quota', coalesce(sum(quota), 0), 'used_quota', coalesce(sum(used_quota), 0), 'request_count', coalesce(sum(request_count), 0)) FROM users),
  'tokens', (SELECT jsonb_build_object('count', count(*), 'remain_quota', coalesce(sum(remain_quota), 0), 'used_quota', coalesce(sum(used_quota), 0)) FROM tokens),
  'channels', (SELECT jsonb_build_object('count', count(*), 'used_quota', coalesce(sum(used_quota), 0)) FROM channels),
  'logs', (SELECT jsonb_build_object('count', count(*), 'quota', coalesce(sum(quota), 0), 'max_id', coalesce(max(id), 0)) FROM logs),
  'quota_data', (SELECT jsonb_build_object('count', count(*), 'quota', coalesce(sum(quota), 0), 'token_used', coalesce(sum(token_used), 0), 'requests', coalesce(sum(count), 0)) FROM quota_data),
  'subscriptions', (SELECT jsonb_build_object('count', count(*), 'amount_total', coalesce(sum(amount_total), 0), 'amount_used', coalesce(sum(amount_used), 0)) FROM user_subscriptions),
  'options', (SELECT jsonb_build_object('count', count(*)) FROM options)
)::text;
SQL
}

assert_master_schema() {
  local missing column_count index_count model_length
  missing=$(psql_target -qAt <<'SQL'
WITH expected(table_name) AS (VALUES
  ('edge_nodes'), ('edge_node_credentials'), ('edge_request_receipts'),
  ('edge_request_nonce_claims'), ('edge_policy_snapshots'), ('edge_node_heartbeats'),
  ('edge_compiled_snapshots'), ('edge_compiled_snapshot_datasets'), ('edge_compiled_snapshot_pages'),
  ('edge_quota_leases'), ('edge_lease_fundings'), ('edge_settlement_blocks'),
  ('edge_usage_events'), ('edge_consume_log_outboxes'), ('edge_quota_data_events'),
  ('edge_quota_data_buckets')
)
SELECT string_agg(expected.table_name, ', ' ORDER BY expected.table_name)
FROM expected
LEFT JOIN information_schema.tables actual
  ON actual.table_schema = 'public' AND actual.table_name = expected.table_name
WHERE actual.table_name IS NULL;
SQL
)
  [[ -z "$missing" ]] || { printf '缺少 master 表: %s\n' "$missing" >&2; return 1; }

  column_count=$(psql_target -qAtc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='logs' AND column_name='billing_event_key'")
  [[ "$column_count" == 1 ]] || { printf 'logs.billing_event_key 未创建\n' >&2; return 1; }
  index_count=$(psql_target -qAtc "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='logs' AND indexname='ux_logs_billing_event_key' AND indexdef LIKE 'CREATE UNIQUE INDEX%'")
  [[ "$index_count" == 1 ]] || { printf 'ux_logs_billing_event_key 唯一索引未创建\n' >&2; return 1; }
  model_length=$(psql_target -qAtc "SELECT character_maximum_length FROM information_schema.columns WHERE table_schema='public' AND table_name='quota_data' AND column_name='model_name'")
  [[ "$model_length" == 256 ]] || { printf 'quota_data.model_name 长度应为 256，实际为 %s\n' "$model_length" >&2; return 1; }
}

edge_rollback_blockers() {
  local leases=0 fundings=0 outbox=0 receipts=0 total
  if table_exists edge_quota_leases; then
    leases=$(psql_target -qAtc "SELECT count(*) FROM edge_quota_leases WHERE status NOT IN ('closed', 'force_closed')")
  fi
  if table_exists edge_lease_fundings; then
    fundings=$(psql_target -qAtc "SELECT count(*) FROM edge_lease_fundings WHERE status = 'reserved'")
  fi
  if table_exists edge_consume_log_outboxes; then
    outbox=$(psql_target -qAtc "SELECT count(*) FROM edge_consume_log_outboxes WHERE status <> 'published'")
  fi
  if table_exists edge_request_receipts; then
    receipts=$(psql_target -qAtc "SELECT count(*) FROM edge_request_receipts WHERE status = 'processing'")
  fi
  total=$((leases + fundings + outbox + receipts))
  printf 'nonterminal_leases=%s\n' "$leases"
  printf 'reserved_fundings=%s\n' "$fundings"
  printf 'unpublished_log_outbox=%s\n' "$outbox"
  printf 'processing_receipts=%s\n' "$receipts"
  printf 'total=%s\n' "$total"
}

case "${1:-}" in
  metrics) metrics ;;
  assert-master-schema) assert_master_schema ;;
  edge-rollback-blockers) edge_rollback_blockers ;;
  quota-model-column-length)
    psql_target -qAtc "SELECT character_maximum_length FROM information_schema.columns WHERE table_schema='public' AND table_name='quota_data' AND column_name='model_name'"
    ;;
  max-model-name-length)
    psql_target -qAtc "SELECT coalesce(max(length(model_name)), 0) FROM quota_data"
    ;;
  edge-node-count)
    if table_exists edge_nodes; then
      psql_target -qAtc "SELECT count(*) FROM edge_nodes"
    else
      printf '0\n'
    fi
    ;;
  *)
    printf '用法: DB_CONTAINER=... DB_USER=... DB_NAME=... %s {metrics|assert-master-schema|edge-rollback-blockers|quota-model-column-length|max-model-name-length|edge-node-count}\n' "$0" >&2
    exit 2
    ;;
esac
