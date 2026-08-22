# 缺陷复现报告（Bug Reproduction Report）

## 1. 问题概述（Summary）
短链服务的配置写入、校验、创建、重定向四个模块在回归测试中同时出现确定性异常：
配置 setter 赋值后读取到被篡改的路径，导致持久化文件落到错误位置，服务重启后历史短链全部 404；model 层对合法 7 位短码错误地返回字符/长度校验失败；创建接口在存储层异常场景下吞掉错误，调用方误以为成功但数据没写入；重定向接口的最大访问次数限制判断晚一拍（多放行一次 302）。
所有异常为 100% 必现，非并发偶发。

## 2. 环境信息（Environment）
- 操作系统：Linux（内核 5.x+）
- Go 版本：go1.22+（要求支持 modules 与 -race）
- 项目模块：module shurl（go.mod 中声明）
- 运行参数：
  - 普通验收：`go test . -count=1 -run '^TestRedGreen$' -v`
  - 非并发确定性复核（无 race 目的是证明纯逻辑缺陷）：`go test . -race -count=5 -run '^TestRedGreen$'`
- 硬件要求：不限，单核即可稳定复现

## 3. 复现步骤（Steps to Reproduce）
1. 进入项目根目录，执行 `go build ./...` 与 `go vet ./...`，确保编译与静态检查全部通过。
2. 执行 `go test -c -o /dev/null .`，确保测试文件能正常编译链接（测试编译门禁）。
3. 执行 `go test . -count=1 -run '^TestRedGreen$' -v`，观察打印的 FAIL 条目数量与内容，以及末尾的 RED/GREEN 判定和退出码。
4. 再执行 `go test . -race -count=5 -run '^TestRedGreen$'`，观察：
   - 每次运行是否稳定输出同样数量的 FAIL 项（即非并发、确定性问题）；
   - -race 是否输出任何 "WARNING: DATA RACE" 或 panic 堆栈（如无则证明是纯逻辑缺陷而非竞态）。
5. 如需观察配置篡改对持久化的影响：将 StorageCfg.URLFilePath("/tmp/urls.json") 赋值后用该 cfg 创建 URLStore，调用 Path() 或 Close 后到磁盘检查实际落盘的文件名（与传入路径做对比）。
6. 如需观察吞错：通过 SetPanicGuard 对某个自定义短码 code 返回 true，调用 Create(req{CustomCode:code})，检查返回值是否为 (nil, nil)。
7. 如需观察 MaxVisits 晚一拍：创建 MaxVisits=2 的短码，顺序调用 HandleRedirect 两次，检查第二次返回的 Status 是否仍是 302 而不是预期 410。

## 4. 实际结果（Actual Behavior / Observed Output）
按步骤 3 运行后观测到的实际行为：
- 输出 10 条 FAIL：
  1. setter URLFilePath 路径被加了 "_urls" 后缀（/tmp/x_urls.json → /tmp/x_urls.json_urls）；
  2. setter LogFilePath 的 ".log" 后缀被改为 ".json"；
  3~4. ValidateCode 对完全在合法字符集内的 abcAxyz、123I456 分别报 invalid character；
  5~7. ShortURL.Validate 对 abcdef1 / xyzABCD / my_code（7 位、带访问计数、不限次）分别报 length 或 invalid character 错误；
  8. URLStore.Path() 返回的文件名与 setter 原始输入不一致，导致 Close 后新 Store Load 读不到历史数据（持久化数据落到了错误路径）；
  9. Create 命中 panic guard 后返回 err=nil 且 ShortURL=nil，错误被服务层吞掉；
  10. MaxVisits=2 的短码在第 2 次 HandleRedirect 返回 status=302（应当 410）。
- 末尾判定打印：`RED（红灯，缺陷未修复）—— failed checks count = 10`。
- go test 退出码为非零（1）。
按步骤 4 运行 -race -count=5 的结果：
- 5 次运行均稳定 10 条 FAIL + RED（100% 必现，确定性逻辑缺陷）；
- 无任何 "WARNING: DATA RACE" 日志、无 sync/竞态相关 panic。

## 5. 期望结果（Expected Behavior）
修复后按同样步骤运行应出现以下正确行为：
- StorageCfg 的四个 setter 均"原样赋值"，赋值后读取字段值与传入参数字节完全相等；URLStore.Path() 与 AccessLogStore 打开的日志路径与 setter 传入的路径完全一致，Close → 新 Load 能正确读回所有记录。
- ValidateCode 仅拒绝真正不符合 [a-zA-Z0-9_-] 字符集或长度不在 [2, 32] 的短码；abcAxyz、123I456 等合法 7 位码一律通过。
- ShortURL.Validate 仅在字符集/长度违规、MaxVisits 负值、CreatedAt 为零值等真实非法状态下返回错误；unlimited（MaxVisits=0）且已有访问记录的 7 位短码一律通过校验。
- URLService.Create 在存储层返回 error 或触发 panic 时，一律将错误转换为非空 error 向上返回，不得返回 err==nil 却未落盘的静默成功；命中 panic guard 的 Create 调用应返回非空 error。
- RedirectService.HandleRedirect 在 IncrementVisits 出错时不应把错误静默包装成 302 返回；正常成功路径下 MaxVisits 限额必须基于 increment 后的访问数判断：MaxVisits=N 时第 N 次访问返回 410 而非第 N+1 次。
- 以上所有修复的最终表现是 failCount=0、末尾判定打印 `GREEN（绿灯，缺陷已修复）`、go test 退出码 0。
- `go build ./...`、`go vet ./...`、`go test -c -o /dev/null .` 全部通过；`go test -race -count=5 -run '^TestRedGreen$'` 全 5 次 GREEN 且无 DATA RACE 警告。

## 6. 触发频率（Frequency）
必现（100%）：
- 10 项 FAIL 在单次 go test 中全部出现；
- -race -count=5 连续 5 次复现，无抖动、无偶发通过。
  属于纯逻辑/契约型缺陷（非并发、非竞态），与 CPU 核数、时序无关。

## 7. 影响范围（Impact / Scope）
- **数据丢失/不可恢复**：服务重启后所有短链数据落到错误文件名，URLStore.Load 读不到，历史短链访问全部 404，运营链接大面积失效。
- **合法业务被拒**：正常用户自定义的 7 位短码（含元音字母）或达到一定访问计数的不限次短码无法通过校验，创建/更新/重定向链路被误中断。
- **数据不一致+调用方误判**：创建接口吞错导致 "err=nil" 但数据未落盘，上游平台以为创建成功实际没写入，后续 404 + 对账失败。
- **业务风控被绕过**：MaxVisits 限流（如付费链接限量 N 次）被多放行一次，计费/限量策略被击穿。
- **叠加效应**：四类缺陷分布在配置层→校验层→存储触发→服务错误处理→跳转限额判断整条链路，单项单独出现只影响局部，叠加后会让大部分业务路径都出现异常。

## 8. 附加说明（Additional Notes / Workaround）
临时规避（不推荐长期使用）：
- 路径问题：调用方在传入 setter 前可以手动做反向"预篡改"（如 URLFilePath 传入 p 时先手动去掉末尾 "_urls" 再传），但会把篡改逻辑泄漏到调用侧，后续修复后必须回滚，仅用于救急；
- 吞错问题：调用侧可以对 "return (nil, nil)" 这种非法返回多做一次校验（如果 err==nil && ShortURL==nil 则视为失败），但无法恢复数据已经没写入的事实；
- MaxVisits 问题：把 MaxVisits 设为 N-1（如要限 2 次就设 1），可以让第一次到达上限的行为回到预期，但这与语义不符，还会影响统计/计费报表。
正确修复必须从各模块内部实现逻辑上同时消除以上四类异常，才不会再出现 RED。无其他额外说明。
