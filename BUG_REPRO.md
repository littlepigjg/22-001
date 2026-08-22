# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在特定异常路径下出现错误传播失效：当创建短链接或处理重定向请求的过程中，如果底层存储模块因为某些特殊输入触发 panic（异常分支），上层业务并不会向上返回 error，而是错误地将该异常当作一次"成功但返回值类型特殊"的调用处理。表现为：
- 创建接口返回自定义短码以外的奇怪短码（如 FALLBACK-、COERCED-、FB- 前缀），且这些垃圾记录会被真正写入持久化存储；
- 重定向接口对已存在的有效短码，不会返回预期的 error 或 404/410，而是返回一个 302 跳转到形如 `https://panic.invalid/?detail=...` 的假地址；
- 系统状态被污染，管理后台列表、短码总数等统计也会包含这些无意义的垃圾条目。

正常请求（没有命中上述异常输入的情况）的创建 / 查询 / 重定向行为不受影响。

## 2. 环境信息（Environment）
- 操作系统：Linux（任意现代发行版即可，复现用运行环境为 go1.26.5 linux/amd64）
- Go 版本：go version go1.26.5 linux/amd64（理论上 go 1.22+ 均可，项目 go.mod 要求 1.22）
- 项目模块/依赖：module `shurl`，纯标准库实现，无第三方依赖
- 运行参数：
  - 构建 / 静态检查：`go build ./...`、`go vet ./...`
  - 验证命令：`go test . -count=3 -run '^TestRedGreen$'`
  - 不要求 -race（本缺陷不涉及并发竞态）
- 硬件信息（复现不敏感）：CPU 任意核数均可，内存 >= 256MB 即可

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，确保 go.mod 为 module shurl 且 Go 版本可用：
   ```
   cd <项目根>
   go version
   ```
2. 执行构建与静态检查，确认环境无问题：
   ```
   go build ./...
   go vet ./...
   ```
   两者都应当无输出、返回码为 0。
3. 执行缺陷验证用例（测试文件为项目根目录的 `red_green_test.go`）：
   ```
   go test . -count=3 -run '^TestRedGreen$' -v
   ```
   -count=3 是为了避免偶现，保证缺陷稳定复现。整个测试总运行时应该在 1 秒以内。
4. 观察标准输出中每个子检查的 RED/GREEN 判定打印，以及最后整体判定与退出码：
   - 正常未修复状态下，会看到 `RED（红灯，缺陷未修复）` 被多次打印；
   - 测试最终 FAIL，退出码为非 0。
5. （可选）仅运行正常流程子检查观察未被破坏的回归行为：
   ```
   go test . -count=3 -run 'TestPanicPropagation_RegressionNormalFlow$'
   ```
   此子检查应始终打印 `GREEN（绿灯，缺陷已修复）`。

## 4. 实际结果（Actual Behavior / Observed Output）
- 运行 `go test . -count=1 -run '^TestRedGreen$'` 的关键输出（未修复）：
  ```
  === RUN   TestRedGreen
  === RUN   TestRedGreen/CreateShouldReturnError
  RED（红灯，缺陷未修复）
      ... case CreateShouldReturnError failed
  === RUN   TestRedGreen/RedirectShouldReturnError
  RED（红灯，缺陷未修复）
      ... case RedirectShouldReturnError failed
  === RUN   TestRedGreen/SingleflightReturnSemantics
  RED（红灯，缺陷未修复）
      ... case SingleflightReturnSemantics failed
  === RUN   TestRedGreen/RegressionNormalFlow
  GREEN（绿灯，缺陷已修复）
  RED（红灯，缺陷未修复） — 3/4 子检查未通过
  --- FAIL: TestRedGreen (0.00s)
  FAIL    shurl   0.010s
  FAIL
  ```
- 退出码：1（非 0）
- RED/GREEN 判定：整体 RED（红灯，缺陷未修复）
- 具体缺陷表现：
  - CreateShouldReturnError：创建请求命中存储异常时，`Create(...)` 返回 err=nil，返回的 `*ShortURL.Code` 不是用户请求的自定义短码，而是以 FALLBACK- / COERCED- 开头的合成短码；并且存储快照中确实能看到这些合成记录被持久化，污染了系统状态。
  - RedirectShouldReturnError：已存在的有效短码在解析阶段触发底层异常时，`HandleRedirect` 返回 err=nil，`RedirectResult.Status` 被设置为 302，`RawURL` 指向 `https://panic.invalid/?detail=...` 这种系统合成的假地址，而不是真实存储中的 RawURL，也没有返回任何 error。
  - SingleflightReturnSemantics：连续 3 次在会触发底层 panic 的请求上调用 `Create`，每一次都返回 nil error，最终存储中残留 1 条以上以 FALLBACK- / COERCED- / FB- 开头的垃圾短码。
- 本缺陷不涉及并发竞态，`go test -race` 不会报告 DATA RACE。

## 5. 期望结果（Expected Behavior）
- 底层存储/调用链中的 panic 或异常，必须以 error 的形式正确向上传播：
  - `URLService.Create` 在遇到存储层 panic 时，返回非 nil error，且不得写入任何短码记录（不出现 FALLBACK- / COERCED- / FB- 前缀的垃圾条目）。
  - `RedirectService.HandleRedirect` 在遇到存储层 panic 时，返回非 nil error，且 res.Status 不得为 302，不得跳转任何 panic.invalid 相关合成地址。
- 验证命令判定：
  - `go test . -count=3 -run '^TestRedGreen$'` 所有子检查均打印 `GREEN（绿灯，缺陷已修复）`，最终退出码为 0。
  - 即使在会触发 panic 的输入上反复调用，存储快照也不会出现任何 FALLBACK- / COERCED- / FB- 前缀的记录。
- 回归行为：
  - 正常创建自定义短码 / 自动短码、正常重定向、正常删除等操作结果与修复前一致，数据不丢失、跳转不中断。
  - `go build ./...` 与 `go vet ./...` 全部通过，无警告无错误。

## 6. 触发频率（Frequency）
必现（100%）。只要输入命中 panic 触发条件（自定义短码的特定前缀、RawURL 的特定后缀、或通过代码注入 guard），缺陷表现就能在单次运行中稳定观察到；使用 -count=3 也 100% 复现。

## 7. 影响范围（Impact / Scope）
- 数据一致性：系统会产生大量无意义的 FALLBACK- / COERCED- 前缀垃圾短码，占用存储，污染管理列表、统计数据与聚合结果，后续人工清理成本高。
- 客户端语义：调用方以为创建/重定向成功，实际拿到的是系统捏造的假结果。客户端会被跳转到 panic.invalid 这样的无效地址，用户体验极差。
- 错误掩盖：真正的底层 panic/存储异常被静默吞掉，监控、告警完全无法捕捉，运维无法感知故障。
- 二次故障风险：客户端把返回的假短码当作真实短码继续使用，会触发后续重定向接口再次命中假跳转，形成链式错误。
- 正常流程不受影响（RegressionNormalFlow 保持 GREEN），所以问题呈现"局部异常、全局污染"的特征，较难在冒烟测试中发现。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法（workaround）：
- 客户端在拿到创建结果后，立即再调用一次 `Get(code)` 做回读校验，确认存储中的 RawURL 与自己传进去的一致；如果是 panic.invalid 相关域名或 code 以 FALLBACK- / COERCED- 开头则判定为失败并重试。
- 管理后台可以定时扫描并删除 code 以 FALLBACK- / COERCED- / FB- 开头的记录，降低污染，但无法从根本上阻止错误继续发生。
- 对重定向接口，客户端如果拿到 302 且目标域名包含 `panic.invalid`，应视作 500 处理，不要继续跟随跳转。

以上 workaround 仅为临时止损，根因必须通过修复代码解决。
