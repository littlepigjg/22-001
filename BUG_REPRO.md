# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链项目中新增的「字符串 TTL / 时长」解析能力在遇到单字符未知单位（例如 y、z、q 等）的输入时，不会正常返回「非法单位」错误，而是直接触发 runtime panic（slice bounds out of range [:2] with length 1），导致当前请求处理 goroutine 崩溃；如果上层未做全局 panic 恢复，服务进程会被打挂。该问题可以通过直接调用时长解析函数、TTL 校验函数，或通过创建短链 API 的 `ttl_string` 参数、带 `+` 的复合写法、组合分隔写法等多条路径稳定复现。

## 2. 环境信息（Environment）
- 操作系统：Linux（任意现代发行版，内核无特殊要求）
- Go 版本：go1.22（项目 go.mod 指定 go 1.22）
- 项目模块名：shurl（纯标准库，无第三方依赖）
- 运行参数：执行测试命令 `go test . -count=3 -run '^TestRedGreen$' -v`；普通测试即可复现，无需 `-race`
- 硬件信息：任意 CPU（单核即可复现，与并发无关）

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录：`cd <project_root>`
2. 确认编译通过：`go build ./...`（预期无输出，返回码 0）
3. 确认静态检查通过：`go vet ./...`（预期无输出，返回码 0）
4. 执行回归测试：`go test . -count=3 -run '^TestRedGreen$' -v`
5. 或者在代码中分别手动触发以下任一调用（均可独立稳定复现 panic）：
   - `durationutil.ParseDuration("1y")`
   - `durationutil.ParseDuration("+1z")`
   - `durationutil.ParseDuration("t:1y")`
   - `validator.ValidateTTLString("1y")`
   - `validator.ValidateTTLString("1d + 2y")`
   - `durationutil.SplitAndSumDurations("1h,1q", ",")`
   - `validator.ParseTTLWithFallback("1x", 10*time.Second)`
   - `(&model.CreateReq{RawURL:"https://example.com/", TTLString:"1y"}).Validate()`
6. 观察 goroutine 的 panic 堆栈 / 测试输出中的 RED 判定。

## 4. 实际结果（Actual Behavior / Observed Output）
- 具体 panic 信息：
  ```
  panic: runtime error: slice bounds out of range [:2] with length 1
  ```
  go test 形式调用时被测试框架的 recover 捕获后输出：
  ```
  RED（红灯，缺陷未修复）
  runtime error: slice bounds out of range [:2] with length 1
  ```
- RED/GREEN 判定：RED（红灯，缺陷未修复）
- 其他异常现象：
  - 合法输入本身（如 "1d"、"1d6h"、"1周"、"[30m]"、"t:1d"、"1d + 6h"）理论上不应受影响，但只要测试集合中混入任意一个单字符未知 unit，测试就会 FAIL；
  - API 层若通过 CreateReq.TTLString 触发，该次 HTTP 请求处理会以 500 结束（依赖 RecoveryMiddleware 才能不把整进程打死）；
- 本问题与并发无关，`go test -race` 不报告 DATA RACE（不存在数据竞争），但仍然会 panic 并 FAIL。

## 5. 期望结果（Expected Behavior）
修复后按相同步骤运行应满足：
- 无 panic、无 goroutine 崩溃；错误输入以明确的 error 返回（例如 "invalid unit"、"parse ttl ..."、"invalid ttl_string ..." 等）
- RED/GREEN 判定：`go test . -count=3 -run '^TestRedGreen$' -v` 全部 PASS，并显式打印「GREEN（绿灯，缺陷已修复）」
- 具体正确业务行为：
  - `ParseDuration("1y")` 返回 error（未知单位 y），不 panic；
  - `ValidateTTLString("1y")` 返回 error（非法 ttl），不 panic；
  - `ValidateTTLString("1d + 2y")` 返回 error（复合里有非法段 y），不 panic；
  - `SplitAndSumDurations("1h,1q")` 返回 error（段 1q 非法），不 panic；
  - 合法输入必须仍然正确：
    - `ParseDuration("1d6h")` → `30h, nil`
    - `ParseDuration("t:1d")` → `24h, nil`
    - `ValidateTTLString("1周")` → `168h, nil`
    - `ValidateTTLString("1d + 6h")` → `30h, nil`
    - `ValidateTTLString("[30m]")` → `30m, nil`
    - `CreateReq{TTLString:"2d"}.Validate()` → nil 且 TTL 字段被写为 `48h`
- `go build ./...` 与 `go vet ./...` 全部通过。

## 6. 触发频率（Frequency）
必现（100%）。只要调用链上的输入满足「数字段 consume 后剩下一个单独的字符作为 unit」且该字符对应的两字节访问越界，就立刻稳定复现。count=3 连续跑 3 次均必现。

## 7. 影响范围（Impact / Scope）
- 请求级 panic：任意未被全局 RecoveryMiddleware 覆盖的内部调用会把当前 goroutine 拉炸；
- 线上可用性下降：创建短链 API 只要用户传入 TTL 字符串含单字符未知 unit 就直接 500（或进程级崩溃）；
- 错误传播链断裂：本来应该作为业务错误（400）优雅返回给用户的非法输入，变成服务端崩溃，用户无法感知错误原因；
- 管理后台与配置热加载风险：任何使用 ValidateTTLString / ParseTTLWithFallback / SplitAndSumDurations 的组件都可能被类似输入打挂；
- 无数据损坏风险（不涉及写盘与并发共享状态），但会导致服务频繁重启或请求不可用。

## 8. 附加说明（Additional Notes / Workaround）
临时规避方法：
- 在 API 入口层（handler 或网关）把 `ttl_string` 字段暂时回退为使用 `time.Duration` 数值字段（TTL），禁止用户传字符串形式的 TTL；
- 对 TTL 输入先做白名单前缀校验：仅允许以已知合法 unit（ns/us/ms/s/m/h/d/w）结尾的字符串再进入解析函数，其他直接在网关层返回 400。
以上仅为临时 workaround，根本修复仍需代码层面在解析入口与各个 unit 匹配位置补齐边界保护并统一错误返回。
