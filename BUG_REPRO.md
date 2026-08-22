# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务的 metrics JSON 导出接口（/api/metrics、/metrics）在指标正常、序列化结果合法的情况下，仍然返回 HTTP 500 错误并提示 `encode metrics failed`。与此同时，管理面板的健康接口（/api/admin/health）状态被误标为 degraded，note 里出现「snapshot probe failed: snapshot_success: bytes=...」这类「看起来 bytes 有数据却又报成功当失败」的异常信息；强制 flush 接口 /api/admin/flush 也会因为相同原因返回 500。另外短码统计接口 /api/stats/{code} 在正常聚合完成后，有时也会莫名其妙抛回包含 `stats: snapshot probe` 的错误，前端业务无法取到统计结果。问题出在错误返回与成功字节并存的判定上：调用方只要看到非 nil error 就走失败分支，导致监控告警、健康检查误报、统计查询失败三者一起触发。

## 2. 环境信息（Environment）
- 操作系统：Linux（发行版任意，例如 Ubuntu 22.04 / Debian 12）
- Go 版本：Go 1.22 及以上（go.mod 声明 `go 1.22`）
- 项目模块：module `shurl`（零第三方依赖）
- 关键组件：internal/metrics（Service + Registry）、internal/admin（Service）、internal/service（StatsService）、internal/handler（MetricsHandler）
- 运行参数：默认即可；复现测试用 go test，不需要压测或并发工具
- 硬件：与 CPU/内存无关，单核机器也能稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录：`cd /home/admin/code/22/001/22-001-23`
2. 先确认编译与静态检查通过：
   - 执行 `go build ./...` ，无报错、产出二进制
   - 执行 `go vet ./...` ，无警告
3. 直接运行缺陷验证用例：
   - 执行 `go test . -count=1 -run '^TestRedGreen$' -v`
4. （可选）手动触发 HTTP 级现象的复现：
   - 在测试代码或临时 main 中启动一个 httptest.Server，把 MetricsHandler 注册到 /api/metrics 与 /metrics
   - 对 `GET /api/metrics` 发请求，观察响应状态码与 body
   - 对 `GET /metrics` 发请求，同样观察
   - 对绑定了 metrics provider 的 admin.Service 调 `Health()` 与 `FlushAll()` 观察返回
5. 观察测试输出中的 RED/GREEN 判定行、admin health 的 status/note、stats 接口的 error 字符串。

## 4. 实际结果（Actual Behavior / Observed Output）
- `go test . -count=1 -run '^TestRedGreen$' -v` 输出：
  - 判定行打印：`RED（红灯，缺陷未修复）`
  - go test 退出码：FAIL，非 0
  - 具体断言失败（节选，顺序可能因内部步骤不同略有差别）：
    1. `JSONSnapshot err was not nil after success serialization, got: snapshot_success: bytes=... metrics=N`
    2. `GET /api/metrics expected HTTP 200 but got 500; body length=34 body="{\"error\":\"encode metrics failed\"}\n"`
    3. `GET /metrics expected 200 but got status=500 body_len=34 body="{\"error\":\"encode metrics failed\"}\n"`
    4. `admin.Health() expected status=ok after clean JSONSnapshot, got status="degraded" note="snapshot probe failed: snapshot[snapshot_probe]: snapshot_success: bytes=... metrics=..."`
    5. `admin.FlushAll() should return nil when snapshot only has valid bytes, got err: snapshot[snapshot_probe]: snapshot_success: bytes=... metrics=..."`
    6. `RuntimeConfig snapshot_probe_live should have error=nil for valid snapshot, got: snapshot_success: bytes=... metrics=..."`
    7. `stats-chain: admin.Health status should be ok, got "degraded" note=...`
    8. `stats-chain: iteration K JSONSnapshot err != nil: snapshot_success: bytes=... metrics=... (bs len=...)`（K=0/1/2 至少 3 次）
- 现象归纳：
  - JSON 序列化确实产出了非空、可解析的 bytes，但返回的 err 也非 nil；
  - HTTP metrics 接口按 err != nil 走 500，body 被替换成了 `{"error":"encode metrics failed"}`；
  - admin 健康、flush、runtime 均被同一个 marker 错误污染，出现 degraded / join error / non-nil probe.error；
  - stats 的统计调用即便聚合数据正确，也会在最终返回前因为一次 snapshot 探测把错误上抛；
  - 缺陷类型为 error，非并发类，故不涉及 go test -race DATA RACE 报告。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应满足：
- `go build ./...` 全部通过，无编译错误；`go vet ./...` 无警告。
- 调用 JSONSnapshot：序列化成功时 `(bytes != nil, err == nil)`；仅当 JSON 编码真实失败时才 err != nil。
- bytes 能被 json.Unmarshal 成功，且包含 `counters`、`gauges`、`histograms`、`total_metrics` 等字段。
- HTTP：`GET /api/metrics`、`GET /metrics` 在 JSON 模式下均返回 HTTP 200，Content-Type 为 `application/json; charset=utf-8`，body 为上述可解析 JSON，不出现 `{"error":"encode metrics failed"}`。
- admin：
  - `Health().Status == "ok"`（在无真实 flush/sync 失败的情况下），`Note` 不含 `snapshot probe failed`；
  - `FlushAll()` 在没有真实存储 I/O 错误的场景返回 nil，错误列表不包含 `snapshot_success` marker；
  - `RuntimeConfig()` 中 `snapshot_probe_live.error` 字段为 nil，或整体不存在该字段。
- stats：`Overall` 的返回错误仅反映「短码非法、短码不存在、ctx 取消、存储错误」等真实失败，不会出现 `stats: snapshot probe` + `snapshot_success` 类错误；当聚合成功时应返回非 nil 的统计结果与 nil error。
- `go test . -count=3 -run '^TestRedGreen$' -v` 3 次重复执行全部通过（PASS，退出码 0），每次输出都显式打印 `GREEN（绿灯，缺陷已修复）`，无任何 RED 判定行。

## 6. 触发频率（Frequency）
必现（100%）。只要 metrics.Service 已经注册了至少一个可计数的指标或 SetExtra 设置了任意可序列化字段，JSONSnapshot 总会在 bytes 非空时把错误改成 marker 伪错误；后续所有依赖该 provider 的 admin/handler/stats 调用路径都会稳定走到错误分支。即便没有注册任何指标的空快照场景，只要 bytes 非空（默认 generated_at 等字段一定能序列化出 bytes）同样 100% 触发。-count=3 重复执行也能稳定命中。

## 7. 影响范围（Impact / Scope）
- 监控可用性：对外 metrics 接口持续 500，监控面板、告警系统无法拉取 JSON 指标，出现伪告警或数据空白；
- 健康检查：/api/admin/health 被误报为 degraded，导致上游健康探测（K8s liveness/readiness、LB 健康探针、consul/nacos 注册心跳等）周期性失败，引发误摘除或重启；
- 运维命令：/api/admin/flush 在无任何磁盘/存储问题时也会 500，使得运维同学难以判断真实 flush 是否失败，故障排查被干扰；
- 统计业务：/api/stats/{code} 返回错误，前端统计页 / 运营报表查询失败，用户体验与业务运营功能受损；
- 错误传播链污染：多个「下游组件」把同一个 marker 伪错误当作真实失败，日志、告警、追踪系统都会出现 `snapshot_success` 作为失败关键字，造成定位噪音；
- 不涉及 panic / goroutine 泄漏，但接口错误率、可用性指标（SLA）会明显下降。

## 8. 附加说明（Additional Notes / Workaround）
- 临时 workaround：在排查期可以单独跳过 err 判断，只校验 JSON bytes 是否合法（例如 `json.Valid(bs)` 为 true 就视为成功），然后在调用 HTTP metrics 的客户端「如果状态码 500 但 body 其实是合法 metrics JSON」就兼容解析；但这只是临时规避，治标不治本，admin health/flush 与 stats 错误仍然存在。
- 相关日志样例（修复前典型片段）：
  - `http request method=GET path=/api/metrics status=500 elapsed=...`
  - `admin: flushed component ...` 正常后紧接着 join error 里含 `snapshot[snapshot_probe]: snapshot_success: ...`
  - stats 聚合日志打出 `stats: snapshot probe: snapshot_success: bytes=...` 形式的报错
- 注意：不要把 `snapshot_success` 类的 marker 错误当成真实编码失败来排查 JSON 序列化或指标内容，否则会陷入「明明 JSON 还能解析但接口却 500」的方向错误。
