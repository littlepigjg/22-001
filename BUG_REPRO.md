# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在接入「自定义短码签名校验」、「重定向签名校验」以及通用 HMAC 签名 Verify 接口之后，对一类特殊输入（空 signer key、空 payload 但带有合法 hex 签名、或 Verify 直接传空 data）会直接发生空指针崩溃（panic: runtime error: invalid memory address or nil pointer dereference）。影响范围覆盖创建短链请求参数校验、重定向鉴权校验、以及底层签名校验函数的直接调用；一旦外部传入特定组合的字段即可稳定触发进程 panic。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核版本可通过 `uname -a` 查看）
- Go 版本：go 1.22+（`go version` 输出，项目 go.mod 指定 go 1.22）
- 项目模块/依赖：module shurl，零第三方依赖，仅使用 Go 标准库
- 运行参数：go test . -count=3 -run '^TestRedGreen$'（非并发缺陷，不使用 -race）
- 硬件信息：与 CPU 核数无关，单 goroutine 即可稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录（`/home/admin/code/22/001/22-001-11`），执行 `go build ./...` 与 `go vet ./...` 确保编译与静态检查通过（两者均无报错）。
2. 执行签名校验相关的最小复现单元（任选其一即可稳定复现）：
   - A：直接使用 cryptoutil 层：构造 `v := cryptoutil.NewPayloadVerifier(nil, nil)`，然后调用 `v.Verify([]byte(""), strings.Repeat("0", 64))`。
   - B：validator 层：构造 cfg.Key = []byte{}、空 code、合法 hex 签名；调用 `validator.VerifyCustomCodeSignature("", strings.Repeat("a", 64), cfg, policy)`。
   - C：model 层：构造 `CreateReq{RawURL: "https://example.com/", CustomCode: "", CodeSignature: strings.Repeat("f", 64), SignerSalt: ""}`，调用 `req.Validate()`。
   - D：model 重定向校验：构造 `RedirectCheckReq{Code: "abcd1234", Signature: strings.Repeat("e", 64)}`，调用 `r.Validate([]byte{})`。
   - E：Signer 自身：用合法 key 生成 signer，再调用 `signer.Verify([]byte(""), strings.Repeat("0", 64))`（data 长度为 0 即可）。
3. 在项目根目录执行统一的复现命令：`go test . -count=3 -run '^TestRedGreen$' -v`。
4. 观察控制台输出、RED/GREEN 判定与 panic 堆栈；该命令会分别覆盖上述 A~E 共 5 条路径。

## 4. 实际结果（Actual Behavior / Observed Output）
- panic 信息：`runtime error: invalid memory address or nil pointer dereference`，Go 会打印相应 goroutine 的堆栈，栈顶位于 HMAC 初始化 / Verify 相关函数（读取 key 字段时发生 nil 解引用）。
- RED/GREEN 判定结果：缺陷未修复时为 **RED（红灯，缺陷未修复）**，`go test` 整体 FAIL。
- 其他异常现象：
  - 5 个子用例全部在调用处 panic 并被测试捕获，结果校验 PASS:0 FAIL:5。
  - 对上层调用方而言，任何走到对应校验分支的 HTTP 请求都会直接进程 panic（若无全局 recovery 中间件则进程退出）。
- 并发/竞争：本缺陷为必现的 nil 指针解引用，不涉及数据竞争（DATA RACE），-race 非必须。

## 5. 期望结果（Expected Behavior）
- 无 panic：A~E 五条调用路径均不会再产生 `invalid memory address or nil pointer dereference`；底层对 nil Signer、空 key、空 payload 带签名、空 data 等情况均能提前返回 false 或明确错误。
- RED/GREEN 判定结果：修复后相同命令执行结果应为 **GREEN（绿灯，缺陷已修复）**，`go test` 整体 PASS。
- 具体业务行为：
  - 空 key / 空 payload + 签名：返回参数错误或签名校验失败（error / false），不崩溃。
  - 非空合法 key + 正确签名：校验通过；payload 或签名任一字段改动则校验失败。
  - ShortURL 创建 / 重定向流程在常规输入（无 CodeSignature / Signature 字段、或完整合法签名组合）下继续正常工作。
- `go build ./...` 与 `go vet ./...` 全部通过、无报错无告警。

## 6. 触发频率（Frequency）
必现（100%）：满足「空 key + 空 payload + 合法 hex 签名」或「Signer.Verify data 长度为 0 + hex 签名」条件时，单 goroutine 单次调用即可复现，`go test . -count=3` 三次重复全部触发。

## 7. 影响范围（Impact / Scope）
- 进程崩溃：创建短链接口、重定向鉴权、以及任何直接调用签名校验组件的模块都可能因为一条特定请求直接 panic，导致可用性下降（若部署有 recovery 中间件则为单请求 500；无 recovery 则进程退出影响整台实例）。
- 安全面：空 payload 但携带签名的组合被错误接受为「会触发崩溃」而不是被拒绝，容易被外部以极小代价触发 DoS（只需少量请求即可让进程重复 panic）。
- 业务行为一致性：签名校验逻辑在空 key 情况下静默返回 fallback 摘要而不是报错，错误结果扩散到上层之后可能导致签名校验被绕过、或接口返回与预期不一致的状态。
- 可观测性：panic 堆栈与日志会大量出现 nil dereference，影响正常排障与告警信噪比。

## 8. 附加说明（Additional Notes / Workaround）
- 临时规避（Workaround）：
  - 在接入层 / handler 层对传入的 code_signature、redirect signature、custom_code 尾部 hex 片段等字段做前置过滤：空 payload + 非空签名的组合直接拒绝返回 400，避免进入签名校验分支。
  - 对未配置签名密钥的部署（SignerSalt 为空 / signer key 为空），在创建请求与重定向入口处直接 skip 所有签名校验分支，避免把空 key 传给底层 Verifier。
- 相关日志样例：
  ```
  panic: runtime error: invalid memory address or nil pointer dereference
  [signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x...]
  goroutine ... [running]:
  .../crypto/hmac.New(...)
  .../pkg/cryptoutil.(*PayloadVerifier).Verify(...)
  .../pkg/validator.VerifyCustomCodeSignature(...)
  .../internal/model.(*CreateReq).validateCodeSignature(...)
  .../internal/model.(*CreateReq).Validate(...)
  ```
