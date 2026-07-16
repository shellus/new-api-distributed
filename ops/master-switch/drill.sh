#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd)
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.drill.yml"
BUILD_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.build.yml"
OVERLAY_BUILD_COMPOSE_FILE="$SCRIPT_DIR/docker-compose.build-overlay.yml"
DB_AUDIT="$SCRIPT_DIR/db-audit.sh"
SWITCH_ENV_FILE=${SWITCH_ENV_FILE:-$REPO_ROOT/.local/master-switch-drill/.env}

log() {
  printf '[master-switch] %s\n' "$*"
}

die() {
  printf '[master-switch] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"
}

load_env() {
  [[ -f "$SWITCH_ENV_FILE" ]] || die "环境文件不存在: $SWITCH_ENV_FILE"
  # shellcheck disable=SC1090
  set -a
  source "$SWITCH_ENV_FILE"
  set +a

  : "${COMPOSE_PROJECT_NAME:?COMPOSE_PROJECT_NAME is required}"
  : "${RUNTIME_DIR:?RUNTIME_DIR is required}"
  : "${ORIGINAL_IMAGE:?ORIGINAL_IMAGE is required}"
  : "${MASTER_IMAGE:?MASTER_IMAGE is required}"
  : "${DRILL_APP_PORT:?DRILL_APP_PORT is required}"
  : "${DRILL_POSTGRES_PORT:?DRILL_POSTGRES_PORT is required}"
  : "${DRILL_REDIS_PORT:?DRILL_REDIS_PORT is required}"
  : "${DRILL_BIND_ADDRESS:?DRILL_BIND_ADDRESS is required}"
  : "${DRILL_HTTP_HOST:?DRILL_HTTP_HOST is required}"
  : "${DRILL_DB_USER:?DRILL_DB_USER is required}"
  : "${DRILL_DB_PASSWORD:?DRILL_DB_PASSWORD is required}"
  : "${DRILL_DB_NAME:?DRILL_DB_NAME is required}"
  : "${DRILL_SESSION_SECRET:?DRILL_SESSION_SECRET is required}"
  : "${SOURCE_APP_CONTAINER:?SOURCE_APP_CONTAINER is required}"
  : "${SOURCE_DB_CONTAINER:?SOURCE_DB_CONTAINER is required}"
  : "${SOURCE_DB_USER:?SOURCE_DB_USER is required}"
  : "${SOURCE_DB_NAME:?SOURCE_DB_NAME is required}"
  : "${SOURCE_COMPOSE_DIR:?SOURCE_COMPOSE_DIR is required}"

  local allowed runtime
  allowed=$(realpath -m "$REPO_ROOT/.local")
  runtime=$(realpath -m "$RUNTIME_DIR")
  [[ "$runtime" == "$allowed"/* ]] || die "RUNTIME_DIR 必须位于 $allowed 下，当前为 $runtime"
  [[ "$runtime" != "$allowed" ]] || die "RUNTIME_DIR 不得直接指向 .local 根目录"
  RUNTIME_DIR=$runtime
  export RUNTIME_DIR
}

compose() {
  docker compose --env-file "$SWITCH_ENV_FILE" \
    --project-name "$COMPOSE_PROJECT_NAME" \
    --project-directory "$SCRIPT_DIR" \
    -f "$COMPOSE_FILE" "$@"
}

build_compose() {
  docker compose --env-file "$SWITCH_ENV_FILE" \
    --project-name "${COMPOSE_PROJECT_NAME}-build" \
    --project-directory "$SCRIPT_DIR" \
    -f "$BUILD_COMPOSE_FILE" "$@"
}

service_container() {
  compose ps -q "$1"
}

wait_service_healthy() {
  local service=$1 timeout=${2:-180} container deadline status
  container=$(service_container "$service")
  [[ -n "$container" ]] || die "服务未创建: $service"
  deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    case "$status" in
      healthy|running)
        log "$service 已就绪"
        return 0
        ;;
      unhealthy|exited|dead)
        docker logs --tail 200 "$container" >&2 || true
        die "$service 状态异常: $status"
        ;;
    esac
    sleep 2
  done
  docker logs --tail 200 "$container" >&2 || true
  die "等待 $service 就绪超时 (${timeout}s)"
}

wait_app_http() {
  local service=$1 timeout=${2:-300} deadline body
  wait_service_healthy "$service" "$timeout"
  deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    body=$(curl -fsS --max-time 5 "http://${DRILL_HTTP_HOST}:${DRILL_APP_PORT}/api/status" 2>/dev/null || true)
    if [[ -n "$body" ]] && jq -e '.success == true' >/dev/null 2>&1 <<<"$body"; then
      log "$service HTTP 健康检查通过"
      return 0
    fi
    sleep 1
  done
  die "$service 容器健康，但 /api/status 校验失败"
}

preflight() {
  require_command docker
  require_command curl
  require_command jq
  require_command openssl
  require_command realpath
  docker info >/dev/null
  docker compose version >/dev/null
  docker inspect "$SOURCE_APP_CONTAINER" >/dev/null 2>&1 || die "源应用容器不存在: $SOURCE_APP_CONTAINER"
  docker inspect "$SOURCE_DB_CONTAINER" >/dev/null 2>&1 || die "源数据库容器不存在: $SOURCE_DB_CONTAINER"
  [[ $(docker inspect -f '{{.State.Running}}' "$SOURCE_APP_CONTAINER") == true ]] || die "源应用容器未运行"
  [[ $(docker inspect -f '{{.State.Running}}' "$SOURCE_DB_CONTAINER") == true ]] || die "源数据库容器未运行"
  [[ -d "$SOURCE_COMPOSE_DIR/data" ]] || die "源应用数据目录不存在: $SOURCE_COMPOSE_DIR/data"

  local free_kb
  free_kb=$(df -Pk "$REPO_ROOT" | awk 'NR==2 {print $4}')
  (( free_kb >= 6 * 1024 * 1024 )) || die "可用磁盘不足 6 GiB，当前 $((free_kb / 1024 / 1024)) GiB"
  log "预检通过；可用磁盘 $((free_kb / 1024 / 1024)) GiB"
}

prepare_images() {
  local original_id master_id build=(docker)
  original_id=$(docker inspect -f '{{.Image}}' "$SOURCE_APP_CONTAINER")
  docker image inspect "$original_id" >/dev/null
  docker tag "$original_id" "$ORIGINAL_IMAGE"
  log "原版镜像已冻结: $ORIGINAL_IMAGE -> $original_id"

  case "${MASTER_BUILD_MODE:-docker}" in
    docker)
      if command -v proxy-env >/dev/null 2>&1; then
        build=(proxy-env docker)
      fi
      "${build[@]}" compose --env-file "$SWITCH_ENV_FILE" \
        --project-name "${COMPOSE_PROJECT_NAME}-build" \
        --project-directory "$SCRIPT_DIR" \
        -f "$BUILD_COMPOSE_FILE" build master-build
      ;;
    overlay)
      require_command bun
      require_command go
      mkdir -p "$SCRIPT_DIR/.build"
      (
        cd "$REPO_ROOT/web/default"
        DISABLE_ESLINT_PLUGIN=true VITE_REACT_APP_VERSION="$(<"$REPO_ROOT/VERSION")" bun run build
      )
      (
        cd "$REPO_ROOT/web/classic"
        VITE_REACT_APP_VERSION="$(<"$REPO_ROOT/VERSION")" bun run build
      )
      (
        cd "$REPO_ROOT"
        CGO_ENABLED=0 go build \
          -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(<VERSION)'" \
          -o "$SCRIPT_DIR/.build/new-api" .
      )
      docker compose --env-file "$SWITCH_ENV_FILE" \
        --project-name "${COMPOSE_PROJECT_NAME}-build" \
        --project-directory "$SCRIPT_DIR" \
        -f "$OVERLAY_BUILD_COMPOSE_FILE" build master-build
      ;;
    *)
      die "MASTER_BUILD_MODE 只支持 docker 或 overlay"
      ;;
  esac
  master_id=$(docker image inspect -f '{{.Id}}' "$MASTER_IMAGE")
  mkdir -p "$RUNTIME_DIR/artifacts"
  printf '%s\n' "$original_id" > "$RUNTIME_DIR/artifacts/original-image-id"
  printf '%s\n' "$master_id" > "$RUNTIME_DIR/artifacts/master-image-id"
  log "master 镜像构建完成: $MASTER_IMAGE -> $master_id"
}

reset_runtime() {
  compose --profile original --profile master down --remove-orphans >/dev/null 2>&1 || true
  mkdir -p "$RUNTIME_DIR"
  find "$RUNTIME_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  mkdir -p "$RUNTIME_DIR/postgres" "$RUNTIME_DIR/app-data" \
    "$RUNTIME_DIR/logs/original" "$RUNTIME_DIR/logs/master" "$RUNTIME_DIR/artifacts"
  cp -a "$SOURCE_COMPOSE_DIR/data/." "$RUNTIME_DIR/app-data/"
  log "隔离运行目录已重置: $RUNTIME_DIR"
}

target_psql() {
  local container
  container=$(service_container postgres)
  [[ -n "$container" ]] || die "隔离 PostgreSQL 未运行"
  docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U "$DRILL_DB_USER" -d "$DRILL_DB_NAME" "$@"
}

clone_database() {
  local dump_file="$RUNTIME_DIR/artifacts/source.dump" pg_container marker original_id master_id
  compose up -d postgres redis
  wait_service_healthy postgres 180
  wait_service_healthy redis 120

  log "从 $SOURCE_DB_CONTAINER 获取一致性逻辑快照"
  docker exec "$SOURCE_DB_CONTAINER" pg_dump \
    -U "$SOURCE_DB_USER" -d "$SOURCE_DB_NAME" \
    --format=custom --compress=6 --no-owner --no-privileges > "$dump_file"
  [[ -s "$dump_file" ]] || die "数据库快照为空"
  docker exec -i "$SOURCE_DB_CONTAINER" pg_restore --list < "$dump_file" >/dev/null

  pg_container=$(service_container postgres)
  docker exec -i "$pg_container" pg_restore \
    -U "$DRILL_DB_USER" -d "$DRILL_DB_NAME" \
    --exit-on-error --no-owner --no-privileges < "$dump_file"

  marker=$(openssl rand -hex 16)
  target_psql -q -v marker="$marker" <<'SQL'
INSERT INTO options ("key", "value")
VALUES ('MasterSwitchDrillMarker', :'marker')
ON CONFLICT ("key") DO UPDATE SET "value" = EXCLUDED."value";
SQL
  printf '%s\n' "$marker" > "$RUNTIME_DIR/artifacts/marker"
  original_id=$(docker image inspect -f '{{.Id}}' "$ORIGINAL_IMAGE")
  master_id=$(docker image inspect -f '{{.Id}}' "$MASTER_IMAGE")
  printf '%s\n' "$original_id" > "$RUNTIME_DIR/artifacts/original-image-id"
  printf '%s\n' "$master_id" > "$RUNTIME_DIR/artifacts/master-image-id"
  log "生产数据克隆完成；快照大小 $(du -h "$dump_file" | awk '{print $1}')"
}

capture_metrics() {
  local output=$1 container
  container=$(service_container postgres)
  DB_CONTAINER="$container" DB_USER="$DRILL_DB_USER" DB_NAME="$DRILL_DB_NAME" \
    "$DB_AUDIT" metrics > "$output"
  [[ -s "$output" ]] || die "业务指标采集为空"
}

assert_marker() {
  local expected actual
  expected=$(<"$RUNTIME_DIR/artifacts/marker")
  actual=$(target_psql -qAtc "SELECT value FROM options WHERE key = 'MasterSwitchDrillMarker'")
  [[ "$actual" == "$expected" ]] || die "演练标记丢失或改变"
}

assert_metrics_equal() {
  local expected=$1 actual=$2 label=$3
  if ! cmp -s "$expected" "$actual"; then
    diff -u "$expected" "$actual" >&2 || true
    die "$label 业务数据指标与原版基线不一致"
  fi
  log "$label 业务数据指标与原版基线完全一致"
}

assert_master_schema() {
  local container
  container=$(service_container postgres)
  DB_CONTAINER="$container" DB_USER="$DRILL_DB_USER" DB_NAME="$DRILL_DB_NAME" \
    "$DB_AUDIT" assert-master-schema || die "master Schema 校验失败"
  log "master Schema 校验通过：16 张 edge 表、日志幂等索引、model_name(256)"
}

start_original() {
  compose --profile master stop master >/dev/null 2>&1 || true
  compose --profile master rm -f master >/dev/null 2>&1 || true
  local started=$SECONDS
  compose --profile original up -d original
  wait_app_http original 300
  printf '%s\n' "$((SECONDS - started))" > "$RUNTIME_DIR/artifacts/original-start-seconds"
  assert_marker
  capture_metrics "$RUNTIME_DIR/artifacts/original.metrics.json"
  log "原版已使用克隆数据启动"
}

switch_master() {
  [[ -f "$RUNTIME_DIR/artifacts/original.metrics.json" ]] || die "缺少原版基线，请先执行 start-original"
  compose --profile original stop original
  compose --profile original rm -f original
  local started=$SECONDS
  compose --profile master up -d master
  wait_app_http master 900
  printf '%s\n' "$((SECONDS - started))" > "$RUNTIME_DIR/artifacts/master-switch-seconds"
  assert_marker
  assert_master_schema
  capture_metrics "$RUNTIME_DIR/artifacts/master.metrics.json"
  assert_metrics_equal "$RUNTIME_DIR/artifacts/original.metrics.json" "$RUNTIME_DIR/artifacts/master.metrics.json" master
  log "已从原版切换至 master"
}

rollback_original() {
  [[ -f "$RUNTIME_DIR/artifacts/original.metrics.json" ]] || die "缺少原版基线"
  compose --profile master stop master
  compose --profile master rm -f master
  local started=$SECONDS
  compose --profile original up -d original
  wait_app_http original 900
  printf '%s\n' "$((SECONDS - started))" > "$RUNTIME_DIR/artifacts/rollback-seconds"
  assert_marker
  capture_metrics "$RUNTIME_DIR/artifacts/rollback.metrics.json"
  assert_metrics_equal "$RUNTIME_DIR/artifacts/original.metrics.json" "$RUNTIME_DIR/artifacts/rollback.metrics.json" rollback
  DB_CONTAINER="$(service_container postgres)" DB_USER="$DRILL_DB_USER" DB_NAME="$DRILL_DB_NAME" \
    "$DB_AUDIT" quota-model-column-length > "$RUNTIME_DIR/artifacts/rollback-model-name-length"
  log "已从 master 切回原版"
}

write_report() {
  local original_seconds master_seconds rollback_seconds model_length
  original_seconds=$(<"$RUNTIME_DIR/artifacts/original-start-seconds")
  master_seconds=$(<"$RUNTIME_DIR/artifacts/master-switch-seconds")
  rollback_seconds=$(<"$RUNTIME_DIR/artifacts/rollback-seconds")
  model_length=$(<"$RUNTIME_DIR/artifacts/rollback-model-name-length")
  {
    printf 'original_start_seconds=%s\n' "$original_seconds"
    printf 'master_switch_seconds=%s\n' "$master_seconds"
    printf 'rollback_seconds=%s\n' "$rollback_seconds"
    printf 'rollback_quota_data_model_name_length=%s\n' "$model_length"
    printf 'result=passed\n'
  } > "$RUNTIME_DIR/artifacts/result.env"
  log "演练通过；结果: $RUNTIME_DIR/artifacts/result.env"
}

status() {
  compose --profile original --profile master ps -a
  if [[ -f "$RUNTIME_DIR/artifacts/result.env" ]]; then
    printf '\n'
    cat "$RUNTIME_DIR/artifacts/result.env"
  fi
}

cleanup() {
  compose --profile original --profile master down --remove-orphans
  log "隔离容器已停止；演练数据仍保留在 $RUNTIME_DIR"
}

run_all() {
  preflight
  reset_runtime
  prepare_images
  clone_database
  start_original
  switch_master
  rollback_original
  write_report
}

main() {
  load_env
  case "${1:-}" in
    preflight) preflight ;;
    prepare-images) preflight; mkdir -p "$RUNTIME_DIR"; prepare_images ;;
    clone) preflight; reset_runtime; clone_database ;;
    start-original) start_original ;;
    switch-master) switch_master ;;
    rollback) rollback_original ;;
    report) write_report ;;
    status) status ;;
    cleanup) cleanup ;;
    all) run_all ;;
    *)
      printf '用法: SWITCH_ENV_FILE=/path/to/.env %s {preflight|prepare-images|clone|start-original|switch-master|rollback|report|status|cleanup|all}\n' "$0" >&2
      exit 2
      ;;
  esac
}

main "$@"
