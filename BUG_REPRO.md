# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务创建接口 `POST /api/urls` 在处理重复提交相同自定义短码（custom_code）时，应当返回 HTTP 409 Conflict 告知客户端该短码已存在；实际却返回了 HTTP 500 Internal Server Error，但响应 JSON 的 message 字段里仍然保留了"short code already exists"（或中文语义等价的 already exists 字样）这类文本，导致前端无法依据正确 HTTP 状态码判断是冲突还是真的服务端故障。

## 2. 环境信息（Environment）
- 操作系统：Linux (Kernel 5.x / 6.x)
- Go 版本：go1.22+
- 项目模块：shurl（根路径 module shurl，go.mod 无第三方依赖）
- 运行参数：`go test . -count=1 -run '^TestRedGreen$'`，或 `-race -count=5` 亦可触发
- 硬件信息：与 CPU 核数无关，纯串行场景即可稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 确保编译通过；`go vet ./...` 应无警告。
2. 打开命令行运行：`go test . -count=1 -run '^TestRedGreen$' -v`。
3. 观察测试输出中的 "Second POST status" 以及返回 JSON 的 `code`/`message` 字段。
4. 也可以手动启动服务，用 curl 连续发起两次：
   - `curl -s -i -X POST http://127.0.0.1:8080/api/urls -H 'Content-Type: application/json' -d '{"raw_url":"https://example.com/a","custom_code":"dup123"}'`
   - `curl -s -i -X POST http://127.0.0.1:8080/api/urls -H 'Content-Type: application/json' -d '{"raw_url":"https://example.com/b","custom_code":"dup123"}'`
5. 对比两次响应的 HTTP 状态码与响应体。

## 4. 实际结果（Actual Behavior / Observed Output）
- 首次 POST 返回 **201 Created**，正常。
- 第二次 POST 返回 **500 Internal Server Error**，响应体形如：
  ```json
  {"code":50000,"message":"store: RepeatCustomCode [dup123]: model: short code already exists","data":null}
  ```
- message 中包含 `already exists` 文本，证实底层确实是"重复短码冲突"，但被当作服务器内部错误处理了。
- RED/GREEN 判定结果：**RED（红灯，缺陷未修复）**，`go test` 退出码为 1。
- 非并发缺陷：无需 `-race`，仅串行执行即可稳定复现。

## 5. 期望结果（Expected Behavior）
- 无 panic；`go test -race` 无 DATA RACE 警告。
- 第二次重复提交同一 custom_code 必须返回 **HTTP 409 Conflict**，响应体形如：
  ```json
  {"code":40900,"message":"short code already exists","data":null}
  ```
- RED/GREEN 判定结果应为 **GREEN**，`go test` 退出码为 0。
- 其它领域错误（短码不存在返回 404、禁用/过期返回 410、记录超限 413/4xx、请求取消 499 等）也必须被正确映射到对应 HTTP 状态码，不应一律掉落到 500。
- `go build ./...` 与 `go vet ./...` 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。对同一 custom_code 连续 POST 两次，第二次一定会命中该错误返回 500。

## 7. 影响范围（Impact / Scope）
- API 契约被破坏：前端/客户端按约定 409 判断冲突并做重试或提示，现在会被当成 500 走"系统异常"分支，用户体验受损。
- 错误监控/告警失真：真实业务冲突被计入 5xx 错误率，造成假阳性告警，可能掩盖真正的服务端故障。
- 错误传播链断裂会导致相似症状蔓延：其它领域错误（不存在、禁用、过期等）在经相同链路包装后，也存在被误映射为 500 的风险，影响范围不止创建冲突一个接口。
- 无崩溃 panic，但客户端错误处理逻辑全面失配，整体可用性下降。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方案：客户端侧在 500 时额外检查响应 message 文本里是否含有 `already exists` / `not found` / `expired` 等关键词，然后自行模拟判断。但这是客户端 hack，必须在服务端修复错误传播链才能根本解决。
