#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
OVERRIDE_FILE="$SCRIPT_DIR/docker-compose.image-override.yml"
CANDIDATE_OVERRIDE_FILE="$SCRIPT_DIR/docker-compose.candidate-override.yml"
DB_AUDIT="$SCRIPT_DIR/db-audit.sh"
CUTOVER_ENV_FILE=${CUTOVER_ENV_FILE:-}
ROLLBACK_ARMED=false
RECOVERY_IMAGE=

log() {
  printf '[master-cutover] %s\n' "$*"
}

die() {
  printf '[master-cutover] ERROR: %s\n' "$*" >&2
  return 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"
}

load_env() {
  [[ -n "$CUTOVER_ENV_FILE" ]] || die "必须通过 CUTOVER_ENV_FILE 指定私有环境文件"
  [[ -f "$CUTOVER_ENV_FILE" ]] || die "环境文件不存在: $CUTOVER_ENV_FILE"
  local mode
  mode=$(stat -c '%a' "$CUTOVER_ENV_FILE")
  (( (8#$mode & 8#077) == 0 )) || die "环境文件权限必须为 0600 或更严格，当前为 $mode"
  # shellcheck disable=SC1090
  set -a
  source "$CUTOVER_ENV_FILE"
  set +a

  : "${TARGET_COMPOSE_DIR:?TARGET_COMPOSE_DIR is required}"
  : "${STATE_DIR:?STATE_DIR is required}"
  : "${BACKUP_DIR:?BACKUP_DIR is required}"
  : "${ORIGINAL_IMAGE:?ORIGINAL_IMAGE is required}"
  : "${MASTER_IMAGE:?MASTER_IMAGE is required}"
  : "${EXPECTED_MASTER_IMAGE_ID:?EXPECTED_MASTER_IMAGE_ID is required}"
  : "${DB_USER:?DB_USER is required}"
  : "${DB_NAME:?DB_NAME is required}"

  APP_SERVICE=${APP_SERVICE:-new-api}
  DB_SERVICE=${DB_SERVICE:-postgres}
  QUIESCE_SERVICES=${QUIESCE_SERVICES:-$APP_SERVICE}
  RESUME_SERVICES=${RESUME_SERVICES:-}
  CANDIDATE_CONTAINER=${CANDIDATE_CONTAINER:-new-api-master-switch-candidate}
  HEALTH_TIMEOUT_SECONDS=${HEALTH_TIMEOUT_SECONDS:-300}
  TARGET_DISTRIBUTED_ENABLED=${TARGET_DISTRIBUTED_ENABLED:-false}
  export TARGET_DISTRIBUTED_ENABLED

  [[ "$APP_SERVICE" == new-api ]] || die "当前 override 只支持 new-api 服务"
  [[ -f "$TARGET_COMPOSE_DIR/docker-compose.yml" ]] || die "目标 Compose 不存在"
  [[ "$STATE_DIR" == /* && "$STATE_DIR" != / ]] || die "STATE_DIR 必须是非根绝对路径"
  [[ "$BACKUP_DIR" == /* && "$BACKUP_DIR" != / ]] || die "BACKUP_DIR 必须是非根绝对路径"
  mkdir -p "$STATE_DIR" "$BACKUP_DIR"
}

compose_for_image() {
  local image=$1
  shift
  local command=(docker compose
    --project-directory "$TARGET_COMPOSE_DIR"
    -f "$TARGET_COMPOSE_DIR/docker-compose.yml"
    -f "$OVERRIDE_FILE")
  if [[ -n "${COMPOSE_PROJECT_NAME:-}" ]]; then
    command+=(--project-name "$COMPOSE_PROJECT_NAME")
  fi
  TARGET_APP_IMAGE="$image" "${command[@]}" "$@"
}

compose_candidate_for_image() {
  local image=$1
  shift
  local command=(docker compose
    --project-directory "$TARGET_COMPOSE_DIR"
    -f "$TARGET_COMPOSE_DIR/docker-compose.yml"
    -f "$OVERRIDE_FILE"
    -f "$CANDIDATE_OVERRIDE_FILE")
  if [[ -n "${COMPOSE_PROJECT_NAME:-}" ]]; then
    command+=(--project-name "$COMPOSE_PROJECT_NAME")
  fi
  TARGET_APP_IMAGE="$image" "${command[@]}" "$@"
}

service_container() {
  local image=$1 service=$2
  compose_for_image "$image" ps -q "$service"
}

db_container() {
  local container
  container=$(service_container "$MASTER_IMAGE" "$DB_SERVICE")
  [[ -n "$container" ]] || die "数据库服务未运行: $DB_SERVICE"
  printf '%s\n' "$container"
}

db_audit() {
  DB_CONTAINER="$(db_container)" DB_USER="$DB_USER" DB_NAME="$DB_NAME" "$DB_AUDIT" "$@"
}

wait_container_healthy() {
  local container=$1 timeout=${2:-$HEALTH_TIMEOUT_SECONDS} deadline status
  deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    case "$status" in
      healthy|running)
        log "容器已就绪: $container"
        return 0
        ;;
      unhealthy|exited|dead)
        docker logs --tail 200 "$container" >&2 || true
        return 1
        ;;
    esac
    sleep 2
  done
  docker logs --tail 200 "$container" >&2 || true
  return 1
}

verify_image_override() {
  local image=$1 configured_image configured_build
  configured_image=$(compose_for_image "$image" config --format json | jq -r '.services["new-api"].image')
  configured_build=$(compose_for_image "$image" config --format json | jq -r '.services["new-api"].build')
  [[ "$configured_image" == "$image" ]] || die "Compose 镜像覆盖失败"
  [[ "$configured_build" == null ]] || die "Compose build 未被禁用"
}

preflight() {
  require_command docker
  require_command jq
  require_command sha256sum
  docker info >/dev/null
  docker compose version >/dev/null
  verify_image_override "$MASTER_IMAGE"
  verify_image_override "$ORIGINAL_IMAGE"

  local master_id free_kb max_model_length app_container log_dsn
  master_id=$(docker image inspect -f '{{.Id}}' "$MASTER_IMAGE")
  [[ "$master_id" == "$EXPECTED_MASTER_IMAGE_ID" ]] || die "master 镜像 ID 不匹配：期望 $EXPECTED_MASTER_IMAGE_ID，实际 $master_id"
  docker image inspect "$ORIGINAL_IMAGE" >/dev/null 2>&1 || true
  docker exec "$(db_container)" pg_dump --version >/dev/null 2>&1 || die "数据库服务不是可用的 PostgreSQL"
  app_container=$(service_container "$MASTER_IMAGE" "$APP_SERVICE")
  [[ -n "$app_container" ]] || die "应用服务未运行: $APP_SERVICE"
  log_dsn=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$app_container" | awk -F= '$1 == "LOG_SQL_DSN" {sub(/^[^=]*=/, ""); print; exit}')
  [[ -z "$log_dsn" ]] || die "当前脚本只支持主库与日志库相同的部署；检测到 LOG_SQL_DSN"
  max_model_length=$(db_audit max-model-name-length)
  [[ "$max_model_length" =~ ^[0-9]+$ ]] || die "无法读取现有模型名长度"
  free_kb=$(df -Pk "$BACKUP_DIR" | awk 'NR==2 {print $4}')
  (( free_kb >= 2 * 1024 * 1024 )) || die "备份盘可用空间不足 2 GiB"
  log "预检通过；master=$master_id，现有最长模型名=$max_model_length"
}

freeze_original_image() {
  local container image_id
  container=$(service_container "$ORIGINAL_IMAGE" "$APP_SERVICE")
  [[ -n "$container" ]] || die "原版应用服务未运行"
  image_id=$(docker inspect -f '{{.Image}}' "$container")
  docker tag "$image_id" "$ORIGINAL_IMAGE"
  printf '%s\n' "$image_id" > "$STATE_DIR/original-image-id"
  log "原版镜像已冻结: $ORIGINAL_IMAGE -> $image_id"
}

stop_application_writers() {
  local image=$1 services=()
  read -r -a services <<< "$QUIESCE_SERVICES"
  compose_for_image "$image" stop "${services[@]}"
  log "写入服务已停止: $QUIESCE_SERVICES"
}

resume_services() {
  local image=$1 services=()
  if [[ -n "$RESUME_SERVICES" ]]; then
    read -r -a services <<< "$RESUME_SERVICES"
    compose_for_image "$image" up -d --no-deps --no-build "${services[@]}"
    log "配套服务已恢复: $RESUME_SERVICES"
  fi
}

backup_database() {
  local container timestamp dump_file
  container=$(db_container)
  timestamp=$(date +%Y%m%d-%H%M%S)
  dump_file="$BACKUP_DIR/new-api-before-switch-$timestamp.dump"
  docker exec "$container" pg_dump -U "$DB_USER" -d "$DB_NAME" \
    --format=custom --compress=6 --no-owner --no-privileges > "$dump_file"
  [[ -s "$dump_file" ]] || die "数据库备份为空"
  docker exec -i "$container" pg_restore --list < "$dump_file" >/dev/null
  sha256sum "$dump_file" > "$dump_file.sha256"
  printf '%s\n' "$dump_file" > "$STATE_DIR/latest-backup"
  log "停写快照已生成: $dump_file"
}

capture_metrics() {
  local output=$1
  db_audit metrics > "$output"
  [[ -s "$output" ]] || die "业务指标采集失败"
}

assert_metrics_equal() {
  local expected=$1 actual=$2 label=$3
  if ! cmp -s "$expected" "$actual"; then
    diff -u "$expected" "$actual" >&2 || true
    die "$label 业务指标不一致"
  fi
  log "$label 业务指标完全一致"
}

remove_candidate() {
  docker rm -f "$CANDIDATE_CONTAINER" >/dev/null 2>&1 || true
}

assert_container_image() {
  local container=$1 image=$2 actual expected
  actual=$(docker inspect -f '{{.Image}}' "$container")
  expected=$(docker image inspect -f '{{.Id}}' "$image")
  [[ "$actual" == "$expected" ]] || die "容器镜像 ID 不匹配：期望 $expected，实际 $actual"
}

assert_candidate_isolated() {
  local container=$1
  if docker inspect -f '{{json .NetworkSettings.Networks}}' "$container" | \
    jq -e --arg alias "$APP_SERVICE" '[.[] | (.Aliases // [])[]] | index($alias) != null' >/dev/null; then
    die "候选容器意外获得正式服务网络别名: $APP_SERVICE"
  fi
}

validate_candidate() {
  local image=$1 schema_check=$2 baseline=$3 actual=$4 candidate
  remove_candidate
  candidate=$(compose_candidate_for_image "$image" run -d --no-deps --name "$CANDIDATE_CONTAINER" "$APP_SERVICE")
  assert_container_image "$candidate" "$image"
  assert_candidate_isolated "$candidate"
  wait_container_healthy "$candidate" "$HEALTH_TIMEOUT_SECONDS" || die "候选容器启动失败"
  if [[ "$schema_check" == master ]]; then
    db_audit assert-master-schema || die "master Schema 校验失败"
  fi
  capture_metrics "$actual"
  assert_metrics_equal "$baseline" "$actual" "$schema_check 候选容器"
  remove_candidate
}

start_official() {
  local image=$1 container
  compose_for_image "$image" up -d --no-deps --no-build --force-recreate "$APP_SERVICE"
  container=$(service_container "$image" "$APP_SERVICE")
  [[ -n "$container" ]] || die "正式应用容器未创建"
  assert_container_image "$container" "$image"
  wait_container_healthy "$container" "$HEALTH_TIMEOUT_SECONDS" || die "正式应用容器启动失败"
}

emergency_recovery() {
  local exit_code=$?
  if [[ "$ROLLBACK_ARMED" == true && -n "$RECOVERY_IMAGE" ]]; then
    set +e
    log "切换失败，尝试恢复镜像: $RECOVERY_IMAGE"
    remove_candidate
    start_official "$RECOVERY_IMAGE"
    resume_services "$RECOVERY_IMAGE"
    set -e
  fi
  exit "$exit_code"
}

cutover_master() {
  [[ "${CONFIRM_PRODUCTION_SWITCH:-}" == YES ]] || die "必须设置 CONFIRM_PRODUCTION_SWITCH=YES"
  [[ "${MASTER_PARITY_APPROVED:-}" == YES ]] || die "代码功能对齐未确认：必须设置 MASTER_PARITY_APPROVED=YES"
  [[ "$TARGET_DISTRIBUTED_ENABLED" == false ]] || die "首次切换只允许 EDGE_DISTRIBUTED_ENABLED=false"
  preflight
  local max_model_length
  max_model_length=$(db_audit max-model-name-length)
  (( max_model_length <= 64 )) || die "现有 quota_data.model_name 最长 $max_model_length，无法保证原版回滚"
  freeze_original_image
  RECOVERY_IMAGE=$ORIGINAL_IMAGE
  ROLLBACK_ARMED=true
  trap emergency_recovery ERR
  stop_application_writers "$ORIGINAL_IMAGE"
  backup_database
  capture_metrics "$STATE_DIR/before-master.metrics.json"
  validate_candidate "$MASTER_IMAGE" master "$STATE_DIR/before-master.metrics.json" "$STATE_DIR/master-candidate.metrics.json"
  start_official "$MASTER_IMAGE"
  resume_services "$MASTER_IMAGE"
  printf '%s\n' "$EXPECTED_MASTER_IMAGE_ID" > "$STATE_DIR/active-image-id"
  printf 'master\n' > "$STATE_DIR/active-version"
  ROLLBACK_ARMED=false
  trap - ERR
  log "生产已切换至 master；分布式控制面保持关闭"
}

rollback_original() {
  [[ "${CONFIRM_PRODUCTION_ROLLBACK:-}" == YES ]] || die "必须设置 CONFIRM_PRODUCTION_ROLLBACK=YES"
  preflight
  local edge_nodes blockers total max_model_length original_id
  docker image inspect "$ORIGINAL_IMAGE" >/dev/null 2>&1 || die "冻结的原版镜像不存在: $ORIGINAL_IMAGE"
  stop_application_writers "$MASTER_IMAGE"
  edge_nodes=$(db_audit edge-node-count)
  if (( edge_nodes > 0 )) && [[ "${CONFIRM_EDGE_NODES_DRAINED:-}" != YES ]]; then
    start_official "$MASTER_IMAGE"
    resume_services "$MASTER_IMAGE"
    die "存在 $edge_nodes 个 edge 节点，必须完成外部 drain 并设置 CONFIRM_EDGE_NODES_DRAINED=YES"
  fi
  blockers=$(db_audit edge-rollback-blockers)
  total=$(awk -F= '$1 == "total" {print $2}' <<< "$blockers")
  if [[ "$total" != 0 ]]; then
    printf '%s\n' "$blockers" >&2
    start_official "$MASTER_IMAGE"
    resume_services "$MASTER_IMAGE"
    die "edge 结算状态未清空，拒绝回滚"
  fi
  max_model_length=$(db_audit max-model-name-length)
  if (( max_model_length > 64 )); then
    start_official "$MASTER_IMAGE"
    resume_services "$MASTER_IMAGE"
    die "存在超过 64 字符的 quota_data.model_name，拒绝回滚"
  fi
  RECOVERY_IMAGE=$MASTER_IMAGE
  ROLLBACK_ARMED=true
  trap emergency_recovery ERR
  capture_metrics "$STATE_DIR/before-rollback.metrics.json"
  validate_candidate "$ORIGINAL_IMAGE" original "$STATE_DIR/before-rollback.metrics.json" "$STATE_DIR/original-candidate.metrics.json"
  start_official "$ORIGINAL_IMAGE"
  resume_services "$ORIGINAL_IMAGE"
  original_id=$(docker image inspect -f '{{.Id}}' "$ORIGINAL_IMAGE")
  printf '%s\n' "$original_id" > "$STATE_DIR/active-image-id"
  printf 'original\n' > "$STATE_DIR/active-version"
  ROLLBACK_ARMED=false
  trap - ERR
  log "生产已切回冻结的原版镜像；新增 master 表会保留但不影响原版"
}

status() {
  preflight
  compose_for_image "$MASTER_IMAGE" ps -a
  if [[ -f "$STATE_DIR/active-version" ]]; then
    printf 'active_version=%s\n' "$(<"$STATE_DIR/active-version")"
    printf 'active_image_id=%s\n' "$(<"$STATE_DIR/active-image-id")"
  fi
  db_audit edge-rollback-blockers
}

main() {
  load_env
  case "${1:-}" in
    preflight) preflight ;;
    cutover) cutover_master ;;
    rollback) rollback_original ;;
    status) status ;;
    *)
      printf '用法: CUTOVER_ENV_FILE=/private/.env %s {preflight|cutover|rollback|status}\n' "$0" >&2
      exit 2
      ;;
  esac
}

main "$@"
