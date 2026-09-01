# general-exporter

轻量级 Prometheus Exporter，用于监控 HTTP 服务、Docker 容器、远程 Metrics 代理、MongoDB 进度表和自定义脚本指标。

## 特性

- **零依赖部署**：单个二进制文件，无需额外运行时
- **URL 探测**：HTTP/HTTPS 可用性、状态码、响应内容、延迟、响应大小
- **Docker 监控**：容器状态、CPU 使用率、内存使用/上限、磁盘容量（可写层 + rootfs）
- **远程代理**：拉取其他 Exporter 的 /metrics，保留完整 Label 和原始格式
- **MongoDB 心跳监控**：只读查询进度表/状态集合，输出进程存活与进度指标（如扫描器的 `blockno` 进度表、定时任务的 `process_status`）
- **自定义脚本**：执行任意脚本，输出自定义指标（一行一个，`指标名 数值` 格式）
- **全局标签**：`defaults.global_labels` 自动注入所有指标（url/docker/custom/mongodb，remote 除外）
- **YAML 配置**：声明式配置文件，灵活扩展

## 快速开始

### 1. 编译

```bash
go build -o general-exporter
```

### 2. 配置

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml
```

### 3. 运行

```bash
./general-exporter
```

### 4. 访问

| 端点 | 说明 |
|------|------|
| `http://localhost:8081/metrics` | Prometheus 指标 |
| `http://localhost:8081/health` | 健康检查 |
| `http://localhost:8081/config` | 当前配置（JSON） |

默认端口 8081，可在 `config.yaml` 中修改。

## 信号处理

| 信号 | 作用 |
|------|------|
| `SIGHUP` | 热加载配置，无需重启 |
| `SIGTERM` | 优雅关闭 |

```bash
kill -HUP $(pgrep -f general-exporter)  # 热加载
kill $(pgrep -f general-exporter)        # 优雅关闭
```

## 配置示例

### URL 探测

```yaml
- name: "frontend_main"
  type: url
  url: "https://example.com"
  method: GET
  expected_status: 200
  expected_body_contains: "welcome"
  labels:
    env: "prod"
    tier: "frontend"
```

### Docker 容器监控

```yaml
# 所有容器
- name: "all_containers"
  type: docker
  mode: all
  labels:
    env: "prod"
    tier: "docker"

# 指定容器
- name: "key_services"
  type: docker
  mode: filter
  names:
    - "nginx"
    - "redis"
  labels:
    env: "prod"
    tier: "docker"
```

### 自定义脚本指标

```yaml
- name: "skywell_node"
  type: custom
  script: "/opt/scripts/skywell-stats.sh"
  labels:
    env: "prod"
    tier: "node"
```

脚本输出格式（每行 `指标名 数值`，支持 `#` 注释）：
```bash
#!/bin/bash
echo "skywell_cpu_percent 42.5"
echo "skywell_memory_bytes 512000000"
echo "skywell_uptime_seconds 86400"
echo "skywell_connections 15"
```

### 远程 Metrics 代理

```yaml
- name: "remote_node_exporter"
  type: remote
  remote:
    url: "http://localhost:9100/metrics"
    # headers:         # 可选，自定义请求头
    #   Authorization: "Bearer xxx"
```

### MongoDB 进度表心跳监控（扫描器 / 定时任务）

> 适用场景：区块扫描器、跨链排行 cronjob 等**自己写进度表**的服务。
> exporter 只读查询最新进度文档，输出"进程是否活着 + 扫到哪了"，判定逻辑交给 PromQL 完成。

```yaml
- name: "swtc_balance"
  type: mongodb
  uri: "mongodb://192.168.66.254:27018/?directConnection=true"
  database: "skywell_profit"
  collection: "blockno"
  query: { processName: "scan_balance" }   # 可选；不填 = 取集合最新文档(按 _id 倒序)
  interval: 30s                             # 可选，默认 30s
  metrics:
    - { name: "swtc_scan_last_update", field: "updateAt" }  # 毫秒时间戳
    - { name: "swtc_scan_end", field: "end" }               # 已扫区块高度
  labels:
    env: "prod"
    tier: "blockchain"
```

**字段说明：**

| 字段 | 必填 | 说明 |
|------|------|------|
| `uri` | ✅ | MongoDB 连接串。**只做查询，绝不写入** |
| `database` / `collection` | ✅ | 目标库与集合（如 `skywell_profit.blockno`） |
| `query` | ❌ | 过滤条件（如 `{processName: "scan_balance"}`）；**为空时取集合最新文档** |
| `metrics[].name` | ✅ | 输出的 Prometheus 指标名（原样生成 Gauge） |
| `metrics[].field` | ✅ | 文档中要读取的字段（支持 int/int32/int64/float/string/时间戳） |

**设计要点（方案 A 兜底 + A1 语义）：**
- 每个 target 固定输出 `mongodb_probe_up{name="<target名>"}`：查询成功=1，**任何失败只发 `up=0` 且不发数值指标**（防旧值误判"数据还新鲜"）
- `updateAt` 类毫秒时间戳原样输出；PromQL 用 `time()*1000 - metric > 阈值` 判断心跳新鲜度
- 若集合存在新旧多条记录（如历史遗留无 processName 的记录），**必须配 `query` 过滤**，否则会读到过期文档

## 暴露指标

### URL 指标

| 指标 | 标签 | 说明 |
|------|------|------|
| `url_up` | `name, env, tier` | 探测成功=1，失败=0 |
| `url_http_status` | `name, env, tier` | HTTP 状态码 |
| `url_duration_seconds` | `name, env, tier` | 请求耗时（秒） |
| `url_body_match` | `name, env, tier` | 响应体包含期望内容=1 |
| `url_status_match` | `name, env, tier` | 状态码匹配=1 |
| `url_response_size_bytes` | `name, env, tier` | 响应体大小（字节） |

### Docker 指标

| 指标 | 标签 | 说明 |
|------|------|------|
| `docker_container_up` | `container, image, env, tier` + global | 运行中=1，停止=0 |
| `docker_container_cpu_percent` | 同上 | CPU 使用率 % |
| `docker_container_memory_usage_bytes` | 同上 | 内存使用（字节） |
| `docker_container_memory_limit_bytes` | 同上 | 内存上限（字节） |
| `docker_container_disk_rw_bytes` | 同上 | 可写层磁盘大小（字节） |
| `docker_container_disk_rootfs_bytes` | 同上 | 整个 rootfs 大小（字节） |

### 自定义脚本指标

脚本输出的每一行 `指标名 数值` 自动转换为 Prometheus Gauge，
自动携带 `name, env, tier` 标签。

示例输出：
```
skywell_cpu_percent{env="prod",name="skywell_node",tier="node"} 42.5
skywell_memory_bytes{env="prod",name="skywell_node",tier="node"} 5.12e+08
```

### 远程代理指标

直接透传远程 Exporter 的原始指标文本，保留所有 `HELP`、`TYPE` 注释和 `Label`，
与 Prometheus 直接抓取该 Exporter 的效果完全一致。

### MongoDB 指标

每个 `type: mongodb` target 固定输出一个存活指标 + 按 `metrics` 配置输出的数值指标：

| 指标 | 标签 | 说明 |
|------|------|------|
| `mongodb_probe_up` | `name, env, tier` + global | 查询成功=1；失败=0（且不发数值指标） |
| `swtc_scan_last_update`（自定义名） | 同上 | 文档 `updateAt` 等毫秒时间戳 |
| `swtc_scan_end`（自定义名） | 同上 | 文档 `end` 等进度数值（如已扫区块高度） |

示例输出：
```
mongodb_probe_up{name="swtc_balance", env="prod", tier="blockchain"} 1
swtc_scan_end{name="swtc_balance", env="prod", tier="blockchain"} 3.5831831e+07
swtc_scan_last_update{name="swtc_balance", env="prod", tier="blockchain"} 1.788169295011e+12
```

## 配置项说明

详见 [CONFIG.md](CONFIG.md)。

## Prometheus 配置

```yaml
scrape_configs:
  - job_name: 'general-exporter'
    static_configs:
      - targets: ['localhost:8081']
    scrape_interval: 15s
```

## License

MIT
