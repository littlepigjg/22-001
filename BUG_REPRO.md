# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链接服务的优雅关闭流程与管理侧强制 flush 接口在组件发生 IO 错误（磁盘写失败、文件句柄异常、权限问题、组件等待超时等）时不会正常返回错误，而是直接触发运行时 panic 崩溃，导致后续资源清理步骤被跳过、进程直接退出，无法完成优雅关闭。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核版本不敏感，任意现代 Linux 发行版均可复现）
- Go 版本：Go 1.21+ （建议 go1.22 或更高，项目本身使用标准库，无第三方依赖）
- 项目模块/依赖：module 名为 `shurl`，零第三方依赖，仅使用标准库（context、errors、sync、testing、time、net/http 等）
- 运行参数：go test . -count=1 -run '^TestRedGreen$' （验证命令，总耗时 < 1s）
- 硬件信息（如与并发/性能相关可补充）：任意 CPU 核数均可稳定复现，与并发无关

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（go.mod 所在目录），执行 `go build ./...` 确保所有包编译通过，执行 `go vet ./...` 确保静态检查通过。
2. 在当前目录下确认存在 red_green_test.go，测试中使用了会返回错误的假组件（Flusher/Syncer/Closer 的失败实现），模拟磁盘写满、权限拒绝、连接重置等常见失败场景。
3. 执行验证命令：`go test . -count=1 -run '^TestRedGreen$' -v`。
4. 观察输出：
   - 控制台会打印 `RED（红灯，缺陷未修复）`。
   - go test 以 FAIL 结束（退出码非 0）。
   - 多个子用例会触发 `panic: runtime error: index out of range [N] with length N`（N 取决于当次错误个数，典型如 index out of range [1] with length 1）。
5. （可选）多次执行验证稳定性：`go test . -count=3 -run '^TestRedGreen$'`，每次均为 RED，100% 复现。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体的 panic 信息：
  ```
  panic: runtime error: index out of range [1] with length 1
  ```
  或
  ```
  panic: runtime error: index out of range [3] with length 3
  ```
  形式为 `index out of range [len(errs)] with length len(errs)`，属于典型的切片下标越界崩溃。
- RED/GREEN 判定结果：RED（红灯，缺陷未修复）。
- 其他异常现象：
  - 错误路径下 Stop 无法返回 error，直接 panic，调用方无法兜底处理错误信息；所有失败 hook 的原始错误信息在 panic 后丢失。
  - admin 管理接口在磁盘 IO 故障时调用 FlushAll/CloseAll 会直接中断进程而非返回 500 错误。
  - 服务接收到 SIGTERM 的优雅关闭阶段，若任一持久组件 flush/sync/close 失败，会跳过剩余资源清理步骤直接崩溃。
- go test -race 是否报告 DATA RACE：该缺陷与并发无关，-race 下无 DATA RACE 告警（但切片越界 panic 仍会发生）。

## 5. 期望结果（Expected Behavior）
- 无 panic、无数据竞争：任何 hook 错误、flush/sync/close 错误、Stop 超时都只通过 error 返回值向上传递，不会出现 runtime panic。
- RED/GREEN 判定结果应为 GREEN（绿灯，缺陷已修复），控制台输出 `GREEN（绿灯，缺陷已修复）` 字样，go test 以 PASS 退出（退出码 0）。
- 具体的正确业务行为：
  - Stop 在单错误时返回第一个或最后一个错误（语义稳定即可），多错误时通过 errors.Join 返回合并错误，nil 时返回 nil。
  - admin.FlushAll / CloseAll 对失败组件收集全部错误并合并返回，同时保证成功的组件仍能正常完成 flush/close；不会因为某一个组件失败导致其他组件无法清理。
  - 服务优雅关闭阶段所有 flush/sync/close 钩子按顺序执行完成，即使有错误也能把错误以 warn 日志输出后正常退出，进程不会 panic。
- `go build ./...` 与 `go vet ./...` 全部通过，无新引入的静态告警。

## 6. 触发频率（Frequency）
必现（100%）。只要 Stop 过程中存在至少一个错误（不管是 hook 返回错误、Stop 等待超时、AppendNamedError 追加的组件错误），走到返回 switch 就会越界，与并发、CPU、负载无关。`-count=3` 连续运行 3 次均稳定命中。

## 7. 影响范围（Impact / Scope）
- 服务可用性：线上优雅关闭阶段或手动触发 flush 时，任何存储 IO 抖动都会引发进程直接 panic 崩溃，可能导致请求中断、正在处理的短链接重定向 5xx 或直接中断连接。
- 数据一致性：panic 后 CloseAll 的后续 close/final sync 步骤被跳过，可能造成未落盘数据丢失（访问日志 NDJSON 尾部截断、URL 映射 JSON 版本落后于内存）。
- 运维与可观测性：错误返回信息全部丢失，只能看到 runtime panic 堆栈，排障时无法知道具体是哪个 flusher/syncer/closer 出了问题。
- 管理接口可用性：/api/admin/flush 或 /api/admin/health（健康检查中会触发 FlushAllSilent）在存储故障时直接返回 500 并伴随服务崩溃，破坏健康探测的降级语义。

## 8. 附加说明（Additional Notes / Workaround）
- 临时规避：暂不使用强制 flush 的管理接口，且在停机前确保磁盘可用空间充足、文件句柄权限正常；避免在 Stop 钩子中注册可能失败的操作，尽量在停机前提前完成 flush。
- 该问题与并发/竞态无关，-race 参数不会影响复现率，多次运行 -count=N 结果一致。
- 验证测试独立可运行，不依赖外部环境变量、网络服务或配置文件。
