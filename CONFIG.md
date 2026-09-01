# 配置文件说明 (config.yaml)

## 📐 整体结构

```yaml
server:        # 服务配置
defaults:      # 全局默认值
targets:       # 采集目标列表
  - name: "xxx"
    type: xxx  # url | docker | custom | remote | mongodb
    ...
```

---

## 🔧 server — 服务配置

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `port` | `8080` | 服务端口 |
| `metrics_path` | `/metrics` | 指标端点路径 |
| `max_concurrent` | `10` | URL/Docker/脚本采集最大并发数（防止资源耗尽） |
| `expose_runtime_metrics` | `true` | 是否暴露 Go runtime / Process 指标（`go_*`、`process_*`） |

### 新增端点

| 路径 | 说明 |
|------|------|
| `/config` | 返回当前加载的配置（JSON），方便调试 |

---

## 🌍 defaults — 全局默认值

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `interval` | `30s` | 采集间隔（每个 target 可覆盖） |
| `timeout` | `10s` | 请求超时 |
| `global_labels` | 空 | 全局标签，自动注入 url/docker/custom/mongodb 指标（remote 除外） |

### global_labels 示例

```yaml
defaults:
  interval: 30s
  timeout: 10s
  global_labels:
    cluster: "prod-1"
    region: "cn-shanghai"
```

效果：
```
url_up{name="frontend", env="prod", tier="frontend", cluster="prod-1", region="cn-shanghai"} 1
docker_container_up{container="nginx", image="nginx:latest", env="prod", tier="docker", cluster="prod-1", region="cn-shanghai"} 1
```

Keys 按字母排序，确保 label 顺序一致。

---

## 🎯 targets — 采集目标

公共字段：

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | ✅ | 指标名称标识 |
| `type` | ✅ | `url`、`docker`、`custom`、`remote` 或 `mongodb` |
| `interval` | ❌ | 覆盖全局采集间隔（预留） |
| `labels` | ❌ | 附加标签，会写入所有指标 |

### labels 字段

每个 target 可自定义 `labels`，目前支持：

| Key | 说明 | 示例 |
|-----|------|------|
| `env` | 环境标识 | `prod`、`dev`、`staging` |
| `tier` | 服务层级 | `frontend`、`backend`、`docker`、`database` |

配置示例：
```yaml
labels:
  env: "prod"
  tier: "frontend"
```

生成的指标会自动携带这些标签：
```
url_up{name="frontend_main", env="prod", tier="frontend"} 1
docker_container_up{container="nginx", image="nginx:latest", env="prod", tier="docker"} 1
```

**用途**：
- Grafana 面板按 `env` 过滤不同环境
- PromQL 按 `tier` 分组聚合（`sum by (tier)`）
- 告警规则按层级路由不同通道

未配置的 label 值为空字符串 `""`。

---

### 1️⃣ type: url — 前端 / 后端服务

探测 HTTP/HTTPS 服务的可用性、状态码、响应内容。

| 字段 | 必填 | 说明 |
|------|------|------|
| `url` | ✅ | 目标 URL |
| `method` | ❌ | 请求方法，默认 `GET` |
| `expected_status` | ❌ | 期望的状态码 |
| `expected_body_contains` | ❌ | 期望响应体包含的文本 |
| `headers` | ❌ | 自定义请求头 |

**暴露指标：**

| 指标 | 标签 | 说明 |
|------|------|------|
| `url_up` | `name, env, tier` | 探测成功=1，失败=0 |
| `url_http_status` | `name, env, tier` | 实际 HTTP 状态码 |
| `url_duration_seconds` | `name, env, tier` | 请求耗时（秒） |
| `url_body_match` | `name, env, tier` | 响应体匹配=1 |
| `url_status_match` | `name, env, tier` | 状态码匹配=1 |
| `url_response_size_bytes` | `name, env, tier` | 响应体大小（字节） |

**示例：**

```yaml
- name: "frontend_main"
  type: url
  url: "https://example.com"
  expected_status: 200
  interval: 15s
  labels:
    env: "prod"
    tier: "frontend"
```

---

### 2️⃣ type: docker — Docker 容器

监控容器运行状态、CPU、内存。

| 字段 | 必填 | 说明 |
|------|------|------|
| `mode` | ❌ | `all`（所有容器）或 `filter`（指定容器） |
| `names` | ❌ | mode=filter 时指定容器名列表 |

**暴露指标：**

| 指标 | 标签 | 说明 |
|------|------|------|
| `docker_container_up` | `container, image, env, tier` + global | 运行中=1，停止=0 |
| `docker_container_cpu_percent` | 同上 | CPU 使用率 % |
| `docker_container_memory_usage_bytes` | 同上 | 内存使用（字节） |
| `docker_container_memory_limit_bytes` | 同上 | 内存上限（字节） |
| `docker_container_disk_rw_bytes` | 同上 | 可写层磁盘大小（字节） |
| `docker_container_disk_rootfs_bytes` | 同上 | 整个 rootfs 大小（字节） |

**示例 — 所有容器：**

```yaml
- name: "all_containers"
  type: docker
  mode: all
  interval: 30s
  labels:
    env: "prod"
    tier: "docker"
```

**示例 — 指定容器：**

```yaml
- name: "key_containers"
  type: docker
  mode: filter
  names:
    - "nginx"
    - "redis"
  interval: 15s
  labels:
    env: "prod"
    tier: "docker"
```

---

### 3️⃣ type: custom — 自定义脚本指标

通过执行外部脚本，灵活采集任意自定义指标。

| 字段 | 必填 | 说明 |
|------|------|------|
| `script` | ✅ | 脚本路径（绝对路径或相对路径） |

**脚本输出格式：**
每行一个指标，格式为 `指标名 数值`，支持 `#` 开头注释行。

```
# 这是一行注释，会被忽略
skywell_cpu_percent 42.5
skywell_memory_bytes 512000000
skywell_uptime_seconds 86400
```

**环境变量：**
脚本执行时可读取 `TARGET_NAME` 环境变量获取 target 名称。

**自动注册：**
- 脚本输出的指标名自动注册为 Prometheus Gauge
- 自动携带 `name`、`env`、`tier` 标签
- 每次采集前重置，不残留旧数据

**示例：**

```yaml
- name: "skywell_node"
  type: custom
  script: "/opt/scripts/skywell-stats.sh"
  labels:
    env: "prod"
    tier: "node"
```

生成的指标：
```
skywell_cpu_percent{env="prod",name="skywell_node",tier="node"} 42.5
skywell_memory_bytes{env="prod",name="skywell_node",tier="node"} 5.12e+08
skywell_uptime_seconds{env="prod",name="skywell_node",tier="node"} 86400
```

---

### 4️⃣ type: remote — 远程 Metrics 代理

直接拉取另一个 Prometheus Exporter 的 `/metrics` 文本，**保留所有原始指标和 Label**。

| 字段 | 必填 | 说明 |
|------|------|------|
| `remote.url` | ✅ | 远程 metrics 端点 URL |
| `remote.headers` | ❌ | 自定义请求头（如认证） |

**特点：**
- 原始文本解析，完整保留 `HELP`、`TYPE`、`Label` 等信息
- 指标直接合并到 `/metrics` 输出，与 Prometheus 原生抓取效果一致
- 支持热加载，修改配置后 `SIGHUP` 即可生效

**示例 — 代理 Node Exporter：**

```yaml
- name: "remote_node_exporter"
  type: remote
  remote:
    url: "http://localhost:9100/metrics"
    headers:
      Authorization: "Bearer your-token"  # 可选
```

**示例 — 代理 Blackbox Exporter：**

```yaml
- name: "remote_blackbox"
  type: remote
  remote:
    url: "http://localhost:9115/metrics"
```

---

### 5️⃣ type: mongodb — MongoDB 进度表心跳监控

监控"自己写进度表"的服务（区块扫描器、定时任务等）：只读查询集合中的进度文档，
输出**进程是否存活 + 进度数值**。典型用例：SWTC 公链扫描器把已扫区块高度写进
`<库>.blockno`，跨链排行 cronjob 把进度写进 `skywell_sum.process_status`。

**核心设计（方案 A 兜底 + A1 语义）：**

| 行为 | 说明 |
|------|------|
| 存活指标 | 每个 target 固定输出 `mongodb_probe_up{name="<target名>"}`：查询成功=1 |
| 失败语义 | 连接失败 / 库表不存在 / 无匹配文档 → 只发 `up=0`，**不发任何数值指标**（防旧值误判"数据还新鲜"） |
| 数值指标 | 按 `metrics` 配置从文档字段读取，原样输出为 Gauge（int/float/string/时间戳都支持） |
| 只读 | 仅执行 find/findOne，**绝不写入** MongoDB |

**字段说明：**

| 字段 | 必填 | 说明 |
|------|------|------|
| `uri` | ✅ | MongoDB 连接串（如 `mongodb://host:27017/?directConnection=true`） |
| `database` | ✅ | 目标数据库 |
| `collection` | ✅ | 目标集合 |
| `query` | ❌ | 过滤条件；**为空=取集合最新文档**（`find().sort({_id:-1}).limit(1)`） |
| `metrics[]` | ✅ | 数组，每项 `{name, field}` 定义一个输出指标 |
| `metrics[].name` | ✅ | Prometheus 指标名（如 `swtc_scan_end`） |
| `metrics[].field` | ✅ | 文档中的字段名（如 `end`、`updateAt`） |

**示例 — 区块扫描器进度：**

```yaml
- name: "swtc_balance"
  type: mongodb
  uri: "mongodb://192.168.66.254:27018/?directConnection=true"
  database: "skywell_profit"
  collection: "blockno"
  query: { processName: "scan_balance" }   # 必须配：该集合存在新旧两条记录
  interval: 30s
  metrics:
    - { name: "swtc_scan_last_update", field: "updateAt" }  # 毫秒时间戳
    - { name: "swtc_scan_end", field: "end" }               # 已扫区块高度
  labels:
    env: "prod"
    tier: "blockchain"
```

**示例 — 定时任务（进度在 process_status 表）：**

```yaml
- name: "swtc_cron_cross_chain"
  type: mongodb
  uri: "mongodb://192.168.66.254:27018/?directConnection=true"
  database: "skywell_sum"
  collection: "process_status"
  query: { processName: "scan_cross_chain_rank_v2" }
  metrics:
    - { name: "swtc_cron_last_update", field: "updateAt" }
  labels:
    env: "prod"
    tier: "blockchain"
```

**暴露指标：**

| 指标 | 标签 | 说明 |
|------|------|------|
| `mongodb_probe_up` | `name, env, tier` + global | 查询成功=1，失败=0（失败时无数值指标） |
| `<metrics[].name>` | 同上 | 文档字段值（原始输出） |

**PromQL 使用建议（判定逻辑放查询端）：**

```promql
# 心跳新鲜度: updateAt 是毫秒 → 与 time()*1000 比较
time()*1000 - swtc_scan_last_update > 300000   # 5 分钟未更新 → 告警

# 进程探测失败
mongodb_probe_up{name="swtc_balance"} < 1

# cron 任务停滞（比如 > 90 分钟未跑）
time()*1000 - swtc_cron_last_update > 5400000
```

**注意事项：**

- ⚠️ 若集合存在多条记录（如历史遗留无 `processName` 的旧记录），**必须配 `query` 按 processName 过滤**，否则 `findOne` 会读到过期文档；
- `updateAt` 若为 `Date.now()` 毫秒值，PromQL 需用 `time()*1000` 对齐；若为秒级时间戳则用 `time()`；
- 连接串建议用 `?directConnection=true` 直连避免 discovery

---

## 🚀 使用方法

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml
go build
./general-exporter
```

访问 `http://localhost:8081/metrics` 查看指标。

### 信号处理

| 信号 | 作用 |
|------|------|
| `SIGHUP` | 热加载配置文件，无需重启 |
| `SIGTERM/SIGINT` | 优雅关闭，停止后台采集后退出 |

```bash
# 不中断服务地重新加载配置
kill -HUP $(pgrep -f general-exporter)

# 优雅关闭
kill $(pgrep -f general-exporter)
```
