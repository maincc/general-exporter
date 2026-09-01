#!/bin/sh
# SSH 隧道健康检查脚本
# 用于 general-exporter custom collector
# 输出格式: 指标名 数值
# 兼容 Alpine /bin/sh（POSIX sh）

TUNNELS_DEF="${SSH_TUNNELS:-}"
TIMEOUT="${SSH_CHECK_TIMEOUT:-5}"

now_ms() {
  python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || date +%s000
}

if [ -z "$TUNNELS_DEF" ]; then
  echo "ssh_tunnel_config_error 1"
  echo "# No tunnels configured, set SSH_TUNNELS in config.yaml env"
  exit 0
fi

echo "ssh_tunnel_config_error 0"

# POSIX 兼容：逐个解析隧道（空格分隔，冒号分段）
for entry in $TUNNELS_DEF; do
  [ -z "$entry" ] && continue

  name=$(echo "$entry" | cut -d: -f1)
  local_port=$(echo "$entry" | cut -d: -f2)
  remote_host=$(echo "$entry" | cut -d: -f3)
  remote_port=$(echo "$entry" | cut -d: -f4)
  check_url=$(echo "$entry" | cut -d: -f5-)

  if [ -z "$name" ] || [ -z "$local_port" ]; then
    echo "# skip malformed entry: $entry"
    continue
  fi

  remote_host="${remote_host:-127.0.0.1}"
  remote_port="${remote_port:-$local_port}"

  # 1) 按服务名识别协议，进行协议级探测
  port_open=0
  latency=-1
  start_ms=$(now_ms)

  name_lower=$(echo "$name" | tr '[:upper:]' '[:lower:]')

  case "$name_lower" in
    *rabbitmq*|*amqp*)
      # RabbitMQ: curl --http0.9 读取 AMQP 握手标识
      resp=$(curl -s --connect-timeout "$TIMEOUT" --max-time "$TIMEOUT" --http0.9 "http://127.0.0.1:${local_port}/" 2>/dev/null)
      case "$resp" in
        AMQP*) port_open=1 ;;
      esac
      ;;
    *redis*|*valkey*)
      # Redis/Valkey: 发 PING 看协议响应
      resp=$(echo -e "PING\r\n" | nc -w "$TIMEOUT" 127.0.0.1 "$local_port" 2>/dev/null)
      case "$resp" in
        +*|-*|:*|\$*) port_open=1 ;;
      esac
      ;;
    *mysql*|*mariadb*)
      # MySQL: 连接后首字节 0x0A 是握手包
      resp=$(echo "" | nc -w "$TIMEOUT" 127.0.0.1 "$local_port" 2>/dev/null || true)
      case "$resp" in
        *[![:cntrl:]]*) port_open=1 ;;
      esac
      ;;
    *mongo*)
      # MongoDB: 尝试发送 isMaster BSON 请求（简化探测）
      resp=$(echo "" | nc -w "$TIMEOUT" 127.0.0.1 "$local_port" 2>/dev/null || true)
      [ -n "$resp" ] && port_open=1
      ;;
    *postgres*|*pg*|*postgresql*)
      # PostgreSQL: 发 SSL 请求看是否响应
      resp=$(echo -e "\x00\x00\x00\x08\x04\xD2\x16\x2F" | nc -w "$TIMEOUT" 127.0.0.1 "$local_port" 2>/dev/null || true)
      [ -n "$resp" ] && port_open=1
      ;;
    *http*|*nginx*|*web*|*api*)
      # HTTP: curl 检查 2xx/3xx
      http_code=$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout "$TIMEOUT" --max-time "$TIMEOUT" "http://127.0.0.1:${local_port}/" 2>/dev/null)
      case "$http_code" in
        [23]*) port_open=1 ;;
      esac
      ;;
    *)
      # 未知服务 → 回退端口扫描
      if command -v nc >/dev/null 2>&1; then
        nc -z -w "$TIMEOUT" 127.0.0.1 "$local_port" 2>/dev/null && port_open=1
      elif command -v curl >/dev/null 2>&1; then
        curl -s --connect-timeout "$TIMEOUT" "127.0.0.1:${local_port}" >/dev/null 2>&1 && port_open=1
      else
        pgrep -f "ssh.*:${local_port}" >/dev/null 2>&1 && port_open=1
      fi
      ;;
  esac

  end_ms=$(now_ms)
  [ "$port_open" -eq 1 ] && latency=$((end_ms - start_ms))

  if [ "$port_open" -eq 0 ]; then
    echo "ssh_tunnel_up 0"
    echo "ssh_tunnel_latency_ms -1"
    echo "# tunnel=$name"
    continue
  fi

  # 2) 可选：HTTP 端到端检查（覆盖 nc 延迟）
  if [ -n "$check_url" ]; then
    start_ms=$(now_ms)
    http_code=$(curl -sk -o /dev/null -w '%{http_code}' \
      --connect-timeout "$TIMEOUT" --max-time "$((TIMEOUT + 2))" \
      "$check_url" 2>/dev/null)
    end_ms=$(now_ms)
    latency=$((end_ms - start_ms))

    # HTTP 状态码 < 200 或 >= 400 视为失败
    if [ "$http_code" -lt 200 ] 2>/dev/null || [ "$http_code" -ge 400 ] 2>/dev/null; then
      echo "ssh_tunnel_up 0"
      echo "ssh_tunnel_latency_ms -1"
      echo "# tunnel=$name"
      continue
    fi
  fi

  echo "ssh_tunnel_up 1"
  echo "ssh_tunnel_latency_ms $latency"
done
