# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
使用可控「假时钟」对统计服务做确定性单测时，当统计缓存的 TTL 时长设置为 24 小时（或相关时长值），构造出的带超时上下文会被立刻取消，导致统计聚合逻辑还没开始执行就直接返回「已取消」错误。同样的问题在直接使用 Fake 时钟创建 24h 超时时也会出现：刚拿到的 ctx 已经处于 Done 状态。其他时长（如 1h、3h、12h、48h）则正常。

## 2. 环境信息（Environment）
- 操作系统：Linux（发行版不限，验证环境为 Linux x86_64）
- Go 版本：go 1.22（模块要求 1.22，输出示例：`go version go1.22 linux/amd64`）
- 项目模块/依赖：module `shurl`，纯标准库实现，无第三方依赖
- 运行参数：`go test . -count=1 -run '^TestRedGreen$'`（验证主命令）；可辅以 `-count=3` 重复执行确认稳定性
- 硬件信息（如与并发/性能相关可补充）：无特殊要求，单核 / 多核均可稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（含 go.mod 的目录），执行 `go build ./...` 确认整个模块可以正常编译；再执行 `go vet ./...` 确认没有静态检查错误。
2. 直接运行根目录下的红/绿验证测试：
   ```
   go test . -count=1 -run '^TestRedGreen$' -v
   ```
3. 观察测试输出：重点关注 C/E/F/G/H 这几个与 24h TTL 相关的子测试。
4. 如需最小化手动复现核心链路：实例化一个 clock.Fake，调用 `Fake.ContextWithTimeout(context.Background(), 24*time.Hour)`，紧接着对返回的 ctx 做 `select { case <-ctx.Done(): ... default: }` 检查，即可观察到 ctx 立即被取消。
5. 也可通过配置方式验证：调用 `config.Default()` 取其 Stats.CacheTTL（值为 24h），再用 `StatsCfg.BuildCacheContext(context.Background(), fakeClock)` 构造上下文，同样会立即命中取消。

## 4. 实际结果（Actual Behavior / Observed Output）
- 运行验证命令后，子测试 `C_FakeClock_ContextWithTimeout_24h_not_canceled`、`E_StatsCfg_BuildCacheContext_24h_Fake_not_cancel`、`F_StatsService_Overall_24h_TTL_Fake_not_canceled`、`G_Default_Stats_CacheTTL_BuildCacheContext_sane`、`H_FakeClock_ContextWithDeadline_24h_not_canceled` 全部 FAIL，每个失败点都会输出形如：
  ```
  RED（红灯，缺陷未修复）: Fake+24h 上下文立即被取消, err=context deadline exceeded (期望保持 ctx 直到真正超时)
  RED（红灯，缺陷未修复）: Stats.Overall(24h TTL+Fake) 立即返回取消错误: model: operation canceled (期望正常完成)
  RED（红灯，缺陷未修复）: 默认 CacheTTL=24h0m0s 经 BuildCacheContext(Fake) 立即取消, err=context deadline exceeded
  ```
- StatsService 内部日志输出：
  ```
  {"caller":"service/stats_service.go:120","code":"test123","err":"context deadline exceeded","level":"WARN","msg":"stats overall canceled before aggregate", ...}
  ```
- 测试最终总体判定为：`RED（红灯，缺陷未修复）`，go test 退出码为 1（FAIL）。
- 最小复现脚本现象：Fake.ContextWithTimeout(ctx, 24h) 返回后立刻读 ctx.Err()，得到 `context deadline exceeded`，等价于 context 从创建之初就被取消。
- 该问题不涉及并发数据竞争，因此 `-race` 下无「DATA RACE」报告（仅出现上面的取消类错误）。

## 5. 期望结果（Expected Behavior）
- 对于 Fake 时钟创建的 24h 超时 ctx：在尚未对 Fake 时钟执行 Advance 推进到 24h 之前，ctx.Done() 不应被触发；必须至少 Advance(24h) 之后，ctx.Err() 才返回 deadline 超时错误。
- StatsCfg.BuildCacheContext + Fake 时钟、CacheTTL=24h：构造出的 ctx 在未 Advance 的情况下保持未取消状态，select default 分支应被命中。
- StatsService.Overall 在 CacheTTL=24h + Fake 时钟的组合下，应真正进入 aggregate 函数并完成聚合（即使结果为空统计），返回的 err 必须为 nil，且不得是 model.ErrCanceled / context.Canceled / context.DeadlineExceeded 中任何一种。
- 环境变量字符串形式的 TTL（如 "24h"、"1d"）被解析并送入 ContextWithTimeout 后，行为应与直接写死的 24*time.Hour 完全一致（都不应该立即取消）。
- 运行 `go test . -count=3 -run '^TestRedGreen$'` 三次全部通过，所有 A~H 子测试 PASS，最终输出：`GREEN（绿灯，缺陷已修复）`，退出码为 0。
- `go build ./...` 与 `go vet ./...` 全部无报错通过。

## 6. 触发频率（Frequency）
必现（100%）。任何使用 Fake 时钟 + 恰好 24h TTL 的组合都会稳定触发；用字符串 "24h" / "1d" 经 TTL 友好解析器写入配置也同样 100% 触发。多次重复运行 -count=3 结果一致，不依赖随机、不依赖调度。

## 7. 影响范围（Impact / Scope）
- 后台统计聚合任务：当管理员使用 24h 作为统计缓存 TTL 的常见默认值时，所有使用 Fake 时钟进行的确定性测试都会报告「取消错误」，导致测试套件红灯；若生产环境也复用了同样的规范化 / 校验工具链且存在类似的负时长 sentinel 泄漏，则长 TTL 配置可能导致真实请求的 context 提前被取消，统计接口不可用。
- 超时语义被破坏：调用方写入了合理的 24h 长 TTL，却得到了「立即取消」的上下文，ctx.Err() 返回 deadline exceeded，业务逻辑层无法区分是「真的超过 deadline」还是「错误的立即取消」，造成错误传播链污染。
- 可配置的长寿命后台任务（如 Janitor、Sync 等）如果同样经过该 TTL 解析 + Context 创建链路，也会在恰好为 1d / 1w / 12h 这种整时段的配置上出现「任务一启动就被取消」的异常，引发后台清理、落盘、统计功能失效。
- 回归风险：修复时若破坏非 24h 时长（如 1h、10s）的上下文创建，可能导致其它正常超时代码路径回归。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法：把 TTL 从 24h 改成略偏离 24h 的值（例如 23h59m59s + 1ms 或 24h1s），或在创建上下文前手动把时长改成 23h、48h 等其它非窗口内的值，可以绕开立即取消的问题继续使用；但这只是临时 workaround，并不是真正修复。另外该问题只出现在 Fake 时钟路径中，Real 时钟没有受影响（但 sentinel 值泄漏的链路在 Real 时钟下也可能表现为 timeout 被设成了极小值，实际业务里建议同样修复）。
