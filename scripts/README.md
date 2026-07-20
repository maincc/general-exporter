# ssh_tunnel_check.sh — SSH 隧道健康检查脚本

## 📌 用途

用于 `general-exporter` custom collector，探测 SSH 隧道连通性并输出 Prometheus 指标。

## 📍 部署位置

| 环境 | 路径 |
|------|------|
| **容器内** | `/app/scripts/ssh_tunnel_check.sh` |
| **宿主机源码** | `scripts/ssh_tunnel_check.sh` |

## 📡 输出指标

| 指标名 | 类型 | 说明 |
|--------|------|------|
| `ssh_tunnel_up` | gauge | `1` = 隧道正常，`0` = 隧道异常 |
| `ssh_tunnel_latency_ms` | gauge | 探测耗时（毫秒），失败时 `-1` |
| `ssh_tunnel_config_error` | gauge | `1` = 未配置 SSH_TUNNELS 变量 |

## ⚙️ 配置

在 `config.yaml` 的 custom target 中通过环境变量传入：

```yaml
- name: "ssh_rabbitmq"
  type: custom
  script: "/app/scripts/ssh_tunnel_check.sh"
  env:
    SSH_TUNNELS: "ssh_rabbitmq:5672:192.168.200.123:5672"
    SSH_CHECK_TIMEOUT: "5"
  labels:
    env: "prod"
    tier: "rabbitmq"
```

### SSH_TUNNELS 格式

```
name:local_port:remote_host:remote_port[:check_url]
```

| 字段 | 说明 | 示例 |
|------|------|------|
| `name` | 隧道名称（决定探测协议） | `ssh_rabbitmq` |
| `local_port` | 本地 SSH 转发端口 | `5672` |
| `remote_host` | 远程目标地址 | `192.168.200.123` |
| `remote_port` | 远程目标端口 | `5672` |
| `check_url`（可选）| HTTP 端到端检查 URL | `http://localhost:15672` |

多个隧道用空格分隔：
```
SSH_TUNNELS: "rabbitmq-main:5672:10.0.0.1:5672 redis-cache:6380:10.0.0.2:6380"
```

## 🔍 协议识别规则

根据 `name` 前缀自动选择探测方式：

| name 包含 | 探测方式 | 判断条件 |
|-----------|---------|---------|
| `rabbitmq`, `amqp` | `curl --http0.9` 读握手 | 以 `AMQP` 开头 |
| `redis`, `valkey` | 发 `PING` 命令 | 收到 `+`/`-`/`:`/`$` 开头的 RESP 响应 |
| `mysql`, `mariadb` | nc 连接读握手包 | 有非控制字符响应 |
| `mongo` | nc 连接 | 有响应数据 |
| `postgres`, `pg` | 发 SSL 请求 | 有响应 |
| `http`, `nginx`, `web`, `api` | curl HTTP | 2xx/3xx 状态码 |
| **其他** | 回退 `nc -z` | 端口能连上 |

## 🚀 手动测试

```bash
# 测试单个隧道
docker exec \
  -e SSH_TUNNELS="ssh_rabbitmq:5672:192.168.200.123:5672" \
  -e SSH_CHECK_TIMEOUT="5" \
  general-exporter sh -c "/app/scripts/ssh_tunnel_check.sh"
```

预期输出：
```
ssh_tunnel_config_error 0
ssh_tunnel_up 1
ssh_tunnel_latency_ms 150
```

## 📊 查看指标

```bash
curl -s http://localhost:8081/metrics | grep ssh_tunnel
```

## 🔄 更新脚本

### 方式一：直接替换容器内文件

```bash
# 从宿主机拷贝新脚本到容器
docker cp scripts/ssh_tunnel_check.sh general-exporter:/app/scripts/ssh_tunnel_check.sh
docker exec general-exporter chmod +x /app/scripts/ssh_tunnel_check.sh

# 重新加载配置（触发新采集）
docker kill -s HUP general-exporter
```

### 方式二：重新构建镜像

```bash
docker compose build --no-cache
docker compose up -d
```

## ⚠️ 注意事项

- 脚本需容器内有 `nc`（busybox 或 ncat）和 `curl`
- `SSH_TUNNELS` 变量名不能有空格
- 未配置 `SSH_TUNNELS` 时输出 `config_error=1` 并退出
