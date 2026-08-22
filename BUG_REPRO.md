# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务新增的后台诊断功能在处理失败任务时会直接崩溃。当向任务池提交若干会返回错误的后台任务，在任务全部执行结束后尝试聚合错误列表、生成失败报告时，服务立即发生运行时 panic，调用方无法拿到任何统计结果，进而影响管理面板中失败任务检查、组件状态诊断等功能的可用性。

## 2. 环境信息（Environment）
- 操作系统：Linux（Ubuntu 20.04 / 22.04 等主流发行版）
- Go 版本：go1.22（项目 go.mod 指定 go 1.22），使用 `go version` 确认
- 项目模块/依赖：module `shurl`，零第三方依赖，仅使用 Go 标准库
- 运行参数：普通 `go test` 即可复现；无需 -race（非并发缺陷）；-count=1 到 -count=5 均可
- 硬件信息（如与并发/性能相关可补充）：与 CPU 核数无关，单核/多核均能复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，先执行 `go build ./...` 确保全项目编译通过；再执行 `go vet ./...` 确认无静态错误。
2. 创建一个 worker pool（workers=2 即可），调用 Start 启动。
3. 向池中连续提交 5 个任务，每个任务的执行函数都返回一个非 nil 的错误（错误消息可任意，例如 "simulated failure #k"）。
4. 调用 pool 的 Stop 方法等待所有任务执行完成并关闭队列。
5. 调用 pool.Errors() 取出错误切片，随后依次调用下列任意一个聚合辅助函数：
   - CompactErrors / DeduplicateErrors / FlattenErrors / ReportErrors（错误摘要工具）
   - 或 LenientErrorCount / FirstErr / LastErr / ExtractTaskNames（错误切片宽松工具）
   - 也可直接创建 admin.Service 并调用 RunTaskDiagnostics 走完整的诊断链路。
6. 观察程序输出 / 日志 / panic 堆栈信息。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体错误 / panic 信息：
  ```
  panic: runtime error: index out of range [5] with length 5
  goroutine 1 [running]:
  ...（调用栈中包含对错误切片扫描、聚合、摘要的调用点）
  ```
  在测试环境下通过 `go test -v` 捕获到的等价关键输出：
  ```
  RED（红灯，缺陷未修复）
  captured panic (slice index out of range): runtime error: index out of range [5] with length 5
  ```
- RED/GREEN 判定结果：缺陷未修复时为 **RED**（红灯），`go test` 退出码为 1。
- 其他异常现象：
  - 一旦触发，整条聚合链路无法恢复（panic 未在该层 recover），最终会冒泡到上层（HTTP handler 中的 recovery 中间件才能兜住，管理面板返回 500）。
  - ReportErrors/CompactErrors 等工具函数返回值完全不可用（中途 panic，根本返回不到），调用方拿不到任何统计数字。
- go test -race 是否报告 DATA RACE：不涉及，这是纯切片越界的确定性缺陷，-race 不报警告，但必然 panic。

## 5. 期望结果（Expected Behavior）
修复后，按相同步骤操作应满足以下全部行为：
- 无 panic、无任何运行时崩溃；即使传入 0 条、1 条、50 条全部非 nil 的错误，也能正常返回。
- RED/GREEN 判定结果应为 **GREEN**（绿灯）：`go test . -count=1 -run '^TestRedGreen$'` 全部 PASS，退出码 0，输出中显式打印 `GREEN（绿灯，缺陷已修复）`。
- 具体的正确业务行为：
  1. 错误聚合工具 CompactErrors / DeduplicateErrors / FlattenErrors / ReportErrors 对 0 条 / nil / 全非 nil / 混合 nil 四类输入，均以切片真实长度（len）作为遍历终止边界，不依赖任何哨兵约定。
  2. ReportErrors 返回的 ErrorReport 中 Total、TaskErrors、StoreOps、DomainErrs、OtherErrs、FailedTasks、TopMessages 各字段与实际输入一致，无丢失、不多算。
  3. LenientErrorCount / FirstErr / LastErr / ExtractTaskNames 行为与 for range 一致，空切片返回 0 / nil / nil / 空列表。
  4. admin.Service.RunTaskDiagnostics 在注册 flushers/syncers/closers 或未注册的情况下，均能返回一个合法的 *TaskDiagnosticReport，其中 Passed、Failed、Summary 字段数值正确，可被 JSON 序列化。
- `go build ./...` 与 `go vet ./...` 全部通过，无报错、无警告。

## 6. 触发频率（Frequency）
必现（100%）。只要满足：`Pool.Errors()` 返回的切片中没有 nil 元素（即提交的失败任务数 >= 1 且全部为非 nil 错误），后续调用任一个依赖 "末尾 sentinel nil" 的错误聚合工具，就必然在第 N 次索引访问时越界。失败任务数为 0 时输入是空切片（len=0），同样会在 n=0 位置触发 `[0] with length 0` 越界。该缺陷与并发、操作系统、Go 小版本无关，纯确定性复现。

## 7. 影响范围（Impact / Scope）
- 管理侧接口可用性下降：管理面板的"失败任务检查"、"组件诊断报告"等接口直接返回 500，运维无法通过后台了解失败任务情况。
- 服务稳定性：若调用处无上层 panic 恢复中间件，会直接导致 HTTP worker goroutine 异常终止；多次命中时会造成服务处理能力下降、日志被大量堆栈刷屏。
- 数据观测缺失：错误聚合工具本身返回值不可信 / 不可用，后续依赖它做的报警摘要、失败任务 Top N 等功能全部失效，问题排查链路被截断。
- 扩展性风险：任何后续新增使用 ReportErrors / CompactErrors / LenientErrorCount 等工具的业务代码，都会继承同样的越界问题，形成隐性"地雷"。

## 8. 附加说明（Additional Notes / Workaround）
临时规避办法（仅供应急、最终仍需修复代码）：
- 在调用上述错误聚合工具前，先对输入做一层防御性包装，例如 `append(append([]error(nil), errs...), nil)`，手动在末尾补一个 nil 哨兵，但这样会让实际错误数统计、FirstErr/LastErr 语义发生变化（最后一个真实错误之后多一个 nil，某些函数可能把 nil 当成"到末尾"信号而截断后面的有效元素），只能作为短期止血手段，不可长期依赖。
- 管理面板中暂时禁用触发 RunTaskDiagnostics 的入口，改为直接调用底层 pool.Stop() 拿原始 error（Stop 用 errors.Join 不受影响），避免走到错误摘要链路。

