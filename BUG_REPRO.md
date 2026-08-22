# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
批量统计功能在调用批量短码聚合接口时，若传入的「采样步数」参数大于实际记录的样本数量，会立即触发 `runtime error: slice bounds out of range [-N:]` panic，导致当前 goroutine 崩溃并连带测试/服务进程退出。在不经过批量统计接口的其他调用链（直接调用批量释放采样封装、或者直接调用公共切片辅助函数取「尾部 N 条」）也能稳定复现同类越界 panic。普通单短码统计、基础令牌桶限流、基础加权信号量等场景不受影响，仅在「采样步数 > 已记录样本量」时出问题。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核版本依据部署环境而定，复现过程不依赖特定发行版）
- Go 版本：go1.22 系列（go.mod 声明 go 1.22）；`go version` 输出示例：`go version go1.22 linux/amd64`
- 项目模块/依赖：模块名 `shurl`，零第三方依赖（全部使用 Go 标准库）。关键子包：`pkg/ratelimit`（令牌桶+采样辅助）、`pkg/semaphore`（加权信号量+批量调度封装）、`internal/service`（StatsService 统计聚合）、`internal/store`（URLStore/AccessLogStore）。
- 运行参数：`go test . -count=3 -run '^TestRedGreen$' -v`；单次运行也可复现。不涉及 `-race`（非并发类缺陷）。
- 硬件信息（如与并发/性能相关可补充）：任意 CPU 架构均可稳定复现（本次复现基于 amd64，多核单核都可）。

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（`/home/admin/code/22/001/22-001-15`），执行 `go build ./...` 并确认编译通过。
2. （可选）执行 `go vet ./...` 并确认无静态检查告警，确保代码层面能正常编译与分析。
3. 执行验证命令：`go test . -count=3 -run '^TestRedGreen$' -v`。
4. 观察输出中三个子用例的行为：
   - `direct_TakeLastN_oversize`：构造长度为 3 的切片，请求取尾部 50000 条。
   - `WeightedBatch_ReleaseBurst_oversize`：直接构造加权信号量批量封装，释放 burst=100000。
   - `StatsService_BatchOverallAggregate_oversize`：在临时目录里初始化 URLStore 与 AccessLogStore、写入 5 条短码、调用批量聚合并传 burst=100000。
5. 观察最后 RED/GREEN 判定输出以及 go test 退出码。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体 panic 信息（典型片段，三条链路均复现）：
  - `runtime error: slice bounds out of range [-49997:]`（TakeLastN 直接调用）
  - `runtime error: slice bounds out of range [-99968:]`（WeightedBatch.ReleaseBurst 调用链）
  - `runtime error: slice bounds out of range [-99936:]`（StatsService.BatchOverallAggregate 调用链）
- RED/GREEN 判定结果：输出中显式打印 `RED（红灯，缺陷未修复）`。
- 其他异常现象：
  - 所有三个子用例的 defer recover 均捕获到 panic；根 `TestRedGreen` 用 `t.Errorf` 报错。
  - 若作为服务在线上调用该批量统计接口，命中参数后请求处理 goroutine 会因未 recover 的 panic 导致进程级崩溃（取决于上层 recover）。
- go test -race 是否报告 DATA RACE：本缺陷为纯切片越界（非并发类），`-race` 不报告数据竞争，但 panic 仍然存在。
- 退出码：`go test` 返回非 0（FAIL，EXIT_CODE=1），三次 `-count=3` 结果均一致为 RED。

## 5. 期望结果（Expected Behavior）
- 无 panic：当请求的步数（n / burst）大于实际已记录样本数时，下游辅助函数应返回全部已有样本或空切片，不得再出现 `slice bounds out of range` 的负值下界 panic。
- RED/GREEN 判定应为 `GREEN（绿灯，缺陷已修复）`，go test 退出码 0 且 `-count=3` 全部通过。
- 业务层面正确行为：
  - `ratelimit.TakeLastN(samples, n)`：n<=len(samples) 返回最后 n 条；n>len(samples) 返回全部 samples 或等价安全结果。
  - `semaphore.WeightedBatch.ReleaseBurst`：burst 过大时返回实际已采样的尾部曲线，长度不得超过 recorder 当前长度。
  - `StatsService.BatchOverallAggregate`：批量统计成功完成，返回各短码的聚合结果集以及调度曲线（长度 <= 实际 recorder 容量）；即便 burst=100000 也不会 panic。
- 回归保证：普通 `Overall` 单短码统计、`TokenBucket.Allow/AllowN`、`Weighted.Acquire/Release` 等原公开 API 的功能不受影响；`go build ./...` 与 `go vet ./...` 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。只要满足「请求步数 n / burst > 当前已写入样本数」的条件即可稳定复现；即使 recorder 为空（刚 Reset 过或 burst 从零起步）也会产生 `-N` 下界而必 panic。单测 `-count=3` 重复 3 次每次均触发。

## 7. 影响范围（Impact / Scope）
- 服务调用批量统计聚合接口时，如果上游把 burst 估算过大（例如想直接一次取「最近容量上限条」），会直接 panic 导致接口异常，严重时进程崩溃。
- 任何间接使用 WeightedBatch.ReleaseBurst、BucketHelper.DrainAndSample、TakeLastN 的后台任务、报表生成、压测观测脚本都可能被触发，进而影响可用性。
- 不会引发数据一致性问题（panic 发生在输出/采样阶段，未落到存储），但会造成服务中断、管理端不可用、短码统计面板 500；日志中会积累大量 goroutine 堆栈，排障成本高。
- 与并发不直接相关，所以 `-race` 不会报竞争，但属于确定性的崩溃 bug。

## 8. 附加说明（Additional Notes / Workaround）
- 临时规避：在调用方把 burst 裁剪到「保守的小值」（例如 <= 当前 recorder 容量的最小估计，或 <= 当前样本数若可查询），避免传入明显大于实际可能样本数的 burst；或在入口层添加 recover 兜底，避免请求级 panic 升级为进程崩溃（仅限临时，不建议长期使用）。
- 相关日志样例（典型）：
  ```
  panic: runtime error: slice bounds out of range [-99936:]
  goroutine ... [...]:
      shurl/pkg/ratelimit.TakeLastN(...)
          pkg/ratelimit/tokenbucket.go:... +0xNN
      shurl/pkg/ratelimit.(*BucketHelper).DrainAndSample(...)
          pkg/ratelimit/tokenbucket.go:... +0xNN
      shurl/pkg/semaphore.(*WeightedBatch).ReleaseBurst(...)
          pkg/semaphore/semaphore.go:... +0xNN
      shurl/internal/service.(*StatsService).BatchOverallAggregate(...)
          internal/service/stats_service.go:... +0xNN
  ```
- 复现无需额外依赖或外部网络，纯本地即可跑通；临时目录由测试自行创建清理。
