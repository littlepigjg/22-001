# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链接服务的存储与业务层在高并发混合读写场景下存在严重的数据竞争与状态污染问题。同时对多条短码执行「访问计数自增 + 按条件修改 Remark / MaxVisits / Disabled + 批量禁用 + 批量改备注 + 高强度并发压测」等组合操作时，进程会以 runtime panic 直接崩溃，即便侥幸未崩溃，最终落盘的短链接记录也会出现 Visits 为负数、Code 字段被清空为 ""、Disabled / Remark / MaxVisits 等字段互相覆盖或来回跳变等脏数据。问题仅在并发场景下显现，单独串行调用一切正常，使用 go test -race 可稳定捕获到数据竞争告警。

## 2. 环境信息（Environment）
- 操作系统：Linux（任意发行版，例如 Ubuntu 22.04 / CentOS 7）
- Go 版本：Go 1.22 及以上（go version 输出形如 go1.22.X linux/amd64）
- 项目模块/依赖：module shurl，零第三方纯标准库实现
- 运行参数：go test -race -count=3/-count=10 -run '^TestRedGreen$'，内部并发 goroutine 量级约为 (12×2×4) + (2×4) + (12×3) = 140 个，混合访问计数自增与字段更新两种路径
- 硬件信息（与并发/性能相关）：建议 CPU ≥ 4 核，-race 下内存占用较大但本用例限制在 20 秒内完成，无需额外硬件

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（包含 go.mod 的目录），先执行 `go build ./...` 与 `go vet ./...` 确保项目能正常编译且无静态检查错误；
2. 执行并发压测验证命令：`go test -race . -count=3 -run '^TestRedGreen$' -v`；
3. 观察控制台输出，重点关注：
   - 是否出现 `WARNING: DATA RACE` 开头的竞争报告；
   - 是否出现 `fatal error: concurrent map read and map write` 或 `concurrent map writes` 的 runtime panic 堆栈；
   - 每轮测试末尾的「RED（红灯，缺陷未修复）/ GREEN（绿灯，缺陷已修复）」打印；
4. 若希望进一步确认数据脏读现象，可把 -count 提高到 10 并降低并发数跑多轮，关注最终校验报告中的 bad 计数（Visits<0 或 Code="" 的条目的数量）；
5. 也可以单独跑 `go test . -count=1 -run '^TestRedGreen$'` 不加 race，复现高概率直接进程崩溃的 RED 现象。

## 4. 实际结果（Actual Behavior / Observed Output）
按上述复现步骤执行后，可稳定观察到以下异常：
- 具体 panic / 错误信息：
    ```
    fatal error: concurrent map read and map write

    goroutine 68 [running]:
    ...
    shurl/internal/store.(*URLStore).IncrementVisits(...)
        .../internal/store/url_store.go:322
    shurl/internal/store.(*URLStore).Get(...)
        .../internal/store/url_store.go:180
    ```
    以及在 -race 模式下的：
    ```
    ==================
    WARNING: DATA RACE
    Write at 0x... by goroutine 46:
      shurl/internal/store.(*URLStore).IncrementVisits()
    Previous read / write at ...
    ==================
    ```
- RED/GREEN 判定结果：RED（红灯，缺陷未修复）；若进程先崩溃则直接 FAIL 退出，exit code 非 0；
- 其他异常现象：在极少数无进程崩溃的轮次中，测试末尾的完整性校验会报告 bad>0，表现为若干条短链的 u.Visits 为负数、u.Code 被清空成空字符串，Remark/Disabled/MaxVisits 字段也存在乱值（如 Remark 被重复拼接前缀、MaxVisits 被改成字符串长度、Disabled 被异步置 true 后又被置 false）；
- go test -race 是否报告 DATA RACE：是，稳定在 fastCache 读写路径与 ShortURL 共享指针字段上报告多处数据竞争。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤执行应得到如下正确行为：
- 无 panic、无数据竞争：go test -race 运行过程中不出现任何 `fatal error: concurrent map ...` 的 runtime panic，也不打印任何 `WARNING: DATA RACE`；
- RED/GREEN 判定结果为 GREEN（绿灯，缺陷已修复），所有 -count=N 轮次测试一致 PASS，对应 exit code 全为 0；
- 具体业务正确性：
  - 高并发混合操作结束后，所有被压测的短码记录 Get 均返回非 nil；
  - 每条记录 Visits 为非负整数，不存在丢失更新或整数溢出负值；
  - Code 字段始终等于创建时的短码值，绝不再被清空为 ""；
  - Disabled / Remark / MaxVisits 等业务字段的并发写表现为可串行化的最终一致结果（如批量禁用对某条生效后不能被其他"批量改备注"莫名其妙恢复，改备注不会额外拼接前缀或把 MaxVisits 改成乱值）；
- go build ./... 与 go vet ./... 全部通过，无新的编译错误或静态检查告警。

## 6. 触发频率（Frequency）
- 无 -race 单次运行（`go test . -count=1 -run ...`）：约 95%+ 概率在并发阶段直接触发 runtime map 并发写 panic 而进程崩溃；
- 带 -race 运行（`go test -race . -count=3 ...`）：100% 必现，每轮均至少出现 1 次 DATA RACE 或 fatal map panic；
- 降低并发后的个别稀有轮次：进程不崩溃但 bad>0（Visits<0 / Code 清空），数据完整性校验失败；
- 仅在串行单线程调用各接口单独功能：不会触发。

## 7. 影响范围（Impact / Scope）
该缺陷会在生产高并发业务流量下造成严重影响：
- 服务进程间歇性直接 panic 崩溃，接口不可用、短链跳转失败，线上可用性大幅下降；
- 即使进程未崩溃，也会产生脏数据：访问计数 Visits 出现负数或乱值、短链 Code 被清空导致后续跳转 404、被 Disable 的短链莫名恢复可访问、备注与最大访问次数字段被互相覆盖导致业务展示混乱；
- 在后台统计、管理后台查询、后台巡检/清理链路中读取到被污染的短码记录时，可能进一步引发 nil 指针解引用、空字符串短码被写入磁盘持久化、JSON 反序列化异常等连锁错误；
- 对于对外网关类短链接服务，相当于存在高危的并发崩溃路径与数据静默损坏路径，在并发 > 数百 QPS 的场景几乎必现。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方案：
- 在修复前可以临时把服务的并发度降到极低（如单 worker 串行处理写请求），或者所有写路径（Disable、UpdateRemark、BatchDisable、BatchUpdateRemark、IncrementVisits 相关业务）使用一个全局互斥锁串行化，能避免崩溃但会显著损失性能；
- 不可作为长期方案，仅能在紧急期临时使用。因为共享对象裸指针层面的读写竞态仍可能在无锁读路径上读到半更新状态，临时串行化仍无法覆盖所有读场景竞态；
- 相关日志样例：一旦问题触发，可在 stderr 看到完整 goroutine 堆栈（如前述 IncrementVisits / Get 两行堆栈），可通过该堆栈判断与本问题属于同一现象。
