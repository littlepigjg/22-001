# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务的创建接口在触发存储层故障演练（PanicGuard）时，应当返回错误却返回成功，同时存储中被写入多条 COERCED- 前缀的脏记录以及一条指向 panic.invalid 的占位记录；对该短码执行重定向时，也应当返回 4xx 或错误却返回 302 跳转到 panic.invalid。正常不触发故障演练的短码可正常创建和跳转，故障注入场景下错误被整条链路吞掉、状态被污染。

## 2. 环境信息（Environment）
- 操作系统：Linux（任意发行版，复现不依赖特定 OS）
- Go 版本：go 1.22 及以上（go version 输出 go1.22 或更高）
- 项目模块/依赖：module shurl，零第三方依赖，纯标准库实现
- 运行参数：无需 -race（非并发类），默认 go test -count=1 即可稳定复现；如需多次执行可加 -count=N
- 硬件信息（与本次复现无关）：任意 CPU/内存

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 go build ./... 确保全部编译通过，再执行 go vet ./... 确保无静态错误；
2. 准备一个最小复现测试：构造 config.Default()，通过 Storage 的链式 setter 设置临时目录下的 url.json 与 access.log 路径，SyncInterval(0)、FlushOnWrite(true) 便于观察；
3. 调用 store.NewURLStore + Load(ctx) 构造 URLStore，调用 store.NewAccessLogStore + Open(ctx) 构造 AccessLogStore；
4. 对 URLStore 调用 SetPanicGuard 设置回调：当短码 code 前缀为 "BAD-" 时返回 true（模拟存储层故障注入）；
5. 构造 service.NewURLService(cfg, urlStore) 与 service.NewRedirectService(urlStore, logStore)；
6. 调用 urlSvc.Create(ctx, &CreateReq{RawURL: "https://example.test/x", CustomCode: "BAD-01"})，记录返回值 (created, err)；
7. 调用 urlStore.RawSnapshot() 获取当前存储快照，检查 key 是否出现 "COERCED-BAD-01"、"BAD-01"，以及对应 value 的 RawURL 字段；
8. 对 created.Code（非空时）或直接对 "BAD-01" 调用 redirectSvc.HandleRedirect(ctx, &RedirectRequest{Code: code, Timestamp: time.Now()})，记录返回值 (result, err) 以及 result.Status / result.RawURL；
9. 对照相同流程下对 "GOOD-01"（不触发 guard 的短码）的调用，验证正常短码的创建与重定向结果是否正确；
10. 或直接执行根目录的验证命令：go test . -count=1 -run '^TestRedGreen$' -v，观察输出的判定字样和失败项列表。

## 4. 实际结果（Actual Behavior / Observed Output）
- 步骤 6 中 Create 返回 err=nil，且 created 不为 nil，created.RawURL 是 "https://panic.invalid/" 而非原始请求的 URL，错误被吞掉；
- 步骤 7 RawSnapshot 中能看到至少两条脏记录：键 "COERCED-BAD-01" 指向 panic.invalid，键 "BAD-01" 本身也被写入且 RawURL = panic.invalid，存储状态污染；
- 步骤 8 HandleRedirect 返回 err=nil，result.Status=302，result.RawURL="https://panic.invalid/"，实际返回假的 302 重定向；
- 执行 go test . -count=1 -run '^TestRedGreen$' 的输出：
  ```
  RED（红灯，缺陷未修复）
    RED-REASON-1: Guard触发场景下 Create 返回错误为 nil（应返回非 nil error）
    RED-REASON-2: Guard触发场景下 Create 返回 ShortURL.RawURL 包含 panic.invalid 占位
    RED-REASON-3: Guard触发场景下 BAD-01 仍被登记为成功记录，存在脏数据
    RED-REASON-4: RawSnapshot 中检测到 COERCED- 前缀脏记录
    RED-REASON-5: RawSnapshot 中 BAD-01 的 RawURL 被污染为 panic.invalid
    RED-REASON-6: Guard污染记录命中重定向时返回 302->panic.invalid
    RED-REASON-7: BAD-01 直接重定向返回 302->panic.invalid
  --- FAIL: TestRedGreen
  FAIL  exit code 1
  ```
- RED/GREEN 判定结果：RED（红灯，缺陷未修复）；
- 本缺陷不涉及并发竞态，go test -race 也不会产生 DATA RACE 报告（但错误仍然必现）。

## 5. 期望结果（Expected Behavior）
- 触发 PanicGuard（BAD-* 短码）时 Create 必须返回 非 nil error，created 应为 nil，错误向上暴露给调用方；
- RawSnapshot 中绝不出现 "COERCED-" 前缀的任何键，BAD-* 短码在创建失败后不应写入存储，存储状态保持干净；
- HandleRedirect 对不存在 / 未创建成功的 BAD-* 短码返回 result.Status=404（或错误状态）不得合成 Status=302 指向 panic.invalid 的假跳转，err 应体现实际情况；
- 正常（不触发 guard）的 GOOD-01 短码创建后 RawURL 保持原样，重定向返回 302 指向原始 RawURL，所有行为符合预期；
- go test . -count=3 -run '^TestRedGreen$' 全部通过，测试明确打印「GREEN（绿灯，缺陷已修复）」字样，exit code=0；
- go build ./... 与 go vet ./... 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。只要 SetPanicGuard 命中（短码命中 guard 条件），创建脏写 + 重定向假 302 组合就稳定复现，与并发、时序、-race 均无关，单线程顺序执行同样 100% 复现。

## 7. 影响范围（Impact / Scope）
- 数据一致性：存储中出现大量 COERCED- 前缀和 panic.invalid 占位的脏记录，随故障演练次数累积污染持久化文件，后续快照查询和管理后台列表均会展示错误数据；
- 接口语义错误：调用方以为创建成功（err=nil）并把假的 ShortURL 返回给用户，但实际短码对应的 RawURL 是无效占位地址，业务逻辑被严重误导；
- 重定向错误：用户/爬虫命中被污染的短码时会被 302 引导到 panic.invalid 等无效域，而非返回 410/404/error，下游（网关、告警、监控）看不到异常返回码，线上无法感知故障；
- 错误传播链整体失效：panic、store error、service 层异常在多层被层层吞掉，排查难度极大，只有依赖 RawSnapshot 的诊断钩子才能观察到脏状态发生；
- 线上可用性：如果故障演练/混沌工程的开关被误打开，会在生产上制造大面积的假成功和假 302，用户跳转体验和业务统计都会出错。

## 8. 附加说明（Additional Notes / Workaround）
临时规避办法：暂时关闭 SetPanicGuard 的调用（恢复 nil 或永远返回 false），避免命中故障演练路径；同时对 RawSnapshot 做清理，删除 COERCED- 前缀键以及 RawURL=panic.invalid 的占位记录。临时规避只是止血方案，创建和重定向流程中的错误传递链问题仍然存在，需要代码级修复。
