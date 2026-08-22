# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在高并发场景下进行统计聚合、短链元信息查询与更新、全局缓存读写时，会发生 Go runtime 的 `concurrent map read and map write` / `concurrent map writes` panic，同时 `-race` 探测器能稳定捕获多条 DATA RACE 告警。场景特征是多个 goroutine 同时调用统计接口（Overall）、URL 查询/更新备注接口、共享缓存 Set/Get 与合并操作，以及 LRU 缓存过期清理。表现为压测下进程崩溃或偶发 nil pointer deref，请求取消（context 超时/abort）时聚合任务也无法被正确打断。

## 2. 环境信息（Environment）
- 操作系统：Linux（如 Ubuntu 20.04 / 发行版任意）
- Go 版本：go1.22（`go version` 输出为 go1.22.x linux/amd64）
- 项目模块：`shurl`（go.mod 中 module shurl，无第三方依赖）
- 运行参数：`go test -race -count=N -run '^TestRedGreen$' . -timeout 30s`，并发 goroutine 数量 10~12，每 goroutine 轮次 30~40
- 硬件信息（相关）：多 CPU 核心，-race 模式下内存访问竞争被放大

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（`go.mod` 所在目录），执行 `go build ./...` 与 `go vet ./...` 确保编译与静态检查均通过。
2. 执行单次带竞态检测的测试：
   ```
   go test -race -count=1 -run '^TestRedGreen$' . -timeout 30s
   ```
3. 若单次未能 100% 复现 panic，增大重复次数以提高竞态触发概率：
   ```
   go test -race -count=10 -run '^TestRedGreen$' . -timeout 180s
   ```
4. 观察 stdout/stderr 的三类关键输出：
   - 是否显式打印 `RED（红灯，缺陷未修复）`；
   - `-race` 是否报出 `WARNING: DATA RACE`（read/write/write 对同一 map 地址）；
   - 是否发生 `fatal error: concurrent map read and map write` 或 `concurrent map writes`，以及偶尔的 nil pointer deref（PurgeExpired 链路）。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体 panic / error 信息：
  - `fatal error: concurrent map writes`（goroutine 在 StatsService.aggregate 的 Scan 回调中写 uniqueIPs / sources / buckets，与 ComputeMerge 的写路径重合）
  - `fatal error: concurrent map read and map write`（safemap.ComputeMerge / SetRef / SetWithTTL 对 GlobalResults.m 以及 MustGet / Snapshot 的无锁遍历）
  - 偶发 `panic: runtime error: invalid memory address or nil pointer dereference`（LRU.PurgeExpired 在 `next = nextPrev.Prev()` 处当链表只剩一个元素且 nextPrev==nil 时触发）
- RED/GREEN 判定：缺陷未修复时显式输出 `RED（红灯，缺陷未修复）`。
- 其他异常：-race 中可见 safemap 的 `Map.Get`、`GetWithTTL`、`SharedSet`、`ComputeMerge` 以及 StatsService aggregate 回调对 `partialAgg.UniqueIPs / Sources / Buckets` 的并发读写；URLService.Get/Disable/UpdateRemark 通过 safemap 共享 ShortURL 指针后在锁外修改 Visits/Remark/MaxVisits，与 store.IncrementVisits 返回的 clone 写形成数据竞争。
- go test -race 报告 DATA RACE：稳定可复现（≥ 1 次执行即可见到），多组 Write/Read 对，分别位于 safemap 全局 Map、StatsService.aggregate 的局部 map、LRU PurgeExpired。

## 5. 期望结果（Expected Behavior）
- 无 panic、无数据竞争：执行 `go test -race -count=20 -run '^TestRedGreen$' . -timeout 180s`，所有 20 次全部通过，stdout/stderr 中不存在任何 `WARNING: DATA RACE`、`fatal error: concurrent map`、`nil pointer dereference` 等告警或崩溃；最终退出码 0。
- RED/GREEN 判定结果应为 GREEN：测试最后一行显式打印 `GREEN（绿灯，缺陷已修复）`。
- 具体业务正确性：
  - 并发 Overall(ctx, code, days) 返回的 TotalPV、TotalUV、Daily 聚合稳定无错，context 被 cancel/timeout 时 aggregate 能及时停止扫描并返回 ErrCanceled（不出现 goroutine 悬挂继续扫日志）。
  - URLService.Get/UpdateRemark/Disable 并发下不会出现 "同一 ShortURL 被多个 goroutine 同时无锁写 Visits/Remark/MaxVisits" 的状态污染；store.Save 与 IncrementVisits 的写结果与内存反射一致。
  - LRU.PurgeExpired 在空缓存、单元素缓存、大量 TTL 过期场景下能稳定执行，不会 nil deref。
  - 全局 safemap、SharedStats LRU 的 Set/Get/Merge/Purge 语义一致，不会在 TTL 删除与并发 Set 之间丢数据或崩溃。
- `go build ./...` 与 `go vet ./...` 全部无错误无告警。

## 6. 触发频率（Frequency）
- -race -count=1 即可稳定见到 DATA RACE；panic 在 1 次运行内出现概率约 70%（视 goroutine 调度），-race -count=5 基本 100% 出现一次以上 `concurrent map writes` panic 或 nil deref。
- 非 race 模式（`go test -count=20`）下偶发，但随并发度提升会增加。
- PurgeExpired nil deref 仅当链表尾部元素被移除后 nextPrev 为 nil 时触发，在 race 压力下会放大其可见性。

## 7. 影响范围（Impact / Scope）
- 线上服务在统计查询 + 短链更新并发下可能直接 panic，导致可用性下降、请求失败。
- 共享 safemap / SharedStats LRU 中的数据可能出现竞态损坏：统计 TotalPV/UV 漏计或重计、短链 Remark/MaxVisits 字段被并发写覆盖，导致业务语义错误。
- context 取消无法及时停止统计聚合，导致高并发取消场景下 goroutine 泄漏、CPU/IO 空转，进一步放大雪崩。
- LRU PurgeExpired 的 nil deref 会使后台清理崩溃，缓存不会被主动清理，最终可能内存增长。
- 总体表现为：压测不稳定、进程偶发退出、统计结果不一致、短链访问计数漂移、资源泄漏。

## 8. 附加说明（Additional Notes / Workaround）
临时规避建议（未修缺陷期间缓解）：
- 暂时禁用并发统计接口调用（同一时刻只允许单 worker 跑 Overall）。
- 关闭全局共享缓存路径的写入（绕过 safemap 的 ComputeMerge/SetRef），所有缓存回退到各 service 的局部加锁实现。
- 避免短链同时被 Get + UpdateRemark + Disable 并发访问，或在业务入口加全局互斥。
以上只是临时缓解，本质问题在于跨文件的共享别名与锁粒度不匹配，需要完整修复。
