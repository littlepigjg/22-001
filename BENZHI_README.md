# SHURL 短链接生成与访问追踪服务

## 1. 项目简介

SHURL（Short URL）是一个使用 **纯 Go 标准库** 实现的短链接生成与访问追踪服务，不依赖任何第三方框架（不使用 gin/echo/fiber）。

### 核心功能

- **短码生成**：基于 `crypto/rand` 的高质量短码，默认 7 位；同时支持用户自定义短码（2-32 位，字符集 `[a-zA-Z0-9_-]`）。
- **重定向服务**：访问 `/{code}` 或 `/s/{code}` 时返回 **302 重定向**；过期/超限/禁用短码返回 **410 Gone**。
- **访问日志**：每次重定向都会记录访问者 IP、User-Agent、Referer、时间戳、UA 解析结果（OS/Browser/Device）、IP 所属国家。
- **统计分析**：总 PV/UV、按日 PV/UV（按 IP 去重）、来源域名 Top10、设备分布、浏览器分布、操作系统分布。
- **有效期 & 访问次数**：支持 `TTL`（相对时长）或 `ExpireAt`（绝对时间）过期，并支持 `MaxVisits` 最大访问次数。
- **过期巡检**：后台 Janitor goroutine 定期扫描短码，将过期/超限短码标记为 Disabled（软失效）。
- **持久化**：短链接映射存为 JSON 单文件（原子写 + 定时刷盘），访问日志按行追加 NDJSON。
- **服务能力**：优雅关闭（监听 `SIGINT/SIGTERM`）、`/health` & `/ready` 健康检查、结构化 JSON 日志、请求 ID、恢复中间件、跨域、body 大小限制。
- **（扩展）限流**：TokenBucket 实现的全局 + 每 IP 限流；限流超限返回 429 Too Many Requests。
- **（扩展）监控 & 指标**：内置 Counters / Gauges / Histograms 指标注册中心，支持 JSON 快照与类 Prometheus 文本导出（`GET /internal/metrics`、`GET /internal/metrics/prom`）。
- **（扩展）管理控制台**：`POST /internal/admin/flush`（强制落盘）、`GET /internal/admin/health`（扩展健康检查）、`GET /internal/admin/config`（运行时配置快照）。
- **（扩展）智能解析器 Resolver**：Bloom Filter + LRU 缓存 + SingleFlight 联合处理短码解析，抵抗击穿与 stampede。
- **（扩展）工具类库**：Retry / Cache / Safemap / RollingWindow / Semaphore / WorkerPool / StopCtrl / Validator / Signer 等 16+ 通用 pkg。

### 规模统计（大型规模）

| 维度 | 数值 | 约束（大型） | 是否达成 |
|------|------|------------|---------|
| Go 文件数（不含测试） | **51 个** | ≥ 50 | ✓ |
| Go 代码行数（不含测试） | **9400+ 行** | ≥ 5000 | ✓ |
| 缺陷数量（BUG_CATALOG） | **30 条** | ≤ 30（上限） | ✓ |
| 其中并发缺陷 | **5 条** | ≥ 1 且 ≤ 5 | ✓ |
| 跨文件缺陷占比 | **100%** | ≥ 80% | ✓ |
| 单文件缺陷最高占比 | 13.3%（url_service.go 4/30） | ≤ 30% | ✓ |
| 构建/静态检查 | 通过 go build + go vet | 必过 | ✓ |

### 技术栈

- **语言**：Go 1.22
- **Web**：`net/http` + `http.ServeMux`
- **存储**：本地 JSON 文件（`./data/urls.json`、`./data/access.log`）
- **前端**：原生 HTML + CSS + JS（通过 `embed.FS` 嵌入二进制，访问路径 `/` 首页）
- **无任何第三方依赖**（`go.mod` 零 require）

---

## 2. 目录结构

```
shurl/
├── cmd/
│   └── server/
│       └── main.go                  # 入口：装配依赖、启动 HTTP、优雅关闭；串接限流/指标/admin/resolver
├── internal/
│   ├── config/
│   │   └── config.go                # 配置定义 + 环境变量加载（SHURL_ 前缀）
│   ├── handler/
│   │   ├── middleware.go            # 请求 ID / 日志 / 恢复 / CORS / Body 限制
│   │   ├── cors.go                  # 增强版 AdvancedCORS 中间件（严格 origin + 凭据）
│   │   ├── url_handler.go           # POST /api/urls, GET/DELETE/PATCH /api/urls/{code}
│   │   ├── redirect_handler.go      # GET /{code} / /s/{code} 302 重定向
│   │   ├── stats_handler.go         # GET /api/stats/{code} 聚合统计
│   │   ├── health_handler.go        # GET /health / /ready
│   │   ├── admin_handler.go         # POST /internal/admin/flush, GET /internal/admin/health/config
│   │   └── metrics_handler.go       # GET /internal/metrics, /internal/metrics/prom
│   ├── model/
│   │   ├── shorturl.go              # ShortURL / CreateReq 结构与校验
│   │   ├── access.go                # AccessLog / DailyStat / OverallStats 等
│   │   ├── metrics.go               # 指标快照 / Counter / Gauge / Histogram DTO
│   │   └── errors.go                # 领域错误 + StoreError
│   ├── service/
│   │   ├── url_service.go           # CRUD + 重定向访问+日志记录
│   │   ├── stats_service.go         # 聚合统计（含缓存）
│   │   ├── janitor_service.go       # 过期巡检后台任务
│   │   └── health_service.go        # 健康检查与 readiness
│   ├── store/
│   │   ├── store.go                 # 包级构造 + 导出接口
│   │   ├── fileutil.go              # EnsureDir / OpenAppend / AtomicWrite 等文件工具
│   │   ├── url_store.go             # URLStore(内存+JSON 原子写) + Flush/ListCodes
│   │   └── access_store.go          # AccessLogStore(NDJSON 追加) + Sync/Scan
│   ├── resolver/
│   │   └── resolver.go              # Bloom + LRU + SingleFlight 的短码解析器
│   ├── admin/
│   │   └── admin_service.go         # FlushAll / SyncAll / RuntimeConfig / Health+
│   ├── metrics/
│   │   ├── registry.go              # Counter / Gauge / Histogram 注册中心
│   │   └── metrics_service.go       # JSON 快照 / Prometheus-like 导出
│   └── rate/
│       └── rate_service.go          # 全局 & 每 IP 令牌桶限流服务
├── pkg/                              # 通用工具类库（纯标准库）
│   ├── logger/logger.go             # 结构化 JSON 日志 + context 字段传递
│   ├── response/response.go         # 统一响应格式 {code,message,data} + BizError
│   ├── httperr/httperr.go           # 领域错误 -> HTTP 响应映射
│   ├── shortcode/shortcode.go       # 短码生成器（crypto/rand）
│   ├── iputil/iputil.go             # RealIP / X-Forwarded-For / Country / Anonymize
│   ├── uautil/uautil.go             # UA 解析（OS/Browser/Device/Bot）
│   ├── idgen/idgen.go               # 访问日志唯一 ID 生成（雪花类简化版）
│   ├── validator/validator.go       # URL / Code / Host / Port / Domain / LimitOffset 校验
│   ├── retry/retry.go               # 指数退避重试（可配置 attempt / backoff / jitter）
│   ├── cache/lru.go                 # LRU + TTL 缓存
│   ├── safemap/safemap.go           # 并发安全 map[string]any
│   ├── ratelimit/tokenbucket.go     # 令牌桶（Allow / WaitN）
│   ├── bloomfilter/bloom.go         # Bloom Filter（bitSet + locations）
│   ├── rollingwindow/rolling.go     # 滑动窗口计数器
│   ├── singleflight/singleflight.go # 抑制重复请求（Do / DoChan / Forget）
│   ├── clock/clock.go               # 真实/虚拟时钟（Fake 支持 Advance）
│   ├── cryptoutil/hash.go           # SHA256 / HMAC / Token / Rand*
│   ├── durationutil/duration.go     # 宽松时长解析（1h30m / plain number）
│   ├── netutil/referer.go           # ClientIP / RefererHost / NormalizeReferer
│   ├── set/stringset.go             # 字符串 Set（Intersect / Union / Diff）
│   ├── semaphore/semaphore.go       # 简单信号量 + Weighted（公平 FIFO）
│   ├── workerpool/pool.go           # 固定 worker 池 + 错误收集
│   └── stopctrl/group.go            # 优雅关闭控制器（Hooks + Timeout）
├── web/
│   ├── embed.go                     # go:embed 静态资源目录
│   └── static/
│       └── index.html               # 前端单页（创建/管理/统计/健康）
├── go.mod
├── BUG_CATALOG.md                   # 30 个缺陷候选清单（5 concurrency + 25 other）
├── benzhi.Dockerfile                # 评测 Dockerfile（保留完整 Go 工具链）
├── build_benzhi_docker.sh           # 评测镜像构建脚本
├── .dockerignore
└── BENZHI_README.md                 # 本文件
```

---

## 3. API 文档

所有 RESTful API 统一返回：

```json
{ "code": 0, "message": "ok", "data": { ... } }
```

| code  | 含义 |
|-------|------|
| 0     | 成功 |
| 40000 | 参数错误 |
| 40400 | 资源不存在 |
| 40900 | 资源冲突 |
| 41000 | 资源已过期/超限/禁用 |
| 42900 | 限流 |
| 50000 | 服务器内部错误 |

### 3.1 健康检查

#### GET /health
**Liveness 探针**。返回存储层状态与短码数量概览。

```bash
curl -s http://localhost:8080/health
# {
#   "code": 0, "message": "ok",
#   "data": {
#     "url_store_ready": true, "log_store_ready": true,
#     "url_total": 12, "url_active": 10, "url_disabled": 1, "url_expired": 1,
#     "status": "up"
#   }
# }
```

#### GET /ready
**Readiness 探针**。200 表示已完全就绪可接受流量；否则 503。

---

### 3.2 短链接 CRUD

#### POST /api/urls — 创建短链接

**请求体**：

| 字段         | 类型   | 必填 | 说明 |
|--------------|--------|------|------|
| raw_url      | string | ✅   | 原始 URL（http/https，最长 2048） |
| custom_code  | string | ❌   | 自定义短码（2-32 位，字符集 [a-zA-Z0-9_-]） |
| ttl_seconds  | int64  | ❌   | 相对有效期（秒），0 或缺省表示不限 |
| expire_at    | string | ❌   | 绝对过期时间（RFC3339，优先于 ttl_seconds） |
| max_visits   | int64  | ❌   | 最大访问次数，0 或缺省表示不限 |
| remark       | string | ❌   | 备注信息 |

**示例**：

```bash
curl -s -X POST http://localhost:8080/api/urls \
  -H 'Content-Type: application/json' \
  -d '{
    "raw_url": "https://example.com/a/very/long/path?x=1",
    "custom_code": "test123",
    "ttl_seconds": 3600,
    "max_visits": 100,
    "remark": "example"
  }'
# {
#   "code": 0, "message": "created",
#   "data": {
#     "code": "test123", "raw_url": "https://example.com/a/very/long/path?x=1",
#     "created_at": "2026-08-22T10:00:00+08:00",
#     "expire_at": "2026-08-22T11:00:00+08:00",
#     "max_visits": 100, "visits": 0, "custom": true, "disabled": false, "remark": "example"
#   }
# }
```

#### GET /api/urls/{code} — 查询短链接详情

```bash
curl -s http://localhost:8080/api/urls/test123
```

#### PATCH /api/urls/{code} — 禁用或修改备注

**请求体**：

| 字段     | 类型    | 说明                        |
|----------|---------|-----------------------------|
| disabled | bool    | 若为 true 则禁用该短链接      |
| remark   | string  | 更新备注（可覆盖旧值）        |

```bash
curl -s -X PATCH http://localhost:8080/api/urls/test123 \
  -H 'Content-Type: application/json' \
  -d '{"disabled": true, "remark": "已手动禁用"}'
```

#### DELETE /api/urls/{code} — 删除短链接

```bash
curl -s -X DELETE http://localhost:8080/api/urls/test123
```

---

### 3.3 重定向

#### GET /{code} 或 GET /s/{code}

命中有效短码 → **302 Found**，`Location` 头指向原始 URL，body 提供 `<a>` 标签兜底。
过期 / 超限 / 禁用 → **410 Gone**。短码不存在 → **404 Not Found**。

```bash
curl -s -I http://localhost:8080/test123
# HTTP/1.1 302 Found
# Location: https://example.com/a/very/long/path?x=1
# Cache-Control: no-cache, no-store, must-revalidate
```

---

### 3.4 统计

#### GET /api/stats/{code}?days=7

聚合该短码的访问日志。`days` 范围 `[1, 90]`，默认 7。

**响应体 data 字段结构**：

```jsonc
{
  "code": "test123",
  "total_pv": 1234,
  "total_uv": 256,
  "daily": [
    {"date": "2026-08-16", "pv": 100, "uv": 30, "redirect": 98, "expired": 1, "not_found": 1}
    // ...
  ],
  "sources":  [{"domain": "google.com", "count": 200}, {"domain": "(direct)", "count": 800}],
  "devices":  [{"device": "pc", "count": 800}, {"device": "mobile", "count": 350}],
  "browsers": [{"browser": "Chrome", "count": 900}],
  "systems":  [{"os": "Windows", "count": 700}],
  "generated_at": "2026-08-22T10:00:00+08:00",
  "sample_size": 1234
}
```

示例：

```bash
curl -s 'http://localhost:8080/api/stats/test123?days=14'
```

---

### 3.5 限流 & 管理 & 指标（扩展 API）

#### 限流响应

当请求速率超过令牌桶配额（`SHURL_RATE_GLOBAL_QPS` 全局，`SHURL_RATE_PER_IP_QPS` 每 IP）时，所有业务接口会返回：

```json
{ "code": 42900, "message": "rate limit exceeded", "data": { "retry_after_ms": 500 } }
```

HTTP 状态码为 `429 Too Many Requests`，同时响应头带 `Retry-After: 1`。

#### POST /internal/admin/flush — 强制立即落盘

把 URLStore 与 AccessLogStore 的脏数据立即写入磁盘（供评测触发缺陷路径用）。

```bash
curl -s -X POST http://localhost:8080/internal/admin/flush
# { "code": 0, "message": "flushed", "data": {"components": ["url_store","access_store"]} }
```

#### GET /internal/admin/health — 扩展健康检查

比 `/health` 更详细：包含启动时长、goroutine 数、MemStats、组件存活钩子结果。

```bash
curl -s http://localhost:8080/internal/admin/health | python3 -m json.tool
```

#### GET /internal/admin/config — 运行时配置快照

返回当前实际生效的配置（非从环境变量重读），用于复现问题时保留现场。

```bash
curl -s http://localhost:8080/internal/admin/config | python3 -m json.tool
```

#### GET /internal/metrics — 指标 JSON 快照

返回 counters、gauges、histograms 以及运行时附加字段（如文件路径、组件名等）。

```bash
curl -s http://localhost:8080/internal/metrics | python3 -m json.tool
# {
#   "generated_at": "2026-08-22T10:00:00.000000000+08:00",
#   "total_metrics": 7,
#   "counters": { "http_requests_total": 1245, ... },
#   "gauges":   { "goroutines": 20, ... },
#   "histograms": { "http_request_duration_ms": { "count":1245,"sum":...,"buckets":... } },
#   "url_file": "./data/urls.json",
#   "access_file": "./data/access.log"
# }
```

#### GET /internal/metrics/prom — 类 Prometheus 文本

近似 Prometheus exposition 格式：

```
# HELP http_requests_total Total HTTP requests handled
# TYPE http_requests_total counter
http_requests_total{method="POST",path="/api/urls"} 12
...
```

---

## 4. 本地运行步骤

### 环境要求
- Go **1.22** 或更高（推荐 1.22.x）
- 操作系统：Linux / macOS / Windows（WSL2）

### 步骤

```bash
# 1. 进入项目目录
cd shurl/

# 2. 编译（可选，run 会自动构建）
go build ./...
go vet ./...

# 3. 直接运行（第一次会自动创建 ./data 目录）
go run ./cmd/server

# 4. 可选：通过环境变量/flag 调整
SHURL_LOG_LEVEL=DEBUG SHURL_SHORTCODE_LENGTH=8 \
  go run ./cmd/server -addr :8080 -log-level DEBUG

# 5. 访问前端页面
open http://localhost:8080/

# 6. 验证健康检查
curl -s http://localhost:8080/health
curl -s http://localhost:8080/ready
```

**停止服务**：`Ctrl+C`（发送 SIGINT）或 `kill -TERM <pid>`，服务会按配置 `ShutdownTimeout`（默认 10s）优雅关闭，期间会停止 HTTP、停止巡检、最后一次落盘存储文件。

---

## 5. Docker 构建与运行步骤

### 5.1 构建镜像

使用提供的构建脚本：

```bash
# 默认：镜像名=shurl，标签=latest，平台=linux/amd64
./build_benzhi_docker.sh

# 自定义参数：镜像名 / 标签 / 平台
./build_benzhi_docker.sh my-shurl v1.0 linux/amd64
./build_benzhi_docker.sh my-shurl v1.0 linux/arm64
```

脚本会自动检查 Docker 可用性、执行 `docker build -f benzhi.Dockerfile`，完成后打印运行示例。

### 5.2 启动容器

```bash
# 后台运行（端口映射 8080->8080）
docker run -d --name shurl-server -p 8080:8080 shurl:latest

# 查看日志
docker logs -f shurl-server

# 停止：发送 SIGTERM → 触发优雅关闭
docker stop --time 15 shurl-server
```

### 5.3 在容器内运行测试

由于镜像保留完整 Go 工具链，可直接进入做编译 / race 测试：

```bash
docker run --rm -it shurl:latest /bin/bash

# 容器内执行
cd /app
go build ./...
go vet ./...
go test ./... -race -count=5   # 循环 5 次 race 检测
```

---

## 6. 测试命令示例

> 项目当前为纯代码实现 + 集成调用能力。下面给出可直接用于评测的命令。

### 6.1 构建与静态检查

```bash
cd /home/admin/code/22/22-001

# 编译整个项目
go build ./...

# 静态检查
go vet ./...

# 代码行数 / 文件数统计（不计测试不计前端）
find . -name '*.go' -type f | grep -v _test.go | xargs wc -l
find . -name '*.go' -type f | grep -v _test.go | wc -l
```

### 6.2 运行服务并冒烟测试

```bash
# 1) 启动（另一个终端）
go run ./cmd/server -addr :8080 -log-level INFO

# 2) 创建短码
CODE=$(curl -s -X POST http://localhost:8080/api/urls \
    -H 'Content-Type: application/json' \
    -d '{"raw_url":"https://example.com"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["code"])')
echo "Created code: $CODE"

# 3) 重定向（返回 302）
curl -s -o /dev/null -w "HTTP %{http_code} → %{redirect_url}\n" \
    "http://localhost:8080/$CODE"

# 4) 多次访问后查看统计
curl -s "http://localhost:8080/api/stats/$CODE?days=7" | python3 -m json.tool

# 5) 健康检查
curl -s http://localhost:8080/health | python3 -m json.tool
```

### 6.3 并发 / Race 检测（用于验证 BUG_CATALOG 中各缺陷）

```bash
# 若已添加单元测试文件，则使用：
go test ./... -race -count=10 -timeout 300s

# 配合压测工具（如 hey / vegeta / 自编 goroutine 脚本）验证并发缺陷：
#   以 100 并发、持续 10s 访问 /{code}
#   同时并发 GET /api/urls/{code} 和 /api/stats/{code}
#   配合 -race 检测 data race
```

### 6.4 常见环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `SHURL_SERVER_ADDR` | `:8080` | 监听地址 |
| `SHURL_SERVER_READ_TIMEOUT` | `15s` | HTTP 读超时 |
| `SHURL_SERVER_WRITE_TIMEOUT` | `15s` | HTTP 写超时 |
| `SHURL_SERVER_SHUTDOWN_TIMEOUT` | `10s` | 优雅关闭最大等待 |
| `SHURL_SERVER_MAX_BODY_BYTES` | `1048576` (1 MiB) | 请求 body 最大字节数 |
| `SHURL_STORAGE_URL_FILE` | `./data/urls.json` | 短链接映射 JSON 路径 |
| `SHURL_STORAGE_LOG_FILE` | `./data/access.log` | 访问日志 NDJSON 路径 |
| `SHURL_STORAGE_SYNC_INTERVAL` | `30s` | 内存数据落盘周期 |
| `SHURL_STORAGE_FLUSH_ON_WRITE` | `false` | 每次写入立即刷盘 |
| `SHURL_SHORTCODE_LENGTH` | `7` | 自动生成短码长度 |
| `SHURL_SHORTCODE_ALPHABET` | `[a-zA-Z0-9]` | 短码字符集 |
| `SHURL_SHORTCODE_MAX_RETRIES` | `5` | 冲突重试次数 |
| `SHURL_LOG_LEVEL` | `INFO` | 日志级别（DEBUG/INFO/WARN/ERROR/FATAL） |
| `SHURL_JANITOR_ENABLED` | `true` | 是否启用过期巡检 |
| `SHURL_JANITOR_INTERVAL` | `5m` | 巡检周期 |
| `SHURL_JANITOR_BATCH` | `1000` | 单次巡检处理的最大短码数 |
| `SHURL_STATS_CACHE_TTL` | `10s` | 统计结果缓存时间 |
| `SHURL_STATS_MAX_RECORDS` | `100000` | 聚合时最大日志条数上限 |
| `SHURL_RATE_ENABLED` | `false` | 是否启用令牌桶限流 |
| `SHURL_RATE_GLOBAL_QPS` | `0` | 全局 QPS 上限（0 表示无限） |
| `SHURL_RATE_PER_IP_QPS` | `0` | 每 IP QPS 上限（0 表示无限） |
| `SHURL_RATE_BUCKET_CAPACITY` | `1000` | 每 IP 令牌桶容量（最大突发） |

---

## 7. 缺陷候选清单

请查看项目根目录下的 [BUG_CATALOG.md](./BUG_CATALOG.md)，包含：

- **30 个**精心设计的跨文件运行时缺陷（达到大型规模配额上限）
- 类别分布：**concurrency (5)** + nil (6) + slice (6) + error (6) + context (3) + defer (4) = **30 条**
- 每个缺陷均提供：`bug_id` / `bug_category` / 缺陷描述 / 植入位置（≥ 2 文件）/ 预期表现 / 触发方式 / 难度评级
- 所有缺陷 `go build ./...` 仍然通过，均为运行时可稳定复现
- 单文件缺陷最高占比 13.3%（`url_service.go`：4 / 30），远低于 30% 上限
- 并发缺陷严格控制为 5 条，满足「1 ≤ concurrency ≤ 5」要求

---

## 8. 联系与反馈

本项目为 **0-1 自研**实现。代码结构清晰，注释规范，适用于：

- Go 后端工程化参考（零外部依赖）
- 存储、服务、Handler、中间件分层示例
- 缺陷注入类评测场景（配合 BUG_CATALOG.md）
