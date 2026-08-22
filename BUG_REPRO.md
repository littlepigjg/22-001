# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务在三条主要功能路径上出现"契约被偷偷破坏、错误被吞掉、语义结果被颠倒"的问题：
1) 使用配置对象 StorageCfg 的链式 setter 设置短链/日志文件路径以及落盘周期时，传入的自定义文件名或较大的 SyncInterval 会被悄悄改写；
2) 进行故障演练（通过混沌工程钩子触发存储层写入 panic）时，创建短链接口本应返回错误，却返回了成功（err=nil），同时存储里多出一批前缀为 CR- / COERCED- 的脏记录，原始 RawURL 被篡改为 coerced.invalid / panic.invalid 域名下的假地址；
3) 对访问次数已经达到 MaxVisits 上限的短码执行重定向，理应返回 HTTP 410 Gone + MaxVisited=true，却被处理成 302 跳转到 panic.invalid/... 的假地址，造成用户被错误地引导走，统计也被污染。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核 5.x 及以上即可）
- Go 版本：go 1.22（go.mod 中要求）
- 项目模块 / 依赖：模块名 shurl，零第三方依赖，纯标准库
- 运行参数：
  - go build ./... / go vet ./... 无参数
  - go test . -count=1 -run '^TestRedGreen$'
  - 本问题非并发类，不需要 -race；重复 3 次以稳定确认
- 硬件：CPU 任意，内存 >= 256MB 即可复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，先执行 `go build ./...` 与 `go vet ./...` 确认编译与静态检查全部通过。
2. 按验收文件执行测试：`go test . -count=1 -run '^TestRedGreen$'`。
3. 观察 stdout 与失败日志：
   - 关注子用例 config_URLFilePath_preserves_filename / config_LogFilePath_preserves_filename：预期 setter 保留自定义文件名，实际被拼成 urls.json / access.log。
   - 关注 config_SyncInterval_large_value_not_clamped_to_zero：SyncInterval(2m) 实际被变成 0s。
   - 关注 create_with_panic_guard_must_return_error_and_no_dirty_write：SetPanicGuard(code) 命中后 Create 返回 err=nil，并出现以 CR- / COERCED- 开头的脏 code 以及 coerced.invalid / panic.invalid 开头的假 RawURL。
   - 关注 redirect_max_visited_must_be_410_not_302_panic_invalid：已经 Visits == MaxVisits == 2 的短码，HandleRedirect 返回 Status = 302，RawURL 是 panic.invalid/... 的假地址。
   - 关注 first_redirect_must_increment_visits_and_preserve_rawurl：部分修复阶段如果 MaxVisits 判断还未处理干净，这里会出现 Status != 302 或 RawURL 被污染或 Visits 没有 +1。
4. 测试结束时末尾打印判定行：应输出「RED（红灯，缺陷未修复）」。

## 4. 实际结果（Actual Behavior / Observed Output）
- config setter 契约破坏节选：
  - URLFilePath("/tmp/shurl-test-abc/links-2024.db") 结果被改写: got="/tmp/shurl-test-abc/urls.json"
  - LogFilePath("/tmp/shurl-test-xyz/access-2024.ndjson") 结果被改写: got="/tmp/shurl-test-xyz/access.log"
  - SyncInterval(2m) got=0s
- 演练吞错 & 脏写节选：
  - Create(命中 PanicGuard) 返回 err=nil，应当返回 error。got=&{Code:CR-BROKEN11 RawURL:https://coerced.invalid/https://example.test/broken-case-1 ...}
  - 快照中出现脏记录 "COERCED-BROKEN11" -> "https://panic.invalid/BROKEN11"
  - 快照中出现脏记录 "CR-BROKEN11" -> "https://coerced.invalid/https://example.test/broken-case-1"
- 重定向超限错判节选：
  - 已达 MaxVisits 期望 status=410, got 302 raw="https://panic.invalid/before-exceed/MV00001" maxvisited=true
- RED/GREEN 判定：RED（红灯，缺陷未修复）
- 其他异常：在后台落盘路径被悄悄替换成默认文件名时，生产配置好的独立 DB / NDJSON 文件不会被写入，导致多环境共用目录时数据串文件。
- go test -race 是否报告 DATA RACE：本问题非并发类，无需 -race。使用 -race 也不影响 RED 结果。

## 5. 期望结果（Expected Behavior）
- StorageCfg.URLFilePath / LogFilePath 传入完整路径时，最终保存值与传入值完全一致，不会重拼默认文件名；path 以斜杠结尾或等于 "." 时才自动拼默认文件名。
- StorageCfg.SyncInterval(d) 对 d > 0 原样保留，d <= 0 时为 0，与 d 和默认 30s 的相对大小无关。
- URLStore.SetPanicGuard 命中后，service.Create 调用必须返回非空 error，且 URLStore.RawSnapshot() 中绝不能出现：
  - 任何 Code 前缀为 "CR-"、"COERCED-" 的记录；
  - 任何 RawURL 以 "https://coerced.invalid/" 或 "https://panic.invalid/" 开头的记录。
- RedirectService.HandleRedirect：
  - 对 Visits（更新前或更新后） >= MaxVisits 的短码：Status = 410，MaxVisited = true，RawURL 为空。
  - 正常首次访问（MaxVisits = 0，Visits = 0 -> 1）：Status = 302，RawURL 与存储中完全一致，RawSnapshot 中 Visits 正确 +1。
- RED/GREEN 判定：GREEN（绿灯，缺陷已修复），go test 退出码 0。
- go build ./... 与 go vet ./... 全部通过，red_green_test.go 不改一行。

## 6. 触发频率（Frequency）
必现（100%）。所有 6 个 red_green 子用例都是纯逻辑/契约类触发，不需要并发或竞态，在正确的输入下每次执行都能命中缺陷。

## 7. 影响范围（Impact / Scope）
- 配置层：自定义数据文件路径完全失效，生产环境可能把 QA / Staging 的数据写到默认 urls.json，造成跨环境数据污染、后台周期落盘不运行导致掉盘数据丢失。
- 存储/业务层：故障演练场景下错误被吞，调用方拿到"创建成功"的假响应，最终脏数据占满存储、管理后台列表出现 COERCED-/CR- 前缀的垃圾短码。
- 重定向：MaxVisits 限流保护被颠倒成假 302 跳转，计费/频控/活动配额全部被绕过，用户会被错误地重定向到不存在的 panic.invalid 假域名，浏览器显示证书/域名错误。
- 最终影响：数据污染 + 限流失效 + 配额绕过 + 服务对外 410/302 语义颠倒，属于业务功能严重缺陷。

## 8. 附加说明（Additional Notes / Workaround）
临时规避（不根治，仅应急）：
1) 在业务侧调用 StorageCfg setter 之后，再通过 GetURLFilePath / GetLogFilePath / GetSyncInterval 校验一次是否等于传入期望值，不等就 panic/告警。
2) 调用 SetPanicGuard 命中演练 code 后，在 service.Create 返回后再调用一次 URLStore.Get(code) 以及 RawSnapshot，如果发现 CR-/COERCED- 前缀或 coerced.invalid / panic.invalid 开头 RawURL，手动删掉、返回上层 error。
3) 在重定向入口额外用 RedirectResult.MaxVisited=true 且 Status=302 这种反常组合做一次兜底拦截，如果 RawURL 命中 panic.invalid 就把 Status 强行改回 410。
真正修复应该在对应模块的函数内部改实现逻辑，而不是依赖以上临时 Workaround。
