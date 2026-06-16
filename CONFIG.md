# 配置文件说明 (config.yaml)

## 📐 整体结构

```yaml
server:        # 服务配置
defaults:      # 全局默认值
targets:       # 采集目标列表
  - name: "xxx"
    type: xxx  # url | docker | custom
    ...
```

---

## 🔧 server — 服务配置

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `port` | `8080` | 服务端口 |
| `metrics_path` | `/metrics` | 指标端点路径 |

---

## 🌍 defaults — 全局默认值

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `interval` | `30s` | 采集间隔（预留，当前未生效） |
| `timeout` | `10s` | 请求超时 |

单个 target 可覆盖这些值。

---

## 🎯 targets — 采集目标

公共字段：

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | ✅ | 指标名称标识 |
| `type` | ✅ | `url`、`docker` 或 `custom` |
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
| `docker_container_up` | `container, image, env, tier` | 运行中=1，停止=0 |
| `docker_container_cpu_percent` | `container, image, env, tier` | CPU 使用率 % |
| `docker_container_memory_usage_bytes` | `container, image, env, tier` | 内存使用（字节） |
| `docker_container_memory_limit_bytes` | `container, image, env, tier` | 内存上限（字节） |

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

## 🚀 使用方法

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml
go build
./general-exporter
```

访问 `http://localhost:8081/metrics` 查看指标。
