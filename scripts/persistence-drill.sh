#!/usr/bin/env bash
# 验证 Redis 业务状态和 RabbitMQ 持久消息能跨容器重建保留。
# 本脚本会短暂中断 Redis/RabbitMQ 连接，只应在本地或维护窗口运行。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose=(docker compose --project-directory "$repo_root" -f "$repo_root/docker-compose.yml")
run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
queue="persistence.drill.$run_id"
message="persistent-message-$run_id"
rabbitmq_user="${PERSISTENCE_DRILL_RABBITMQ_USER:-guest}"
rabbitmq_password="${PERSISTENCE_DRILL_RABBITMQ_PASSWORD:-guest}"
redis_user="${PERSISTENCE_DRILL_REDIS_USER:-default}"
redis_password="${PERSISTENCE_DRILL_REDIS_PASSWORD:-}"

order_idem_key="order:idem:persistence-drill:$run_id"
flash_stock_key="flashsale:stock:persistence-drill:$run_id"
flash_count_key="flashsale:count:persistence-drill:user:$run_id"
flash_idem_key="flashsale:idem:persistence-drill:user:$run_id"
flash_reservation_key="flashsale:reservation:persistence-drill:$run_id"
coupon_total_key="coupon:claimed:persistence-drill:$run_id"
coupon_user_key="coupon:peruser:persistence-drill:user:$run_id"
redis_keys=(
  "$order_idem_key"
  "$flash_stock_key"
  "$flash_count_key"
  "$flash_idem_key"
  "$flash_reservation_key"
  "$coupon_total_key"
  "$coupon_user_key"
)

log() {
  printf '[persistence-drill] %s\n' "$*"
}

fail() {
  printf '[persistence-drill] 失败: %s\n' "$*" >&2
  exit 1
}

has_line() {
  local expected="$1"
  local line
  while IFS= read -r line; do
    if [[ "$line" == "$expected" ]]; then
      return 0
    fi
  done
  return 1
}

redis() {
  local args=(--raw)
  if [[ -n "$redis_password" ]]; then
    args+=(--user "$redis_user" --pass "$redis_password" --no-auth-warning)
  fi
  "${compose[@]}" exec -T redis redis-cli "${args[@]}" "$@"
}

rabbitmqadmin() {
  "${compose[@]}" exec -T rabbitmq rabbitmqadmin --username="$rabbitmq_user" --password="$rabbitmq_password" "$@"
}

assert_redis_value() {
  local key="$1"
  local expected="$2"
  local actual
  actual="$(redis GET "$key")"
  [[ "$actual" == "$expected" ]] || fail "Redis 键 $key: 期望 $expected，实际 ${actual:-<空>}"
}

assert_positive_ttl() {
  local key="$1"
  local ttl
  ttl="$(redis TTL "$key")"
  [[ "$ttl" =~ ^[0-9]+$ && "$ttl" -gt 0 ]] || fail "Redis 键 $key 的 TTL 应大于 0，实际 $ttl"
}

assert_no_ttl() {
  local key="$1"
  local ttl
  ttl="$(redis TTL "$key")"
  [[ "$ttl" == "-1" ]] || fail "Redis 键 $key 应持久保留，实际 TTL=$ttl"
}

assert_queue_ready() {
  local rows
  local name
  local durable
  local ready
  rows="$("${compose[@]}" exec -T rabbitmq rabbitmqctl -q list_queues name durable messages_ready)"
  while IFS=$'\t' read -r name durable ready; do
    if [[ "$name" == "$queue" && "$durable" == "true" && "$ready" == "1" ]]; then
      return 0
    fi
  done <<< "$rows"
  fail "RabbitMQ 队列 $queue 不是 durable=true/messages_ready=1"
}

cleanup() {
  local status=$?
  set +e
  redis DEL "${redis_keys[@]}" >/dev/null 2>&1
  rabbitmqadmin delete queue name="$queue" >/dev/null 2>&1
  if [[ "$status" -eq 0 ]]; then
    log "测试数据已清理"
  else
    log "演练失败；已尽力清理测试数据"
  fi
  exit "$status"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || fail "未找到 docker"
docker compose version >/dev/null 2>&1 || fail "未安装 Docker Compose 插件"

configured_volumes="$("${compose[@]}" config --volumes)"
has_line redis_data <<< "$configured_volumes" || fail "Compose 未定义 redis_data 命名卷"
has_line rabbitmq_data <<< "$configured_volumes" || fail "Compose 未定义 rabbitmq_data 命名卷"

# 首次从匿名卷升级到命名卷必须先按部署指南迁移，不能让 compose 静默换空卷。
if docker inspect go_single_redis >/dev/null 2>&1; then
  existing_redis_mount="$(docker inspect go_single_redis --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')"
  [[ "$existing_redis_mount" == "go_single_redis_data" ]] || fail "检测到 Redis 旧匿名卷；请先按 docs/DEPLOYMENT.md 的首次迁移步骤备份和切换"
fi
if docker inspect go_single_rabbitmq >/dev/null 2>&1; then
  existing_rabbitmq_mount="$(docker inspect go_single_rabbitmq --format '{{range .Mounts}}{{if eq .Destination "/var/lib/rabbitmq"}}{{.Name}}{{end}}{{end}}')"
  [[ "$existing_rabbitmq_mount" == "go_single_rabbitmq_data" ]] || fail "检测到 RabbitMQ 旧匿名卷；请先按 docs/DEPLOYMENT.md 的首次迁移步骤备份和切换"
fi

log "启动 Redis 与 RabbitMQ"
"${compose[@]}" up -d --wait redis rabbitmq

redis_container="$("${compose[@]}" ps -q redis)"
rabbitmq_container="$("${compose[@]}" ps -q rabbitmq)"
redis_mount="$(docker inspect "$redis_container" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')"
rabbitmq_mount="$(docker inspect "$rabbitmq_container" --format '{{range .Mounts}}{{if eq .Destination "/var/lib/rabbitmq"}}{{.Name}}{{end}}{{end}}')"
[[ "$redis_mount" == "go_single_redis_data" ]] || fail "Redis /data 未使用 go_single_redis_data，实际 ${redis_mount:-<无挂载>}"
[[ "$rabbitmq_mount" == "go_single_rabbitmq_data" ]] || fail "RabbitMQ 数据目录未使用 go_single_rabbitmq_data，实际 ${rabbitmq_mount:-<无挂载>}"

appendonly_config="$(redis CONFIG GET appendonly)"
appendfsync_config="$(redis CONFIG GET appendfsync)"
[[ "$appendonly_config" == *$'\nyes' ]] || fail "Redis appendonly 未启用"
[[ "$appendfsync_config" == *$'\neverysec' ]] || fail "Redis appendfsync 不是 everysec"

log "写入订单幂等、秒杀预扣和优惠券计数状态"
redis SET "$order_idem_key" "order-persistence-drill-$run_id" EX 900 >/dev/null
redis SET "$flash_stock_key" 17 EX 1800 >/dev/null
redis SET "$flash_count_key" 2 >/dev/null
redis SET "$flash_idem_key" "order-persistence-drill-flash-$run_id" EX 1800 >/dev/null
redis SET "$flash_reservation_key" "order-persistence-drill-flash-$run_id" >/dev/null
redis SET "$coupon_total_key" 9 >/dev/null
redis SET "$coupon_user_key" 1 >/dev/null

log "声明 durable 队列并发布 delivery_mode=2 消息"
rabbitmqadmin declare queue name="$queue" durable=true auto_delete=false >/dev/null
rabbitmqadmin publish \
  exchange=amq.default \
  routing_key="$queue" \
  payload="$message" \
  properties='{"delivery_mode":2,"content_type":"application/json"}' >/dev/null
assert_queue_ready

# appendfsync=everysec 最坏丢失约 1 秒，等待跨过一次 fsync 周期再重建容器。
sleep 2

log "删除并重建 Redis/RabbitMQ 容器（保留命名卷）"
"${compose[@]}" rm -sf redis rabbitmq >/dev/null
"${compose[@]}" up -d --wait redis rabbitmq

log "验证 Redis 状态与 TTL"
assert_redis_value "$order_idem_key" "order-persistence-drill-$run_id"
assert_positive_ttl "$order_idem_key"
assert_redis_value "$flash_stock_key" 17
assert_positive_ttl "$flash_stock_key"
assert_redis_value "$flash_count_key" 2
assert_no_ttl "$flash_count_key"
assert_redis_value "$flash_idem_key" "order-persistence-drill-flash-$run_id"
assert_positive_ttl "$flash_idem_key"
assert_redis_value "$flash_reservation_key" "order-persistence-drill-flash-$run_id"
assert_no_ttl "$flash_reservation_key"
assert_redis_value "$coupon_total_key" 9
assert_no_ttl "$coupon_total_key"
assert_redis_value "$coupon_user_key" 1
assert_no_ttl "$coupon_user_key"

log "验证 durable 队列与 persistent 消息"
assert_queue_ready
delivery="$(rabbitmqadmin --format=raw_json get queue="$queue" ackmode=ack_requeue_false count=1)"
[[ "$delivery" == *"$message"* ]] || fail "RabbitMQ 重建后未读到测试消息"
[[ "$delivery" == *'"delivery_mode": 2'* || "$delivery" == *'"delivery_mode":2'* ]] || fail "RabbitMQ 消息不是 delivery_mode=2"

log "通过：Redis 关键状态和 RabbitMQ 持久消息均跨容器重建保留"
