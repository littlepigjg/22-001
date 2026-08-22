# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在落盘链路上出现数据丢失问题：后台周期性 fsync 任务运行很短一段时间后就不再继续，导致后续写入的访问日志和短链接映射无法被定时落盘；开启 FlushOnWrite 配置期望每次写入立即 Sync，实际行为却与配置相反（开启时不 Sync、关闭时才 Sync）；优雅 Close 的收尾阶段，后台同步任务还没完成最后一次落盘、文件句柄就已经先被关闭。三者叠加后，只要进程发生强制退出（机器宕机、kill -9、容器被 kill），重启后就会发现 access.log 和 urls.json 缺失最近一段时间的大量写入，只能恢复到 Close 时的快照。

## 2. 环境信息（Environment）
- 操作系统：Linux（发行版任意，内核 ≥ 4.x）
- Go 版本：Go 1.22 及以上（go.mod 声明 go 1.22）
- 项目模块/依赖：module `shurl`，纯标准库实现，无第三方依赖
- 运行参数：测试复现时 SyncInterval 建议 50ms ~ 150ms、FlushOnWrite 分别取 true/false 两组对照；verify 命令使用 `go test . -count=3 -run '^TestRedGreen$' -timeout 60s`，不需额外并发参数（不是 race 类问题，不加 -race）
- 硬件信息（相关可补充）：CPU 核数 ≥2，磁盘任意（HDD/SSD 均可，不影响复现）

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 确保整个工程编译通过；`go vet ./...` 也应该没有静态警告。
2. 执行 `go test -c -o /dev/null .`，确认根目录的 red_green_test.go 与被测代码签名完全一致、能编译出测试二进制。
3. 执行验证命令：
   ```
   go test . -count=3 -run '^TestRedGreen$' -timeout 60s
   ```
4. 观察 5 个场景的单独输出与最终判定：
   - `flush_on_write_true_access_log`：FlushOnWrite=true、SyncInterval=24h，Append 500 条访问日志。
   - `flush_on_write_false_access_log`：FlushOnWrite=false、SyncInterval=24h，Append 200 条访问日志。
   - `bg_sync_lifecycle_access_log`：SyncInterval=50ms、FlushOnWrite=false，分别在 ~600ms 和 ~2100ms 两个时间点采样 BackgroundSync 计数。
   - `bg_sync_lifecycle_url_store`：SyncInterval=50ms、FlushOnWrite=false，同样两阶段采样 URLStore 的 BackgroundSync 计数。
   - `close_final_sync_both_stores`：SyncInterval=24h、FlushOnWrite=false，产生 URL + 重定向访问日志后走 Close 流程，检查 Close 时的 final sync 计数。

## 4. 实际结果（Actual Behavior / Observed Output）
按上述步骤运行，3 次 count 结论一致（退出码 1，FAIL），终端打印：

```
RED（红灯，缺陷未修复） — 4/5 个场景失败 (1/5 通过)
--- FAIL: TestRedGreen (4.62s)
    [flush_on_write_true_access_log      ] FlushOnWrite=true but FlushOnWriteSyncs=0 (AppendCalls=500); expected >=450 — condition likely inverted
    [flush_on_write_false_access_log     ] FlushOnWrite=false but FlushOnWriteSyncs=200 (AppendCalls=200); expected <=20 — condition likely inverted
    [bg_sync_lifecycle_access_log        ] early stage: BackgroundSyncs=0 after 600ms (ticker every 50ms, ~12 ticks expected >=5); bg syncer likely died due to 50ms context timeout
    [bg_sync_lifecycle_url_store         ] URLStore phase1 bg syncs=1 after 600ms (expected >=2 with 50ms ticker); bg syncer likely exited early on short context timeout
    [close_final_sync_both_stores        ] close final sync ok
    RED — 1/5 scenarios passed
FAIL  shurl   13.855s
```

- RED/GREEN 判定结果：缺陷未修复时应为 RED（红灯）。
- 其他异常现象：
  - FlushOnWrite 配置语义与实际行为完全相反，true/false 的效果都对不上；
  - 后台周期性 fsync 工作几百毫秒后就完全停止，后续数据只能依赖 Close 时的那一次 final sync；
  - Close 场景在测试内虽然能拿到 final sync 计数，但在真实进程被强制终止、Close 未执行的情形下，会直接丢掉后台 Sync 停止后窗口内的全部写入。
- 本问题不涉及并发竞态，`go test -race` 不会报告 DATA RACE；现象 100% 稳定复现，不需要高并发压测。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应观察到：

- 无 panic、无 DATA RACE（本身非 race 类）；
- RED/GREEN 判定为 GREEN（绿灯），3 次 count 全部 PASS，退出码 0；
- 具体正确业务行为：
  1. FlushOnWrite=true：AppendCalls(500) 对应的 FlushOnWriteSyncs ≥450，即时落盘与配置一致；
  2. FlushOnWrite=false：FlushOnWriteSyncs ≤20，不做冗余的每次 Sync；
  3. AccessLogStore 后台 Sync 在 50ms ticker 下持续运行到 Close：600ms 阶段 BackgroundSyncs≥3、再 1.5s 后的增量 delta≥10；
  4. URLStore 后台 Sync 同样活到 Close：phase1 BackgroundSyncs≥2、phase1→phase2 的增量 delta≥8；
  5. Close 收尾顺序正确：cancel 通知后台 → wg.Wait 等 goroutine 结束 → 再做 Sync + Close 文件，AccessLogStore.CloseFinalSyncs≥1、URLStore.CloseFinalSyncs≥1；
  6. 模拟 kill -9 的场景下，即使 Close 未执行，后台 Sync 也不会提前退出，写入的脏数据能在最多一个 SyncInterval 内被落盘，不会大面积缺失。
- go build 与 go vet 全部通过。

## 6. 触发频率（Frequency）
必现（100%），count=3 连续 3 次运行均命中相同的失败场景，与 CPU 核数、并发量无关，单线程顺序执行同样能稳定复现。

## 7. 影响范围（Impact / Scope）
- 数据一致性：服务异常重启后 access.log（访问统计、审计回溯用）和 urls.json（短码映射）会缺失「后台 Sync 停止到 kill -9 发生」这一时间窗口内的全部新写入；数据丢失量取决于 SyncInterval 配置和后台 Sync 何时提前停止，最坏情况可达几小时甚至自启动起的全部增量。
- 线上可用性：业务重定向本身暂时还能走（因为走的是内存 map），但一旦遇到机器重启就会出现「昨天还能跳转的短码今天查不到」「统计报表突然少了一大段访问日志」的问题，审计和运营数据都会受影响。
- 资源开销：FlushOnWrite=false 时被错误地每条 Append 都做 Sync，QPS 高时会出现频繁 fsync 拖慢写入吞吐和磁盘 IO；反过来需要强一致（FlushOnWrite=true）的场景下完全不落盘，又带来数据丢失风险，两头不靠。

## 8. 附加说明（Additional Notes / Workaround）
临时规避建议：把 SyncInterval 调小到非常短（<100ms）、然后无论是否需要都把 FlushOnWrite 显式设成相反值可以勉强缓解其中一部分症状，但服务生命周期内后台 Sync 提前退出和 Close 顺序错位的本质问题无法绕过，强制退出仍然会丢数据，仅可作为救火手段。根本修复需要从 context 生命周期、配置条件判断、Close 收尾顺序三处同时处理干净。
