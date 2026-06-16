# general-exporter

轻量级 Prometheus Exporter，用于监控 HTTP 服务、Docker 容器和自定义脚本指标。

## 特性

- **零依赖部署**：单个二进制文件，无需额外运行时
- **URL 探测**：HTTP/HTTPS 可用性、状态码、响应内容、延迟、响应大小
- **Docker 监控**：容器状态、CPU 使用率、内存使用/上限
- **自定义脚本**：执行任意脚本，输出自定义指标（一行一个，`指标名 数值` 格式）
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

默认端口 8081，可在 `config.yaml` 中修改。

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
| `docker_container_up` | `container, image, env, tier` | 运行中=1，停止=0 |
| `docker_container_cpu_percent` | `container, image, env, tier` | CPU 使用率 % |
| `docker_container_memory_usage_bytes` | `container, image, env, tier` | 内存使用（字节） |
| `docker_container_memory_limit_bytes` | `container, image, env, tier` | 内存上限（字节） |

### 自定义脚本指标

脚本输出的每一行 `指标名 数值` 自动转换为 Prometheus Gauge，
自动携带 `name, env, tier` 标签。

示例输出：
```
skywell_cpu_percent{env="prod",name="skywell_node",tier="node"} 42.5
skywell_memory_bytes{env="prod",name="skywell_node",tier="node"} 5.12e+08
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
